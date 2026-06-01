//go:build linux

package snapshotter

import (
	"context"
	"io"
	"net/http"

	snapshotsv1 "github.com/containerd/containerd/api/services/snapshots/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/contrib/snapshotservice"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/gin-gonic/gin"
	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"google.golang.org/grpc"

	"github.com/humble-mun/chassis/pkg/metrics"
)

const (
	// Name is the service identifier used by the daemon.
	Name = "daemon"

	flagUnixSocketPath    = "unix-socket-path"
	defaultUnixSocketPath = "/var/run/pv-snapshotter/daemon.sock"
	flagContainerdSocket  = "containerd-socket"
	flagAnnotationPrefix  = "annotation-prefix"

	// flagShareOverlayfsLowers enables the dedup path: when a container image's
	// read-only layers are already present in the host's native overlayfs
	// snapshotter, pv-snapshotter creates reference snapshots (with fs/ as a
	// symlink into the overlayfs layer directory) instead of re-unpacking.
	// Disabled by default; enable only after validating P0-5.
	flagShareOverlayfsLowers = "share-overlayfs-lowers"
)

// RegisterFlags registers the snapshotter flags with the provided flag set.
func RegisterFlags(pfs *pflag.FlagSet) {
	pfs.String(flagUnixSocketPath, defaultUnixSocketPath, "The path to the unix socket file.")
	pfs.String(flagContainerdSocket, defaultContainerdSocket,
		"Path to the containerd gRPC socket, used to resolve pod annotations.")
	pfs.String(flagAnnotationPrefix, defaultAnnotationPrefix,
		"DNS subdomain prefix for all pv-snapshotter pod annotations (must be a valid "+
			"RFC 1123 DNS subdomain, no trailing slash). The following annotation keys are "+
			"derived from this prefix at startup:\n"+
			"  <prefix>/upperdir-path          – literal upperdir root path\n"+
			"  <prefix>/upperdir-path-template – Go template rendered to upperdir root path\n"+
			"  <prefix>/var.<Name>             – template variable injected into template data")
	pfs.Bool(flagShareOverlayfsLowers, false,
		"Enable dedup: reuse overlayfs read-only layers instead of re-unpacking. "+
			"When set, Stat() lazily creates reference snapshots (fs/ symlinked into "+
			"the host overlayfs layer directory) for image layers already present in "+
			"the native overlayfs snapshotter.")
}

// GetUnixSocketPath returns the configured Unix socket path for the snapshotter gRPC listener.
func GetUnixSocketPath() string {
	return viper.GetString(flagUnixSocketPath)
}

// Service is the lifecycle interface returned by RegisterGRPCService.
// It exposes the Prometheus scrape hook, the HTTP route registrar, and
// a Close method that tears down the underlying overlay snapshotter and
// the containerd client connection.
type Service interface {
	io.Closer
	RegisterScrapeHook(context.Context)
	RegisterRoute(*gin.Engine)
}

// RegisterGRPCService creates the native overlay snapshotter, wires up the containerd resolver,
// registers the snapshot gRPC service on srv, and returns a Closer that tears down both.
func RegisterGRPCService(logger logr.Logger, nodeName string, srv *grpc.Server) (svc Service, err error) {
	logger = logger.WithName("snapshotter")

	closers := make(map[string]io.Closer)
	defer func() {
		if err == nil {
			return
		}
		for name, closer := range closers {
			if e := closer.Close(); e != nil {
				logger.Error(err, "close failed", "name", name)
			}
		}
	}()

	var sn snapshots.Snapshotter
	if sn, err = createNativeOverlaySnapshotter(logger); err != nil {
		logger.Error(err, "failed to create overlay snapshotter")
		return
	}
	closers["snapshotter"] = sn

	socketPath := viper.GetString(flagContainerdSocket)
	var client *containerd.Client
	if client, err = containerd.New(socketPath); err != nil {
		logger.Error(err, "failed to create containerd client", "socketPath", socketPath)
		return
	}
	closers["containerd"] = client

	var res *resolver
	if res, err = newResolver(logger, client); err != nil {
		logger.Error(err, "failed to create containerd resolver", "socket", socketPath)
		return
	}

	var dedup *dedupManager
	if enabled := viper.GetBool(flagShareOverlayfsLowers); enabled {
		logger.Info("dedup path enabled (--share-overlayfs-lowers=true)")
		dedup = newDedupManager(logger, nodeName, res.client, sn)
	}

	ss := snapshotter{logger: logger, Snapshotter: sn, client: client, resolver: res, dedup: dedup}

	svc = ss

	// Wrap sn in the appropriate type depending on whether the underlying
	// overlay snapshotter implements snapshots.Cleaner (deferred directory
	// removal after Remove()).  The type assertion is performed once here at
	// startup so that snapshotservice.FromSnapshotter can expose the gRPC
	// Cleanup RPC without a per-call runtime assertion inside Cleanup().
	var wrapped snapshots.Snapshotter
	if c, ok := sn.(snapshots.Cleaner); ok {
		wrapped = &cleanerSnapshotter{
			snapshotter: ss,
			cleaner:     c,
		}
	} else {
		wrapped = &ss
	}

	snapshotService := snapshotservice.FromSnapshotter(wrapped)
	snapshotsv1.RegisterSnapshotsServer(srv, snapshotService)
	return
}

var (
	// pinnedSnapshotsTotal tracks the number of overlayfs snapshots currently
	// pinned by a pv-snapshotter-managed lease (i.e., referenced containers
	// whose image layers are kept alive via the dedup path).
	// Label: node_name — identifies the node where the lease was created,
	// making it possible to correlate alerts to a specific node.
	pinnedSnapshotsTotal = metrics.Register(func(registry promauto.Factory) *prometheus.GaugeVec {
		return registry.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pv_snapshotter_pinned_snapshots_total",
			Help: "Number of overlayfs snapshots currently held by a pv-snapshotter dedup lease.",
		}, []string{"node_name"})
	})

	// unpinFailuresTotal counts the number of times unpinByActiveKey failed to
	// delete a dedup lease during Remove().  A non-zero rate indicates dangling
	// leases that require manual cleanup via DELETE /dedup/leases/:leaseID.
	// Alert rule: rate(pv_snapshotter_unpin_failures_total[5m]) > 0
	unpinFailuresTotal = metrics.Register(func(registry promauto.Factory) *prometheus.CounterVec {
		return registry.NewCounterVec(prometheus.CounterOpts{
			Name: "pv_snapshotter_unpin_failures_total",
			Help: "Total number of failed dedup lease deletions during snapshot Remove. " +
				"Non-zero values indicate dangling leases requiring manual cleanup.",
		}, []string{"node_name"})
	})

	// orphanLeasesTotal is the current number of pv-snapshotter dedup leases
	// whose owning active snapshot no longer exists in the local snapshotter.
	// Refreshed on every Prometheus scrape via the scrape hook.
	// Alert rule: pv_snapshotter_orphan_leases_total > 0
	orphanLeasesTotal = metrics.Register(func(registry promauto.Factory) *prometheus.GaugeVec {
		return registry.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pv_snapshotter_orphan_leases_total",
			Help: "Number of pv-snapshotter dedup leases whose owning active snapshot no " +
				"longer exists. Refreshed on scrape. Non-zero values indicate GC is needed.",
		}, []string{"node_name"})
	})
)

type snapshotter struct {
	logger   logr.Logger
	resolver *resolver
	// dedup is non-nil only when --share-overlayfs-lowers is enabled.
	dedup *dedupManager
	snapshots.Snapshotter
	client *containerd.Client
}

func (sn snapshotter) RegisterScrapeHook(ctx context.Context) {
	if sn.dedup == nil {
		return
	}
	count := sn.dedup.countOrphanLeases(ctx, containerdNamespaceK8s)
	orphanLeasesTotal.With(prometheus.Labels{"node_name": sn.dedup.nodeName}).Set(float64(count))
}

func (sn snapshotter) RegisterRoute(mux *gin.Engine) {
	// Dedup lease management endpoints.
	// GET    /dedup/leases        – list all pv-snapshotter-managed leases
	// DELETE /dedup/leases/:id   – force-delete a specific lease (for dangling-lease recovery)
	// POST   /dedup/leases/gc    – delete all orphan leases (owner snapshot no longer exists)
	mux.GET("/dedup/leases", sn.handleListLeases)
	mux.DELETE("/dedup/leases/:leaseID", sn.handleDeleteLease)
	mux.POST("/dedup/leases/gc", sn.handleGCLeases)
}

// handleListLeases lists all leases owned by pv-snapshotter's dedup path.
// Intended for operational visibility and dangling-lease detection.
//
// Response (200 OK):
//
//	{ "leases": [ { "id": "...", "createdAt": "...", "labels": {...} }, ... ] }
//
// Returns 501 when --share-overlayfs-lowers is disabled.
func (sn snapshotter) handleListLeases(ctx *gin.Context) {
	if sn.dedup == nil {
		ctx.JSON(http.StatusNotImplemented, gin.H{"error": "dedup not enabled (--share-overlayfs-lowers=false)"})
		return
	}
	all, err := sn.dedup.listManagedLeases(ctx, containerdNamespaceK8s)
	if err != nil {
		sn.logger.Error(err, "listing dedup leases")
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	type leaseEntry struct {
		ID        string            `json:"id"`
		CreatedAt string            `json:"createdAt"`
		Labels    map[string]string `json:"labels,omitempty"`
	}
	out := make([]leaseEntry, 0, len(all))
	for _, l := range all {
		out = append(out, leaseEntry{
			ID:        l.ID,
			CreatedAt: l.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			Labels:    l.Labels,
		})
	}
	ctx.JSON(http.StatusOK, gin.H{"leases": out})
}

// handleDeleteLease force-deletes the lease identified by :leaseID.
// Use this to recover from a dangling lease when the automatic unpin in
// Remove() was skipped due to a bug or crash.
//
// Response: 204 No Content on success, 404 when not found, 500 on error.
func (sn snapshotter) handleDeleteLease(ctx *gin.Context) {
	if sn.dedup == nil {
		ctx.JSON(http.StatusNotImplemented, gin.H{"error": "dedup not enabled (--share-overlayfs-lowers=false)"})
		return
	}
	leaseID := ctx.Param("leaseID")
	if leaseID == "" {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "leaseID is required"})
		return
	}
	if err := sn.dedup.deleteLease(ctx, containerdNamespaceK8s, leaseID); err != nil {
		sn.logger.Error(err, "deleting dedup lease via API", "leaseID", leaseID)
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ctx.Status(http.StatusNoContent)
}

// handleGCLeases deletes all leases whose owning active snapshot no longer
// exists in the local snapshotter (orphan leases).  It is the trigger for
// the startup-time audit described in the architecture docs.
//
// Response (200 OK):
//
//	{ "deleted": <count> }
//
// Returns 501 when --share-overlayfs-lowers is disabled.
func (sn snapshotter) handleGCLeases(ctx *gin.Context) {
	if sn.dedup == nil {
		ctx.JSON(http.StatusNotImplemented, gin.H{"error": "dedup not enabled (--share-overlayfs-lowers=false)"})
		return
	}
	deleted := sn.dedup.gcOrphanLeases(ctx, containerdNamespaceK8s)
	sn.logger.Info("GC sweep completed", "deletedLeases", deleted)
	ctx.JSON(http.StatusOK, gin.H{"deleted": deleted})
}

func (sn snapshotter) Stat(ctx context.Context, key string) (info snapshots.Info, err error) {
	sn.logger.V(4).Info("Stat called", "key", key)

	info, err = sn.Snapshotter.Stat(ctx, key)
	if err == nil {
		sn.logger.V(4).Info("Stat completed", "key", key, "labels", info.Labels)
		return
	}

	// Dedup path: when the local snapshotter reports not-found and dedup is
	// enabled, check whether the key is an image-layer chainID present in the
	// host overlayfs.  If so, materialise a reference snapshot so the CRI
	// unpacker skips re-unpacking.
	if sn.dedup != nil {
		info, err = sn.dedup.statWithLazyMaterialise(ctx, key, sn.Snapshotter)
		if err == nil {
			sn.logger.V(4).Info("Stat resolved via dedup reference", "key", key)
			return
		}
	}

	sn.logger.Error(err, "failed to stat", "key", key)
	return
}

func (sn snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (
	si snapshots.Info, err error) {

	sn.logger.V(4).Info("Update called", "info", si.Name, "labels", si.Labels)
	if si, err = sn.Snapshotter.Update(ctx, info, fieldpaths...); err != nil {
		sn.logger.Error(err, "failed to update", "info", info.Name)
	} else {
		sn.logger.V(4).Info("Update completed", "info", si.Name, "labels", si.Labels)
	}
	return
}

func (sn snapshotter) Usage(ctx context.Context, key string) (usage snapshots.Usage, err error) {
	sn.logger.V(4).Info("Usage called", "key", key)
	if usage, err = sn.Snapshotter.Usage(ctx, key); err != nil {
		sn.logger.Error(err, "failed to compute usage", "key", key)
	} else {
		sn.logger.V(4).Info("Usage completed", "key", key)
	}
	return
}
func (sn snapshotter) Mounts(ctx context.Context, key string) (mounts []mount.Mount, err error) {
	sn.logger.V(4).Info("Mounts called", "key", key)
	if mounts, err = sn.Snapshotter.Mounts(ctx, key); err != nil {
		sn.logger.Error(err, "failed to get mounts", "key", key)
		return
	}

	upperdirPath, resolveErr := sn.resolver.resolveAtMountsTime(ctx, key)
	if resolveErr != nil {
		sn.logger.Error(resolveErr, "failed to resolve upperdir path; falling back to native overlay", "key", key)
	}
	if upperdirPath != "" {
		sn.logger.V(4).Info("PV-backed routing active, redirecting upperdir",
			"key", key, "upperdirPath", upperdirPath)
		if len(mounts) > 0 {
			sn.logger.V(4).Info("native overlay mount options (before replacement)",
				"key", key, "type", mounts[0].Type, "options", mounts[0].Options)
		}
		if err = ensureUpperdirReady(upperdirPath); err != nil {
			sn.logger.Error(err, "upperdir not ready, aborting PV-backed mount", "key", key)
			return
		}
		mounts = replaceUpperdirOptions(mounts, upperdirPath)
		sn.logger.V(4).Info("PV-backed mount options applied",
			"key", key, "options", mounts[0].Options)
	}

	sn.logger.V(4).Info("Mounts completed", "key", key)
	return
}

func (sn snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) (
	mounts []mount.Mount, err error) {

	var info snapshots.Info
	for _, opt := range opts {
		if optErr := opt(&info); optErr != nil {
			sn.logger.Error(optErr, "failed to apply prepare option", "key", key)
		}
	}
	sn.logger.V(4).Info("Prepare called", "key", key, "parent", parent, "labels", info.Labels)
	if mounts, err = sn.Snapshotter.Prepare(ctx, key, parent, opts...); err != nil {
		sn.logger.Error(err, "failed to prepare", "key", key)
	} else {
		sn.logger.V(4).Info("Prepare completed", "key", key)
	}

	// Dedup path: when a container's active snapshot is created on top of a
	// dedup-materialised chainID, pin the overlayfs layer so GC cannot reclaim
	// it while the container is running.
	//
	// Conditions:
	//   - dedup is enabled
	//   - key is a container active snapshot (k8s.io/N/<containerID> format,
	//     not a sha256 chainID — image-layer Prepare calls have key==chainID)
	//   - parent's third segment is a chainID — the metadata layer wraps the
	//     parent as "<ns>/<seq>/<chainID>", so isChainID must be checked against
	//     the parsed containerID field, not the raw parent string
	//
	// pinLayer is intentionally called even when Prepare itself returned an
	// error: the error surface is independent and the caller (CRI) will handle
	// the Prepare failure.  Pin failure is logged but non-fatal.
	if sn.dedup != nil {
		ns, activeID, nsOk := parseSnapshotKey(key)
		if !nsOk {
			ns = containerdNamespaceK8s
		}
		_, parentChainID, parentOk := parseSnapshotKey(parent)
		if nsOk && !isChainID(activeID) && parentOk && isChainID(parentChainID) {
			if _, pinErr := sn.dedup.pinLayer(ctx, ns, parentChainID, key); pinErr != nil {
				sn.logger.Error(pinErr, "failed to pin dedup layer; GC protection missing for this container",
					"key", key, "parent", parent, "chainID", parentChainID)
			}
		}
	}
	return
}

func (sn snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) (
	mounts []mount.Mount, err error) {

	var info snapshots.Info
	for _, opt := range opts {
		if optErr := opt(&info); optErr != nil {
			sn.logger.Error(optErr, "failed to apply view option", "key", key)
		}
	}
	sn.logger.V(4).Info("View called", "key", key, "parent", parent, "labels", info.Labels)
	if mounts, err = sn.Snapshotter.View(ctx, key, parent, opts...); err != nil {
		sn.logger.Error(err, "failed to view", "key", key)
	} else {
		sn.logger.V(4).Info("View completed", "key", key)
	}
	return
}

func (sn snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) (err error) {
	sn.logger.V(4).Info("Commit called", "key", key, "name", name)
	if err = sn.Snapshotter.Commit(ctx, name, key, opts...); err != nil {
		sn.logger.Error(err, "failed to commit", "key", key)
	} else {
		sn.logger.V(4).Info("Commit completed", "key", key)
	}
	return
}

func (sn snapshotter) Remove(ctx context.Context, key string) (err error) {
	sn.logger.V(4).Info("Remove called", "key", key)
	if err = sn.Snapshotter.Remove(ctx, key); err != nil {
		sn.logger.Error(err, "failed to remove", "key", key)
	} else {
		sn.logger.V(4).Info("Remove completed", "key", key)
	}

	// Dedup path: release the overlayfs layer lease pinned for this active
	// snapshot, if any.  Errors are logged inside unpinByActiveKey and never
	// surfaced — an unpin failure must not block snapshot removal.
	if sn.dedup != nil {
		if ns, _, ok := parseSnapshotKey(key); ok {
			sn.dedup.unpinByActiveKey(ctx, ns, key)
		}
	}
	return
}

func (sn snapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, filters ...string) (err error) {
	sn.logger.V(4).Info("Walk called", "filters", filters)
	if err = sn.Snapshotter.Walk(ctx, fn, filters...); err != nil {
		sn.logger.Error(err, "failed to walk", "filters", filters)
	} else {
		sn.logger.V(4).Info("Walk completed", "filters", filters)
	}
	return
}

func (sn snapshotter) Close() (err error) {
	sn.logger.V(4).Info("Close called")
	if err = sn.client.Close(); err != nil {
		sn.logger.Error(err, "failed to close containerd client")
	}
	if err = sn.Snapshotter.Close(); err != nil {
		sn.logger.Error(err, "failed to close")
	} else {
		sn.logger.V(4).Info("Close completed")
	}
	return
}

// cleanerSnapshotter extends snapshotter for base snapshotters that implement
// snapshots.Cleaner (deferred directory removal after Remove()).
//
// snapshotservice.FromSnapshotter dispatches the gRPC Cleanup RPC by asserting
// the wrapped snapshotter to snapshots.Cleaner.  Because snapshotter embeds
// snapshots.Snapshotter as an interface, Go cannot reach the concrete Cleanup
// method through the embedding automatically.  This type holds a pre-checked
// snapshots.Cleaner reference obtained once at startup in RegisterGRPCService,
// eliminating the per-call runtime assertion.
type cleanerSnapshotter struct {
	snapshotter
	cleaner snapshots.Cleaner
}

func (sn cleanerSnapshotter) Cleanup(ctx context.Context) error {
	sn.logger.V(4).Info("Cleanup called")
	if err := sn.cleaner.Cleanup(ctx); err != nil {
		sn.logger.Error(err, "Cleanup failed")
		return err
	}
	sn.logger.V(4).Info("Cleanup completed")
	return nil
}
