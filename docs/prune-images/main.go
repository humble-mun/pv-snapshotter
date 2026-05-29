// prune-images.go
//
// One-shot tool to delete containerd image RECORDS whose backing content
// blobs are actually missing FOR THE CURRENT NODE'S PLATFORM, so that
// kubelet stops believing the image is present and re-pulls it.
//
// BACKGROUND (pv-snapshotter v0.1.4 incident, doc §5.4 缺口2 — the deadlock core):
//   A GC pass reclaimed layer blobs but left the image record (manifest
//   entry in containerd's image store) intact.  kubelet's PullIfNotPresent
//   then sees the image record, reports "image present", skips PullImage,
//   and the missing blobs are never re-downloaded — so the on-demand unpack
//   at CreateContainer fails ("missing parent" / "content digest not found").
//   Deleting just the broken image record forces kubelet to re-pull.
//
// PLATFORM-SCOPED — THE HARD CONSTRAINT (doc §5.6 / §6 / §9):
//   `ctr images check` reports "incomplete" for healthy multi-arch images
//   merely because attestation/SBOM/other-architecture layers are absent.
//   Blanket "delete incomplete" deletes healthy in-use images (even this
//   recovery image itself).  This tool resolves ONLY the current node's
//   platform manifest (platforms.Default) and verifies ONLY that platform's
//   config + layer blobs.  An image missing some OTHER platform's layers is
//   left untouched.
//
// SAFETY:
//   - Defaults to --dry-run.  Pass --apply to actually delete records.
//   - Phase 1 always runs read-only first and logs the complete plan, so the
//     intended deletions can be audited before --apply is used.
//   - Deletion is per-record and synchronous (images.SynchronousDelete).
//   - This tool NEVER touches the content store, snapshots, or metadata
//     buckets directly — it only calls the containerd image store Delete API.
//     Deleting an image record does not delete shared blobs that other images
//     still reference; containerd GC handles that safely.
//   - containerd MUST be running (the tool talks to its socket).  Run it
//     AFTER `fix-meta --apply` + containerd restart, BEFORE `systemctl start kubelet`.
//
// Usage:
//   go build -o prune-images main.go
//   ./prune-images                                   # dry-run, k8s.io ns, host sock
//   ./prune-images --apply                           # commit deletions
//   ./prune-images --socket /run/containerd/containerd.sock --namespace k8s.io --apply
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	defaultSocket    = "/run/containerd/containerd.sock"
	defaultNamespace = "k8s.io"
)

func main() {
	args := os.Args[1:]
	apply := false
	socket := defaultSocket
	namespace := defaultNamespace
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--apply":
			apply = true
		case "--socket":
			i++
			if i >= len(args) {
				fatalf("--socket requires a value")
			}
			socket = args[i]
		case "--namespace", "-n":
			i++
			if i >= len(args) {
				fatalf("--namespace requires a value")
			}
			namespace = args[i]
		case "-h", "--help":
			usage()
			return
		default:
			fmt.Fprintln(os.Stderr, "ERROR: unexpected argument:", a)
			usage()
			os.Exit(2)
		}
	}

	logf("=== prune-images starting ===")
	logf("socket      : %s", socket)
	logf("namespace   : %s", namespace)
	logf("apply mode  : %v", apply)
	if !apply {
		logf("(dry-run — no records will be deleted; pass --apply to commit)")
	}

	platform := platforms.DefaultSpec()
	logf("node platform: %s/%s%s", platform.OS, platform.Architecture, variantSuffix(platform.Variant))
	matcher := platforms.Default()

	ctx := namespaces.WithNamespace(context.Background(), namespace)

	c, err := client.New(socket, client.WithDefaultNamespace(namespace))
	if err != nil {
		fatalf("connect containerd at %s: %v", socket, err)
	}
	defer c.Close()

	imgStore := c.ImageService()
	cs := c.ContentStore()

	// ── Phase 1: read-only scan, build the deletion plan ────────────────────
	logf("--- Phase 1: scanning image records (read-only) ---")
	all, err := imgStore.List(ctx)
	if err != nil {
		fatalf("list images: %v", err)
	}
	logf("found %d image record(s) in namespace %q", len(all), namespace)

	var doomed []verdict
	healthy := 0
	for _, img := range all {
		v := inspect(ctx, cs, img, matcher)
		switch v.action {
		case actionDelete:
			doomed = append(doomed, v)
			logf("  %-60q -> WILL DELETE (%s)", img.Name, v.reason)
		case actionKeep:
			healthy++
			logf("  %-60q -> keep (%s)", img.Name, v.reason)
		}
	}
	logf("plan summary: %d record(s) to delete, %d healthy, %d total", len(doomed), healthy, len(all))

	if len(doomed) == 0 {
		logf("nothing to delete; all image records have their node-platform blobs present.")
		logf("=== prune-images done ===")
		return
	}

	if !apply {
		logf("--- Phase 2: SKIPPED (dry-run).  Re-run with --apply to commit. ---")
		logf("=== prune-images done (dry-run) ===")
		return
	}

	// ── Phase 2: delete the broken records (synchronous) ────────────────────
	logf("--- Phase 2: deleting broken image records ---")
	deleted := 0
	failed := 0
	for _, v := range doomed {
		if err := imgStore.Delete(ctx, v.name, images.SynchronousDelete()); err != nil {
			if errdefs.IsNotFound(err) {
				// Raced with another deleter; treat as success.
				logf("  deleted (already gone): %q", v.name)
				deleted++
				continue
			}
			logf("  ERROR deleting %q: %v", v.name, err)
			failed++
			continue
		}
		logf("  deleted: %q (%s)", v.name, v.reason)
		deleted++
	}
	logf("delete summary: %d deleted, %d failed", deleted, failed)
	if failed > 0 {
		logf("=== prune-images done WITH ERRORS ===")
		os.Exit(1)
	}
	logf("kubelet will re-pull the deleted images on next pod admission.")
	logf("=== prune-images done ===")
}

type actionKind int

const (
	actionKeep actionKind = iota
	actionDelete
)

type verdict struct {
	name   string
	action actionKind
	reason string
}

// inspect resolves the node-platform manifest for one image and verifies that
// its config blob and every layer blob exist in the content store.
//
// Deletion verdict is returned ONLY when the current node's platform manifest
// is resolvable-but-broken, OR is unresolvable because its own manifest/index
// content is missing.  Images that simply do not target this node's platform
// at all are KEPT (platform-scoped guarantee, doc §6).
func inspect(ctx context.Context, provider content.Store, img images.Image, matcher platforms.MatchComparer) verdict {
	// images.Manifest reads the index/manifest blobs from the content store and
	// selects the descriptor matching `matcher`.  Errors fall into two classes:
	//   - not-found: the manifest/index blob (or the node-platform child
	//     manifest) is missing from content -> the record is broken, delete.
	//   - no-match : the image genuinely has no manifest for this platform
	//     (e.g. an arm64-only image on an amd64 node) -> KEEP, not our problem.
	manifest, err := images.Manifest(ctx, provider, img.Target, matcher)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return verdict{img.Name, actionDelete, fmt.Sprintf("node-platform manifest/content not found: %v", err)}
		}
		// Any non-not-found error (including "no match for platform") is treated
		// conservatively as KEEP — we never delete on ambiguity.
		return verdict{img.Name, actionKeep, fmt.Sprintf("manifest not resolved for node platform, leaving intact: %v", err)}
	}

	// Config blob must be present.
	if missing, derr := blobMissing(ctx, provider, manifest.Config); derr != nil {
		return verdict{img.Name, actionKeep, fmt.Sprintf("config blob check errored, leaving intact: %v", derr)}
	} else if missing {
		return verdict{img.Name, actionDelete, fmt.Sprintf("config blob missing: %s", manifest.Config.Digest)}
	}

	// Every layer blob must be present.
	for _, layer := range manifest.Layers {
		if missing, derr := blobMissing(ctx, provider, layer); derr != nil {
			return verdict{img.Name, actionKeep, fmt.Sprintf("layer blob check errored, leaving intact: %v", derr)}
		} else if missing {
			return verdict{img.Name, actionDelete, fmt.Sprintf("layer blob missing: %s", layer.Digest)}
		}
	}

	return verdict{img.Name, actionKeep, fmt.Sprintf("config + %d layer(s) present", len(manifest.Layers))}
}

// blobMissing reports whether a blob is absent from the content store.
// A nil error with missing=false means present; missing=true means a clean
// not-found; a non-nil error means the check itself failed (caller keeps the
// image to stay safe).
func blobMissing(ctx context.Context, provider content.Store, desc ocispec.Descriptor) (missing bool, err error) {
	_, ierr := provider.Info(ctx, desc.Digest)
	if ierr == nil {
		return false, nil
	}
	if errdefs.IsNotFound(ierr) {
		return true, nil
	}
	return false, ierr
}

func variantSuffix(v string) string {
	if v == "" {
		return ""
	}
	return "/" + v
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: prune-images [--socket PATH] [--namespace NS] [--apply]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Deletes containerd image records whose CURRENT-NODE-PLATFORM config or")
	fmt.Fprintln(os.Stderr, "layer blobs are missing from the content store, forcing kubelet to re-pull.")
	fmt.Fprintln(os.Stderr, "Without --apply, runs in dry-run mode and only logs the plan.")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintf(os.Stderr, "  --socket PATH      containerd socket (default %s)\n", defaultSocket)
	fmt.Fprintf(os.Stderr, "  --namespace NS     containerd namespace (default %s)\n", defaultNamespace)
	fmt.Fprintln(os.Stderr, "  --apply            actually delete (otherwise dry-run)")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "containerd must be RUNNING.  Platform-scoped: never deletes images that")
	fmt.Fprintln(os.Stderr, "merely lack a DIFFERENT platform's layers.")
}

func logf(format string, args ...any) {
	fmt.Printf("[%s] %s\n", time.Now().Format("2006-01-02T15:04:05Z07:00"), fmt.Sprintf(format, args...))
}

func fatalf(format string, args ...any) {
	logf("FATAL: "+format, args...)
	os.Exit(1)
}
