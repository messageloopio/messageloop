package grpcstream

import (
	"github.com/messageloopio/messageloop"
	serverpb "github.com/messageloopio/messageloop/shared/genproto/server/v1"
	"google.golang.org/grpc"
)

// PrepareAdminServer pre-binds a listener and registers the server-side admin API.
func PrepareAdminServer(opts Options, node *messageloop.Node) (*Server, error) {
	var extraOpts []grpc.ServerOption
	if opts.AdminAuthToken != "" {
		extraOpts = append(extraOpts, grpc.UnaryInterceptor(adminAuthInterceptor(opts.AdminAuthToken)))
	}
	return prepareServer("grpc-admin-server", opts, func(grpcServer *grpc.Server) {
		serverpb.RegisterAPIServiceServer(grpcServer, NewAPIServiceHandler(node))
	}, extraOpts...)
}
