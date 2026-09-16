package main

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/lynx-go/lynx"

	"github.com/messageloopio/messageloop/config"
	"github.com/messageloopio/messageloop/internal/runtime"
	"github.com/messageloopio/messageloop/internal/serverapi"
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

func newGRPCAPIServer(cfg *config.Config, node *runtime.Node, adminAuthRequests, adminRPCs *prometheus.CounterVec) (*grpc.Server, error) {
	return serverapi.PrepareServer(grpc.Options{
		Addr:                 cfg.Server.API.Addr,
		TLSCertFile:          cfg.Server.API.TLS.CertFile,
		TLSKeyFile:           cfg.Server.API.TLS.KeyFile,
		AuthTokens:           cfg.Server.API.AuthTokens,
		APIAllowInsecure:     cfg.Server.API.AllowInsecure,
		APIAuthCacheTTL:      apiAuthCacheTTL(cfg),
		APIFindProxy:         apiAuthFindProxy(cfg, node),
		APICapabilityCeiling: cfg.Server.API.Capabilities,
	}, node, adminAuthRequests, adminRPCs)
}

// apiAuthCacheTTL parses server.api.auth_cache_ttl. An empty
// value resolves to 0 (= the resolver's 30s default); unparsable or
// non-positive values cannot occur — config.Validate rejects them — and are
// defensively treated as 0.
func apiAuthCacheTTL(cfg *config.Config) time.Duration {
	if cfg.Server.API.AuthCacheTTL == "" {
		return 0
	}
	d, err := time.ParseDuration(cfg.Server.API.AuthCacheTTL)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// apiAuthFindProxy returns the proxy assigned with api_auth: true (at
// most one — config.Validate enforces the uniqueness), or nil when nothing
// is assigned. The instance is resolved by probing the node's proxy router
// with the assigned entry's own route patterns: AddFromConfig compiles each
// pattern into a glob, and a glob always matches its own pattern text, so
// the probe is guaranteed to hit at least the assigned entry's routes.
func apiAuthFindProxy(cfg *config.Config, node *runtime.Node) func() proxyproxy.Proxy {
	var assigned *config.ProxyConfig
	for i := range cfg.Proxy {
		if cfg.Proxy[i].APIAuth {
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
func prepareGRPCServers(cfg *config.Config, node *runtime.Node, adminAuthRequests, adminRPCs *prometheus.CounterVec) (*preparedGRPCServers, error) {
	clientServer, err := newGRPCClientServer(cfg, node)
	if err != nil {
		return nil, err
	}

	adminServer, err := newGRPCAPIServer(cfg, node, adminAuthRequests, adminRPCs)
	if err != nil {
		_ = clientServer.Close()
		return nil, err
	}

	return &preparedGRPCServers{client: clientServer, admin: adminServer}, nil
}
