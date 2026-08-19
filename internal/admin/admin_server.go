// Package admin holds the server-side admin gRPC API (APIService handler and
// server preparation) split out of the gRPC transport package in PR-KA-D12
// (KD-K26 phase two). The shared gRPC server groundwork stays in
// pkg/transport/grpc and is called back from here.
package admin

import (
	"context"

	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop/internal/runtime"
	"github.com/messageloopio/messageloop/pkg/transport/grpc"
	serverv2 "github.com/messageloopio/messageloop/shared/genproto/server/v2"
	googlegrpc "google.golang.org/grpc"
)

// PrepareAdminServer pre-binds a listener and registers the server-side admin API.
func PrepareAdminServer(opts grpc.Options, node *runtime.Node) (*grpc.Server, error) {
	var extraOpts []googlegrpc.ServerOption
	if opts.AdminAuthToken != "" {
		extraOpts = append(extraOpts, googlegrpc.UnaryInterceptor(grpc.AdminAuthInterceptor(opts.AdminAuthToken)))
	} else if opts.AdminAllowInsecure {
		log.WarnContext(context.Background(), "admin gRPC running WITHOUT authentication (allow_insecure)")
	}
	return grpc.PrepareServer("grpc-admin-server", opts, func(grpcServer *googlegrpc.Server) {
		serverv2.RegisterAPIServiceServer(grpcServer, NewAPIServiceHandler(node))
	}, extraOpts...)
}
