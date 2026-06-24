package main

import (
	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	"github.com/humble-mun/chassis/pkg/app"
	"github.com/humble-mun/chassis/pkg/metrics"
	"github.com/humble-mun/chassis/pkg/server"
	"github.com/humble-mun/chassis/pkg/version"

	"github.com/humble-mun/pv-snapshotter/pkg/annotation"
	"github.com/humble-mun/pv-snapshotter/pkg/containerd/snapshotter"
	"github.com/humble-mun/pv-snapshotter/pkg/webhook"
)

func newRootCommand() *cobra.Command {
	var init func() error
	cmd := &cobra.Command{
		Use:   version.Name,
		Short: "containerd proxy snapshotter (gRPC plugin) that redirects the overlay upperdir/workdir.",
		Long: "A containerd proxy snapshotter that rewrites overlay mount options so that upperdir and " +
			"workdir point at a caller-provided path (for example, a mounted PersistentVolume) when a pod " +
			"carries the configured upperdir annotation. Without the annotation it is a transparent " +
			"pass-through to the native overlay snapshotter.",
		FParseErrWhitelist: cobra.FParseErrWhitelist{
			UnknownFlags: true,
		},
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) (err error) {
			srv := grpc.NewServer()

			var base app.Base
			if base, err = app.BaseContext(
				app.WithInit(init), app.WithGRPCServer(srv),
				app.WithUnixListener(server.WithAddr(snapshotter.GetUnixSocketPath))); err != nil {
				return
			}

			var svc snapshotter.Service
			if svc, err = snapshotter.RegisterGRPCService(base.RootLogger, base.NodeName, srv); err != nil {
				base.Logger.Error(err, "register snapshotter GRPC service failed")
				return
			}
			defer func() {
				if e := svc.Close(); e != nil {
					base.Logger.Error(e, "close snapshotter grpc service failed")
				}
			}()

			base.HTTPGin.RegisterRoute(svc.RegisterRoute)
			metrics.RegisterScrapeHook(svc.RegisterScrapeHook)

			if webhook.Enabled() {
				var h *webhook.Handler
				if h, err = webhook.New(base.RootLogger); err != nil {
					return
				}
				base.HTTPGin.RegisterRoute(h.RegisterRoute)
			}

			base.Logger.Info("snapshotter started")
			defer base.Logger.Info("snapshotter finished")
			if err = base.HTTPGin.Start(base.Ctx); err != nil {
				base.Logger.Error(err, "start manager failed")
				return
			}
			<-base.Ctx.Done()
			return
		},
	}

	init = app.PrepareFlags(version.Name, cmd,
		annotation.RegisterFlags, snapshotter.RegisterFlags, webhook.RegisterFlags)
	cmd.AddCommand(newConfigCommand())
	return cmd
}

func main() {
	_ = newRootCommand().Execute()
}
