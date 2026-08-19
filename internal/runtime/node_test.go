package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/messageloopio/messageloop/config"
	"github.com/messageloopio/messageloop/proxy"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

func TestNewNode(t *testing.T) {
	node := NewNode(nil)
	if node == nil {
		t.Fatal("NewNode(nil) should not return nil")
	}
	if node.hub == nil {
		t.Error("hub should be initialized")
	}
	if node.broker == nil {
		t.Error("broker should be initialized")
	}
	// subLocks is a fixed-size array, always has numSubLocks elements
	if len(node.subLocks) != numSubLocks {
		t.Errorf("len(subLocks) = %d, want %d", len(node.subLocks), numSubLocks)
	}
}

func TestNewNode_HeartbeatDefaultIdleTimeout(t *testing.T) {
	// No heartbeat configuration at all: the default idle timeout must be
	// applied so idle connections are still disconnected.
	node := NewNode(nil)
	require.NotNil(t, node.heartbeatManager, "heartbeat manager must be created with the default idle timeout")
	assert.Equal(t, DefaultHeartbeatIdleTimeout, node.GetHeartbeatIdleTimeout())

	// An explicit configuration wins over the default.
	explicit := NewNode(&config.Server{Heartbeat: config.Heartbeat{IdleTimeout: "45s"}})
	assert.Equal(t, 45*time.Second, explicit.GetHeartbeatIdleTimeout())
}

func TestNode_Hub(t *testing.T) {
	node := NewNode(nil)
	hub := node.Hub()
	if hub == nil {
		t.Error("Hub() should not return nil")
	}
	if hub != node.hub {
		t.Error("Hub() should return the same hub instance")
	}
}

func TestNode_Broker(t *testing.T) {
	node := NewNode(nil)
	broker := node.Broker()
	if broker == nil {
		t.Error("Broker() should not return nil")
	}
	if broker != node.broker {
		t.Error("Broker() should return the same broker instance")
	}
}

func TestNode_SetBroker(t *testing.T) {
	node := NewNode(nil)
	newBroker := NewMemoryBroker(MemoryBrokerOptions{})

	node.SetBroker(newBroker)

	if node.broker != newBroker {
		t.Error("SetBroker() should set the broker")
	}
}

func TestNode_Run(t *testing.T) {
	node := NewNode(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := node.Run(ctx)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestNode_MaxMessageSize_DefaultWhenZero(t *testing.T) {
	node := NewNode(nil)
	if got := node.MaxMessageSize(); got != DefaultMaxMessageSize {
		t.Errorf("MaxMessageSize() = %d, want default %d", got, DefaultMaxMessageSize)
	}
}

func TestNode_MaxMessageSize_ConfiguredValue(t *testing.T) {
	node := NewNode(&config.Server{
		Limits: config.Limits{MaxMessageSize: 4096},
	})
	if got := node.MaxMessageSize(); got != 4096 {
		t.Errorf("MaxMessageSize() = %d, want 4096", got)
	}
}

func TestNode_HandlePublication(t *testing.T) {
	node := NewNode(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = node.Run(ctx)

	transport := &capturingTransport{}

	// Add a client subscribed to the channel
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	client.MarkAuthenticated()
	require.NoError(t, client.Attach(client.Attachment()))

	_ = node.AddClient(client)
	_ = node.AddSubscription(ctx, "test-channel", Subscriber{Session: client, Ephemeral: false})

	// Publish via the broker so the internal handler is triggered.
	_, err = node.Publish("test-channel", publishPub([]byte("test payload"), false))
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	// Client should receive a message (delivery is asynchronous).
	waitMessageCount(t, transport, 1)
}

func TestNode_HandlePublication_NoSubscribers(t *testing.T) {
	node := NewNode(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = node.Run(ctx)

	// Publish to a channel with no subscribers — should not error.
	_, err := node.Publish("empty-channel", publishPub([]byte("test payload"), false))
	if err != nil {
		t.Fatalf("Publish() to empty channel error = %v", err)
	}
}

func TestNode_Publish(t *testing.T) {
	node := NewNode(nil)
	_ = node.Run(context.Background())

	transport := &capturingTransport{}
	ctx := context.Background()

	// Add a client subscribed to the channel
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	client.MarkAuthenticated()
	require.NoError(t, client.Attach(client.Attachment()))

	_ = node.AddClient(client)
	_ = node.AddSubscription(ctx, "test-channel", Subscriber{Session: client, Ephemeral: false})

	// Clear transport messages from subscription
	transport.messages = nil

	_, err = node.Publish("test-channel", publishPub([]byte("test payload"), false))
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	// Client should receive a message (delivery is asynchronous).
	waitMessageCount(t, transport, 1)
}

func TestNode_Publish_WithOptions(t *testing.T) {
	node := NewNode(nil)
	_ = node.Run(context.Background())

	_, err := node.Publish("test-channel", publishPub([]byte("test payload"), false))
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
}

// TestNode_PresenceEventsNotInHistory verifies P2-19: presence join/leave
// events are delivered transiently and never appear in the History recovery
// stream, while regular publications on the same channel still do.
func TestNode_PresenceEventsNotInHistory(t *testing.T) {
	node := NewNode(nil)
	require.NoError(t, node.Run(context.Background()))

	ch := presenceChannel("presence-hist.ch")
	node.PublishPresenceJoin("presence-hist.ch", "client-1", "user-1")
	node.PublishPresenceLeave("presence-hist.ch", "client-1", "user-1")

	page, err := node.Broker().History(ch, 0, 0)
	require.NoError(t, err)
	require.Empty(t, page.Pubs(), "presence join/leave events must not appear in history")

	_, err = node.Broker().Publish(ch, publishPub([]byte("normal"), true))
	require.NoError(t, err)
	page, err = node.Broker().History(ch, 0, 0)
	require.NoError(t, err)
	require.Len(t, page.Pubs(), 1, "a regular publish on the same channel must still be recorded")
}

// TestNode_PublishPresenceJoin_DistinctMessageIDs verifies the final-review
// fix for the presence message ID collision: transient presence events
// (offset 0) must not all share the "channel-0" ID — every event delivered
// on the presence channel gets a unique message ID.
func TestNode_PublishPresenceJoin_DistinctMessageIDs(t *testing.T) {
	node := NewNode(nil)
	require.NoError(t, node.Run(context.Background()))
	transport := &capturingTransport{}
	ctx := context.Background()

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	connectMsg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{Version: testProtocolVersion, ClientId: "client-1"},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, connectMsg))

	subMsg := &clientpb.InboundMessage{
		Id: "msg-2",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{
				Subscriptions: []*clientpb.Subscription{{Channel: presenceChannel("presence-id.ch")}},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, subMsg))
	transport.messages = nil

	node.PublishPresenceJoin("presence-id.ch", "client-1", "user-1")
	node.PublishPresenceJoin("presence-id.ch", "client-2", "user-2")

	waitMessageCount(t, transport, 2)
	msgs := transport.snapshotMessages()
	id1 := capturedPublicationID(t, msgs[0])
	id2 := capturedPublicationID(t, msgs[1])
	assert.NotEqual(t, id1, id2, "each presence event must carry a distinct message ID")
	assert.NotEqual(t, presenceChannel("presence-id.ch")+"-0", id1,
		"transient events must not reuse the channel-0 ID")
}

func capturedPublicationID(t *testing.T, data []byte) string {
	t.Helper()
	var out clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(data, &out))
	pub := out.GetPublication()
	require.NotNil(t, pub)
	require.Len(t, pub.GetMessages(), 1)
	return pub.GetMessages()[0].GetId()
}

func TestNode_AddClient(t *testing.T) {
	node := NewNode(nil)
	transport := &capturingTransport{}
	ctx := context.Background()

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	_ = node.AddClient(client)

	// Check that client was added to hub
	hub := node.Hub()
	exists := hub.LookupSession(client.SessionID()) != nil

	if !exists {
		t.Error("Client should be added to hub sessions")
	}
}

func TestNode_AddSubscription(t *testing.T) {
	node := NewNode(nil)
	_ = node.Run(context.Background())
	transport := &capturingTransport{}
	ctx := context.Background()

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	err = node.AddSubscription(ctx, "test-channel", Subscriber{Session: client, Ephemeral: false})
	if err != nil {
		t.Fatalf("AddSubscription() error = %v", err)
	}

	// Check that subscription was added
	count := node.Hub().NumSubscribers("test-channel")
	if count != 1 {
		t.Errorf("Channel should have 1 subscriber, got %d", count)
	}
}

func TestNode_AddSubscription_FirstSubscriber(t *testing.T) {
	node := NewNode(nil)
	_ = node.Run(context.Background())
	transport := &capturingTransport{}
	ctx := context.Background()

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	// First subscriber should trigger broker.Subscribe
	err = node.AddSubscription(ctx, "test-channel", Subscriber{Session: client, Ephemeral: false})
	if err != nil {
		t.Fatalf("AddSubscription() error = %v", err)
	}
}

func TestNode_RemoveSubscription(t *testing.T) {
	node := NewNode(nil)
	_ = node.Run(context.Background())
	transport := &capturingTransport{}
	ctx := context.Background()

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	// Add subscription
	_ = node.AddSubscription(ctx, "test-channel", Subscriber{Session: client, Ephemeral: false})

	// Remove subscription
	err = node.RemoveSubscription("test-channel", client)
	if err != nil {
		t.Fatalf("removeSubscription() error = %v", err)
	}

	// Check that subscription was removed
	count := node.Hub().NumSubscribers("test-channel")
	if count != 0 {
		t.Errorf("Channel should have 0 subscribers, got %d", count)
	}
}

func TestNode_SubLock(t *testing.T) {
	node := NewNode(nil)

	lock1 := node.subLock("test-channel-1")
	lock2 := node.subLock("test-channel-2")
	lock3 := node.subLock("test-channel-1") // Same as lock1

	if lock1 == nil {
		t.Error("subLock() should not return nil")
	}
	if lock2 == nil {
		t.Error("subLock() should not return nil")
	}
	if lock1 != lock3 {
		t.Error("subLock() should return same lock for same channel")
	}
	if lock1 == lock2 {
		t.Error("subLock() should return different locks for different channels (probabilistically)")
	}
}

func TestNode_SubLock_Distribution(t *testing.T) {
	node := NewNode(nil)

	// Test that different channels get distributed across locks
	lockCounts := make(map[*sync.Mutex]int)
	for i := 0; i < 1000; i++ {
		ch := string(rune('a' + i))
		lock := node.subLock(ch)
		lockCounts[lock]++
	}

	// With 1000 channels and 16384 locks, we expect good distribution
	// Check that we have at least some unique locks
	if len(lockCounts) < 10 {
		t.Errorf("Lock distribution seems poor, only %d unique locks for 1000 channels", len(lockCounts))
	}

	// Check that no single lock has too many channels
	maxCount := 0
	for _, count := range lockCounts {
		if count > maxCount {
			maxCount = count
		}
	}
	if maxCount > 500 {
		t.Errorf("One lock has %d channels, distribution may be poor", maxCount)
	}
}

func TestNode_SubLock_Concurrent(t *testing.T) {
	node := NewNode(nil)
	const numGoroutines = 100
	var wg sync.WaitGroup

	// Test concurrent access to subLock
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ch := string(rune('a' + (n % 10))) // Use only 10 distinct channels
			lock := node.subLock(ch)
			lock.Lock()
			time.Sleep(1 * time.Microsecond)
			lock.Unlock()
		}(i)
	}

	wg.Wait()
	// If we get here without deadlock, the test passes
}

func TestNode_FindProxy_NoProxy(t *testing.T) {
	node := NewNode(nil)

	p := node.FindProxy("test-channel", "test.method")
	if p != nil {
		t.Error("FindProxy() should return nil when no proxy configured")
	}
}

func TestNode_AddProxy(t *testing.T) {
	node := NewNode(nil)

	// Mock proxy
	mockProxy := &mockRPCProxy{}

	err := node.AddProxy(mockProxy, "test-channel", "test.method")
	if err != nil {
		t.Fatalf("AddProxy() error = %v", err)
	}

	p := node.FindProxy("test-channel", "test.method")
	if p == nil {
		t.Error("FindProxy() should return the added proxy")
	}
}

func TestNode_AddProxy_Wildcard(t *testing.T) {
	node := NewNode(nil)

	mockProxy := &mockRPCProxy{}

	err := node.AddProxy(mockProxy, "test.*", "test.*")
	if err != nil {
		t.Fatalf("AddProxy() error = %v", err)
	}

	// Should match
	p1 := node.FindProxy("test.channel1", "test.method1")
	if p1 == nil {
		t.Error("FindProxy() should match wildcard pattern")
	}

	p2 := node.FindProxy("test.channel2", "test.method2")
	if p2 == nil {
		t.Error("FindProxy() should match wildcard pattern for different channel/method")
	}
}

func TestNode_RPC_NoProxy(t *testing.T) {
	node := NewNode(nil)
	ctx := context.Background()

	req := &proxy.RPCProxyRequest{
		ID:        "req-1",
		ClientID:  "client-1",
		SessionID: "session-1",
		UserID:    "user-1",
		Channel:   "test-channel",
		Method:    "test.method",
	}

	_, err := node.ProxyRPC(ctx, "test-channel", "test.method", req)
	if err == nil {
		t.Error("RPC() should return error when no proxy configured")
	}
	if err.Error() != "no proxy found for channel/method" {
		t.Errorf("Error message = %v, want 'no proxy found for channel/method'", err)
	}
}

func TestNode_RPC_WithProxy(t *testing.T) {
	node := NewNode(nil)
	ctx := context.Background()

	s, _ := structpb.NewStruct(map[string]interface{}{"id": "response-1"})
	mockProxy := &mockRPCProxy{
		response: &proxy.RPCProxyResponse{
			Payload: &sharedv2.Payload{
				Data: &sharedv2.Payload_Json{
					Json: s,
				},
			},
		},
	}

	err := node.AddProxy(mockProxy, "test-channel", "test.method")
	if err != nil {
		t.Fatalf("AddProxy() error = %v", err)
	}

	req := &proxy.RPCProxyRequest{
		ID:        "req-1",
		ClientID:  "client-1",
		SessionID: "session-1",
		UserID:    "user-1",
		Channel:   "test-channel",
		Method:    "test.method",
	}

	resp, err := node.ProxyRPC(ctx, "test-channel", "test.method", req)
	if err != nil {
		t.Fatalf("RPC() error = %v", err)
	}
	if resp == nil {
		t.Fatal("RPC() should return response")
	}
	if resp.Payload == nil {
		t.Error("Response payload should not be nil")
	}
}

func TestNode_RPC_ProxyError(t *testing.T) {
	node := NewNode(nil)
	ctx := context.Background()

	mockProxy := &mockRPCProxy{
		err: errors.New("proxy error"),
	}

	err := node.AddProxy(mockProxy, "test-channel", "test.method")
	if err != nil {
		t.Fatalf("AddProxy() error = %v", err)
	}

	req := &proxy.RPCProxyRequest{
		ID:        "req-1",
		ClientID:  "client-1",
		SessionID: "session-1",
		UserID:    "user-1",
		Channel:   "test-channel",
		Method:    "test.method",
	}

	_, err = node.ProxyRPC(ctx, "test-channel", "test.method", req)
	if err == nil {
		t.Error("RPC() should return proxy error")
	}
	if err.Error() != "proxy error" {
		t.Errorf("Error = %v, want 'proxy error'", err)
	}
}

func TestNode_SetupProxy(t *testing.T) {
	t.Skip("Skipping TestNode_SetupProxy - requires actual server for gRPC connection")
}

func TestNode_SetupProxy_HTTP(t *testing.T) {
	t.Skip("Skipping TestNode_SetupProxy_HTTP - requires actual server for HTTP connection")
}

func TestNode_BrokerEventHandler(t *testing.T) {
	// Test that Node can be used as a broker publication handler
	node := NewNode(nil)
	if node == nil {
		t.Error("node should not be nil")
	}
}

func TestNode_ConcurrentPublish(t *testing.T) {
	node := NewNode(nil)
	_ = node.Run(context.Background())

	transport := &capturingTransport{}
	ctx := context.Background()

	// Add a client subscribed to the channel
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	client.MarkAuthenticated()
	require.NoError(t, client.Attach(client.Attachment()))

	_ = node.AddClient(client)
	_ = node.AddSubscription(ctx, "test-channel", Subscriber{Session: client, Ephemeral: false})

	// Clear transport messages from subscription
	transport.messages = nil

	const numPubs = 100
	var wg sync.WaitGroup

	for i := 0; i < numPubs; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			payload := []byte(string(rune('a' + (n % 26))))
			_, _ = node.Publish("test-channel", publishPub(payload, false))
		}(i)
	}

	wg.Wait()

	// All 100 publications land on the same channel's dispatch shard and are
	// delivered in order (delivery is asynchronous).
	waitMessageCount(t, transport, numPubs)
}

func TestNode_ConcurrentSubscriptions(t *testing.T) {
	node := NewNode(nil)
	_ = node.Run(context.Background())
	ctx := context.Background()

	const numSubs = 100
	var wg sync.WaitGroup

	for i := 0; i < numSubs; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			transport := &capturingTransport{}
			client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
			if err != nil {
				return
			}
			_ = node.AddSubscription(ctx, "test-channel", Subscriber{Session: client, Ephemeral: false})
		}(i)
	}

	wg.Wait()

	count := node.Hub().NumSubscribers("test-channel")
	if count != numSubs {
		t.Errorf("Channel should have %d subscribers, got %d", numSubs, count)
	}
}

func TestNode_MultipleChannels(t *testing.T) {
	node := NewNode(nil)
	_ = node.Run(context.Background())
	transport := &capturingTransport{}
	ctx := context.Background()

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	channels := []string{"channel-1", "channel-2", "channel-3"}
	for _, ch := range channels {
		err = node.AddSubscription(ctx, ch, Subscriber{Session: client, Ephemeral: false})
		if err != nil {
			t.Fatalf("AddSubscription() error for %s: %v", ch, err)
		}
	}

	for _, ch := range channels {
		count := node.Hub().NumSubscribers(ch)
		if count != 1 {
			t.Errorf("Channel %s should have 1 subscriber, got %d", ch, count)
		}
	}
}

func TestNode_Publish_MultipleChannels(t *testing.T) {
	node := NewNode(nil)
	_ = node.Run(context.Background())

	transport1 := &capturingTransport{}
	transport2 := &capturingTransport{}
	ctx := context.Background()

	client1, _, err := NewClient(ctx, node, transport1, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	client1.MarkAuthenticated()
	require.NoError(t, client1.Attach(client1.Attachment()))

	client2, _, err := NewClient(ctx, node, transport2, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	client2.MarkAuthenticated()
	require.NoError(t, client2.Attach(client2.Attachment()))

	_ = node.AddClient(client1)
	_ = node.AddClient(client2)
	_ = node.AddSubscription(ctx, "channel-1", Subscriber{Session: client1, Ephemeral: false})
	_ = node.AddSubscription(ctx, "channel-2", Subscriber{Session: client2, Ephemeral: false})

	// Clear transport messages from subscriptions
	transport1.messages = nil
	transport2.messages = nil

	// Publish to channel-1
	_, _ = node.Publish("channel-1", publishPub([]byte("payload-1"), false))

	// Only client1 should receive (delivery is asynchronous).
	waitMessageCount(t, transport1, 1)
	if transport2.getMessageCount() != 0 {
		t.Errorf("client2 should receive 0 messages, got %d", transport2.getMessageCount())
	}
}

// mockRPCProxy is a mock implementation of proxy.RPCProxy for testing
type mockRPCProxy struct {
	response *proxy.RPCProxyResponse
	err      error
}

func (m *mockRPCProxy) RPC(ctx context.Context, req *proxy.RPCProxyRequest) (*proxy.RPCProxyResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.response, nil
}

func (m *mockRPCProxy) Authenticate(ctx context.Context, req *proxy.AuthenticateProxyRequest) (*proxy.AuthenticateProxyResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &proxy.AuthenticateProxyResponse{}, nil
}

func (m *mockRPCProxy) SubscribeAcl(ctx context.Context, req *proxy.SubscribeAclProxyRequest) (*proxy.SubscribeAclProxyResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &proxy.SubscribeAclProxyResponse{}, nil
}

func (m *mockRPCProxy) PublishAcl(ctx context.Context, req *proxy.PublishAclProxyRequest) (*proxy.PublishAclProxyResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &proxy.PublishAclProxyResponse{}, nil
}

func (m *mockRPCProxy) OnConnected(ctx context.Context, req *proxy.OnConnectedProxyRequest) (*proxy.OnConnectedProxyResponse, error) {
	return &proxy.OnConnectedProxyResponse{}, nil
}

func (m *mockRPCProxy) OnSubscribed(ctx context.Context, req *proxy.OnSubscribedProxyRequest) (*proxy.OnSubscribedProxyResponse, error) {
	return &proxy.OnSubscribedProxyResponse{}, nil
}

func (m *mockRPCProxy) OnUnsubscribed(ctx context.Context, req *proxy.OnUnsubscribedProxyRequest) (*proxy.OnUnsubscribedProxyResponse, error) {
	return &proxy.OnUnsubscribedProxyResponse{}, nil
}

func (m *mockRPCProxy) OnDisconnected(ctx context.Context, req *proxy.OnDisconnectedProxyRequest) (*proxy.OnDisconnectedProxyResponse, error) {
	return &proxy.OnDisconnectedProxyResponse{}, nil
}

func (m *mockRPCProxy) Name() string {
	return "mock-proxy"
}

func (m *mockRPCProxy) Close() error {
	return nil
}

func BenchmarkNode_Publish(b *testing.B) {
	node := NewNode(nil)
	_ = node.Run(context.Background())

	transport := &capturingTransport{}
	ctx := context.Background()

	client, _, _ := NewClient(ctx, node, transport, JSONMarshaler{})
	client.MarkAuthenticated()
	_ = client.Attach(client.Attachment())

	_ = node.AddClient(client)
	_ = node.AddSubscription(ctx, "test-channel", Subscriber{Session: client, Ephemeral: false})

	payload := []byte("test payload")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = node.Publish("test-channel", publishPub(payload, false))
	}
}

func BenchmarkNode_AddSubscription(b *testing.B) {
	node := NewNode(nil)
	_ = node.Run(context.Background())
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		transport := &capturingTransport{}
		client, _, _ := NewClient(ctx, node, transport, JSONMarshaler{})
		_ = node.AddSubscription(ctx, "test-channel", Subscriber{Session: client, Ephemeral: false})
	}
}

func BenchmarkNode_SubLock(b *testing.B) {
	node := NewNode(nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ch := string(rune('a' + (i % 100)))
		_ = node.subLock(ch)
	}
}

// Task 13c: a failed AddClient (cluster sync error) must not count the
// connection in ConnectionsTotal.
func TestNode_AddClient_ClusterSyncFailure_NoGaugeIncrease(t *testing.T) {
	directory := &fakeSessionDirectory{}
	directory.casErr = errors.New("lease write failed")
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       &fakeClusterCommandBus{},
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	node := NewNode(nil)
	node.SetCluster(runtime)
	node.SetMetrics(metrics)

	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-metric", "user-metric", "client-metric")

	err = node.AddClient(client)
	require.Error(t, err)
	require.Equal(t, float64(0), testutil.ToFloat64(metrics.ConnectionsTotal.WithLabelValues("ws")),
		"failed AddClient must not count the connection")
}

// Task 13c: restoreLocalSubscription/removeLocalSubscriptionOnly must keep
// ActiveChannels consistent with normal subscriptions.
func TestNode_RestoreLocalSubscription_ActiveChannelsMetric(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	node := NewNode(nil)
	node.SetMetrics(metrics)

	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-metric2", "user-metric2", "client-metric2")

	require.NoError(t, node.restoreLocalSubscription(context.Background(), "news", NewSubscriber(client, false)))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.ActiveChannels),
		"restored subscription must count the channel")

	removed, err := node.removeLocalSubscriptionOnly("news", client, true)
	require.NoError(t, err)
	require.True(t, removed)
	require.Equal(t, float64(0), testutil.ToFloat64(metrics.ActiveChannels),
		"removing the last subscription must release the channel")
}

// Task 13c: PublishToSession (cluster publish command) must count
// MessagesDelivered like the hub broadcast path.
func TestNode_ClusterPublishCommand_MessagesDeliveredMetric(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	node := NewNode(nil)
	node.SetMetrics(metrics)
	_ = node.Run(context.Background())

	transport := &capturingTransport{}
	client, _, err := NewClient(context.Background(), node, transport, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-delivered", "user-delivered", "client-delivered")
	require.NoError(t, node.AddClient(client))

	msg := &clientpb.Message{Id: "m-1", Channel: "x", Payload: &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: "hi"}}}
	ok, err := node.PublishToSession(context.Background(), "sess-delivered", msg)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.MessagesDelivered))
}

// failTransientBroker fails every transient publish so presence events fail.
type failTransientBroker struct{}

func (failTransientBroker) Start(context.Context, PublicationHandler) error { return nil }
func (failTransientBroker) Subscribe(string) error                          { return nil }
func (failTransientBroker) Unsubscribe(string) error                        { return nil }
func (failTransientBroker) Publish(string, *Publication) (uint64, error)    { return 0, nil }
func (failTransientBroker) PublishOccupancy(string, OccupancyEvent) error   { return nil }
func (failTransientBroker) SetOccupancyHandler(OccupancyHandler) error      { return nil }
func (failTransientBroker) SetGapHandler(GapHandler)                        {}
func (failTransientBroker) PublishTransient(string, *Publication) error {
	return errors.New("injected transient failure")
}
func (failTransientBroker) History(string, uint64, int) (*HistoryPage, error) { return nil, nil }

// Task 13d follow-up (P1): presence publish failures must increment the
// PresencePublishFailures gauge, and successful publishes must not.
func TestNode_PublishPresenceFailure_IncrementsMetric(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	node := NewNode(nil)
	node.SetMetrics(metrics)
	node.SetBroker(failTransientBroker{})

	node.PublishPresenceJoin("chat", "c1", "u1")
	node.PublishPresenceLeave("chat", "c1", "u1")
	require.Equal(t, float64(2), testutil.ToFloat64(metrics.PresencePublishFailures),
		"failed presence publishes must be counted")

	// Successful publishes (memory broker, no handler) must not count.
	okReg := prometheus.NewRegistry()
	okMetrics := NewMetrics(okReg)
	okNode := NewNode(nil)
	okNode.SetMetrics(okMetrics)
	okNode.PublishPresenceJoin("chat", "c2", "u2")
	okNode.PublishPresenceLeave("chat", "c2", "u2")
	require.Equal(t, float64(0), testutil.ToFloat64(okMetrics.PresencePublishFailures))
}
