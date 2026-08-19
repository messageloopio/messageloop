package grpc

import (
	"github.com/messageloopio/messageloop"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	googlegrpc "google.golang.org/grpc"
)

// PrepareClientServer pre-binds a listener and registers the client streaming gRPC service.
func PrepareClientServer(opts Options, node *messageloop.Node) (*Server, error) {
	return PrepareServer("grpc-client-server", opts, func(grpcServer *googlegrpc.Server) {
		var handlerOpts []GRPCHandlerOption
		if opts.WriteTimeout > 0 {
			handlerOpts = append(handlerOpts, WithWriteTimeout(opts.WriteTimeout))
		}
		clientpb.RegisterMessageLoopServiceServer(grpcServer, NewGRPCHandler(node, handlerOpts...))
	})
}
