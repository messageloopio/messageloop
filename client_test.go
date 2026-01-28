package messageloop

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/binding/format/protobuf/v2/pb"
	clientpb "github.com/fleetlit/messageloop/genproto/v1"
)

// capturingTransport captures all written messages for inspection
type capturingTransport struct {
	mu            sync.Mutex
	messages      [][]byte
	closeCount    atomic.Int32
	closed        atomic.Bool
	closeReason   Disconnect
	writeDelay    time.Duration
	writeError    error
	closeOnWrite  bool
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
	c.closeReason = disconnect
	return nil
}

func (c *capturingTransport) getMessages() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.messages
}

func (c *capturingTransport) getMessageCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.messages)
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
	return c.closeReason
}

// failTransport simulates transport failures
type failTransport struct {
	writeErr  error
	closeErr  error
	closeCalled bool
	mu        sync.Mutex
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

func (f *failTransport) wasCloseCalled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeCalled
}

func TestNewClientSession(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}
	marshaler := JSONMarshaler{}

	client, closeFunc, err := NewClientSession(ctx, node, transport, marshaler)
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
	}
	if client == nil {
		t.Fatal("NewClientSession() should return a client")
	}
	if closeFunc == nil {
		t.Fatal("NewClientSession() should return a close function")
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

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
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
	client2, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
	}
	if client2.SessionID() == sessionID {
		t.Error("Different clients should have different session IDs")
	}
}

func TestClientSession_ClientID(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
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

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
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

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
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

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
	}

	info := client.ClientInfo()
	if info == nil {
		t.Fatal("ClientInfo() should not return nil")
	}
	if info.SessionID != client.SessionID() {
		t.Error("ClientInfo.SessionID should match ClientSession.SessionID")
	}
}

func TestClientSession_Channels(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
	}

	channels := client.Channels()
	if channels == nil {
		t.Error("Channels() should not return nil")
	}
	if len(channels) != 0 {
		t.Errorf("Channels() should return empty slice, got %d channels", len(channels))
	}
}

func TestClientSession_HandleMessage_Connect(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
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

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
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

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
	}

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

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
	}

	msg := &clientpb.InboundMessage{
		Id:      "msg-1",
		Channel: "test-channel",
		Envelope: &clientpb.InboundMessage_Publish{
			Publish: &cloudevents.CloudEvent{
				Id:     "event-1",
				Source: "test-source",
				Type:   "test.event",
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
	if reason.Code != DisconnectStale.Code {
		t.Errorf("Close code should be %d (stale), got %d", DisconnectStale.Code, reason.Code)
	}
}

func TestClientSession_HandleMessage_Publish_AfterAuth(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	_ = node.Run() // Register event handler
	transport := &capturingTransport{}

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
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
		Id:      "msg-2",
		Channel: "test-channel",
		Envelope: &clientpb.InboundMessage_Publish{
			Publish: &cloudevents.CloudEvent{
				Id:     "event-1",
				Source: "test-source",
				Type:   "test.event",
				Data: &cloudevents.CloudEvent_TextData{
					TextData: "test payload",
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

func TestClientSession_HandleMessage_Subscribe(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
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

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
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
	event := &cloudevents.CloudEvent{
		Id:     "rpc-1",
		Source: "test-channel",
		Type:   "test.method",
	}
	rpcMsg := &clientpb.InboundMessage{
		Id:      "msg-2",
		Channel: "test-channel",
		Method:  "test.method",
		Envelope: &clientpb.InboundMessage_RpcRequest{
			RpcRequest: event,
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

	client, closeFunc, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
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

	_, closeFunc, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
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

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
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

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
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

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
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

func TestClientSession_HandleMessage_Unsupported(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
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

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
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

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
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
		Id:       "in-1",
		Metadata: map[string]string{"key": "value"},
	}

	outMsg := MakeOutboundMessage(inMsg, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Pong{
			Pong: &clientpb.Pong{},
		}
	})

	if outMsg.Id != "in-1" {
		t.Errorf("Id should be copied from InboundMessage, got %s", outMsg.Id)
	}
	if outMsg.Metadata["key"] != "value" {
		t.Error("Metadata should be copied from InboundMessage")
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
	if outMsg.Metadata == nil {
		t.Error("Metadata should be initialized")
	}
	if outMsg.Time == 0 {
		t.Error("Time should be set")
	}
}

func TestClientSession_Publish_WithChannelFromEvent(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	_ = node.Run() // Register event handler
	transport := &capturingTransport{}

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
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

	// Publish without channel in InboundMessage, but with source in event
	pubMsg := &clientpb.InboundMessage{
		Id: "msg-2",
		// Channel not set
		Envelope: &clientpb.InboundMessage_Publish{
			Publish: &cloudevents.CloudEvent{
				Id:     "event-1",
				Source: "event-source-channel", // Will be used as channel
				Type:   "test.event",
				Data: &cloudevents.CloudEvent_TextData{
					TextData: "test payload",
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

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
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
	client, _, err := NewClientSession(ctx, node, transport, ProtobufMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
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
	_ = node.Run() // Register event handler
	transport := &capturingTransport{}

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
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
		Id:      "msg-2",
		Channel: "test-channel",
		Envelope: &clientpb.InboundMessage_Publish{
			Publish: &cloudevents.CloudEvent{
				Id:     "event-1",
				Source: "test-source",
				Type:   "test.event",
				Data: &cloudevents.CloudEvent_BinaryData{
					BinaryData: binaryPayload,
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

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		b.Fatalf("NewClientSession() error = %v", err)
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

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		b.Fatalf("NewClientSession() error = %v", err)
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

	client, _, err := NewClientSession(ctx, node, transport, ProtobufMarshaler{})
	if err != nil {
		b.Fatalf("NewClientSession() error = %v", err)
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
