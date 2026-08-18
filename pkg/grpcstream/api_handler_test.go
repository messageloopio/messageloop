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
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	serverv2 "github.com/messageloopio/messageloop/shared/genproto/server/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
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

func (b *failPublishBroker) PublishOccupancy(ch string, evt messageloop.OccupancyEvent) error {
	return nil
}

func (b *failPublishBroker) SetOccupancyHandler(messageloop.OccupancyHandler) error { return nil }
func (b *failPublishBroker) SetGapHandler(messageloop.GapHandler)                   {}

func (b *failPublishBroker) History(ch string, sinceOffset uint64, limit int) (*messageloop.HistoryPage, error) {
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

// probeBroker records every Publish / History call so tests can assert
// whether the data plane was exercised.
type probeBroker struct {
	publishCalls   int
	publishChannel string
	historyCalls   int
	historyChannel string
}

func (b *probeBroker) Start(ctx context.Context, handler messageloop.PublicationHandler) error {
	<-ctx.Done()
	return nil
}

func (b *probeBroker) Subscribe(ch string) error   { return nil }
func (b *probeBroker) Unsubscribe(ch string) error { return nil }

func (b *probeBroker) Publish(ch string, pub *messageloop.Publication) (uint64, error) {
	b.publishCalls++
	b.publishChannel = ch
	return 1, nil
}

func (b *probeBroker) PublishTransient(ch string, pub *messageloop.Publication) error { return nil }

func (b *probeBroker) PublishOccupancy(ch string, evt messageloop.OccupancyEvent) error {
	return nil
}

func (b *probeBroker) SetOccupancyHandler(messageloop.OccupancyHandler) error { return nil }
func (b *probeBroker) SetGapHandler(messageloop.GapHandler)                   {}

func (b *probeBroker) History(ch string, sinceOffset uint64, limit int) (*messageloop.HistoryPage, error) {
	b.historyCalls++
	b.historyChannel = ch
	return &messageloop.HistoryPage{}, nil
}

func policyBoolPtr(v bool) *bool { return &v }

// newUserTestClient registers a client in the node hub under the given user
// ID (bypassing proxy auth via ForceTestIDs) and returns it.
func newUserTestClient(t *testing.T, node *messageloop.Node, transport messageloop.Transport, sessionID, userID string) *messageloop.Client {
	t.Helper()
	client, _, err := messageloop.NewClient(context.Background(), node, transport, messageloop.JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs(sessionID, userID, "client-"+sessionID)
	require.NoError(t, node.AddClient(client))
	return client
}

// transportContainsText reports whether the capture transport ever wrote a
// frame containing the given text.
func transportContainsText(transport *captureTransport, text string) bool {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	for _, raw := range transport.messages {
		if bytes.Contains(raw, []byte(text)) {
			return true
		}
	}
	return false
}

// TestAPIServiceHandler_AddHistoryDeniedByPolicy verifies that an admin
// publish with add_history=true to a policy-disabled-history channel is
// counted as failed and never reaches a history-writing broker, while other
// channels in the same request still succeed (partial-success semantics).
func TestAPIServiceHandler_AddHistoryDeniedByPolicy(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(&config.Server{
		Authorizer: config.AuthorizerConfig{
			Rules: []config.AuthorizerRule{
				{Pattern: "game.tick.**", ChannelPolicySpec: config.ChannelPolicySpec{TransientOnly: policyBoolPtr(true)}},
				{Pattern: "no-history.**", ChannelPolicySpec: config.ChannelPolicySpec{History: policyBoolPtr(false)}},
			},
		},
	})
	probe := &probeBroker{}
	node.SetBroker(probe)
	handler := NewAPIServiceHandler(node)

	req := &serverv2.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverv2.Publication{
			{
				Id: "admin-tick",
				Destination: &serverv2.Publication_Destination{
					Channels: []string{"ok-channel", "game.tick.fps", "no-history.ch"},
				},
				Payload: &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: "x"}},
				Options: &serverv2.Publication_Options{AddHistory: true},
			},
		},
	}

	resp, err := handler.Publish(ctx, req)
	require.NoError(t, err, "the denied channels must not fail the whole RPC (partial success)")
	require.NotNil(t, resp)
	require.Equal(t, 1, probe.publishCalls, "broker.Publish must be called only for the allowed channel")
	require.Equal(t, "ok-channel", probe.publishChannel)

	// A single add_history publication on a disabled channel: all attempts
	// fail, so the RPC itself reports an error (existing all-failed
	// semantics), and the broker is still never called.
	probe2 := &probeBroker{}
	node2 := messageloop.NewNode(&config.Server{
		Authorizer: config.AuthorizerConfig{
			Rules: []config.AuthorizerRule{
				{Pattern: "game.tick.**", ChannelPolicySpec: config.ChannelPolicySpec{TransientOnly: policyBoolPtr(true)}},
			},
		},
	})
	node2.SetBroker(probe2)
	handler2 := NewAPIServiceHandler(node2)
	_, err = handler2.Publish(ctx, &serverv2.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverv2.Publication{
			{
				Id: "admin-tick-only",
				Destination: &serverv2.Publication_Destination{
					Channels: []string{"game.tick.fps"},
				},
				Payload: &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: "x"}},
				Options: &serverv2.Publication_Options{AddHistory: true},
			},
		},
	})
	require.Error(t, err, "when every attempt fails the RPC must report the failure")
	require.Equal(t, codes.Internal, status.Code(err))
	require.Equal(t, 0, probe2.publishCalls, "the history-writing broker must not be called")
}

// TestAPIServiceHandler_PublishToChannelsWithoutAddHistoryOnDisabledChannel
// verifies that a transient admin publish (no add_history) on a
// policy-disabled-history channel is still delivered.
func TestAPIServiceHandler_PublishToChannelsWithoutAddHistoryOnDisabledChannel(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(&config.Server{
		Authorizer: config.AuthorizerConfig{
			Rules: []config.AuthorizerRule{
				{Pattern: "game.tick.**", ChannelPolicySpec: config.ChannelPolicySpec{TransientOnly: policyBoolPtr(true)}},
			},
		},
	})
	probe := &probeBroker{}
	node.SetBroker(probe)
	handler := NewAPIServiceHandler(node)

	resp, err := handler.Publish(ctx, &serverv2.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverv2.Publication{
			{
				Id: "admin-tick-transient",
				Destination: &serverv2.Publication_Destination{
					Channels: []string{"game.tick.fps"},
				},
				Payload: &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: "x"}},
			},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, 0, probe.publishCalls, "transient admin publish must not call the history-writing broker")
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
	req := &serverv2.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverv2.Publication{
			{
				Id: uuid.NewString(),
				Destination: &serverv2.Publication_Destination{
					Sessions: []string{client.SessionID()},
				},
				Payload: &sharedv2.Payload{
					Data: &sharedv2.Payload_Json{
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
	req := &serverv2.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverv2.Publication{
			{
				Id: uuid.NewString(),
				Destination: &serverv2.Publication_Destination{
					Sessions: []string{"non-existent-session-id"},
				},
				Payload: &sharedv2.Payload{
					Data: &sharedv2.Payload_Json{
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
	req := &serverv2.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverv2.Publication{
			{
				Id: uuid.NewString(),
				Destination: &serverv2.Publication_Destination{
					Channels: []string{"test-channel-1", "test-channel-2"},
				},
				Payload: &sharedv2.Payload{
					Data: &sharedv2.Payload_Binary{
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

	req := &serverv2.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverv2.Publication{
			{
				Id: "admin-history-msg",
				Destination: &serverv2.Publication_Destination{
					Channels: []string{"history-channel"},
				},
				Payload: &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: "hello history"}},
				Options: &serverv2.Publication_Options{AddHistory: true},
			},
		},
	}

	resp, err := handler.Publish(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	history, err := handler.GetHistory(ctx, &serverv2.GetHistoryRequest{Channel: "history-channel"})
	require.NoError(t, err)
	require.Len(t, history.Publications, 1)
	require.Equal(t, "hello history", history.Publications[0].Payload.GetText())
}

func TestAPIServiceHandler_PublishWithoutAddHistoryNotInHistory(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	_ = node.Run(ctx)
	handler := NewAPIServiceHandler(node)

	req := &serverv2.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverv2.Publication{
			{
				Id: "admin-transient-msg",
				Destination: &serverv2.Publication_Destination{
					Channels: []string{"no-history-channel"},
				},
				Payload: &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: "hello transient"}},
			},
		},
	}

	resp, err := handler.Publish(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	history, err := handler.GetHistory(ctx, &serverv2.GetHistoryRequest{Channel: "no-history-channel"})
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

	req := &serverv2.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverv2.Publication{
			{
				Id: "admin-explicit-transient-msg",
				Destination: &serverv2.Publication_Destination{
					Channels: []string{"explicit-no-history-channel"},
				},
				Payload: &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: "hello explicit transient"}},
				Options: &serverv2.Publication_Options{AddHistory: false},
			},
		},
	}

	resp, err := handler.Publish(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	history, err := handler.GetHistory(ctx, &serverv2.GetHistoryRequest{Channel: "explicit-no-history-channel"})
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
			Connect: &clientpb.Connect{Version: "2.0.0"},
		},
	}))
	sessionID := client.SessionID()
	require.NotEmpty(t, sessionID)

	req := &serverv2.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverv2.Publication{
			{
				Id: "session-history-msg",
				Destination: &serverv2.Publication_Destination{
					Sessions: []string{sessionID},
				},
				Payload: &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: "session hello"}},
				Options: &serverv2.Publication_Options{AddHistory: true},
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
	history, err := handler.GetHistory(ctx, &serverv2.GetHistoryRequest{Channel: ""})
	require.NoError(t, err)
	require.Len(t, history.Publications, 0)
}

func TestAPIServiceHandler_PublishBrokerFailureReturnsError(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	node.SetBroker(&failPublishBroker{})
	handler := NewAPIServiceHandler(node)

	req := &serverv2.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverv2.Publication{
			{
				Id: uuid.NewString(),
				Destination: &serverv2.Publication_Destination{
					Channels: []string{"broken-channel"},
				},
				Payload: &sharedv2.Payload{
					Data: &sharedv2.Payload_Text{Text: "hello"},
				},
				Options: &serverv2.Publication_Options{AddHistory: true},
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

	req := &serverv2.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverv2.Publication{
			{
				Id: uuid.NewString(),
				Destination: &serverv2.Publication_Destination{
					Channels: []string{"ok-channel", "broken-channel"},
				},
				Payload: &sharedv2.Payload{
					Data: &sharedv2.Payload_Text{Text: "hello"},
				},
				Options: &serverv2.Publication_Options{AddHistory: true},
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
	req := &serverv2.DisconnectRequest{
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
	req := &serverv2.DisconnectRequest{
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
	req := &serverv2.SubscribeRequest{
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
	req := &serverv2.SubscribeRequest{
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
	subReq := &serverv2.SubscribeRequest{
		SessionId: client.SessionID(),
		Channels:  []string{"test-channel-1"},
	}
	_, err = handler.Subscribe(ctx, subReq)
	require.NoError(t, err)

	// Then unsubscribe
	req := &serverv2.UnsubscribeRequest{
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
	req := &serverv2.UnsubscribeRequest{
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

	req := &serverv2.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverv2.Publication{
			{
				Id: "admin-m-1",
				Destination: &serverv2.Publication_Destination{
					Channels: []string{"history-meta"},
				},
				Payload: &sharedv2.Payload{
					ContentType: "application/json",
					Data:        &sharedv2.Payload_Text{Text: `{"k":"v"}`},
				},
				Metadata: &sharedv2.Metadata{Entries: map[string]string{"origin": "admin"}},
				Options:  &serverv2.Publication_Options{AddHistory: true},
			},
		},
	}
	_, err := handler.Publish(ctx, req)
	require.NoError(t, err)

	resp, err := handler.GetHistory(ctx, &serverv2.GetHistoryRequest{Channel: "history-meta"})
	require.NoError(t, err)
	require.Len(t, resp.Publications, 1)
	p := resp.Publications[0]
	require.Equal(t, "admin-m-1", p.Id)
	require.Equal(t, map[string]string{"origin": "admin"}, p.Metadata.GetEntries())
	require.NotNil(t, p.Payload)
	require.Equal(t, "application/json", p.Payload.ContentType)
	require.Equal(t, `{"k":"v"}`, p.Payload.GetText())
	require.NotZero(t, p.Time)
	require.NotNil(t, p.Position)
	require.Equal(t, uint64(1), p.Position.GetOffset())
	require.NotEmpty(t, p.Position.GetStreamEpoch())
}

// TestAdmin_GetHistorySincePosition covers the server.v2 GetHistoryRequest.since
// Position semantics (D6): nil reads from the head, an offset-only position
// resumes from that offset, a matching stream_epoch reads, and a stale epoch
// fails with FailedPrecondition before the broker is read.
func TestAdmin_GetHistorySincePosition(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	require.NoError(t, node.Run(ctx))
	handler := NewAPIServiceHandler(node)

	for _, text := range []string{"m1", "m2", "m3"} {
		_, err := node.Publish("pos.ch", &messageloop.Publication{Payload: []byte(text), Kind: messageloop.PayloadKindText})
		require.NoError(t, err)
	}
	epoch := node.Broker().(interface{ Epoch() string }).Epoch()
	require.NotEmpty(t, epoch)

	// since == nil: from the head within the limit.
	resp, err := handler.GetHistory(ctx, &serverv2.GetHistoryRequest{Channel: "pos.ch", Limit: 10})
	require.NoError(t, err)
	require.Len(t, resp.Publications, 3)

	// offset-only position: resumes from that offset.
	off := uint64(2)
	resp, err = handler.GetHistory(ctx, &serverv2.GetHistoryRequest{
		Channel: "pos.ch",
		Since:   &sharedv2.Position{Offset: &off},
		Limit:   10,
	})
	require.NoError(t, err)
	require.Len(t, resp.Publications, 2)
	require.Equal(t, "m2", resp.Publications[0].Payload.GetText())
	require.Equal(t, uint64(2), resp.Publications[0].Position.GetOffset())

	// matching epoch: reads normally.
	resp, err = handler.GetHistory(ctx, &serverv2.GetHistoryRequest{
		Channel: "pos.ch",
		Since:   &sharedv2.Position{StreamEpoch: epoch, Offset: &off},
		Limit:   10,
	})
	require.NoError(t, err)
	require.Len(t, resp.Publications, 2)

	// stale epoch: FailedPrecondition, broker History never reached (probe).
	probe := &probeBroker{}
	probeNode := messageloop.NewNode(nil)
	probeNode.SetBroker(probe)
	probeHandler := NewAPIServiceHandler(probeNode)
	_, err = probeHandler.GetHistory(ctx, &serverv2.GetHistoryRequest{
		Channel: "pos.ch",
		Since:   &sharedv2.Position{StreamEpoch: "stale-epoch"},
		Limit:   10,
	})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Zero(t, probe.historyCalls, "epoch mismatch must fail before the broker is read")

	// the memory broker itself rejects a stale epoch too.
	_, err = handler.GetHistory(ctx, &serverv2.GetHistoryRequest{
		Channel: "pos.ch",
		Since:   &sharedv2.Position{StreamEpoch: "stale-epoch"},
		Limit:   10,
	})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}

// Task 13a: admin subscribe/publish must respect the authorizer rules.
func TestAPIServiceHandler_Subscribe_ACLDenied(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(&config.Server{
		Authorizer: config.AuthorizerConfig{
			Rules: []config.AuthorizerRule{
				{Pattern: "private.*", AllowSubscribe: []string{"alice"}},
			},
		},
	})
	handler := NewAPIServiceHandler(node)

	resp, err := handler.Subscribe(ctx, &serverv2.SubscribeRequest{
		SessionId: "sess-1",
		Channels:  []string{"private.room"},
	})
	require.NoError(t, err)
	require.False(t, resp.Results["private.room"], "admin subscribe to an ACL-denied channel must be rejected")

	// Without ACL rules the admin operation proceeds (session not found).
	openNode := messageloop.NewNode(nil)
	openHandler := NewAPIServiceHandler(openNode)
	openResp, err := openHandler.Subscribe(ctx, &serverv2.SubscribeRequest{
		SessionId: "sess-1",
		Channels:  []string{"open.room"},
	})
	require.NoError(t, err)
	require.NotNil(t, openResp)
}

func TestAPIServiceHandler_Publish_ACLDenied(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(&config.Server{
		Authorizer: config.AuthorizerConfig{
			Rules: []config.AuthorizerRule{
				{Pattern: "private.*", AllowPublish: []string{"bob"}},
			},
		},
	})
	handler := NewAPIServiceHandler(node)

	_, err := handler.Publish(ctx, &serverv2.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverv2.Publication{
			{
				Id: "admin-pub",
				Destination: &serverv2.Publication_Destination{
					Channels: []string{"private.room"},
				},
				Payload: &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: "x"}},
			},
		},
	})
	require.Error(t, err, "admin publish to an ACL-denied channel must fail")
}

// TestAdmin_GetPresenceFillsNewFields verifies the server.v2 PresenceInfo
// semantics (D6): session_id is the formal session ID (falling back to the
// legacy client_id key) and client_id is the Connect.client_id.
func TestAdmin_GetPresenceFillsNewFields(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	require.NoError(t, node.Run(ctx))

	transport := &mockTransport{}
	client, _, err := messageloop.NewClient(ctx, node, transport, messageloop.JSONMarshaler{})
	require.NoError(t, err)

	connect := &clientpb.InboundMessage{
		Id: "admin-connect",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{Version: "2.0.0", ClientId: "device-42"},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, connect))

	sub := &clientpb.InboundMessage{
		Id: "admin-sub",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{
				Subscriptions: []*clientpb.Subscription{{Channel: "admin.presence.ch"}},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, sub))

	handler := NewAPIServiceHandler(node)
	resp, err := handler.GetPresence(ctx, &serverv2.GetPresenceRequest{Channel: "admin.presence.ch"})
	require.NoError(t, err)

	info, ok := resp.GetClients()[client.SessionID()]
	require.True(t, ok, "the subscribed session must be present")
	require.Equal(t, client.SessionID(), info.GetSessionId(), "session_id is the formal session ID")
	require.Equal(t, "device-42", info.GetClientId(), "client_id is the Connect.client_id (device endpoint)")
	require.NotZero(t, info.GetConnectedAt())
}

// TestAdmin_GetPresence_LegacyKeyFallsBackToClientID verifies that a store
// record written without the new fields (legacy Redis JSON) still reports a
// session_id derived from client_id and an empty client_id (no
// Connect.client_id was recorded).
func TestAdmin_GetPresence_LegacyKeyFallsBackToClientID(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	require.NoError(t, node.Run(ctx))

	store := messageloop.NewMemoryPresenceStore()
	node.SetPresenceStore(store)
	// Simulate a legacy record: only client_id/user_id/connected_at set.
	require.NoError(t, store.Add(ctx, "legacy.ch", &messageloop.PresenceInfo{
		ClientID:    "legacy-session",
		UserID:      "legacy-user",
		ConnectedAt: 1,
	}))

	handler := NewAPIServiceHandler(node)
	resp, err := handler.GetPresence(ctx, &serverv2.GetPresenceRequest{Channel: "legacy.ch"})
	require.NoError(t, err)
	info, ok := resp.GetClients()["legacy-session"]
	require.True(t, ok)
	require.Equal(t, "legacy-session", info.GetSessionId(), "session_id falls back to the legacy client_id key")
	require.Empty(t, info.GetClientId(), "no Connect.client_id was recorded for the legacy record")
}

// --- PR-06: Admin publish/disconnect/subscribe by user_id ---

// TestAdmin_PublishDestinationUsers verifies that a users-only destination
// (no sessions, no channels) fans the publication out to every local session
// of the user and to no one else.
func TestAdmin_PublishDestinationUsers(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	handler := NewAPIServiceHandler(node)

	transportA := &captureTransport{}
	transportB := &captureTransport{}
	otherTransport := &captureTransport{}
	clientA := newUserTestClient(t, node, transportA, "sess-user-a-1", "U")
	clientB := newUserTestClient(t, node, transportB, "sess-user-a-2", "U")
	newUserTestClient(t, node, otherTransport, "sess-other-user", "other-user")

	resp, err := handler.Publish(ctx, &serverv2.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverv2.Publication{
			{
				Id:          "user-fanout-pub",
				Destination: &serverv2.Publication_Destination{Users: []string{"U"}},
				Payload:     &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: "hello user fanout"}},
			},
		},
	})
	require.NoError(t, err, "a users-only destination is a valid destination")
	require.NotNil(t, resp)

	require.True(t, transportContainsText(transportA, "hello user fanout"),
		"session %s must receive the user publication", clientA.SessionID())
	require.True(t, transportContainsText(transportB, "hello user fanout"),
		"session %s must receive the user publication", clientB.SessionID())
	require.False(t, transportContainsText(otherTransport, "hello user fanout"),
		"other users must not receive the publication")
}

// TestAdmin_PublishUsersNoCluster verifies the single-node path explicitly:
// with cluster disabled the expansion uses only the local Hub.SessionsByUser,
// no session directory or Redis is involved.
func TestAdmin_PublishUsersNoCluster(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil) // cluster.enabled=false
	handler := NewAPIServiceHandler(node)

	transport := &captureTransport{}
	client := newUserTestClient(t, node, transport, "sess-nocluster", "U")

	resp, err := handler.Publish(ctx, &serverv2.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverv2.Publication{
			{
				Id:          "nocluster-pub",
				Destination: &serverv2.Publication_Destination{Users: []string{"U"}},
				Payload:     &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: "local only"}},
			},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.True(t, transportContainsText(transport, "local only"),
		"local SessionsByUser must be enough without a cluster (session %s)", client.SessionID())
}

// TestAdmin_DisconnectUsers verifies that Disconnect by user fans out to
// every session of the user and reports per-session results.
func TestAdmin_DisconnectUsers(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	handler := NewAPIServiceHandler(node)

	transportA := &mockTransport{}
	transportB := &mockTransport{}
	clientA := newUserTestClient(t, node, transportA, "sess-disc-a", "U")
	clientB := newUserTestClient(t, node, transportB, "sess-disc-b", "U")
	newUserTestClient(t, node, &mockTransport{}, "sess-disc-other", "other-user")

	resp, err := handler.Disconnect(ctx, &serverv2.DisconnectRequest{
		Users:  []string{"U"},
		Code:   3500,
		Reason: "admin user disconnect",
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Len(t, resp.Results, 2, "results must be keyed per session")
	require.True(t, resp.Results[clientA.SessionID()], "session A must be disconnected")
	require.True(t, resp.Results[clientB.SessionID()], "session B must be disconnected")
	require.True(t, transportA.closed)
	require.True(t, transportB.closed)
}

// TestAdmin_EmptyUserInvalidArgument verifies that empty user IDs in any
// user-targeted field are rejected with InvalidArgument before any scanning,
// and that a registered session survives the rejected requests untouched.
func TestAdmin_EmptyUserInvalidArgument(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	handler := NewAPIServiceHandler(node)

	transport := &mockTransport{}
	client := newUserTestClient(t, node, transport, "sess-no-scan", "U")

	_, err := handler.Publish(ctx, &serverv2.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverv2.Publication{
			{
				Id:          "bad-user-pub",
				Destination: &serverv2.Publication_Destination{Users: []string{""}},
				Payload:     &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: "x"}},
			},
		},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "empty users entry must be InvalidArgument")

	_, err = handler.Disconnect(ctx, &serverv2.DisconnectRequest{Users: []string{""}})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "empty users entry must be InvalidArgument")

	_, err = handler.Subscribe(ctx, &serverv2.SubscribeRequest{Channels: []string{"ch"}})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "empty session_id and user_id must be InvalidArgument")

	_, err = handler.Unsubscribe(ctx, &serverv2.UnsubscribeRequest{Channels: []string{"ch"}})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "empty session_id and user_id must be InvalidArgument")

	// No scanning side effects: the registered session is still connected.
	require.Same(t, client, node.Hub().LookupSession("sess-no-scan"))
	require.False(t, transport.closed)
}

// TestAdmin_SubscribeByUser verifies Subscribe/Unsubscribe by user_id: the
// user's local sessions enter the hub subscription, other users are
// untouched, and the results are keyed by channel.
func TestAdmin_SubscribeByUser(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	handler := NewAPIServiceHandler(node)

	newUserTestClient(t, node, &mockTransport{}, "sess-sub-user", "U")
	newUserTestClient(t, node, &mockTransport{}, "sess-sub-other", "other-user")

	resp, err := handler.Subscribe(ctx, &serverv2.SubscribeRequest{
		UserId:   "U",
		Channels: []string{"user.sub.channel"},
	})
	require.NoError(t, err)
	require.True(t, resp.Results["user.sub.channel"], "the channel must be subscribed for the user")
	require.Equal(t, 1, node.Hub().NumSubscribers("user.sub.channel"),
		"only the user's session may be subscribed, not other users")

	// Unsubscribe by user works symmetrically.
	unsubResp, err := handler.Unsubscribe(ctx, &serverv2.UnsubscribeRequest{
		UserId:   "U",
		Channels: []string{"user.sub.channel"},
	})
	require.NoError(t, err)
	require.True(t, unsubResp.Results["user.sub.channel"])
	require.Zero(t, node.Hub().NumSubscribers("user.sub.channel"))
}

// --- PR-KA-A4 §9.9: capability gates ---

// TestAdmin_GetHistoryRequiresCapability verifies that without history.read
// GetHistory fails with PERMISSION_DENIED and the broker History is never
// called (spy broker).
func TestAdmin_GetHistoryRequiresCapability(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(&config.Server{
		GRPCAdmin: config.GRPCAdmin{Capabilities: []string{"channels.list"}},
	})
	probe := &probeBroker{}
	node.SetBroker(probe)
	handler := NewAPIServiceHandler(node)

	_, err := handler.GetHistory(ctx, &serverv2.GetHistoryRequest{Channel: "cap.history.ch"})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "missing history.read must fail softly")
	require.Zero(t, probe.historyCalls, "the broker must never be touched without history.read")
}

// TestAdmin_GetHistoryDefaultCapabilities verifies that omitted capabilities
// keep GetHistory usable (the default bits include history.read).
func TestAdmin_GetHistoryDefaultCapabilities(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(nil)
	probe := &probeBroker{}
	node.SetBroker(probe)
	handler := NewAPIServiceHandler(node)

	resp, err := handler.GetHistory(ctx, &serverv2.GetHistoryRequest{Channel: "cap.history.ch"})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, 1, probe.historyCalls, "the default bits must include history.read")
}

// TestAdmin_GetHistoryExplicitEmptyCapabilities verifies that an explicit
// empty capability list locks the admin data plane: GetHistory is denied and
// the broker is never touched.
func TestAdmin_GetHistoryExplicitEmptyCapabilities(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(&config.Server{
		GRPCAdmin: config.GRPCAdmin{Capabilities: []string{}},
	})
	probe := &probeBroker{}
	node.SetBroker(probe)
	handler := NewAPIServiceHandler(node)

	_, err := handler.GetHistory(ctx, &serverv2.GetHistoryRequest{Channel: "cap.history.ch"})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.Zero(t, probe.historyCalls)

	// GetPresence / GetChannels are locked too.
	_, err = handler.GetPresence(ctx, &serverv2.GetPresenceRequest{Channel: "cap.presence.ch"})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "presence.read missing")
	_, err = handler.GetChannels(ctx, &serverv2.GetChannelsRequest{})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "channels.list missing")
}

// TestAdmin_GetHistoryDecideDenied verifies GetHistory also requires
// Decide(admin, ActionRecover, ch): a deny_all rule on the channel fails the
// read before the broker is touched, even with history.read held.
func TestAdmin_GetHistoryDecideDenied(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(&config.Server{
		Authorizer: config.AuthorizerConfig{
			Rules: []config.AuthorizerRule{{Pattern: "secret.**", DenyAll: true}},
		},
	})
	probe := &probeBroker{}
	node.SetBroker(probe)
	handler := NewAPIServiceHandler(node)

	_, err := handler.GetHistory(ctx, &serverv2.GetHistoryRequest{Channel: "secret.room"})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.Zero(t, probe.historyCalls, "deny_all must fail GetHistory before the broker is read")

	// A non-denied channel is served.
	resp, err := handler.GetHistory(ctx, &serverv2.GetHistoryRequest{Channel: "open.room"})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, 1, probe.historyCalls)
}

// TestAdmin_GetPresenceDecideDenied verifies GetPresence requires
// Decide(admin, ActionPresence, ch): presence=false channels and deny_all
// channels fail softly.
func TestAdmin_GetPresenceDecideDenied(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(&config.Server{
		Authorizer: config.AuthorizerConfig{
			Rules: []config.AuthorizerRule{
				{Pattern: "secret.**", DenyAll: true},
				{Pattern: "nopres.**", ChannelPolicySpec: config.ChannelPolicySpec{Presence: policyBoolPtr(false)}},
			},
		},
	})
	handler := NewAPIServiceHandler(node)

	_, err := handler.GetPresence(ctx, &serverv2.GetPresenceRequest{Channel: "secret.room"})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "deny_all must fail GetPresence")
	_, err = handler.GetPresence(ctx, &serverv2.GetPresenceRequest{Channel: "nopres.room"})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "presence=false must fail GetPresence")
}

// TestAdmin_PublishSessionRequiresCapability verifies session/user
// destinations are capability-gated (session.act / user.fanout).
func TestAdmin_PublishSessionRequiresCapability(t *testing.T) {
	ctx := context.Background()
	node := messageloop.NewNode(&config.Server{
		GRPCAdmin: config.GRPCAdmin{Capabilities: []string{"user.fanout"}},
	})
	handler := NewAPIServiceHandler(node)

	_, err := handler.Publish(ctx, &serverv2.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverv2.Publication{
			{
				Id: "no-session-act",
				Destination: &serverv2.Publication_Destination{
					Sessions: []string{"sess-x"},
				},
				Payload: &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: "x"}},
			},
		},
	})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "session publish requires session.act")

	// user.fanout without session.act is not enough for user destinations.
	_, err = handler.Publish(ctx, &serverv2.PublishRequest{
		RequestId: uuid.NewString(),
		Publications: []*serverv2.Publication{
			{
				Id: "no-session-act-user",
				Destination: &serverv2.Publication_Destination{
					Users: []string{"U"},
				},
				Payload: &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: "x"}},
			},
		},
	})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "user fanout still delivers per-session")
}
