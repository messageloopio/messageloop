package main

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/lynx-go/lynx"

	"github.com/messageloopio/messageloop/config"
	"github.com/messageloopio/messageloop/internal/admin"
	"github.com/messageloopio/messageloop/internal/runtime"
	"github.com/messageloopio/messageloop/pkg/transport/grpc"
	proxyproxy "github.com/messageloopio/messageloop/proxy"
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
// runner's OnPreStop hook as a defensive measure so listeners cannot leak even
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

func newGRPCAdminServer(cfg *config.Config, node *runtime.Node, adminAuthRequests *prometheus.CounterVec) (*grpc.Server, error) {
	return admin.PrepareAdminServer(grpc.Options{
		Addr:                   cfg.Server.GRPCAdmin.Addr,
		TLSCertFile:            cfg.Server.GRPCAdmin.TLS.CertFile,
		TLSKeyFile:             cfg.Server.GRPCAdmin.TLS.KeyFile,
		AuthTokens:             cfg.Server.GRPCAdmin.AuthTokens,
		AdminAllowInsecure:     cfg.Server.GRPCAdmin.AllowInsecure,
		AdminAuthCacheTTL:      adminAuthCacheTTL(cfg),
		AdminFindProxy:         adminAuthFindProxy(cfg, node),
		AdminCapabilityCeiling: cfg.Server.GRPCAdmin.Capabilities,
	}, node, adminAuthRequests)
}

// adminAuthCacheTTL parses server.grpc_admin.admin_auth_cache_ttl. An empty
// value resolves to 0 (= the resolver's 30s default); unparsable or
// non-positive values cannot occur — config.Validate rejects them — and are
// defensively treated as 0.
func adminAuthCacheTTL(cfg *config.Config) time.Duration {
	if cfg.Server.GRPCAdmin.AdminAuthCacheTTL == "" {
		return 0
	}
	d, err := time.ParseDuration(cfg.Server.GRPCAdmin.AdminAuthCacheTTL)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// adminAuthFindProxy returns the proxy assigned with admin_auth: true (at
// most one — config.Validate enforces the uniqueness), or nil when nothing
// is assigned. The instance is resolved by probing the node's proxy router
// with the assigned entry's own route patterns: AddFromConfig compiles each
// pattern into a glob, and a glob always matches its own pattern text, so
// the probe is guaranteed to hit at least the assigned entry's routes.
func adminAuthFindProxy(cfg *config.Config, node *runtime.Node) func() proxyproxy.Proxy {
	var assigned *config.ProxyConfig
	for i := range cfg.Proxy {
		if cfg.Proxy[i].AdminAuth {
			assigned = &cfg.Proxy[i]
			break
		}
	}
	if assigned == nil {
		return nil
	}
	return func() proxyproxy.Proxy {
		for _, route := range assigned.Routes {
			if p := node.FindProxy(route.Channel, route.Method); p != nil {
				return p
			}
		}
		// An assigned entry without routes is unreachable via the router;
		// key verification is effectively absent (fail-closed).
		return nil
	}
}

// prepareGRPCServers pre-binds both gRPC listeners. If the admin server fails
// to prepare, the client server is closed so its listener is released.
func prepareGRPCServers(cfg *config.Config, node *runtime.Node, adminAuthRequests *prometheus.CounterVec) (*preparedGRPCServers, error) {
	clientServer, err := newGRPCClientServer(cfg, node)
	if err != nil {
		return nil, err
	}

	adminServer, err := newGRPCAdminServer(cfg, node, adminAuthRequests)
	if err != nil {
		_ = clientServer.Close()
		return nil, err
	}

	return &preparedGRPCServers{client: clientServer, admin: adminServer}, nil
}
