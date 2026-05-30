//go:build linux

package snapshotter

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	// leaseLabelOwnerKey is stamped on every pv-snapshotter dedup lease so the
	// lease can be found and released when the owning active snapshot is removed.
	// Value: the active snapshot key (e.g. "k8s.io/42/<containerID>").
	leaseLabelOwnerKey = "pv-snapshotter.io/owner-snapshot"

	// leaseLabelManagedBy marks leases as owned by this component, making
	// them filterable for operational listing.
	leaseLabelManagedBy      = "pv-snapshotter.io/managed-by"
	leaseLabelManagedByValue = "pv-snapshotter"

	// overlayfsSnapshotterName is the name of the native overlay snapshotter
	// registered in containerd that holds the shared read-only layers.
	overlayfsSnapshotterName = "overlayfs"
)

// dedupManager implements the layer-sharing (dedup) logic for the
// --share-overlayfs-lowers feature.
//
// When the feature is enabled, Stat(chainID) lazily creates a local
// "reference snapshot" whose fs/ directory is a symlink into the overlayfs
// layer directory, so the CRI unpacker skips re-unpacking image layers that
// are already present in the host's native overlayfs snapshotter.
//
// All overlayfs snapshots referenced this way are pinned with a containerd
// lease to prevent GC from reclaiming them while containers are running.
type dedupManager struct {
	logger logr.Logger

	// nodeName is the Kubernetes node name; used as a Prometheus label on all
	// metrics emitted by dedupManager so that alerts can be correlated to a
	// specific node.
	nodeName string

	// containerdClient is used to reach the overlayfs snapshotter and the
	// leases service.  It is the same client used by the resolver.
	containerdClient *containerd.Client

	// localSn is the pv-snapshotter's own native overlay snapshotter, used to
	// create and commit reference snapshots under the local root.
	localSn snapshots.Snapshotter

	// mu guards active pinning operations to prevent duplicate lease creation
	// when two containers start simultaneously using the same image layer.
	mu sync.Mutex
}

// newDedupManager constructs a dedupManager.  localSn must be the same
// snapshots.Snapshotter that RegisterGRPCService wired to the gRPC service.
// nodeName is the Kubernetes node name, stamped on all emitted Prometheus
// metrics as the "node_name" label.
func newDedupManager(logger logr.Logger, nodeName string, client *containerd.Client, localSn snapshots.Snapshotter) *dedupManager {
	return &dedupManager{
		logger:           logger.WithName("dedup"),
		nodeName:         nodeName,
		containerdClient: client,
		localSn:          localSn,
	}
}

// -------------------------------------------------------------------------
// Layer-path resolution
// -------------------------------------------------------------------------

// overlayfsLayerPath returns the physical directory path of chainID in the
// host's native overlayfs snapshotter.
//
// It creates a temporary View snapshot, reads the lowerdir (first segment for
// multi-layer images) or bind Source (single-layer images) from the returned
// mount options, and then removes the View.
//
// Returns ("", nil) when chainID is not present in overlayfs.
func (m *dedupManager) overlayfsLayerPath(ctx context.Context, ns, chainID string) (path string, err error) {
	nsCtx := namespaces.WithNamespace(context.Background(), ns)

	ovlSvc := m.containerdClient.SnapshotService(overlayfsSnapshotterName)

	// Check existence first to avoid spurious errors in the normal miss path.
	if _, statErr := ovlSvc.Stat(nsCtx, chainID); statErr != nil {
		m.logger.V(4).Info("chainID not in overlayfs, dedup miss", "chainID", chainID)
		return "", nil
	}

	viewKey := fmt.Sprintf("pv-snapshotter-probe-%s", chainID)
	mounts, viewErr := ovlSvc.View(nsCtx, viewKey, chainID)
	if viewErr != nil {
		return "", fmt.Errorf("creating probe view for chainID %s: %w", chainID, viewErr)
	}
	defer func() {
		if rmErr := ovlSvc.Remove(nsCtx, viewKey); rmErr != nil {
			m.logger.Error(rmErr, "failed to remove probe view", "viewKey", viewKey)
		}
	}()

	if len(mounts) == 0 {
		return "", fmt.Errorf("no mounts returned for probe view of chainID %s", chainID)
	}

	path, err = layerPathFromMounts(mounts)
	return
}

// layerPathFromMounts extracts the physical layer directory from the mount
// options returned by a View snapshot.
//
//   - overlay mount:  lowerdir=<path1>:<path2>:...  → path1 (topmost = the layer itself)
//   - bind mount:     Source=<path>                 → path
func layerPathFromMounts(mounts []mount.Mount) (string, error) {
	m := mounts[0]
	switch m.Type {
	case "overlay":
		for _, opt := range m.Options {
			if after, ok := strings.CutPrefix(opt, "lowerdir="); ok {
				first, _, _ := strings.Cut(after, ":")
				return first, nil
			}
		}
		return "", fmt.Errorf("overlay mount missing lowerdir option: %v", m.Options)
	case "bind":
		return m.Source, nil
	default:
		return "", fmt.Errorf("unexpected mount type %q for probe view", m.Type)
	}
}

// -------------------------------------------------------------------------
// Reference snapshot materialisation
// -------------------------------------------------------------------------

// materializeReference creates a reference snapshot for chainID in the local
// snapshotter.  The snapshot's fs/ directory is replaced with a symlink
// pointing to ovlLayerPath (the physical directory in the host overlayfs).
//
// This makes the local overlay snapshotter's Mounts() emit lowerdir entries
// that resolve to the overlayfs physical paths, so only one copy of each
// read-only layer exists on disk.
//
// The caller must hold m.mu when invoking this method to serialise concurrent
// materialisation of the same chainID.
func (m *dedupManager) materializeReference(ctx context.Context, chainID, parent, ovlLayerPath string) error {
	// Temporary key for the active (writable) snapshot before commit.
	tmpKey := fmt.Sprintf("pv-snapshotter-ref-tmp-%s", chainID)

	if _, err := m.localSn.Prepare(ctx, tmpKey, parent); err != nil {
		return fmt.Errorf("preparing reference snapshot for chainID %s: %w", chainID, err)
	}

	// Derive the fs/ path from the Mounts returned by the local snapshotter.
	// overlay.NewSnapshotter lays out: <root>/snapshots/<id>/fs
	localMounts, err := m.localSn.Mounts(ctx, tmpKey)
	if err != nil {
		_ = m.localSn.Remove(ctx, tmpKey)
		return fmt.Errorf("getting local mounts for tmp reference %s: %w", tmpKey, err)
	}
	localFSPath, err := upperdirFromMounts(localMounts)
	if err != nil {
		_ = m.localSn.Remove(ctx, tmpKey)
		return fmt.Errorf("extracting upperdir from local mounts for %s: %w", tmpKey, err)
	}

	// Atomically replace the local fs/ directory with a symlink to the
	// overlayfs layer directory.
	if err = replaceWithSymlink(localFSPath, ovlLayerPath); err != nil {
		_ = m.localSn.Remove(ctx, tmpKey)
		return fmt.Errorf("creating symlink %s → %s: %w", localFSPath, ovlLayerPath, err)
	}

	// Commit flips the metadata kind from Active to Committed.  The overlay
	// snapshotter does not move the directory on Commit, so the symlink
	// persists as the committed snapshot's fs/.
	if err = m.localSn.Commit(ctx, chainID, tmpKey); err != nil {
		// Best-effort cleanup; the symlink is already in place but the snapshot
		// metadata was not updated.
		_ = m.localSn.Remove(ctx, tmpKey)
		return fmt.Errorf("committing reference snapshot for chainID %s: %w", chainID, err)
	}

	m.logger.V(4).Info("reference snapshot materialised",
		"chainID", chainID, "ovlLayerPath", ovlLayerPath)
	return nil
}

// upperdirFromMounts extracts the upperdir path from the mount options of an
// active (writable) overlay snapshot.
func upperdirFromMounts(mounts []mount.Mount) (string, error) {
	if len(mounts) == 0 {
		return "", fmt.Errorf("no mounts")
	}
	for _, opt := range mounts[0].Options {
		if after, ok := strings.CutPrefix(opt, "upperdir="); ok {
			return after, nil
		}
	}
	return "", fmt.Errorf("no upperdir option in mounts: %v", mounts[0].Options)
}

// replaceWithSymlink removes path (must be an existing directory) and creates
// a symlink at the same location pointing to target.
func replaceWithSymlink(path, target string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("removing %s: %w", path, err)
	}
	if err := os.Symlink(target, path); err != nil {
		return fmt.Errorf("symlinking %s → %s: %w", path, target, err)
	}
	return nil
}

// -------------------------------------------------------------------------
// Lazy materialisation: entry point called from snapshotter.Stat()
// -------------------------------------------------------------------------

// statWithLazyMaterialise is called by snapshotter.Stat() when the local
// snapshotter returned not-found for key.
//
// It checks whether key looks like a chainID (sha256:... format) and whether
// the same chainID exists in the host overlayfs.  If so it materialises a
// reference snapshot and returns its Info.
//
// Returns the original not-found error unchanged when:
//   - key is not a chainID (e.g. active container key)
//   - chainID is not in overlayfs (dedup miss → CRI will unpack normally)
func (m *dedupManager) statWithLazyMaterialise(
	ctx context.Context,
	key string,
	localSn snapshots.Snapshotter,
) (snapshots.Info, error) {

	// Only image-layer chainIDs look like "sha256:<hex>".
	if !isChainID(key) {
		return snapshots.Info{}, fmt.Errorf("not a chainID: %s", key)
	}

	// Namespace comes from the context already set by the CRI caller.
	ns, _ := namespaces.Namespace(ctx)
	if ns == "" {
		ns = "k8s.io"
	}

	// Resolve the physical overlayfs layer directory.
	ovlPath, err := m.overlayfsLayerPath(ctx, ns, key)
	if err != nil {
		return snapshots.Info{}, fmt.Errorf("resolving overlayfs layer path for %s: %w", key, err)
	}
	if ovlPath == "" {
		// Not in overlayfs — dedup miss, return not-found so CRI unpacks normally.
		return snapshots.Info{}, fmt.Errorf("chainID %s not found in overlayfs (dedup miss)", key)
	}

	// Serialise concurrent materialisations of the same chainID.
	m.mu.Lock()
	defer m.mu.Unlock()

	// Re-check under the lock — another goroutine may have materialised first.
	if info, statErr := localSn.Stat(ctx, key); statErr == nil {
		return info, nil
	}

	// Determine the parent chainID for this layer (the parent of key in the
	// local snapshotter, which must already exist if it is not the root layer).
	parent := ""
	if parentInfo, parentErr := m.resolveParentChainID(ctx, ns, key); parentErr == nil {
		parent = parentInfo
	}

	if err = m.materializeReference(ctx, key, parent, ovlPath); err != nil {
		return snapshots.Info{}, fmt.Errorf("materialising reference for %s: %w", key, err)
	}

	return localSn.Stat(ctx, key)
}

// resolveParentChainID returns the parent chainID of chainID from the overlayfs
// snapshotter.  Returns ("", nil) for root layers (no parent).
func (m *dedupManager) resolveParentChainID(ctx context.Context, ns, chainID string) (string, error) {
	nsCtx := namespaces.WithNamespace(context.Background(), ns)
	info, err := m.containerdClient.SnapshotService(overlayfsSnapshotterName).Stat(nsCtx, chainID)
	if err != nil {
		return "", err
	}
	return info.Parent, nil
}

// isChainID returns true when s looks like a content-addressable chainID
// (currently: "sha256:" prefix with a 64-char hex digest).
func isChainID(s string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	hex := s[len(prefix):]
	if len(hex) != 64 {
		return false
	}
	for _, c := range hex {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}



// pinLayer creates a permanent containerd lease that pins chainID in the
// overlayfs snapshotter, preventing GC from reclaiming it or its ancestors
// for as long as the lease exists.
//
// The lease is labelled with:
//   - leaseLabelManagedBy = "pv-snapshotter"   (for operational listing)
//   - leaseLabelOwnerKey  = activeSnapshotKey   (for lookup at Remove time)
//
// Pinning the top-level chainID is sufficient: overlayfs GC preserves the
// entire ancestor chain transitively.
//
// Returns the created lease so the caller can store it for later release.
func (m *dedupManager) pinLayer(ctx context.Context, ns, chainID, activeSnapshotKey string) (leases.Lease, error) {
	nsCtx := namespaces.WithNamespace(context.Background(), ns)
	leaseSvc := m.containerdClient.LeasesService()

	l, err := leaseSvc.Create(nsCtx,
		leases.WithRandomID(),
		leases.WithLabels(map[string]string{
			leaseLabelManagedBy: leaseLabelManagedByValue,
			leaseLabelOwnerKey:  activeSnapshotKey,
		}),
		// Deliberately no WithExpiration — the lease must outlive the container.
		// It is released explicitly in unpinByActiveKey when Remove() is called.
	)
	if err != nil {
		return leases.Lease{}, fmt.Errorf("creating dedup lease for chainID %s (owner=%s): %w",
			chainID, activeSnapshotKey, err)
	}

	res := leases.Resource{
		ID:   chainID,
		Type: "snapshots/" + overlayfsSnapshotterName,
	}
	if err = leaseSvc.AddResource(nsCtx, l, res); err != nil {
		// Best-effort cleanup of the orphaned empty lease.
		_ = leaseSvc.Delete(nsCtx, l)
		return leases.Lease{}, fmt.Errorf("adding snapshot resource to dedup lease (owner=%s): %w",
			activeSnapshotKey, err)
	}

	m.logger.V(4).Info("overlayfs layer pinned",
		"chainID", chainID, "leaseID", l.ID, "activeSnapshotKey", activeSnapshotKey)
	pinnedSnapshotsTotal.With(prometheus.Labels{"node_name": m.nodeName}).Inc()
	return l, nil
}

// unpinByActiveKey finds and deletes the dedup lease whose leaseLabelOwnerKey
// matches activeSnapshotKey.  It is called from Remove() when a container's
// active snapshot is torn down.
//
// Errors are logged but do not affect the return value — a failed unpin must
// not prevent the snapshot Remove from completing.  Dangling leases can be
// cleaned up manually via DELETE /dedup/leases/:leaseID.
func (m *dedupManager) unpinByActiveKey(ctx context.Context, ns, activeSnapshotKey string) {
	nsCtx := namespaces.WithNamespace(context.Background(), ns)
	leaseSvc := m.containerdClient.LeasesService()

	filter := fmt.Sprintf("labels.%q==%q", leaseLabelOwnerKey, activeSnapshotKey)
	all, err := leaseSvc.List(nsCtx, filter)
	if err != nil {
		m.logger.Error(err, "listing dedup leases for unpin", "activeSnapshotKey", activeSnapshotKey)
		return
	}
	if len(all) == 0 {
		m.logger.V(4).Info("no dedup lease found for active snapshot (not a dedup-backed container)",
			"activeSnapshotKey", activeSnapshotKey)
		return
	}
	for _, l := range all {
		if err = leaseSvc.Delete(nsCtx, l); err != nil {
			m.logger.Error(err, "deleting dedup lease; snapshot may dangle — use DELETE /dedup/leases/:id to clean up",
				"leaseID", l.ID, "activeSnapshotKey", activeSnapshotKey)
			unpinFailuresTotal.With(prometheus.Labels{"node_name": m.nodeName}).Inc()
			continue
		}
		m.logger.V(4).Info("dedup lease released", "leaseID", l.ID, "activeSnapshotKey", activeSnapshotKey)
		pinnedSnapshotsTotal.With(prometheus.Labels{"node_name": m.nodeName}).Dec()
	}
}

// -------------------------------------------------------------------------
// Operational listing (backing the HTTP API)
// -------------------------------------------------------------------------

// ListManagedLeases returns all leases created by pv-snapshotter's dedup path
// across all namespaces.  It is called by the HTTP handler for
// GET /dedup/leases.
func (m *dedupManager) listManagedLeases(ctx context.Context, ns string) ([]leases.Lease, error) {
	nsCtx := namespaces.WithNamespace(context.Background(), ns)
	filter := fmt.Sprintf("labels.%q==%q", leaseLabelManagedBy, leaseLabelManagedByValue)
	return m.containerdClient.LeasesService().List(nsCtx, filter)
}

// DeleteLease deletes a specific lease by ID.  It is called by the HTTP
// handler for DELETE /dedup/leases/:leaseID.
func (m *dedupManager) deleteLease(ctx context.Context, ns, leaseID string) error {
	nsCtx := namespaces.WithNamespace(context.Background(), ns)
	if err := m.containerdClient.LeasesService().Delete(nsCtx, leases.Lease{ID: leaseID}); err != nil {
		return fmt.Errorf("deleting lease %s: %w", leaseID, err)
	}
	pinnedSnapshotsTotal.With(prometheus.Labels{"node_name": m.nodeName}).Dec()
	return nil
}
