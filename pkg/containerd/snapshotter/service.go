//go:build linux

package snapshotter

import (
	"context"
	"errors"
	"io"

	snapshotsv1 "github.com/containerd/containerd/api/services/snapshots/v1"
	"github.com/containerd/containerd/v2/contrib/snapshotservice"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/go-logr/logr"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
)

const (
	// Name is the service identifier used by the daemon.
	Name = "daemon"

	flagUnixSocketPath    = "unix-socket-path"
	defaultUnixSocketPath = "/var/run/pv-snapshotter/daemon.sock"
	flagContainerdSocket  = "containerd-socket"
	flagAnnotationPrefix  = "annotation-prefix"
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
}

// GetUnixSocketPath returns the configured Unix socket path for the snapshotter gRPC listener.
func GetUnixSocketPath() string {
	return viper.GetString(flagUnixSocketPath)
}

// RegisterGRPCService creates the native overlay snapshotter, wires up the containerd resolver,
// registers the snapshot gRPC service on srv, and returns a Closer that tears down both.
func RegisterGRPCService(logger logr.Logger, srv *grpc.Server) (closer io.Closer, err error) {
	logger = logger.WithName("snapshotter")

	var closers closerFuncs
	defer func() {
		if err != nil {
			if closeErr := closers.Close(); closeErr != nil {
				logger.Error(closeErr, "failed to close resources during initialization failure")
			}
		}
	}()

	var sn snapshots.Snapshotter
	if sn, err = createNativeOverlaySnapshotter(logger); err != nil {
		logger.Error(err, "failed to create overlay snapshotter")
		return
	}
	closers = append(closers, sn.Close)

	socketPath := viper.GetString(flagContainerdSocket)
	res, err := newResolver(logger)
	if err != nil {
		logger.Error(err, "failed to create containerd resolver", "socket", socketPath)
		return
	}
	closers = append(closers, res.Close)

	closer = closers

	wrapped := &snapshotter{logger: logger, Snapshotter: sn, resolver: res}
	snapshotService := snapshotservice.FromSnapshotter(wrapped)
	snapshotsv1.RegisterSnapshotsServer(srv, snapshotService)
	return
}

// closerFunc is a function that implements io.Closer.
type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// closerFuncs is a slice of closerFunc that itself implements io.Closer.
// Close calls each element in order and returns the first non-nil error.
type closerFuncs []closerFunc

func (cs closerFuncs) Close() error {
	var errs []error
	for _, c := range cs {
		if err := c(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type snapshotter struct {
	logger   logr.Logger
	resolver *resolver
	snapshots.Snapshotter
}

func (sn snapshotter) Stat(ctx context.Context, key string) (info snapshots.Info, err error) {
	sn.logger.V(4).Info("Stat called", "key", key)
	if info, err = sn.Snapshotter.Stat(ctx, key); err != nil {
		sn.logger.Error(err, "failed to stat", "key", key)
	} else {
		sn.logger.V(4).Info("Stat completed", "key", key, "labels", info.Labels)
	}
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
	if err = sn.Snapshotter.Close(); err != nil {
		sn.logger.Error(err, "failed to close")
	} else {
		sn.logger.V(4).Info("Close completed")
	}
	return
}
