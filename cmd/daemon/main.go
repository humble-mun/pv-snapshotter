package main

import (
	"context"
	"io"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	"github.com/humble-mun/chassis/pkg/app"
	"github.com/humble-mun/chassis/pkg/server"

	"github.com/humble-mun/pv-snapshotter/pkg/containerd/snapshotter"
	"github.com/humble-mun/pv-snapshotter/pkg/service"
)

func newRootCommand() *cobra.Command {
	var init func() error
	cmd := &cobra.Command{
		Use:   service.Name,
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

			var rootLogger, logger logr.Logger
			var httpGin *server.HTTPServer
			var ctx context.Context
			var nodeName string
			if rootLogger, logger, httpGin, ctx, nodeName, err = app.BaseContext(
				app.WithInit(init), app.WithGRPCServer(srv),
				app.WithUnixListener(server.WithAddr(snapshotter.GetUnixSocketPath))); err != nil {
				return
			}
			logger = logger.WithValues("nodeName", nodeName)

			var sn io.Closer
			if sn, err = snapshotter.RegisterGRPCService(rootLogger, srv); err != nil {
				logger.Error(err, "register snapshotter GRPC service failed")
				return
			}
			defer func() {
				if e := sn.Close(); e != nil {
					logger.Error(e, "close snapshotter grpc service failed")
				}
			}()

			logger.Info("snapshotter started")
			defer logger.Info("snapshotter finished")
			if err = httpGin.Start(ctx); err != nil {
				logger.Error(err, "start manager failed")
				return
			}
			<-ctx.Done()
			return
		},
	}

	init = app.PrepareFlags(service.Name, cmd, snapshotter.RegisterFlags)
	cmd.AddCommand(newConfigCommand())
	return cmd
}

func main() {
	_ = newRootCommand().Execute()
}
