package grpcstream

import (
	"github.com/messageloopio/messageloop"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	"google.golang.org/grpc"
)

// PrepareClientServer pre-binds a listener and registers the client streaming gRPC service.
func PrepareClientServer(opts Options, node *messageloop.Node) (*Server, error) {
	return prepareServer("grpc-client-server", opts, func(grpcServer *grpc.Server) {
		var handlerOpts []GRPCHandlerOption
		if opts.WriteTimeout > 0 {
			handlerOpts = append(handlerOpts, WithWriteTimeout(opts.WriteTimeout))
		}
		clientpb.RegisterMessageLoopServiceServer(grpcServer, NewGRPCHandler(node, handlerOpts...))
	})
}
