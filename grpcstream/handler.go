package grpcstream

import (
	"github.com/deeploopdev/messageloop"
	clientv1 "github.com/deeploopdev/messageloop-protocol/gen/proto/go/client/v1"
	"google.golang.org/grpc"
	"io"
)

type gRPCHandler struct {
	clientv1.UnimplementedMessageLoopServiceServer
	node *messageloop.Node
}

func (h *gRPCHandler) MessageLoop(stream grpc.BidiStreamingServer[clientv1.ClientMessage, clientv1.ServerMessage]) error {
	transport := newGRPCTransport(stream)
	client, closeFn, err := messageloop.NewClient(stream.Context(), h.node, transport)
	if err != nil {
		return err
	}
	defer closeFn()
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
			if err := client.HandleMessage(in); err != nil {
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
