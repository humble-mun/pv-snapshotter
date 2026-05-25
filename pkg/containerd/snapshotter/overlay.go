//go:build linux

package snapshotter

import (
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/plugins/snapshots/overlay"
	"github.com/containerd/containerd/v2/plugins/snapshots/overlay/overlayutils"
	"github.com/go-logr/logr"
	"github.com/spf13/viper"
)

const (
	flagOverlaySnapshotterConfig = "overlay-snapshotter"
	defaultRootPath              = "/var/lib/containerd"
)

// vendor/github.com/containerd/containerd/plugins/snapshots/overlay/plugin/plugin.go
type config struct {
	RootPath      string   `mapstructure:"root-path"`
	UpperDirLabel bool     `mapstructure:"upper-dir-label"`
	SyncRemove    bool     `mapstructure:"sync-remove"`
	SlowChown     bool     `mapstructure:"slow-chown"`
	MountOptions  []string `mapstructure:"mount-options"`
}

func createNativeOverlaySnapshotter(logger logr.Logger) (snapshots.Snapshotter, error) {
	var cfg config
	if err := viper.UnmarshalKey(flagOverlaySnapshotterConfig, &cfg); err != nil {
		return nil, err
	}
	logger.WithValues("rootPath", cfg.RootPath, "upperDirLabel", cfg.UpperDirLabel,
		"syncRemove", cfg.SyncRemove, "slowChown", cfg.SlowChown, "mountOptions", cfg.MountOptions).Info(
		"overlay snapshotter config")
	if cfg.RootPath == "" {
		cfg.RootPath = defaultRootPath
	}
	var opts []overlay.Opt
	if cfg.UpperDirLabel {
		opts = append(opts, overlay.WithUpperdirLabel)
	}
	if !cfg.SyncRemove {
		opts = append(opts, overlay.AsynchronousRemove)
	}
	if mos := cfg.MountOptions; len(mos) > 0 {
		opts = append(opts, overlay.WithMountOptions(mos))
	}
	if supported, err := overlayutils.SupportsIDMappedMounts(); err == nil && supported {
		opts = append(opts, overlay.WithRemapIDs)
	}
	if cfg.SlowChown {
		opts = append(opts, overlay.WithSlowChown)
	}
	return overlay.NewSnapshotter(cfg.RootPath, opts...)
}
