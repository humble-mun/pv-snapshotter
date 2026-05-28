//go:build linux

package config

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// RuntimeEntry describes one containerd runtime entry to inject.
type RuntimeEntry struct {
	// Name is the runtime handler name (e.g. "runc-pv").
	Name string
	// BaseRuntimeName is the source handler to copy configuration from
	// (e.g. "runc").  The generated entry clones the base's TOML sub-tree
	// and overrides only snapshotter = "pv-snapshotter".
	BaseRuntimeName string
}

// Params holds all parameters needed to patch /etc/containerd/config.toml.
type Params struct {
	// ConfigPath is the path to the containerd config file.
	ConfigPath string
	// SocketPath is the pv-snapshotter Unix socket advertised to containerd.
	SocketPath string
	// Runtimes is the list of runtime entries to inject.
	Runtimes []RuntimeEntry
}

// snippetHeader is written once before the first injected block.  It acts as
// a human-readable marker and is also checked to detect prior runs.
const snippetHeader = "\n# BEGIN pv-snapshotter managed block\n"

// snippetFooter closes the managed block.
const snippetFooter = "# END pv-snapshotter managed block\n"

// Apply patches configPath to add:
//  1. [proxy_plugins.pv-snapshotter] (if absent)
//  2. [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.<name>] for each
//     runtime in params.Runtimes (if absent), cloning the base runtime's config
//     and overriding snapshotter = "pv-snapshotter".
//
// Returns modified=true when the file was changed, false when every section
// was already present.  Returns a non-nil error on I/O or parse failures.
func Apply(params Params) (modified bool, err error) {
	raw, err := os.ReadFile(params.ConfigPath)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", params.ConfigPath, err)
	}

	// Parse into a generic map to detect existing sections — we do NOT
	// re-serialise the whole tree (that would clobber defaults/comments).
	var tree map[string]any
	if err = toml.Unmarshal(raw, &tree); err != nil {
		return false, fmt.Errorf("parsing %s: %w", params.ConfigPath, err)
	}

	missing := collectMissing(tree, params)
	if len(missing) == 0 {
		return false, nil
	}

	snippet := buildSnippet(params.SocketPath, missing)
	if err = appendToFile(params.ConfigPath, snippet); err != nil {
		return false, err
	}
	return true, nil
}

// missingSection describes a section that needs to be injected.
type missingSection struct {
	// kind is "proxy_plugin" or "runtime".
	kind string
	// runtimeName is the new handler name (e.g. "runc-pv").
	runtimeName string
	// baseConfig is the base runtime's existing TOML sub-tree, cloned and
	// modified to produce the new runtime entry.  May be nil when the base
	// runtime is not present in config.toml (fresh install).
	baseConfig map[string]any
}

// collectMissing returns the sections that are absent from the parsed tree.
func collectMissing(tree map[string]any, params Params) []missingSection {
	var missing []missingSection

	// Check [proxy_plugins.pv-snapshotter].
	if !hasKey(tree, "proxy_plugins", "pv-snapshotter") {
		missing = append(missing, missingSection{kind: "proxy_plugin"})
	}

	// Check each runtime entry.
	for _, rt := range params.Runtimes {
		if !hasRuntimeEntry(tree, rt.Name) {
			missing = append(missing, missingSection{
				kind:        "runtime",
				runtimeName: rt.Name,
				baseConfig:  lookupBaseRuntime(tree, rt.BaseRuntimeName),
			})
		}
	}
	return missing
}

// lookupBaseRuntime returns a deep copy of the base runtime's TOML sub-tree
// (the map under runtimes.<baseName>), or nil when the base is not found.
// Checks both the version-2 and version-3 CRI plugin keys.
func lookupBaseRuntime(tree map[string]any, baseName string) map[string]any {
	if baseName == "" {
		return nil
	}
	plugins, ok := tree["plugins"].(map[string]any)
	if !ok {
		return nil
	}
	for _, criKey := range []string{"io.containerd.grpc.v1.cri", "io.containerd.cri.v1.runtime"} {
		cri, ok := plugins[criKey].(map[string]any)
		if !ok {
			continue
		}
		ctrd, ok := cri["containerd"].(map[string]any)
		if !ok {
			continue
		}
		runtimes, ok := ctrd["runtimes"].(map[string]any)
		if !ok {
			continue
		}
		base, ok := runtimes[baseName].(map[string]any)
		if !ok {
			continue
		}
		return deepCopyMap(base)
	}
	return nil
}

// deepCopyMap returns a recursive deep copy of m so that mutations to the
// returned map do not affect the original parsed tree.
func deepCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if sub, ok := v.(map[string]any); ok {
			out[k] = deepCopyMap(sub)
		} else {
			out[k] = v
		}
	}
	return out
}

// hasKey walks the generic map tree along the given path segments.
// Returns true only when every segment exists.
func hasKey(tree map[string]any, path ...string) bool {
	cur := tree
	for i, seg := range path {
		v, ok := cur[seg]
		if !ok {
			return false
		}
		if i == len(path)-1 {
			return true
		}
		next, ok := v.(map[string]any)
		if !ok {
			return false
		}
		cur = next
	}
	return true
}

// hasRuntimeEntry checks whether the containerd CRI runtime entry with the
// given name already exists.  The TOML key path differs between config
// version 2 and version 3:
//
//	version 2: plugins."io.containerd.grpc.v1.cri".containerd.runtimes.<name>
//	version 3: plugins."io.containerd.cri.v1.runtime".containerd.runtimes.<name>
//
// We check both paths so the function works regardless of config version.
func hasRuntimeEntry(tree map[string]any, name string) bool {
	plugins, ok := tree["plugins"].(map[string]any)
	if !ok {
		return false
	}
	for _, criKey := range []string{"io.containerd.grpc.v1.cri", "io.containerd.cri.v1.runtime"} {
		cri, ok := plugins[criKey].(map[string]any)
		if !ok {
			continue
		}
		ctrd, ok := cri["containerd"].(map[string]any)
		if !ok {
			continue
		}
		runtimes, ok := ctrd["runtimes"].(map[string]any)
		if !ok {
			continue
		}
		if _, ok := runtimes[name]; ok {
			return true
		}
	}
	return false
}

// buildSnippet constructs the literal TOML text to append.
// It opens with snippetHeader and closes with snippetFooter.
func buildSnippet(socketPath string, sections []missingSection) string {
	var sb strings.Builder
	sb.WriteString(snippetHeader)

	for _, s := range sections {
		switch s.kind {
		case "proxy_plugin":
			sb.WriteString(proxyPluginSnippet(socketPath))
		case "runtime":
			sb.WriteString(runtimeSnippet(s.runtimeName, s.baseConfig))
		}
	}

	sb.WriteString(snippetFooter)
	return sb.String()
}

// proxyPluginSnippet returns the [proxy_plugins.pv-snapshotter] TOML block.
func proxyPluginSnippet(socketPath string) string {
	return fmt.Sprintf(`
[proxy_plugins.pv-snapshotter]
  type    = "snapshot"
  address = %q

`, socketPath)
}

// runtimeSnippet returns the TOML block for a new CRI runtime entry.
//
// When baseConfig is non-nil (the base runtime exists in config.toml), the
// entry is produced by cloning the base sub-tree and overriding snapshotter.
// This preserves user settings such as SystemdCgroup, BinaryName, etc.
//
// When baseConfig is nil (fresh install, base not yet written), a minimal
// entry with just runtime_type and snapshotter is emitted.
//
// The version-2 plugin key is used because it is accepted by both containerd
// 1.x and 2.x.  We only append a new [runtimes.<name>] sub-table and never
// touch the parent CRI table, so the 1.x whole-section-replace merge bug is
// not triggered.
func runtimeSnippet(name string, baseConfig map[string]any) string {
	const criPrefix = `plugins."io.containerd.grpc.v1.cri".containerd.runtimes.`

	var cfg map[string]any
	if baseConfig != nil {
		cfg = baseConfig
	} else {
		cfg = map[string]any{
			"runtime_type": "io.containerd.runc.v2",
		}
	}
	// Override (or set) snapshotter regardless of what the base had.
	cfg["snapshotter"] = "pv-snapshotter"

	// Separate top-level fields from sub-tables so we can emit them in the
	// correct TOML order: scalars first, then [sub-tables].
	scalars := make(map[string]any)
	subtables := make(map[string]map[string]any)
	for k, v := range cfg {
		if sub, ok := v.(map[string]any); ok {
			subtables[k] = sub
		} else {
			scalars[k] = v
		}
	}

	var sb strings.Builder

	// Emit the top-level runtime entry with its scalar fields.
	sb.WriteString("\n[" + criPrefix + name + "]\n")
	if len(scalars) > 0 {
		b, err := toml.Marshal(scalars)
		if err == nil {
			sb.Write(indentTOML(b, "  "))
		}
	}
	sb.WriteString("\n")

	// Emit each sub-table (e.g. "options") as a separate TOML section.
	for subKey, subMap := range subtables {
		sb.WriteString("[" + criPrefix + name + "." + subKey + "]\n")
		b, err := toml.Marshal(subMap)
		if err == nil {
			sb.Write(indentTOML(b, "  "))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// indentTOML prepends prefix to every non-empty line of b.
func indentTOML(b []byte, prefix string) []byte {
	lines := bytes.Split(b, []byte("\n"))
	var out []byte
	for _, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			out = append(out, '\n')
			continue
		}
		out = append(out, []byte(prefix)...)
		out = append(out, line...)
		out = append(out, '\n')
	}
	return out
}

// appendToFile opens path in append mode and writes text.
func appendToFile(path, text string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening %s for append: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err = f.WriteString(text); err != nil {
		return fmt.Errorf("writing to %s: %w", path, err)
	}
	return nil
}
