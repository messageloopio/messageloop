// Package admin holds the server-side admin gRPC API (APIService handler and
// server preparation) split out of the gRPC transport package in PR-KA-D12
// (KD-K26 phase two). The shared gRPC server groundwork stays in
// pkg/transport/grpc and is called back from here.
package admin

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

// PrepareAdminServer pre-binds a listener and registers the server-side
// admin API behind the admin authentication interceptor (design §2.3): the
// static auth_tokens list, the allow_insecure escape hatch, and the
// admin_auth-assigned proxy verifier. adminAuthRequests receives the
// admin_auth_requests_total counter and adminRPCs the admin_rpc_total
// counter (nil disables each metric).
func PrepareAdminServer(opts grpc.Options, node *runtime.Node, adminAuthRequests, adminRPCs *prometheus.CounterVec) (*grpc.Server, error) {
	if len(opts.AuthTokens) == 0 && !opts.AdminAllowInsecure &&
		(opts.AdminFindProxy == nil || opts.AdminFindProxy() == nil) {
		log.WarnContext(context.Background(), "admin gRPC running WITHOUT authentication (no auth_tokens, allow_insecure, or admin_auth assignment)")
	}
	resolver := newAdminAuthResolver(adminAuthOptions{
		AuthTokens:    opts.AuthTokens,
		AllowInsecure: opts.AdminAllowInsecure,
		FindProxy:     opts.AdminFindProxy,
		Ceiling:       capabilityCeilingFromNames(opts.AdminCapabilityCeiling),
		CacheTTL:      opts.AdminAuthCacheTTL,
		AuthRequests:  adminAuthRequests,
		RPCs:          adminRPCs,
	})
	return grpc.PrepareServer("grpc-admin-server", opts, func(grpcServer *googlegrpc.Server) {
		serverv2.RegisterAPIServiceServer(grpcServer, NewAPIServiceHandler(node))
	}, googlegrpc.UnaryInterceptor(resolver.Interceptor()))
}

// capabilityCeilingFromNames maps closed-set capability names onto the bit
// set. An absent list resolves to the default admin capability set; unknown
// names are dropped (config.Validate already rejects them up front).
func capabilityCeilingFromNames(names []string) authz.Capability {
	if len(names) == 0 {
		return authz.DefaultAdminCapabilities
	}
	var caps authz.Capability
	for _, name := range names {
		if bit, ok := authz.ClosedCapabilityNames[name]; ok {
			caps |= bit
		}
	}
	return caps
}
