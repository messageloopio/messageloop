package main

import (
	"context"
	"time"

	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/config"
	"github.com/messageloopio/messageloop/pkg/grpcstream"
)

type nodeRunner interface {
	Run(context.Context) error
}

func newGRPCClientServer(cfg *config.Config, node *messageloop.Node) (*grpcstream.Server, error) {
	opts := grpcstream.Options{
		Addr:           cfg.Transport.GRPC.Addr,
		TLSCertFile:    cfg.Transport.GRPC.TLS.CertFile,
		TLSKeyFile:     cfg.Transport.GRPC.TLS.KeyFile,
		MaxRecvMsgSize: cfg.Server.Limits.MaxMessageSize,
	}
	if cfg.Transport.GRPC.WriteTimeout != "" {
		if d, err := time.ParseDuration(cfg.Transport.GRPC.WriteTimeout); err == nil {
			opts.WriteTimeout = d
		}
	}
	return grpcstream.PrepareClientServer(opts, node)
}

func newGRPCAdminServer(cfg *config.Config, node *messageloop.Node) (*grpcstream.Server, error) {
	return grpcstream.PrepareAdminServer(grpcstream.Options{
		Addr:           cfg.Server.GRPCAdmin.Addr,
		TLSCertFile:    cfg.Server.GRPCAdmin.TLS.CertFile,
		TLSKeyFile:     cfg.Server.GRPCAdmin.TLS.KeyFile,
		AdminAuthToken: cfg.Server.GRPCAdmin.AuthToken,
	}, node)
}

func runNodeWithPreflight(ctx context.Context, runner nodeRunner, preflight func() error) error {
	if err := preflight(); err != nil {
		return err
	}
	return runner.Run(ctx)
}

var _ nodeRunner = (*messageloop.Node)(nil)
