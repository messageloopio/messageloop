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

func (s *preparedGRPCServers) Components() []lynx.Component {
	if s == nil {
		return nil
	}
	return []lynx.Component{s.client, s.admin}
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

func prepareGRPCServers(cfg *config.Config, node *messageloop.Node) (*preparedGRPCServers, error) {
	clientOpts := grpcstream.Options{
		Addr:        cfg.Transport.GRPC.Addr,
		TLSCertFile: cfg.Transport.GRPC.TLS.CertFile,
		TLSKeyFile:  cfg.Transport.GRPC.TLS.KeyFile,
	}
	if cfg.Transport.GRPC.WriteTimeout != "" {
		if d, err := time.ParseDuration(cfg.Transport.GRPC.WriteTimeout); err == nil {
			clientOpts.WriteTimeout = d
		}
	}

	clientServer, err := grpcstream.PrepareClientServer(clientOpts, node)
	if err != nil {
		return nil, err
	}

	adminServer, err := grpcstream.PrepareAdminServer(grpcstream.Options{
		Addr:        cfg.Server.GRPCAdmin.Addr,
		TLSCertFile: cfg.Server.GRPCAdmin.TLS.CertFile,
		TLSKeyFile:  cfg.Server.GRPCAdmin.TLS.KeyFile,
	}, node)
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
