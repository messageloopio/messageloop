package grpcstream

import (
	"context"
	"testing"

	cloudevents "github.com/cloudevents/sdk-go/binding/format/protobuf/v2/pb"
	"github.com/google/uuid"
	"github.com/messageloopio/messageloop"
	serverpb "github.com/messageloopio/messageloop/shared/genproto/server/v1"
	"github.com/stretchr/testify/require"
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

func TestAPIServiceHandler_PublishToSessions(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	handler := NewAPIServiceHandler(node)

	// Create a test client
	transport := &mockTransport{}
	client, _, err := messageloop.NewClientSession(ctx, node, transport, messageloop.ProtobufMarshaler{})
	require.NoError(t, err)
	defer closeFn()

	// Authenticate the client (required for it to be in the hub)
	node.AddClient(client)

	// Test publishing to the session
	req := &serverpb.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverpb.Publication{
			{
				Id: uuid.NewString(),
				Destination: &serverpb.Publication_Destination{
					Sessions: []string{client.SessionID()},
				},
				Payload: &cloudevents.CloudEvent{
					Id:          uuid.NewString(),
					Source:      "test-source",
					Type:        "test.type",
					SpecVersion: "1.0",
					Data: &cloudevents.CloudEvent_TextData{
						TextData: "test payload",
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

	// Test publishing to a non-existent session - should not error
	req := &serverpb.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverpb.Publication{
			{
				Id: uuid.NewString(),
				Destination: &serverpb.Publication_Destination{
					Sessions: []string{"non-existent-session-id"},
				},
				Payload: &cloudevents.CloudEvent{
					Id:          uuid.NewString(),
					Source:      "test-source",
					Type:        "test.type",
					SpecVersion: "1.0",
					Data: &cloudevents.CloudEvent_TextData{
						TextData: "test payload",
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
	handler := NewAPIServiceHandler(node)

	// Test publishing to channels
	req := &serverpb.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverpb.Publication{
			{
				Id: uuid.NewString(),
				Destination: &serverpb.Publication_Destination{
					Channels: []string{"test-channel-1", "test-channel-2"},
				},
				Payload: &cloudevents.CloudEvent{
					Id:          uuid.NewString(),
					Source:      "test-source",
					Type:        "test.type",
					SpecVersion: "1.0",
					Data: &cloudevents.CloudEvent_BinaryData{
						BinaryData: []byte("test payload"),
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
	client, _, err := messageloop.NewClientSession(ctx, node, transport, messageloop.ProtobufMarshaler{})
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
	client, _, err := messageloop.NewClientSession(ctx, node, transport, messageloop.ProtobufMarshaler{})
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
	client, _, err := messageloop.NewClientSession(ctx, node, transport, messageloop.ProtobufMarshaler{})
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
