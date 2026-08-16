package messageloop

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// capturingTransport captures all written messages for inspection
type capturingTransport struct {
	mu           sync.Mutex
	messages     [][]byte
	closeCount   atomic.Int32
	closed       atomic.Bool
	closeReason  Disconnect
	writeDelay   time.Duration
	writeError   error
	closeOnWrite bool
}

func (c *capturingTransport) Write(data []byte) error {
	if c.writeDelay > 0 {
		time.Sleep(c.writeDelay)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return errors.New("transport closed")
	}
	if c.closeOnWrite {
		c.closed.Store(true)
	}
	if c.writeError != nil {
		return c.writeError
	}
	c.messages = append(c.messages, append([]byte(nil), data...))
	return nil
}

func (c *capturingTransport) RemoteAddr() string {
	return "127.0.0.1:12345"
}

func (c *capturingTransport) WriteMany(data ...[]byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return errors.New("transport closed")
	}
	for _, d := range data {
		c.messages = append(c.messages, append([]byte(nil), d...))
	}
	return nil
}

func (c *capturingTransport) Close(disconnect Disconnect) error {
	c.closed.Store(true)
	c.closeCount.Add(1)
	c.mu.Lock()
	c.closeReason = disconnect
	c.mu.Unlock()
	return nil
}

func (c *capturingTransport) getMessageCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.messages)
}

// resetMessages clears the captured messages under the transport lock.
// Concurrent fan-out (e.g. presence events between simultaneous
// subscribers) may write to the transport while a test resets it, so the
// reset must take the same lock as Write.
func (c *capturingTransport) resetMessages() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = nil
}

// snapshotMessages returns a deep copy of the captured messages so callers
// can inspect them without holding the transport lock.
func (c *capturingTransport) snapshotMessages() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, 0, len(c.messages))
	for _, m := range c.messages {
		out = append(out, append([]byte(nil), m...))
	}
	return out
}

func (c *capturingTransport) getLastMessage() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.messages) == 0 {
		return nil
	}
	return c.messages[len(c.messages)-1]
}

func (c *capturingTransport) isClosed() bool {
	return c.closed.Load()
}

func (c *capturingTransport) getCloseCount() int32 {
	return c.closeCount.Load()
}

func (c *capturingTransport) getCloseReason() Disconnect {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeReason
}

// failTransport simulates transport failures
type failTransport struct {
	writeErr    error
	closeErr    error
	closeCalled bool
	mu          sync.Mutex
}

func (f *failTransport) Write(data []byte) error {
	return f.writeErr
}

func (f *failTransport) WriteMany(data ...[]byte) error {
	return f.writeErr
}

func (f *failTransport) Close(disconnect Disconnect) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalled = true
	return f.closeErr
}

func (f *failTransport) RemoteAddr() string {
	return "127.0.0.1:12345"
}

func TestNewClient(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}
	marshaler := JSONMarshaler{}

	client, closeFunc, err := NewClient(ctx, node, transport, marshaler)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if client == nil {
		t.Fatal("NewClient() should return a client")
	}
	if closeFunc == nil {
		t.Fatal("NewClient() should return a close function")
	}
	if client.ctx != ctx {
		t.Error("client context should match provided context")
	}
	if client.node != node {
		t.Error("client node should match provided node")
	}
	if client.transport != transport {
		t.Error("client transport should match provided transport")
	}
	if client.marshaler != marshaler {
		t.Error("client marshaler should match provided marshaler")
	}
	if client.session == "" {
		t.Error("client should have a session ID generated")
	}
}

func TestClientSession_SessionID(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	sessionID := client.SessionID()
	if sessionID == "" {
		t.Error("SessionID() should return non-empty string")
	}

	// SessionID should be consistent
	if client.SessionID() != sessionID {
		t.Error("SessionID() should return the same value on subsequent calls")
	}

	// Different clients should have different session IDs
	client2, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if client2.SessionID() == sessionID {
		t.Error("Different clients should have different session IDs")
	}
}

func TestClientSession_ClientID(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	// Initially, client ID should be empty (set by connect)
	if client.ClientID() != "" {
		t.Error("ClientID() should be empty before connect")
	}
}

func TestClientSession_UserID(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	// Initially, user ID should be empty (set by connect)
	if client.UserID() != "" {
		t.Error("UserID() should be empty before connect")
	}
}

func TestClientSession_Authenticated(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	// Initially, not authenticated
	if client.Authenticated() {
		t.Error("Client should not be authenticated initially")
	}
}

func TestClientSession_ClientInfo(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	info := client.ClientInfo()
	if info == nil {
		t.Fatal("ClientInfo() should not return nil")
	}
	if info.SessionID != client.SessionID() {
		t.Error("ClientInfo.SessionID should match ClientSession.SessionID")
	}
}

func TestClientSession_HandleMessage_Connect(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	msg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{},
		},
	}

	err = client.HandleMessage(ctx, msg)
	if err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	// Should have sent a Connected response
	if transport.getMessageCount() != 1 {
		t.Errorf("Transport should have 1 message, got %d", transport.getMessageCount())
	}

	// Should be authenticated now
	if !client.Authenticated() {
		t.Error("Client should be authenticated after Connect")
	}
}

func TestClientSession_HandleMessage_Connect_Twice(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	msg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{},
		},
	}

	// First connect should succeed
	err = client.HandleMessage(ctx, msg)
	if err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	// Reset transport messages
	transport.messages = nil

	// Second connect should fail with DisconnectBadRequest
	msg.Id = "msg-2"
	err = client.HandleMessage(ctx, msg)
	if err != nil {
		t.Fatalf("HandleMessage() should not return error for disconnect, got %v", err)
	}

	// Transport should be closed
	if !transport.isClosed() {
		t.Error("Transport should be closed after second connect")
	}

	reason := transport.getCloseReason()
	if reason.Code != DisconnectBadRequest.Code {
		t.Errorf("Close code should be %d, got %d", DisconnectBadRequest.Code, reason.Code)
	}
}

func TestClientSession_HandleMessage_Ping(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	// First authenticate (all non-connect messages require authentication).
	connectMsg := &clientpb.InboundMessage{
		Id: "msg-0",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{},
		},
	}
	err = client.HandleMessage(ctx, connectMsg)
	if err != nil {
		t.Fatalf("HandleMessage() Connect error = %v", err)
	}
	transport.messages = nil

	msg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Ping{
			Ping: &clientpb.Ping{},
		},
	}

	err = client.HandleMessage(ctx, msg)
	if err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	// Should have sent a Pong response
	if transport.getMessageCount() != 1 {
		t.Errorf("Transport should have 1 message, got %d", transport.getMessageCount())
	}
}

func TestClientSession_HandleMessage_Publish_BeforeAuth(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	msg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Publish{
			Publish: &clientpb.Publish{
				Channel: "test-channel",
				Payload: &sharedpb.Payload{
					Data: &sharedpb.Payload_Binary{
						Binary: []byte("test payload"),
					},
				},
			},
		},
	}

	err = client.HandleMessage(ctx, msg)
	// Publish before auth should trigger disconnect but not return error
	if err != nil {
		t.Fatalf("HandleMessage() should not return error for disconnect, got %v", err)
	}

	// Transport should be closed
	if !transport.isClosed() {
		t.Error("Transport should be closed after publish before auth")
	}

	reason := transport.getCloseReason()
	if reason.Code != DisconnectInvalidToken.Code {
		t.Errorf("Close code should be %d (invalid token), got %d", DisconnectInvalidToken.Code, reason.Code)
	}
}

func TestClientSession_HandleMessage_Publish_AfterAuth(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	_ = node.Run(ctx) // Register event handler
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	// First authenticate
	connectMsg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{},
		},
	}
	err = client.HandleMessage(ctx, connectMsg)
	if err != nil {
		t.Fatalf("HandleMessage() Connect error = %v", err)
	}

	// Reset transport messages
	transport.messages = nil

	// Now publish
	pubMsg := &clientpb.InboundMessage{
		Id: "msg-2",
		Envelope: &clientpb.InboundMessage_Publish{
			Publish: &clientpb.Publish{
				Channel: "test-channel",
				Payload: &sharedpb.Payload{
					Data: &sharedpb.Payload_Binary{
						Binary: []byte("test payload"),
					},
				},
			},
		},
	}

	err = client.HandleMessage(ctx, pubMsg)
	if err != nil {
		t.Fatalf("HandleMessage() Publish error = %v", err)
	}

	// Should have sent a PublishAck
	if transport.getMessageCount() != 1 {
		t.Errorf("Transport should have 1 message, got %d", transport.getMessageCount())
	}
}

func TestClientSession_HandleMessage_Publish_Transient(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	_ = node.Run(ctx)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	// First authenticate
	connectMsg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, connectMsg))

	// Reset transport messages
	transport.messages = nil

	// 先做一条非 transient 发布：隔离「历史本来就空」的假阳性，
	// 确保断言的是「transient 不落历史」而非空历史。
	regularMsg := &clientpb.InboundMessage{
		Id: "msg-2",
		Envelope: &clientpb.InboundMessage_Publish{
			Publish: &clientpb.Publish{
				Channel: "test-channel",
				Payload: &sharedpb.Payload{
					Data: &sharedpb.Payload_Binary{
						Binary: []byte("regular payload"),
					},
				},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, regularMsg))

	historyPage, err := node.Broker().History("test-channel", 0, 100)
	require.NoError(t, err)
	require.Len(t, historyPage.Pubs(), 1, "regular publish must be stored in history")

	// Reset transport messages（regular 发布的 ack 不计入 transient 断言）
	transport.messages = nil

	// Now publish a transient message
	pubMsg := &clientpb.InboundMessage{
		Id: "msg-3",
		Envelope: &clientpb.InboundMessage_Publish{
			Publish: &clientpb.Publish{
				Channel: "test-channel",
				Payload: &sharedpb.Payload{
					Data: &sharedpb.Payload_Binary{
						Binary: []byte("test payload"),
					},
				},
				Transient: true,
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, pubMsg))

	// Should have sent exactly one PublishAck
	require.Equal(t, 1, transport.getMessageCount())

	var out clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getLastMessage(), &out))
	ack := out.GetPublishAck()
	require.NotNil(t, ack, "envelope must be PublishAck")
	assert.Equal(t, "msg-3", ack.GetId())
	assert.Equal(t, uint64(0), ack.GetOffset(), "transient publish must ack with offset 0")

	// Transient publish must NOT be stored in history (still exactly the
	// single regular publication).
	historyPage, err = node.Broker().History("test-channel", 0, 100)
	require.NoError(t, err)
	require.Len(t, historyPage.Pubs(), 1, "transient publish must not be stored in history")
}

func TestClientSession_HandleMessage_Subscribe(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	// First authenticate
	connectMsg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{},
		},
	}
	err = client.HandleMessage(ctx, connectMsg)
	if err != nil {
		t.Fatalf("HandleMessage() Connect error = %v", err)
	}

	// Reset transport messages
	transport.messages = nil

	// Subscribe to channels
	subMsg := &clientpb.InboundMessage{
		Id: "msg-2",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{
				Subscriptions: []*clientpb.Subscription{
					{Channel: "channel-1", Ephemeral: false},
					{Channel: "channel-2", Ephemeral: true},
				},
			},
		},
	}

	err = client.HandleMessage(ctx, subMsg)
	if err != nil {
		t.Fatalf("HandleMessage() Subscribe error = %v", err)
	}

	// Should have sent a SubscribeAck
	if transport.getMessageCount() != 1 {
		t.Errorf("Transport should have 1 message, got %d", transport.getMessageCount())
	}

	// Check that subscriptions were added
	count1 := node.Hub().NumSubscribers("channel-1")
	if count1 != 1 {
		t.Errorf("channel-1 should have 1 subscriber, got %d", count1)
	}
	count2 := node.Hub().NumSubscribers("channel-2")
	if count2 != 1 {
		t.Errorf("channel-2 should have 1 subscriber, got %d", count2)
	}
}

func TestClientSession_HandleMessage_RpcRequest_NoProxy(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	// First authenticate
	connectMsg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{},
		},
	}
	err = client.HandleMessage(ctx, connectMsg)
	if err != nil {
		t.Fatalf("HandleMessage() Connect error = %v", err)
	}

	// Reset transport messages
	transport.messages = nil

	// Send RPC request
	rpcMsg := &clientpb.InboundMessage{
		Id: "msg-2",
		Envelope: &clientpb.InboundMessage_RpcRequest{
			RpcRequest: &clientpb.RpcRequest{
				Channel: "test-channel",
				Method:  "test.method",
				Payload: &sharedpb.Payload{
					Data: &sharedpb.Payload_Binary{
						Binary: []byte("rpc payload"),
					},
				},
			},
		},
	}

	err = client.HandleMessage(ctx, rpcMsg)
	if err != nil {
		t.Fatalf("HandleMessage() RpcRequest error = %v", err)
	}

	// Should echo back the event when no proxy is configured
	if transport.getMessageCount() != 1 {
		t.Errorf("Transport should have 1 message, got %d", transport.getMessageCount())
	}
}

func TestClientSession_HandleMessage_Closed(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, closeFunc, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	// Close the client
	_ = closeFunc()

	// Try to handle a message after close
	msg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Ping{
			Ping: &clientpb.Ping{},
		},
	}

	err = client.HandleMessage(ctx, msg)
	if err == nil {
		t.Error("HandleMessage() should return error when client is closed")
	}
}

func TestClientSession_CloseFunc(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	_, closeFunc, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	err = closeFunc()
	if err != nil {
		t.Fatalf("closeFunc() error = %v", err)
	}

	if !transport.isClosed() {
		t.Error("Transport should be closed after closeFunc()")
	}

	if transport.getCloseCount() != 1 {
		t.Errorf("Close should be called once, got %d", transport.getCloseCount())
	}
}

func TestClientSession_CloseFunc_WithDisconnect(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	// The closeFunc doesn't take parameters - it uses the Disconnect from creation
	// Let's test the close method directly
	err = client.close(DisconnectBadRequest)
	if err != nil {
		t.Fatalf("close() error = %v", err)
	}

	if !transport.isClosed() {
		t.Error("Transport should be closed")
	}

	reason := transport.getCloseReason()
	if reason.Code != DisconnectBadRequest.Code {
		t.Errorf("Close code should be %d, got %d", DisconnectBadRequest.Code, reason.Code)
	}
}

func TestClientSession_Send(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	msg := &clientpb.OutboundMessage{
		Id: "out-1",
		Envelope: &clientpb.OutboundMessage_Pong{
			Pong: &clientpb.Pong{},
		},
	}

	err = client.Send(ctx, msg)
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	if transport.getMessageCount() != 1 {
		t.Errorf("Transport should have 1 message, got %d", transport.getMessageCount())
	}
}

func TestClientSession_Send_TransportError(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &failTransport{writeErr: errors.New("write failed")}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	msg := &clientpb.OutboundMessage{
		Id: "out-1",
		Envelope: &clientpb.OutboundMessage_Pong{
			Pong: &clientpb.Pong{},
		},
	}

	err = client.Send(ctx, msg)
	if err == nil {
		t.Error("Send() should return error when transport write fails")
	}
}

// assertDisconnectBeforeAuth handles a message before Connect and asserts the
// client is disconnected with DisconnectInvalidToken.
func assertDisconnectBeforeAuth(t *testing.T, msg *clientpb.InboundMessage) {
	t.Helper()
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	err = client.HandleMessage(ctx, msg)
	if err != nil {
		t.Fatalf("HandleMessage() should not return error for disconnect, got %v", err)
	}

	if !transport.isClosed() {
		t.Error("Transport should be closed after message before auth")
	}

	reason := transport.getCloseReason()
	if reason.Code != DisconnectInvalidToken.Code {
		t.Errorf("Close code should be %d (invalid token), got %d", DisconnectInvalidToken.Code, reason.Code)
	}
}

func TestClientSession_HandleMessage_Subscribe_BeforeAuth(t *testing.T) {
	assertDisconnectBeforeAuth(t, &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{
				Subscriptions: []*clientpb.Subscription{{Channel: "channel-1"}},
			},
		},
	})
}

func TestClientSession_HandleMessage_RPC_BeforeAuth(t *testing.T) {
	assertDisconnectBeforeAuth(t, &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_RpcRequest{
			RpcRequest: &clientpb.RpcRequest{Channel: "channel-1", Method: "echo"},
		},
	})
}

func TestClientSession_HandleMessage_Unsubscribe_BeforeAuth(t *testing.T) {
	assertDisconnectBeforeAuth(t, &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Unsubscribe{
			Unsubscribe: &clientpb.Unsubscribe{
				Subscriptions: []*clientpb.Subscription{{Channel: "channel-1"}},
			},
		},
	})
}

func TestClientSession_HandleMessage_Ping_BeforeAuth(t *testing.T) {
	assertDisconnectBeforeAuth(t, &clientpb.InboundMessage{
		Id:       "msg-1",
		Envelope: &clientpb.InboundMessage_Ping{Ping: &clientpb.Ping{}},
	})
}

func TestClientSession_HandleMessage_SubRefresh_BeforeAuth(t *testing.T) {
	assertDisconnectBeforeAuth(t, &clientpb.InboundMessage{
		Id:       "msg-1",
		Envelope: &clientpb.InboundMessage_SubRefresh{SubRefresh: &clientpb.SubRefresh{}},
	})
}

func TestClientSession_HandleMessage_Unsupported(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	// First authenticate
	connectMsg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{},
		},
	}
	err = client.HandleMessage(ctx, connectMsg)
	if err != nil {
		t.Fatalf("HandleMessage() Connect error = %v", err)
	}

	// Reset transport messages
	transport.messages = nil

	// Try unsubscribe (returns TODO error)
	unsubMsg := &clientpb.InboundMessage{
		Id: "msg-2",
		Envelope: &clientpb.InboundMessage_Unsubscribe{
			Unsubscribe: &clientpb.Unsubscribe{
				Subscriptions: []*clientpb.Subscription{
					{Channel: "channel-1"},
				},
			},
		},
	}

	err = client.HandleMessage(ctx, unsubMsg)
	if err != nil {
		t.Errorf("HandleMessage() Unsubscribe should not return error, got %v", err)
	}

	// Should send UnsubscribeAck response
	if transport.getMessageCount() != 1 {
		t.Errorf("Transport should have 1 message, got %d", transport.getMessageCount())
	}
}

func TestClientSession_HandleMessage_SubRefresh(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	// First authenticate
	connectMsg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{},
		},
	}
	err = client.HandleMessage(ctx, connectMsg)
	if err != nil {
		t.Fatalf("HandleMessage() Connect error = %v", err)
	}

	// Reset transport messages
	transport.messages = nil

	// Try SubRefresh (now implemented)
	refreshMsg := &clientpb.InboundMessage{
		Id: "msg-2",
		Envelope: &clientpb.InboundMessage_SubRefresh{
			SubRefresh: &clientpb.SubRefresh{},
		},
	}

	err = client.HandleMessage(ctx, refreshMsg)
	if err != nil {
		t.Errorf("HandleMessage() SubRefresh should not return error, got %v", err)
	}

	// Should send SubRefreshAck response
	if transport.getMessageCount() != 1 {
		t.Errorf("Transport should have 1 message, got %d", transport.getMessageCount())
	}
}

func TestClientSession_ConcurrentMessages(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	// First authenticate
	connectMsg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{},
		},
	}
	err = client.HandleMessage(ctx, connectMsg)
	if err != nil {
		t.Fatalf("HandleMessage() Connect error = %v", err)
	}

	// Reset transport messages
	transport.messages = nil

	// Send concurrent ping messages
	const numPings = 10
	var wg sync.WaitGroup
	for i := 0; i < numPings; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			pingMsg := &clientpb.InboundMessage{
				Id: "ping-" + string(rune('0'+n)),
				Envelope: &clientpb.InboundMessage_Ping{
					Ping: &clientpb.Ping{},
				},
			}
			_ = client.HandleMessage(ctx, pingMsg)
		}(i)
	}

	wg.Wait()

	// All messages should be processed
	if transport.getMessageCount() != numPings {
		t.Errorf("Transport should have %d messages, got %d", numPings, transport.getMessageCount())
	}
}

func TestMakeOutboundMessage(t *testing.T) {
	inMsg := &clientpb.InboundMessage{
		Id: "in-1",
	}

	outMsg := MakeOutboundMessage(inMsg, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Pong{
			Pong: &clientpb.Pong{},
		}
	})

	if outMsg.Id != "in-1" {
		t.Errorf("Id should be copied from InboundMessage, got %s", outMsg.Id)
	}
	if outMsg.Time == 0 {
		t.Error("Time should be set")
	}
	if outMsg.GetPong() == nil {
		t.Error("Envelope should be set by bodyFunc")
	}
}

func TestMakeOutboundMessage_WithoutInbound(t *testing.T) {
	outMsg := MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Pong{
			Pong: &clientpb.Pong{},
		}
	})

	if outMsg.Id == "" {
		t.Error("Id should be generated")
	}
	if outMsg.Time == 0 {
		t.Error("Time should be set")
	}
}

func TestClientSession_Publish_WithChannelFromEvent(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	_ = node.Run(ctx) // Register event handler
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	// First authenticate
	connectMsg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{},
		},
	}
	err = client.HandleMessage(ctx, connectMsg)
	if err != nil {
		t.Fatalf("HandleMessage() Connect error = %v", err)
	}

	// Reset transport messages
	transport.messages = nil

	// Publish with channel in Publish message
	pubMsg := &clientpb.InboundMessage{
		Id: "msg-2",
		Envelope: &clientpb.InboundMessage_Publish{
			Publish: &clientpb.Publish{
				Channel: "event-source-channel",
				Payload: &sharedpb.Payload{
					Data: &sharedpb.Payload_Binary{
						Binary: []byte("test payload"),
					},
				},
			},
		},
	}

	err = client.HandleMessage(ctx, pubMsg)
	if err != nil {
		t.Fatalf("HandleMessage() Publish error = %v", err)
	}

	// Should have sent a PublishAck
	if transport.getMessageCount() != 1 {
		t.Errorf("Transport should have 1 message, got %d", transport.getMessageCount())
	}
}

func TestClientSession_Marshal(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	msg := &clientpb.OutboundMessage{
		Id: "test-1",
		Envelope: &clientpb.OutboundMessage_Pong{
			Pong: &clientpb.Pong{},
		},
	}

	data, err := client.marshal(msg)
	if err != nil {
		t.Fatalf("marshal() error = %v", err)
	}

	if len(data) == 0 {
		t.Error("marshal() should return non-empty data")
	}

	// Should be valid JSON
	var out map[string]any
	m := JSONMarshaler{}
	if err := m.Unmarshal(data, &out); err != nil {
		t.Errorf("marshal() should produce valid JSON: %v", err)
	}
}

func TestClientSession_Marshal_Protobuf(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	// Create client with protobuf marshaler
	client, _, err := NewClient(ctx, node, transport, ProtobufMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	msg := &clientpb.OutboundMessage{
		Id: "test-1",
		Envelope: &clientpb.OutboundMessage_Pong{
			Pong: &clientpb.Pong{},
		},
	}

	data, err := client.marshal(msg)
	if err != nil {
		t.Fatalf("marshal() error = %v", err)
	}

	if len(data) == 0 {
		t.Error("marshal() should return non-empty data")
	}

	// Should be valid protobuf
	var out clientpb.OutboundMessage
	m := ProtobufMarshaler{}
	if err := m.Unmarshal(data, &out); err != nil {
		t.Errorf("marshal() should produce valid protobuf: %v", err)
	}
}

func TestClientSession_HandleMessage_WithBinaryData(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	_ = node.Run(ctx) // Register event handler
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	// First authenticate
	connectMsg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{},
		},
	}
	err = client.HandleMessage(ctx, connectMsg)
	if err != nil {
		t.Fatalf("HandleMessage() Connect error = %v", err)
	}

	// Reset transport messages
	transport.messages = nil

	// Publish with binary data
	binaryPayload := []byte{0x01, 0x02, 0x03, 0x04}
	pubMsg := &clientpb.InboundMessage{
		Id: "msg-2",
		Envelope: &clientpb.InboundMessage_Publish{
			Publish: &clientpb.Publish{
				Channel: "test-channel",
				Payload: &sharedpb.Payload{
					Data: &sharedpb.Payload_Binary{
						Binary: binaryPayload,
					},
				},
			},
		},
	}

	err = client.HandleMessage(ctx, pubMsg)
	if err != nil {
		t.Fatalf("HandleMessage() Publish error = %v", err)
	}

	// Should have sent a PublishAck
	if transport.getMessageCount() != 1 {
		t.Errorf("Transport should have 1 message, got %d", transport.getMessageCount())
	}
}

// --- P1-3: recovered messages must use the same stable channel-offset IDs
// as realtime delivery, and recovery is capped at a fixed total ---

// fakeHistoryBroker returns a fixed publication list for History and does not
// deliver publications in realtime.
type fakeHistoryBroker struct {
	pubs []*Publication
}

func (f *fakeHistoryBroker) Start(ctx context.Context, handler PublicationHandler) error {
	<-ctx.Done()
	return nil
}

func (f *fakeHistoryBroker) Subscribe(string) error   { return nil }
func (f *fakeHistoryBroker) Unsubscribe(string) error { return nil }

func (f *fakeHistoryBroker) Publish(ch string, pub *Publication) (uint64, error) {
	return 0, nil
}

func (f *fakeHistoryBroker) PublishTransient(ch string, pub *Publication) error {
	return nil
}

func (f *fakeHistoryBroker) History(ch string, sinceOffset uint64, limit int) (*HistoryPage, error) {
	result := make([]*Publication, 0, len(f.pubs))
	for _, p := range f.pubs {
		if p.Offset >= sinceOffset {
			if limit > 0 && len(result) >= limit {
				break
			}
			result = append(result, p)
		}
	}
	return &HistoryPage{Publications: result}, nil
}

func TestNode_Connect_RecoveryIDsMatchRealtime(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	_ = node.Run(ctx)

	transport1 := &capturingTransport{}
	client1, _, err := NewClient(ctx, node, transport1, JSONMarshaler{})
	require.NoError(t, err)

	connectMsg := &clientpb.InboundMessage{
		Id:       "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "client-1"}},
	}
	require.NoError(t, client1.HandleMessage(ctx, connectMsg))

	// Capture the broker epoch from the Connected envelope so the reconnect
	// can prove its offset belongs to the current broker generation.
	var connectedMsg clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(transport1.getLastMessage(), &connectedMsg))
	epoch := connectedMsg.GetConnected().GetEpoch()
	require.NotEmpty(t, epoch)

	transport1.messages = nil

	subMsg := &clientpb.InboundMessage{
		Id: "msg-2",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{Subscriptions: []*clientpb.Subscription{{Channel: "recovery.ch"}}},
		},
	}
	require.NoError(t, client1.HandleMessage(ctx, subMsg))
	transport1.messages = nil

	// Publish 3 messages and capture the realtime IDs per offset.
	realtimeIDs := make(map[uint64]string)
	for i := 0; i < 3; i++ {
		_, err := node.Broker().Publish("recovery.ch", publishPub([]byte(fmt.Sprintf("m%d", i+1)), false))
		require.NoError(t, err)
	}
	require.Equal(t, 3, transport1.getMessageCount())
	for i := 0; i < 3; i++ {
		var out clientpb.OutboundMessage
		require.NoError(t, JSONMarshaler{}.Unmarshal(transport1.getMessage(i), &out))
		pub := out.GetPublication()
		require.NotNil(t, pub)
		require.Len(t, pub.GetMessages(), 1)
		m := pub.GetMessages()[0]
		realtimeIDs[m.GetOffset()] = m.GetId()
		assert.Equal(t, fmt.Sprintf("recovery.ch-%d", m.GetOffset()), m.GetId(),
			"realtime ID must follow the channel-offset rule")
	}

	// Disconnect and reconnect with recovery from offset 1.
	_ = client1.Close(Disconnect{})
	transport2 := &capturingTransport{}
	client2, _, err := NewClient(ctx, node, transport2, JSONMarshaler{})
	require.NoError(t, err)

	recoverMsg := &clientpb.InboundMessage{
		Id: "msg-3",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId: "client-2",
				Subscriptions: []*clientpb.Subscription{
					{Channel: "recovery.ch", Recover: true, Offset: 1, Epoch: epoch},
				},
			},
		},
	}
	require.NoError(t, client2.HandleMessage(ctx, recoverMsg))

	var out clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(transport2.getLastMessage(), &out))
	connected := out.GetConnected()
	require.NotNil(t, connected)
	require.Len(t, connected.GetPublications(), 2)
	for _, pub := range connected.GetPublications() {
		msgs := pub.GetMessages()
		require.Len(t, msgs, 1)
		m := msgs[0]
		expected := fmt.Sprintf("recovery.ch-%d", m.GetOffset())
		assert.Equal(t, expected, m.GetId(), "recovered ID must follow the channel-offset rule")
		assert.Equal(t, realtimeIDs[m.GetOffset()], m.GetId(),
			"recovered ID must equal the realtime ID for offset %d", m.GetOffset())
	}
}

func TestNode_Connect_RecoveryCap(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)

	const total = MaxRecoveredPublications + 500
	pubs := make([]*Publication, 0, total)
	for i := 1; i <= total; i++ {
		pubs = append(pubs, &Publication{
			Channel: "cap-ch",
			Offset:  uint64(i),
			Payload: []byte("m"),
			Time:    int64(i),
		})
	}
	node.SetBroker(&fakeHistoryBroker{pubs: pubs})
	_ = node.Run(ctx)

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	msg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId: "client-1",
				Subscriptions: []*clientpb.Subscription{
					{Channel: "cap-ch", Recover: true, Offset: 1},
				},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, msg))

	var out clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getLastMessage(), &out))
	connected := out.GetConnected()
	require.NotNil(t, connected)
	require.Len(t, connected.GetPublications(), MaxRecoveredPublications)
	// The first recovered publication is offset 2 (sinceOffset = offset+1) and
	// its ID must follow the channel-offset rule.
	msgs := connected.GetPublications()[0].GetMessages()
	require.NotEmpty(t, msgs)
	assert.Equal(t, fmt.Sprintf("cap-ch-%d", msgs[0].GetOffset()), msgs[0].GetId())

	// PR-03: the cap must be visible: truncated=true and a recover_result
	// carrying the last delivered offset.
	require.True(t, connected.GetTruncated(), "hitting the recovery cap must set Connected.truncated")
	require.NotEmpty(t, connected.GetRecoverResults(), "recover_results must cover every recovered channel")
	res := connected.GetRecoverResults()[0]
	require.Equal(t, "cap-ch", res.GetChannel())
	require.True(t, res.GetRecovered())
	require.True(t, res.GetTruncated())
	lastMsgs := connected.GetPublications()[len(connected.GetPublications())-1].GetMessages()
	require.NotEmpty(t, lastMsgs)
	assert.Equal(t, lastMsgs[0].GetOffset(), res.GetOffset(),
		"truncated offset must be the last delivered publication's offset")
}

// --- P1-4: PublishAck.Offset must carry the broker-assigned offset ---

func TestClientSession_PublishAck_Offset(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	_ = node.Run(ctx) // Register event handler

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	connectMsg := &clientpb.InboundMessage{
		Id:       "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{}},
	}
	require.NoError(t, client.HandleMessage(ctx, connectMsg))

	// Seed the channel through the broker so the ack offset is deterministic.
	seedOffset, err := node.Broker().Publish("ack-ch", publishPub([]byte("seed"), false))
	require.NoError(t, err)
	require.Equal(t, uint64(1), seedOffset)

	transport.messages = nil

	pubMsg := &clientpb.InboundMessage{
		Id: "msg-2",
		Envelope: &clientpb.InboundMessage_Publish{
			Publish: &clientpb.Publish{
				Channel: "ack-ch",
				Payload: &sharedpb.Payload{Data: &sharedpb.Payload_Binary{Binary: []byte("hi")}},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, pubMsg))

	var out clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getLastMessage(), &out))
	ack := out.GetPublishAck()
	require.NotNil(t, ack, "expected a PublishAck envelope")
	assert.Equal(t, uint64(2), ack.GetOffset())
}

func TestClientStatus_String(t *testing.T) {
	tests := []struct {
		name   string
		status status
		want   string
	}{
		{"connecting", statusConnecting, "1"},
		{"connected", statusConnected, "2"},
		{"closed", statusClosed, "3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.status.String(); got != tt.want {
				t.Errorf("status.String() = %v, want %v", got, tt.want)
			}
		})
	}
}

func (s status) String() string {
	switch s {
	case statusConnecting:
		return "1"
	case statusConnected:
		return "2"
	case statusClosed:
		return "3"
	default:
		return "unknown"
	}
}

func BenchmarkClientSession_HandleMessage_Ping(b *testing.B) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		b.Fatalf("NewClient() error = %v", err)
	}

	// Authenticate first
	connectMsg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{},
		},
	}
	_ = client.HandleMessage(ctx, connectMsg)

	pingMsg := &clientpb.InboundMessage{
		Id: "ping",
		Envelope: &clientpb.InboundMessage_Ping{
			Ping: &clientpb.Ping{},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = client.HandleMessage(ctx, pingMsg)
	}
}

func BenchmarkClientSession_Marshal_JSON(b *testing.B) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		b.Fatalf("NewClient() error = %v", err)
	}

	msg := &clientpb.OutboundMessage{
		Id: "test",
		Envelope: &clientpb.OutboundMessage_Pong{
			Pong: &clientpb.Pong{},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = client.marshal(msg)
	}
}

func BenchmarkClientSession_Marshal_Protobuf(b *testing.B) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, ProtobufMarshaler{})
	if err != nil {
		b.Fatalf("NewClient() error = %v", err)
	}

	msg := &clientpb.OutboundMessage{
		Id: "test",
		Envelope: &clientpb.OutboundMessage_Pong{
			Pong: &clientpb.Pong{},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = client.marshal(msg)
	}
}
