package main

import (
	"context"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/config"
	"github.com/messageloopio/messageloop/pkg/grpcstream"
)

type nodeRunner interface {
	Run(context.Context) error
}

type preparedGRPCServers struct {
	client *grpcstream.Server
	admin  *grpcstream.Server
}

func (s *preparedGRPCServers) Components() []lynx.Service {
	if s == nil {
		return nil
	}
	return []lynx.Service{s.client, s.admin}
}

func (s *preparedGRPCServers) Close() {
	if s == nil {
		return
	}
	if s.admin != nil {
		_ = s.admin.Close()
	}
	if s.client != nil {
		_ = s.client.Close()
	}
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

// prepareGRPCServers pre-binds both gRPC listeners. If the admin server fails
// to prepare, the client server is closed so its listener is released.
func prepareGRPCServers(cfg *config.Config, node *messageloop.Node) (*preparedGRPCServers, error) {
	clientServer, err := newGRPCClientServer(cfg, node)
	if err != nil {
		return nil, err
	}

	adminServer, err := newGRPCAdminServer(cfg, node)
	if err != nil {
		_ = clientServer.Close()
		return nil, err
	}

	return &preparedGRPCServers{client: clientServer, admin: adminServer}, nil
}

func runNodeWithPreflight(ctx context.Context, runner nodeRunner, preflight func() error) error {
	if err := preflight(); err != nil {
		return err
	}
	return runner.Run(ctx)
}

var _ nodeRunner = (*messageloop.Node)(nil)
