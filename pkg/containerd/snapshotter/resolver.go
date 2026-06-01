//go:build linux

package snapshotter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	typeurlv2 "github.com/containerd/typeurl/v2"
	"github.com/go-logr/logr"
	"github.com/spf13/viper"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// defaultAnnotationPrefix is the default DNS subdomain prefix for all
	// pv-snapshotter pod annotations. Configurable via --annotation-prefix.
	defaultAnnotationPrefix = "pv-snapshotter.humble-mun.io"

	// Annotation name suffixes (appended to prefix + "/").
	//
	//   <prefix>/upperdir-path          – literal upperdir root path
	//   <prefix>/upperdir-path-template – Go template rendered to upperdir root path
	//   <prefix>/var.<VarName>          – template variable injected into template data
	annotationSuffixUpperdirPath         = "upperdir-path"
	annotationSuffixUpperdirPathTemplate = "upperdir-path-template"
	annotationSuffixVarSubPrefix         = "var."

	// criSandboxMetadataExtension is the containerd container extension key
	// where the CRI plugin stores the full PodSandboxConfig (pod annotations
	// included).
	criSandboxMetadataExtension = "io.cri-containerd.sandbox.metadata"

	// criKindLabel / criKindSandbox / criKindContainer distinguish sandbox
	// containers from workload containers in containerd Labels.
	criKindLabel     = "io.cri-containerd.kind"
	criKindSandbox   = "sandbox"
	criKindContainer = "container"

	// defaultContainerdSocket is the default containerd gRPC socket path.
	defaultContainerdSocket = "/run/containerd/containerd.sock"

	// containerdNamespaceK8s is the containerd namespace used by the CRI
	// plugin for all Kubernetes workloads.
	containerdNamespaceK8s = "k8s.io"

	// reservedAnnotationPrefixKubernetes and reservedAnnotationPrefixK8s are
	// the two Kubernetes-reserved DNS subdomains that must not be used as the
	// pv-snapshotter annotation prefix.
	reservedAnnotationPrefixKubernetes = "kubernetes.io"
	reservedAnnotationPrefixK8s        = "k8s.io"
)

// sandboxMetadata mirrors the versioned wrapper used by containerd CRI:
//
//	{ "Version": "v1", "Metadata": { "Config": { "metadata": { "uid": "..." }, "annotations": {...} } } }
type sandboxMetadata struct {
	Metadata sandboxMetadataInner `json:"Metadata"`
}

type sandboxMetadataInner struct {
	Config *podSandboxConfig `json:"Config"`
}

type podSandboxConfig struct {
	// Metadata carries the pod identity fields (name, uid, namespace).
	Metadata    *podObjectMeta    `json:"metadata"`
	Annotations map[string]string `json:"annotations"`
}

// podObjectMeta mirrors k8s ObjectMeta fields available in PodSandboxConfig.
type podObjectMeta struct {
	Name      string `json:"name"`
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
}

// resolver looks up the upperdir-path annotation from a snapshot key.
// It must be called at Mounts() time (not Prepare()), because containerd
// container records are created after Prepare() returns.
type resolver struct {
	client *containerd.Client
	logger logr.Logger
	// Derived at construction time from the configured annotation prefix.
	keyLiteral   string // <prefix>/upperdir-path
	keyTemplate  string // <prefix>/upperdir-path-template
	keyVarPrefix string // <prefix>/var.
}

// validateAnnotationPrefix checks that prefix is a valid Kubernetes annotation
// key prefix (RFC 1123 DNS subdomain, ≤253 chars, no trailing slash).
//
// The reserved prefixes "kubernetes.io" and "k8s.io" are rejected to avoid
// conflicts with built-in annotations.
func validateAnnotationPrefix(prefix string) error {
	if strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("annotation prefix must not include a trailing slash: %q", prefix)
	}
	if errs := validation.IsDNS1123Subdomain(prefix); len(errs) > 0 {
		return fmt.Errorf("annotation prefix %q is not a valid DNS subdomain: %s",
			prefix, strings.Join(errs, "; "))
	}
	if prefix == reservedAnnotationPrefixKubernetes || prefix == reservedAnnotationPrefixK8s ||
		strings.HasSuffix(prefix, "."+reservedAnnotationPrefixKubernetes) || strings.HasSuffix(prefix, "."+reservedAnnotationPrefixK8s) {
		return fmt.Errorf("annotation prefix %q uses a reserved Kubernetes domain", prefix)
	}
	return nil
}

func newResolver(logger logr.Logger, client *containerd.Client) (*resolver, error) {
	prefix := viper.GetString(flagAnnotationPrefix)
	if err := validateAnnotationPrefix(prefix); err != nil {
		return nil, fmt.Errorf("invalid --annotation-prefix: %w", err)
	}
	base := prefix + "/"

	return &resolver{
		client:       client,
		logger:       logger.WithName("resolver"),
		keyLiteral:   base + annotationSuffixUpperdirPath,
		keyTemplate:  base + annotationSuffixUpperdirPathTemplate,
		keyVarPrefix: base + annotationSuffixVarSubPrefix,
	}, nil
}

// resolveAtMountsTime extracts the upperdir-path annotation for the container
// identified by key. Must be called at Mounts() time when the container record
// already exists in containerd.
//
// Returns ("", nil) when the key is not a pv-backed container (pass-through).
// Returns ("", err) on unexpected failures.
func (r resolver) resolveAtMountsTime(_ context.Context, key string) (upperdirPath string, err error) {
	ns, containerID, ok := parseSnapshotKey(key)
	if !ok {
		return
	}

	// Fresh outgoing context — do not reuse the gRPC server-side incoming ctx.
	nsCtx := namespaces.WithNamespace(context.Background(), ns)

	r.logger.V(4).Info("resolving container metadata", "key", key)

	// Query the container record (exists by the time Mounts() is called).
	ctrs, err := r.client.Containers(nsCtx, fmt.Sprintf("id==%s", containerID))
	if err != nil {
		err = fmt.Errorf("querying container %s: %w", containerID, err)
		return
	}
	if len(ctrs) == 0 {
		r.logger.V(4).Info("container not found, skipping resolution", "key", key)
		return
	}

	info, err := ctrs[0].Info(nsCtx)
	if err != nil {
		err = fmt.Errorf("getting container info %s: %w", containerID, err)
		return
	}
	r.logger.V(4).Info("resolved container info",
		"key", key, "kind", info.Labels[criKindLabel], "sandboxID", info.SandboxID)

	kind := info.Labels[criKindLabel]
	switch kind {
	case criKindSandbox:
		// Sandbox (pause) containers do not need upperdir redirection.
		// Only workload containers carry business writes; the pause container
		// writes nothing meaningful to its overlay upperdir.
		//
		// More importantly, both the sandbox and the first workload container
		// belong to the same Pod and therefore share the same annotation
		// (upperdirPath).  Redirecting both mounts to <upperdirPath>/upper and
		// <upperdirPath>/work would cause an overlayfs mount conflict: the kernel
		// requires each overlay mount's workdir to be exclusive — a second mount
		// using the same workdir is rejected with EBUSY.
		//
		// Skipping redirection for the sandbox means its writable layer lives
		// under the native overlay snapshotter root and is discarded on pod
		// deletion.  This is fine: pause writes nothing.
		r.logger.V(4).Info("sandbox container: skipping upperdir redirection", "key", key)
		return

	case criKindContainer:
		// Workload container: look up the parent sandbox by SandboxID.
		if info.SandboxID == "" {
			r.logger.V(4).Info("workload container has no sandbox ID, skipping resolution", "key", key)
			return
		}
		upperdirPath, err = r.upperdirFromSandboxID(nsCtx, info.SandboxID, key)

	default:
		r.logger.V(4).Info("unknown container kind, skipping resolution",
			"key", key, "kind", kind)
	}
	return
}

// upperdirFromSandboxContainer reads the upperdir-path (or upperdir-path-template)
// annotation from the sandbox container's io.cri-containerd.sandbox.metadata extension.
func (r resolver) upperdirFromSandboxContainer(
	ctx context.Context,
	ctr containerd.Container,
	key string,
) (upperdirPath string, err error) {

	exts, err := ctr.Extensions(ctx)
	if err != nil {
		err = fmt.Errorf("reading extensions for sandbox (key=%s): %w", key, err)
		return
	}
	r.logger.V(4).Info("listing sandbox extensions",
		"key", key, "extKeys", extensionKeys(exts))

	extAny, ok := exts[criSandboxMetadataExtension]
	if !ok {
		r.logger.V(4).Info("sandbox metadata extension not present", "key", key)
		return
	}

	// The extension is JSON-encoded (typeurl uses json.Marshal for non-proto
	// types). Read raw bytes and unmarshal into our local struct.
	raw := extAny.GetValue()
	r.logger.V(5).Info("decoded sandbox metadata",
		"key", key, "json", string(raw))

	var meta sandboxMetadata
	if err = json.Unmarshal(raw, &meta); err != nil {
		err = fmt.Errorf("unmarshaling sandbox metadata (key=%s): %w", key, err)
		return
	}
	if meta.Metadata.Config == nil {
		r.logger.V(4).Info("sandbox metadata config is nil", "key", key)
		return
	}

	cfg := meta.Metadata.Config
	r.logger.V(4).Info("read pod annotations", "key", key, "annotations", cfg.Annotations)

	// Fast path: literal upperdir-path annotation takes precedence.
	if v := cfg.Annotations[r.keyLiteral]; v != "" {
		upperdirPath = v
		r.logger.V(4).Info("resolved upperdir path (literal)",
			"key", key, "upperdirPath", upperdirPath)
		return
	}

	// Template path: render upperdir-path-template with pod fields + custom vars.
	tmplStr := cfg.Annotations[r.keyTemplate]
	if tmplStr == "" {
		r.logger.V(4).Info("no upperdir annotation found, skipping pv-backed routing", "key", key)
		return
	}
	r.logger.V(4).Info("found upperdir-path-template annotation, rendering",
		"key", key, "template", tmplStr)

	upperdirPath, err = r.renderUpperdirTemplate(tmplStr, cfg, key)
	return
}

// renderUpperdirTemplate renders tmplStr with a data map built from:
//   - built-in fields: PodUID, PodName, PodNamespace
//   - custom fields: annotations with keyVarPrefix stripped
func (r resolver) renderUpperdirTemplate(
	tmplStr string,
	cfg *podSandboxConfig,
	key string,
) (result string, err error) {

	data := make(map[string]string)

	// Built-in fields from pod identity.
	if cfg.Metadata != nil {
		data["PodUID"] = cfg.Metadata.UID
		data["PodName"] = cfg.Metadata.Name
		data["PodNamespace"] = cfg.Metadata.Namespace
	}

	// Custom fields: collect all annotations under keyVarPrefix.
	for k, v := range cfg.Annotations {
		if !strings.HasPrefix(k, r.keyVarPrefix) {
			continue
		}
		varName := strings.TrimPrefix(k, r.keyVarPrefix)
		data[varName] = v
	}

	if logger := r.logger.V(4); logger.Enabled() {
		vars := make(map[string]string)
		for k, v := range data {
			if k != "PodUID" && k != "PodName" && k != "PodNamespace" {
				vars[k] = v
			}
		}
		logger.Info("template data assembled",
			"key", key,
			"builtins", map[string]string{"PodUID": data["PodUID"], "PodName": data["PodName"], "PodNamespace": data["PodNamespace"]},
			"vars", vars)
	}

	tmpl, err := template.New("upperdir-path").Parse(tmplStr)
	if err != nil {
		err = fmt.Errorf("parsing upperdir-path-template (key=%s): %w", key, err)
		return
	}

	var buf bytes.Buffer
	if err = tmpl.Execute(&buf, data); err != nil {
		err = fmt.Errorf("rendering upperdir-path-template (key=%s): %w", key, err)
		return
	}

	result = buf.String()
	r.logger.V(4).Info("resolved upperdir path (template)",
		"key", key, "upperdirPath", result)
	return
}

// upperdirFromSandboxID queries the sandbox container by ID and delegates to
// upperdirFromSandboxContainer. key is the original snapshot key, passed
// through for log correlation.
func (r resolver) upperdirFromSandboxID(ctx context.Context, sandboxID string, key string) (upperdirPath string, err error) {
	r.logger.V(4).Info("looking up sandbox by ID", "key", key, "sandboxID", sandboxID)

	ctrs, err := r.client.Containers(ctx, fmt.Sprintf("id==%s", sandboxID))
	if err != nil {
		err = fmt.Errorf("querying sandbox container %s: %w", sandboxID, err)
		return
	}
	if len(ctrs) == 0 {
		r.logger.V(4).Info("sandbox container not found, skipping resolution",
			"key", key, "sandboxID", sandboxID)
		return
	}
	upperdirPath, err = r.upperdirFromSandboxContainer(ctx, ctrs[0], key)
	return
}

// extensionKeys returns the keys in an extensions map for compact logging.
func extensionKeys(exts map[string]typeurlv2.Any) []string {
	keys := make([]string, 0, len(exts))
	for k := range exts {
		keys = append(keys, k)
	}
	return keys
}

// parseSnapshotKey extracts the namespace and original container ID from the
// metadata-layer key "<namespace>/<seq>/<container-id>".
// Returns ok=false for image-unpack keys (contain spaces or "extract-" prefix).
func parseSnapshotKey(key string) (ns, containerID string, ok bool) {
	parts := strings.SplitN(key, "/", 3)
	if len(parts) != 3 {
		return
	}
	ns, containerID = parts[0], parts[2]
	// Image layer unpack keys: "k8s.io/8/extract-736531728-zbNy sha256:..."
	if strings.Contains(containerID, " ") || strings.HasPrefix(containerID, "extract-") {
		return "", "", false
	}
	ok = true
	return
}
