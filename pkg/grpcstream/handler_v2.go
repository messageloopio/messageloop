package grpcstream

import (
	"github.com/google/uuid"
	v2 "github.com/messageloopio/messageloop/v2"
	v2pb "github.com/messageloopio/messageloop/shared/genproto/v2"
	"google.golang.org/grpc"
)

// HandlerV2 implements the v2 gRPC MessagingServiceV2 server.
type HandlerV2 struct {
	v2pb.UnimplementedMessagingServiceV2Server

	// onConnect is called when a new client connects.
	// It returns the ClientV2 options to apply (e.g. message handlers).
	onConnect func(sessionID string) []v2.ClientV2Option
}

// NewHandlerV2 creates a new v2 gRPC handler.
// onConnect is called for each new stream connection to configure the ClientV2.
func NewHandlerV2(onConnect func(sessionID string) []v2.ClientV2Option) *HandlerV2 {
	return &HandlerV2{onConnect: onConnect}
}

// MessageLoop handles an incoming v2 gRPC bidirectional stream.
func (h *HandlerV2) MessageLoop(stream grpc.BidiStreamingServer[v2pb.StreamFrame, v2pb.StreamFrame]) error {
	sessionID := uuid.New().String()
	transport := NewTransportV2(stream)

	var opts []v2.ClientV2Option
	if h.onConnect != nil {
		opts = h.onConnect(sessionID)
	}

	client := v2.NewClientV2(sessionID, transport, &v2.ProtobufEncoder{}, opts...)

	// Block until the stream ends
	client.Start()
	return nil
}
