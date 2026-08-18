package grpcstream

import (
	"context"

	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop"
	serverv2 "github.com/messageloopio/messageloop/shared/genproto/server/v2"
	"google.golang.org/grpc"
)

// PrepareAdminServer pre-binds a listener and registers the server-side admin API.
func PrepareAdminServer(opts Options, node *messageloop.Node) (*Server, error) {
	var extraOpts []grpc.ServerOption
	if opts.AdminAuthToken != "" {
		extraOpts = append(extraOpts, grpc.UnaryInterceptor(adminAuthInterceptor(opts.AdminAuthToken)))
	} else if opts.AdminAllowInsecure {
		log.WarnContext(context.Background(), "admin gRPC running WITHOUT authentication (allow_insecure)")
	}
	return prepareServer("grpc-admin-server", opts, func(grpcServer *grpc.Server) {
		serverv2.RegisterAPIServiceServer(grpcServer, NewAPIServiceHandler(node))
	}, extraOpts...)
}
