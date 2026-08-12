package grpcstream

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/config"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	serverpb "github.com/messageloopio/messageloop/shared/genproto/server/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// captureTransport 捕获客户端写出的所有消息（会话组合用例用）。
type captureTransport struct {
	mu       sync.Mutex
	messages [][]byte
}

func (t *captureTransport) Write(data []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.messages = append(t.messages, append([]byte(nil), data...))
	return nil
}

func (t *captureTransport) WriteMany(data ...[]byte) error {
	for _, d := range data {
		if err := t.Write(d); err != nil {
			return err
		}
	}
	return nil
}

func (t *captureTransport) Close(messageloop.Disconnect) error { return nil }

func (t *captureTransport) RemoteAddr() string { return "127.0.0.1:12345" }

// failPublishBroker is a Broker whose Publish fails. When failChannel is set,
// only publications to that channel fail; otherwise every Publish fails.
type failPublishBroker struct {
	failChannel string
}

func (b *failPublishBroker) Start(ctx context.Context, handler messageloop.PublicationHandler) error {
	<-ctx.Done()
	return nil
}

func (b *failPublishBroker) Subscribe(ch string) error   { return nil }
func (b *failPublishBroker) Unsubscribe(ch string) error { return nil }

func (b *failPublishBroker) Publish(ch string, pub *messageloop.Publication) (uint64, error) {
	if b.failChannel == "" || ch == b.failChannel {
		return 0, errors.New("broker unavailable")
	}
	return 1, nil
}

func (b *failPublishBroker) PublishTransient(ch string, pub *messageloop.Publication) error {
	return nil
}

func (b *failPublishBroker) History(ch string, sinceOffset uint64, limit int) ([]*messageloop.Publication, error) {
	return nil, nil
}

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
	defer func() { _ = closeFn() }()

	// Authenticate the client (required for it to be in the hub)
	_ = node.AddClient(client)

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
	_ = node.Run(ctx) // Start broker
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
			},
		},
	}

	resp, err := handler.Publish(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
}

func TestAPIServiceHandler_PublishAddHistory(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	_ = node.Run(ctx)
	handler := NewAPIServiceHandler(node)

	req := &serverpb.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverpb.Publication{
			{
				Id: "admin-history-msg",
				Destination: &serverpb.Publication_Destination{
					Channels: []string{"history-channel"},
				},
				Payload: &sharedpb.Payload{Data: &sharedpb.Payload_Text{Text: "hello history"}},
				Options: &serverpb.Publication_Options{AddHistory: true},
			},
		},
	}

	resp, err := handler.Publish(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	history, err := handler.GetHistory(ctx, &serverpb.GetHistoryRequest{Channel: "history-channel"})
	require.NoError(t, err)
	require.Len(t, history.Publications, 1)
	require.Equal(t, "hello history", history.Publications[0].Payload.GetText())
}

func TestAPIServiceHandler_PublishWithoutAddHistoryNotInHistory(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	_ = node.Run(ctx)
	handler := NewAPIServiceHandler(node)

	req := &serverpb.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverpb.Publication{
			{
				Id: "admin-transient-msg",
				Destination: &serverpb.Publication_Destination{
					Channels: []string{"no-history-channel"},
				},
				Payload: &sharedpb.Payload{Data: &sharedpb.Payload_Text{Text: "hello transient"}},
			},
		},
	}

	resp, err := handler.Publish(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	history, err := handler.GetHistory(ctx, &serverpb.GetHistoryRequest{Channel: "no-history-channel"})
	require.NoError(t, err)
	require.Len(t, history.Publications, 0)
}

// TestAPIServiceHandler_PublishExplicitFalseAddHistoryNotInHistory：
// add_history=false 显式值同样不落历史（与缺省 false 语义一致，防止默认值漂移）。
func TestAPIServiceHandler_PublishExplicitFalseAddHistoryNotInHistory(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	_ = node.Run(ctx)
	handler := NewAPIServiceHandler(node)

	req := &serverpb.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverpb.Publication{
			{
				Id: "admin-explicit-transient-msg",
				Destination: &serverpb.Publication_Destination{
					Channels: []string{"explicit-no-history-channel"},
				},
				Payload: &sharedpb.Payload{Data: &sharedpb.Payload_Text{Text: "hello explicit transient"}},
				Options: &serverpb.Publication_Options{AddHistory: false},
			},
		},
	}

	resp, err := handler.Publish(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	history, err := handler.GetHistory(ctx, &serverpb.GetHistoryRequest{Channel: "explicit-no-history-channel"})
	require.NoError(t, err)
	require.Len(t, history.Publications, 0)
}

// TestAPIServiceHandler_PublishSessionWithAddHistoryStaysSession：
// session 目标带 add_history=true 仍走 PublishToSession（不落频道历史），
// 且在线会话能实际收到消息（组合路径回归）。
func TestAPIServiceHandler_PublishSessionWithAddHistoryStaysSession(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	_ = node.Run(ctx)
	handler := NewAPIServiceHandler(node)

	// 注册一个真实客户端会话
	transport := &captureTransport{}
	client, closeFn, err := messageloop.NewClient(ctx, node, transport, messageloop.JSONMarshaler{})
	require.NoError(t, err)
	defer closeFn()
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id: "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{},
		},
	}))
	sessionID := client.SessionID()
	require.NotEmpty(t, sessionID)

	req := &serverpb.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverpb.Publication{
			{
				Id: "session-history-msg",
				Destination: &serverpb.Publication_Destination{
					Sessions: []string{sessionID},
				},
				Payload: &sharedpb.Payload{Data: &sharedpb.Payload_Text{Text: "session hello"}},
				Options: &serverpb.Publication_Options{AddHistory: true},
			},
		},
	}

	resp, err := handler.Publish(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	// 会话应实际收到该消息（载荷文本出现在写出的 JSON 中即投递成功）
	require.Eventually(t, func() bool {
		for _, raw := range transport.messages {
			if bytes.Contains(raw, []byte("session hello")) {
				return true
			}
		}
		return false
	}, 2*time.Second, 20*time.Millisecond, "session must receive the publication")

	// add_history 对 session 目标无效：不写频道历史
	history, err := handler.GetHistory(ctx, &serverpb.GetHistoryRequest{Channel: ""})
	require.NoError(t, err)
	require.Len(t, history.Publications, 0)
}

func TestAPIServiceHandler_PublishBrokerFailureReturnsError(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	node.SetBroker(&failPublishBroker{})
	handler := NewAPIServiceHandler(node)

	req := &serverpb.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverpb.Publication{
			{
				Id: uuid.NewString(),
				Destination: &serverpb.Publication_Destination{
					Channels: []string{"broken-channel"},
				},
				Payload: &sharedpb.Payload{
					Data: &sharedpb.Payload_Text{Text: "hello"},
				},
				Options: &serverpb.Publication_Options{AddHistory: true},
			},
		},
	}

	resp, err := handler.Publish(ctx, req)
	require.Error(t, err)
	require.Equal(t, codes.Internal, status.Code(err))
	require.Nil(t, resp)
}

func TestAPIServiceHandler_PublishPartialFailureSucceeds(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	node.SetBroker(&failPublishBroker{failChannel: "broken-channel"})
	handler := NewAPIServiceHandler(node)

	req := &serverpb.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverpb.Publication{
			{
				Id: uuid.NewString(),
				Destination: &serverpb.Publication_Destination{
					Channels: []string{"ok-channel", "broken-channel"},
				},
				Payload: &sharedpb.Payload{
					Data: &sharedpb.Payload_Text{Text: "hello"},
				},
				Options: &serverpb.Publication_Options{AddHistory: true},
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
	_ = node.AddClient(client)

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
	defer func() { _ = closeFn() }()

	// Add the client to the hub
	_ = node.AddClient(client)

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
	defer func() { _ = closeFn() }()

	// Add the client to the hub
	_ = node.AddClient(client)

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

// Task 12: admin Publish propagates id/metadata/content_type and GetHistory
// returns them intact.
func TestAPIServiceHandler_GetHistory_ReturnsContentTypeAndId(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	_ = node.Run(ctx) // Start broker
	handler := NewAPIServiceHandler(node)

	req := &serverpb.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverpb.Publication{
			{
				Id: "admin-m-1",
				Destination: &serverpb.Publication_Destination{
					Channels: []string{"history-meta"},
				},
				Payload: &sharedpb.Payload{
					ContentType: "application/json",
					Data:        &sharedpb.Payload_Text{Text: `{"k":"v"}`},
				},
				Metadata: &sharedpb.Metadata{Entries: map[string]string{"origin": "admin"}},
				Options:  &serverpb.Publication_Options{AddHistory: true},
			},
		},
	}
	_, err := handler.Publish(ctx, req)
	require.NoError(t, err)

	resp, err := handler.GetHistory(ctx, &serverpb.GetHistoryRequest{Channel: "history-meta"})
	require.NoError(t, err)
	require.Len(t, resp.Publications, 1)
	p := resp.Publications[0]
	require.Equal(t, "admin-m-1", p.Id)
	require.Equal(t, map[string]string{"origin": "admin"}, p.Metadata)
	require.NotNil(t, p.Payload)
	require.Equal(t, "application/json", p.Payload.ContentType)
	require.Equal(t, `{"k":"v"}`, p.Payload.GetText())
	require.NotZero(t, p.Time)
}
// Task 13a: admin subscribe/publish must respect the built-in ACL rules.
func TestAPIServiceHandler_Subscribe_ACLDenied(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(&config.Server{
		ACL: config.ACLConfig{Rules: []config.ACLRule{
			{ChannelPattern: "private.*", AllowSubscribe: []string{"alice"}},
		}},
	})
	handler := NewAPIServiceHandler(node)

	resp, err := handler.Subscribe(ctx, &serverpb.SubscribeRequest{
		SessionId: "sess-1",
		Channels:  []string{"private.room"},
	})
	require.NoError(t, err)
	require.False(t, resp.Results["private.room"], "admin subscribe to an ACL-denied channel must be rejected")

	// Without ACL rules the admin operation proceeds (session not found).
	openNode := messageloop.NewNode(nil)
	openHandler := NewAPIServiceHandler(openNode)
	openResp, err := openHandler.Subscribe(ctx, &serverpb.SubscribeRequest{
		SessionId: "sess-1",
		Channels:  []string{"open.room"},
	})
	require.NoError(t, err)
	require.NotNil(t, openResp)
}

func TestAPIServiceHandler_Publish_ACLDenied(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(&config.Server{
		ACL: config.ACLConfig{Rules: []config.ACLRule{
			{ChannelPattern: "private.*", AllowPublish: []string{"bob"}},
		}},
	})
	handler := NewAPIServiceHandler(node)

	_, err := handler.Publish(ctx, &serverpb.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverpb.Publication{
			{
				Id: "admin-pub",
				Destination: &serverpb.Publication_Destination{
					Channels: []string{"private.room"},
				},
				Payload: &sharedpb.Payload{Data: &sharedpb.Payload_Text{Text: "x"}},
			},
		},
	})
	require.Error(t, err, "admin publish to an ACL-denied channel must fail")
}