package main

import (
	"time"

	"github.com/messageloopio/messageloop/internal/runtime"
	"github.com/messageloopio/messageloop/config"
	"github.com/messageloopio/messageloop/internal/admin"
	"github.com/messageloopio/messageloop/pkg/transport/grpc"
	"github.com/lynx-go/lynx"
)

type preparedGRPCServers struct {
	client *grpc.Server
	admin  *grpc.Server
}

func (s *preparedGRPCServers) Components() []lynx.Service {
	if s == nil {
		return nil
	}
	return []lynx.Service{s.client, s.admin}
}

// Close releases both pre-bound gRPC listeners. It is invoked from the
// runner's OnStop hook as a defensive measure so listeners cannot leak even
// if a component fails to start after prepareGRPCServers.
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

func newGRPCClientServer(cfg *config.Config, node *runtime.Node) (*grpc.Server, error) {
	opts := grpc.Options{
		Addr:           cfg.Transport.GRPC.Addr,
		TLSCertFile:    cfg.Transport.GRPC.TLS.CertFile,
		TLSKeyFile:     cfg.Transport.GRPC.TLS.KeyFile,
		MaxRecvMsgSize: node.MaxMessageSize(),
	}
	if cfg.Transport.GRPC.WriteTimeout != "" {
		if d, err := time.ParseDuration(cfg.Transport.GRPC.WriteTimeout); err == nil {
			opts.WriteTimeout = d
		}
	}
	return grpc.PrepareClientServer(opts, node)
}

func newGRPCAdminServer(cfg *config.Config, node *runtime.Node) (*grpc.Server, error) {
	return admin.PrepareAdminServer(grpc.Options{
		Addr:               cfg.Server.GRPCAdmin.Addr,
		TLSCertFile:        cfg.Server.GRPCAdmin.TLS.CertFile,
		TLSKeyFile:         cfg.Server.GRPCAdmin.TLS.KeyFile,
		AdminAuthToken:     cfg.Server.GRPCAdmin.AuthToken,
		AdminAllowInsecure: cfg.Server.GRPCAdmin.AllowInsecure,
	}, node)
}

// prepareGRPCServers pre-binds both gRPC listeners. If the admin server fails
// to prepare, the client server is closed so its listener is released.
func prepareGRPCServers(cfg *config.Config, node *runtime.Node) (*preparedGRPCServers, error) {
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
