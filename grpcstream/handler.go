package grpcstream

import (
	"github.com/deeplooplabs/messageloop"
	clientv1 "github.com/deeplooplabs/messageloop-protocol/gen/proto/go/client/v1"
	"github.com/lynx-go/x/log"
	"google.golang.org/grpc"
	"io"
)

type gRPCHandler struct {
	clientv1.UnimplementedMessageLoopServiceServer
	node *messageloop.Node
}

func (h *gRPCHandler) MessageLoop(stream grpc.BidiStreamingServer[clientv1.ClientMessage, clientv1.ServerMessage]) error {
	transport := newGRPCTransport(stream)
	client, closeFn, err := messageloop.NewClient(stream.Context(), h.node, transport, messageloop.DefaultProtoMarshaler)
	if err != nil {
		return err
	}
	defer closeFn()
	ctx := stream.Context()
	ctx = log.Context(ctx, log.FromContext(ctx), "client_id", client.SessionID())

	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-transport.closeCh:
			return nil

		default:
			in, err := stream.Recv()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			if err := client.HandleMessage(ctx, in); err != nil {
				return err
			}
		}
	}
}

func NewGRPCHandler(node *messageloop.Node) clientv1.MessageLoopServiceServer {
	return &gRPCHandler{
		node: node,
	}
}
