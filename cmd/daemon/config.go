package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/humble-mun/chassis/pkg/app"

	pvconfig "github.com/humble-mun/pv-snapshotter/pkg/containerd/config"
)

// configSubcommandName is the name used for viper config file lookup and the
// HM_ env-var namespace for the "config" subcommand.
const configSubcommandName = "config"

// newConfigCommand returns the "config" subcommand.
//
// Lifecycle (run as a sidecar container alongside the snapshotter daemon):
//
//  1. Wait until the pv-snapshotter daemon's /readyz endpoint returns 200.
//     The daemon must be up first: containerd needs to connect to the daemon
//     socket at restart time, so restarting containerd before the daemon is
//     ready would be pointless.
//  2. Patch /etc/containerd/config.toml (idempotent).
//  3. If the file was modified, restart containerd via setns(2) + execve.
//     The process is replaced by systemctl; Kubernetes restarts the sidecar,
//     which re-enters at step 1 (already-configured path, skips restart).
//  4. Block until the process receives a termination signal (SIGTERM/SIGINT),
//     keeping the sidecar alive for the lifetime of the Pod.
//
// Running as a native sidecar (restartPolicy: Always in initContainers,
// K8s ≥ 1.29) ensures the sidecar starts before the main container and is
// restarted if it exits unexpectedly.
func newConfigCommand() *cobra.Command {
	var initFn func() error

	cmd := &cobra.Command{
		Use:   configSubcommandName,
		Short: "Wait for the daemon, patch the containerd config, and block until termination.",
		Long: "Waits until the pv-snapshotter daemon's /readyz endpoint returns 200, then " +
			"idempotently patches /etc/containerd/config.toml to register pv-snapshotter " +
			"and the requested runtime entries. If the file was changed, restarts containerd " +
			"via setns(2)+execve. Finally blocks until the process receives a termination " +
			"signal.\n\n" +
			"Designed to run as a sidecar container (restartPolicy: Always in " +
			"initContainers, Kubernetes ≥ 1.29) alongside the snapshotter daemon.",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			_, logger, _, ctx, _, err := app.BaseContext(
				app.WithInit(initFn),
				app.WithoutHTTPServer(),
			)
			if err != nil {
				return fmt.Errorf("initialising base context: %w", err)
			}
			logger = logger.WithName("config")

			socketPath := pvconfig.GetSocketPath()

			// ── 1. Wait for pv-snapshotter daemon ────────────────────────
			// containerd connects to the daemon socket when it (re)starts.
			// Patching the config and restarting containerd before the daemon
			// is ready would be pointless — the proxy plugin would be marked
			// unhealthy immediately.
			if err = pvconfig.WaitUntilReady(ctx, logger, socketPath); err != nil {
				return fmt.Errorf("waiting for pv-snapshotter daemon: %w", err)
			}

			// ── 2. Patch containerd config.toml ──────────────────────────
			params := pvconfig.GetParams()
			logger.Info("applying containerd config",
				"configPath", params.ConfigPath,
				"socketPath", params.SocketPath,
				"runtimes", runtimeNames(params.Runtimes),
			)

			modified, applyErr := pvconfig.Apply(params)
			if applyErr != nil {
				return fmt.Errorf("applying containerd config: %w", applyErr)
			}

			// ── 3. Restart containerd if the config was changed ───────────
			// RestartContainerd execs systemctl and does not return on
			// success; the process is replaced by systemctl.  Kubernetes
			// restarts the sidecar, which re-enters at step 1 (config now
			// already up-to-date, restart skipped).
			if modified {
				logger.Info("containerd config patched; restarting containerd")
				if err = pvconfig.RestartContainerd(); err != nil {
					return fmt.Errorf("restarting containerd: %w", err)
				}
				// Unreachable on success (execve replaced the process).
			}
			logger.Info("containerd config already up-to-date; no restart needed")

			// ── 4. Block until termination signal ────────────────────────
			logger.Info("configuration complete; sidecar is blocking until termination signal")
			<-ctx.Done()
			logger.Info("termination signal received; sidecar exiting")
			return nil
		},
	}

	initFn = app.PrepareFlags(configSubcommandName, cmd, pvconfig.RegisterFlags)
	return cmd
}

// runtimeNames extracts just the handler names for structured logging.
func runtimeNames(rts []pvconfig.RuntimeEntry) []string {
	names := make([]string, len(rts))
	for i, rt := range rts {
		names[i] = rt.Name
	}
	return names
}
