//go:build linux

package snapshotter

import (
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/containerd/containerd/v2/core/mount"
)

// ensureUpperdirReady validates that upperdirRoot is an existing mountpoint and
// creates the upper/ and work/ subdirectories required by overlayfs.
//
// Returning an error here causes Mounts() to fail hard — intentional, because
// silently falling back to native overlay would cause undetected state loss.
func ensureUpperdirReady(upperdirRoot string) error {
	// Confirm the root path exists.
	if _, err := os.Stat(upperdirRoot); err != nil {
		return fmt.Errorf("upperdir root %q not accessible: %w", upperdirRoot, err)
	}

	// Confirm it is a mountpoint by comparing device IDs with its parent.
	// A path is a mountpoint when its device ID differs from its parent's.
	var rootStat, parentStat syscall.Stat_t
	if err := syscall.Stat(upperdirRoot, &rootStat); err != nil {
		return fmt.Errorf("stat upperdir root %q: %w", upperdirRoot, err)
	}
	// Derive parent path: trim trailing slash then take dirname equivalent.
	parent := strings.TrimRight(upperdirRoot, "/")
	if idx := strings.LastIndex(parent, "/"); idx >= 0 {
		parent = parent[:idx]
	}
	if parent == "" {
		parent = "/"
	}
	if err := syscall.Stat(parent, &parentStat); err != nil {
		return fmt.Errorf("stat parent of upperdir root %q: %w", upperdirRoot, err)
	}
	if rootStat.Dev == parentStat.Dev {
		return fmt.Errorf("upperdir root %q is not a mountpoint (CSI volume not yet mounted)", upperdirRoot)
	}

	// Create upper/ and work/ subdirectories; both must live on the same
	// filesystem (hard overlayfs requirement).
	for _, sub := range []string{"upper", "work"} {
		dir := upperdirRoot + "/" + sub
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s dir %q: %w", sub, dir, err)
		}
	}
	return nil
}

// replaceUpperdirOptions returns a copy of mounts with the upperdir= and
// workdir= options in mounts[0] replaced to point at upperdirRoot/upper and
// upperdirRoot/work respectively. lowerdir= and all other options are
// preserved unchanged.
//
// Returns the original mounts unmodified when:
//   - mounts is empty
//   - mounts[0].Type is not "overlay"
//   - mounts[0].Options contains no "upperdir=" entry (read-only snapshot)
func replaceUpperdirOptions(mounts []mount.Mount, upperdirRoot string) []mount.Mount {
	if len(mounts) == 0 {
		return mounts
	}
	m := mounts[0]
	if m.Type != "overlay" {
		return mounts
	}

	// Check whether this is a writable snapshot (has upperdir=).
	hasUpperdir := false
	for _, opt := range m.Options {
		if strings.HasPrefix(opt, "upperdir=") {
			hasUpperdir = true
			break
		}
	}
	if !hasUpperdir {
		return mounts
	}

	newOpts := make([]string, 0, len(m.Options))
	for _, opt := range m.Options {
		switch {
		case strings.HasPrefix(opt, "upperdir="):
			newOpts = append(newOpts, "upperdir="+upperdirRoot+"/upper")
		case strings.HasPrefix(opt, "workdir="):
			newOpts = append(newOpts, "workdir="+upperdirRoot+"/work")
		default:
			newOpts = append(newOpts, opt)
		}
	}

	result := make([]mount.Mount, len(mounts))
	copy(result, mounts)
	result[0].Options = newOpts
	return result
}
