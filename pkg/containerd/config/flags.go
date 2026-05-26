//go:build linux

package config

import (
	"strings"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

const (
	flagConfigPath         = "config-path"
	flagRuntimeClasses     = "runtime-classes"
	flagRuntimeClassSuffix = "runtime-class-suffix"
	flagSocketPath         = "socket-path"

	defaultConfigPath         = "/etc/containerd/config.toml"
	defaultRuntimeClassSuffix = "-pv"
	// defaultUnixSocketPath is the default pv-snapshotter gRPC socket path.
	// Duplicated here (also in snapshotter/service.go) so that the config
	// package has no import dependency on the snapshotter package.
	defaultUnixSocketPath = "/var/run/pv-snapshotter/daemon.sock"
)

// RegisterFlags registers all flags for the "config" subcommand onto pfs.
// Called via app.PrepareFlags so that viper binding and env-var support are
// set up automatically with the HM_ prefix.
func RegisterFlags(pfs *pflag.FlagSet) {
	pfs.String(flagConfigPath, defaultConfigPath,
		"Path to the containerd config.toml file to patch (host path, accessed via volume mount).")
	pfs.StringSlice(flagRuntimeClasses, nil,
		"Base runtime class names to extend (e.g. runc,nvidia). "+
			"For each name a new '<name><suffix>' runtime entry is injected into the containerd config.")
	pfs.String(flagRuntimeClassSuffix, defaultRuntimeClassSuffix,
		"Suffix appended to each base runtime class name to form the pv-backed handler name "+
			"(e.g. \"-pv\" produces \"runc-pv\", \"nvidia-pv\").")
	pfs.String(flagSocketPath, defaultUnixSocketPath,
		"Unix socket path that pv-snapshotter listens on; used both as the address written "+
			"into containerd config.toml and as the readiness probe target.")
}

// GetParams reads flag values from viper and returns a fully populated Params.
// Must be called after viper has been initialised (i.e. inside RunE).
func GetParams() Params {
	suffix := viper.GetString(flagRuntimeClassSuffix)
	bases := viper.GetStringSlice(flagRuntimeClasses)

	var runtimes []RuntimeEntry
	for _, base := range bases {
		base = strings.TrimSpace(base)
		if base == "" {
			continue
		}
		runtimes = append(runtimes, RuntimeEntry{
			Name:            base + suffix,
			BaseRuntimeName: base,
		})
	}

	return Params{
		ConfigPath: viper.GetString(flagConfigPath),
		SocketPath: viper.GetString(flagSocketPath),
		Runtimes:   runtimes,
	}
}

// GetSocketPath returns the configured Unix socket path.
// Used by the readiness probe in the config command.
func GetSocketPath() string {
	return viper.GetString(flagSocketPath)
}
