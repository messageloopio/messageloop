package grpcstream

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/messageloopio/messageloop"
	serverpb "github.com/messageloopio/messageloop/shared/genproto/server/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

// mockTransport is a simple transport for testing
type mockTransport struct {
	closed bool
}

func (m *mockTransport) Write(data []byte) error {
	return nil
}

func (m *mockTransport) WriteMany(data ...[]byte) error {
	return nil
}

func (m *mockTransport) Close(disconnect messageloop.Disconnect) error {
	m.closed = true
	return nil
}

func (m *mockTransport) RemoteAddr() string {
	return "127.0.0.1:12345"
}

func TestAPIServiceHandler_PublishToSessions(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	handler := NewAPIServiceHandler(node)

	// Create a test client
	transport := &mockTransport{}
	client, closeFn, err := messageloop.NewClient(ctx, node, transport, messageloop.ProtobufMarshaler{})
	require.NoError(t, err)
	defer closeFn()

	// Authenticate the client (required for it to be in the hub)
	node.AddClient(client)

	// Create payload
	s, _ := structpb.NewStruct(map[string]interface{}{"message": "test payload"})

	// Test publishing to the session
	req := &serverpb.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverpb.Publication{
			{
				Id: uuid.NewString(),
				Destination: &serverpb.Publication_Destination{
					Sessions: []string{client.SessionID()},
				},
				Payload: &sharedpb.Payload{
					Data: &sharedpb.Payload_Json{
						Json: s,
					},
				},
			},
		},
	}

	resp, err := handler.Publish(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
}

func TestAPIServiceHandler_PublishToNonExistentSession(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	handler := NewAPIServiceHandler(node)

	// Create payload
	s, _ := structpb.NewStruct(map[string]interface{}{"message": "test payload"})

	// Test publishing to a non-existent session - should not error
	req := &serverpb.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverpb.Publication{
			{
				Id: uuid.NewString(),
				Destination: &serverpb.Publication_Destination{
					Sessions: []string{"non-existent-session-id"},
				},
				Payload: &sharedpb.Payload{
					Data: &sharedpb.Payload_Json{
						Json: s,
					},
				},
			},
		},
	}

	resp, err := handler.Publish(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
}

func TestAPIServiceHandler_PublishToChannels(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	_ = node.Run() // Register event handler
	handler := NewAPIServiceHandler(node)

	// Create payload with binary data
	// Test publishing to channels
	req := &serverpb.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverpb.Publication{
			{
				Id: uuid.NewString(),
				Destination: &serverpb.Publication_Destination{
					Channels: []string{"test-channel-1", "test-channel-2"},
				},
				Payload: &sharedpb.Payload{
					Data: &sharedpb.Payload_Binary{
						Binary: []byte("test payload"),
					},
				},
				Options: &serverpb.Publication_Options{
					AddHistory: true,
				},
			},
		},
	}

	resp, err := handler.Publish(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
}

func TestAPIServiceHandler_Disconnect(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	handler := NewAPIServiceHandler(node)

	// Create a test client
	transport := &mockTransport{}
	client, _, err := messageloop.NewClient(ctx, node, transport, messageloop.ProtobufMarshaler{})
	require.NoError(t, err)

	// Add the client to the hub
	node.AddClient(client)

	// Test disconnecting the session
	req := &serverpb.DisconnectRequest{
		Sessions: []string{client.SessionID()},
		Code:     3500,
		Reason:   "test disconnect",
	}

	resp, err := handler.Disconnect(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.True(t, resp.Results[client.SessionID()])
	require.True(t, transport.closed)
}

func TestAPIServiceHandler_DisconnectNonExistentSession(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	handler := NewAPIServiceHandler(node)

	// Test disconnecting a non-existent session
	req := &serverpb.DisconnectRequest{
		Sessions: []string{"non-existent-session-id"},
		Code:     3500,
		Reason:   "test disconnect",
	}

	resp, err := handler.Disconnect(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.False(t, resp.Results["non-existent-session-id"])
}

func TestAPIServiceHandler_Subscribe(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	handler := NewAPIServiceHandler(node)

	// Create a test client
	transport := &mockTransport{}
	client, closeFn, err := messageloop.NewClient(ctx, node, transport, messageloop.ProtobufMarshaler{})
	require.NoError(t, err)
	defer closeFn()

	// Add the client to the hub
	node.AddClient(client)

	// Test subscribing to channels
	req := &serverpb.SubscribeRequest{
		SessionId: client.SessionID(),
		Channels:  []string{"test-channel-1", "test-channel-2"},
	}

	resp, err := handler.Subscribe(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.True(t, resp.Results["test-channel-1"])
	require.True(t, resp.Results["test-channel-2"])
}

func TestAPIServiceHandler_SubscribeNonExistentSession(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	handler := NewAPIServiceHandler(node)

	// Test subscribing with a non-existent session
	req := &serverpb.SubscribeRequest{
		SessionId: "non-existent-session-id",
		Channels:  []string{"test-channel-1"},
	}

	resp, err := handler.Subscribe(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.False(t, resp.Results["test-channel-1"])
}

func TestAPIServiceHandler_Unsubscribe(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	handler := NewAPIServiceHandler(node)

	// Create a test client
	transport := &mockTransport{}
	client, closeFn, err := messageloop.NewClient(ctx, node, transport, messageloop.ProtobufMarshaler{})
	require.NoError(t, err)
	defer closeFn()

	// Add the client to the hub
	node.AddClient(client)

	// First subscribe to a channel
	subReq := &serverpb.SubscribeRequest{
		SessionId: client.SessionID(),
		Channels:  []string{"test-channel-1"},
	}
	_, err = handler.Subscribe(ctx, subReq)
	require.NoError(t, err)

	// Then unsubscribe
	req := &serverpb.UnsubscribeRequest{
		SessionId: client.SessionID(),
		Channels:  []string{"test-channel-1"},
	}

	resp, err := handler.Unsubscribe(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.True(t, resp.Results["test-channel-1"])
}

func TestAPIServiceHandler_UnsubscribeNonExistentSession(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	handler := NewAPIServiceHandler(node)

	// Test unsubscribing with a non-existent session
	req := &serverpb.UnsubscribeRequest{
		SessionId: "non-existent-session-id",
		Channels:  []string{"test-channel-1"},
	}

	resp, err := handler.Unsubscribe(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.False(t, resp.Results["test-channel-1"])
}
