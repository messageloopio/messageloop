// Package serverapi holds the Server API — the server-side gRPC API
// (APIService handler and server preparation) split out of the gRPC
// transport package in PR-KA-D12 (KD-K26 phase two). The shared gRPC server
// groundwork stays in pkg/transport/grpc and is called back from here.
package serverapi

import (
	"context"

	"github.com/lynx-go/x/log"
	"github.com/prometheus/client_golang/prometheus"
	googlegrpc "google.golang.org/grpc"

	"github.com/messageloopio/messageloop/internal/authz"
	"github.com/messageloopio/messageloop/internal/runtime"
	"github.com/messageloopio/messageloop/pkg/transport/grpc"
	serverv2 "github.com/messageloopio/messageloop/shared/genproto/server/v2"
)

// PrepareServer pre-binds a listener and registers the Server API behind
// the Server API authentication interceptor (design §2.3): the static
// auth_tokens list, the allow_insecure escape hatch, and the api_auth-
// assigned proxy verifier. authRequests receives the
// server_api_auth_requests_total counter and rpcs the
// server_api_rpc_total counter (nil disables each metric).
func PrepareServer(opts grpc.Options, node *runtime.Node, authRequests, rpcs *prometheus.CounterVec) (*grpc.Server, error) {
	if len(opts.AuthTokens) == 0 && !opts.APIAllowInsecure &&
		(opts.APIFindProxy == nil || opts.APIFindProxy() == nil) {
		log.WarnContext(context.Background(), "server API gRPC running WITHOUT authentication (no auth_tokens, allow_insecure, or api_auth assignment)")
	}
	resolver := newAPIAuthResolver(apiAuthOptions{
		AuthTokens:    opts.AuthTokens,
		AllowInsecure: opts.APIAllowInsecure,
		FindProxy:     opts.APIFindProxy,
		Ceiling:       capabilityCeilingFromNames(opts.APICapabilityCeiling),
		CacheTTL:      opts.APIAuthCacheTTL,
		AuthRequests:  authRequests,
		RPCs:          rpcs,
	})
	return grpc.PrepareServer("grpc-api-server", opts, func(grpcServer *googlegrpc.Server) {
		serverv2.RegisterAPIServiceServer(grpcServer, NewAPIServiceHandler(node))
	}, googlegrpc.UnaryInterceptor(resolver.Interceptor()))
}

// capabilityCeilingFromNames maps closed-set capability names onto the bit
// set. An absent list resolves to the default capability ceiling; unknown
// names are dropped (config.Validate already rejects them up front).
func capabilityCeilingFromNames(names []string) authz.Capability {
	if len(names) == 0 {
		return authz.DefaultCapabilityCeiling
	}
	var caps authz.Capability
	for _, name := range names {
		if bit, ok := authz.ClosedCapabilityNames[name]; ok {
			caps |= bit
		}
	}
	return caps
}
