package messageloop

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/messageloopio/messageloop/proxy"
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
	if node.subLocks == nil {
		t.Error("subLocks should be initialized")
	}
	if len(node.subLocks) != numSubLocks {
		t.Errorf("len(subLocks) = %d, want %d", len(node.subLocks), numSubLocks)
	}
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
	newBroker := NewMemoryBroker()

	node.SetBroker(newBroker)

	if node.broker != newBroker {
		t.Error("SetBroker() should set the broker")
	}
}

func TestNode_Run(t *testing.T) {
	node := NewNode(nil)
	err := node.Run()
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// After Run, the broker should have an event handler
	memBroker := node.broker.(*memoryBroker)
	if memBroker.eventHandler == nil {
		t.Error("Broker should have event handler after Run()")
	}
}

func TestNode_HandlePublication(t *testing.T) {
	node := NewNode(nil)
	transport := &capturingTransport{}
	ctx := context.Background()

	// Add a client subscribed to the channel
	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
	}
	client.mu.Lock()
	client.authenticated = true
	client.mu.Unlock()

	node.AddClient(client)
	node.AddSubscription(ctx, "test-channel", Subscriber{Client:client, Ephemeral: false})

	pub := &Publication{
		Channel: "test-channel",
		Offset:  1,
		Payload: []byte("test payload"),
		Time:    time.Now().UnixMilli(),
	}

	err = node.HandlePublication("test-channel", pub)
	if err != nil {
		t.Fatalf("HandlePublication() error = %v", err)
	}

	// Client should receive a message
	if transport.getMessageCount() != 1 {
		t.Errorf("Client should receive 1 message, got %d", transport.getMessageCount())
	}
}

func TestNode_HandlePublication_NoSubscribers(t *testing.T) {
	node := NewNode(nil)

	pub := &Publication{
		Channel: "empty-channel",
		Offset:  1,
		Payload: []byte("test payload"),
		Time:    time.Now().UnixMilli(),
	}

	err := node.HandlePublication("empty-channel", pub)
	if err != nil {
		t.Fatalf("HandlePublication() error = %v", err)
	}
}

func TestNode_HandleJoin(t *testing.T) {
	node := NewNode(nil)
	info := &ClientDesc{
		ClientID:  "client-1",
		SessionID: "session-1",
		UserID:    "user-1",
	}

	err := node.HandleJoin("test-channel", info)
	// Memory broker doesn't handle joins, so this should be a no-op
	if err != nil {
		t.Fatalf("HandleJoin() error = %v", err)
	}
}

func TestNode_HandleLeave(t *testing.T) {
	node := NewNode(nil)
	info := &ClientDesc{
		ClientID:  "client-1",
		SessionID: "session-1",
		UserID:    "user-1",
	}

	err := node.HandleLeave("test-channel", info)
	// Memory broker doesn't handle leaves, so this should be a no-op
	if err != nil {
		t.Fatalf("HandleLeave() error = %v", err)
	}
}

func TestNode_Publish(t *testing.T) {
	node := NewNode(nil)
	_ = node.Run() // Register event handler

	transport := &capturingTransport{}
	ctx := context.Background()

	// Add a client subscribed to the channel
	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
	}
	client.mu.Lock()
	client.authenticated = true
	client.mu.Unlock()

	node.AddClient(client)
	node.AddSubscription(ctx, "test-channel", Subscriber{Client:client, Ephemeral: false})

	// Clear transport messages from subscription
	transport.messages = nil

	err = node.Publish("test-channel", []byte("test payload"))
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	// Client should receive a message
	if transport.getMessageCount() != 1 {
		t.Errorf("Client should receive 1 message, got %d", transport.getMessageCount())
	}
}

func TestNode_Publish_WithOptions(t *testing.T) {
	node := NewNode(nil)
	_ = node.Run()

	err := node.Publish("test-channel", []byte("test payload"),
		WithClientDesc(&ClientDesc{
			ClientID:  "client-1",
			SessionID: "session-1",
			UserID:    "user-1",
		}),
		WithAsBytes(true),
	)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
}

func TestNode_AddClient(t *testing.T) {
	node := NewNode(nil)
	transport := &capturingTransport{}
	ctx := context.Background()

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
	}

	node.AddClient(client)

	// Check that client was added to hub
	hub := node.Hub()
	hub.mu.RLock()
	_, exists := hub.sessions[client.SessionID()]
	hub.mu.RUnlock()

	if !exists {
		t.Error("Client should be added to hub sessions")
	}
}

func TestNode_AddSubscription(t *testing.T) {
	node := NewNode(nil)
	_ = node.Run() // Register event handler
	transport := &capturingTransport{}
	ctx := context.Background()

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
	}

	err = node.AddSubscription(ctx, "test-channel", Subscriber{Client:client, Ephemeral: false})
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
	_ = node.Run()
	transport := &capturingTransport{}
	ctx := context.Background()

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
	}

	// First subscriber should trigger broker.Subscribe
	err = node.AddSubscription(ctx, "test-channel", Subscriber{Client:client, Ephemeral: false})
	if err != nil {
		t.Fatalf("AddSubscription() error = %v", err)
	}
}

func TestNode_RemoveSubscription(t *testing.T) {
	node := NewNode(nil)
	_ = node.Run()
	transport := &capturingTransport{}
	ctx := context.Background()

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
	}

	// Add subscription
	_ = node.AddSubscription(ctx, "test-channel", Subscriber{Client:client, Ephemeral: false})

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

	mockProxy := &mockRPCProxy{
		response: &proxy.RPCProxyResponse{
			Event: func() *cloudevents.Event {
				e := cloudevents.NewEvent()
				e.SetID("response-1")
				return &e
			}(),
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
		t.Error("RPC() should return response")
	}
	if resp.Event.ID() != "response-1" {
		t.Errorf("Response event ID = %s, want response-1", resp.Event.ID())
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
	// Test that Node implements BrokerEventHandler
	var _ BrokerEventHandler = new(Node)
}

func TestNode_ConcurrentPublish(t *testing.T) {
	node := NewNode(nil)
	_ = node.Run()

	transport := &capturingTransport{}
	ctx := context.Background()

	// Add a client subscribed to the channel
	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
	}
	client.mu.Lock()
	client.authenticated = true
	client.mu.Unlock()

	node.AddClient(client)
	node.AddSubscription(ctx, "test-channel", Subscriber{Client:client, Ephemeral: false})

	// Clear transport messages from subscription
	transport.messages = nil

	const numPubs = 100
	var wg sync.WaitGroup

	for i := 0; i < numPubs; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			payload := []byte(string(rune('a' + (n % 26))))
			_ = node.Publish("test-channel", payload)
		}(i)
	}

	wg.Wait()

	// Client should receive messages (may be less due to race conditions, but should be close)
	count := transport.getMessageCount()
	if count < numPubs/2 {
		t.Errorf("Client should receive at least half of %d messages, got %d", numPubs, count)
	}
}

func TestNode_ConcurrentSubscriptions(t *testing.T) {
	node := NewNode(nil)
	_ = node.Run()
	ctx := context.Background()

	const numSubs = 100
	var wg sync.WaitGroup

	for i := 0; i < numSubs; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			transport := &capturingTransport{}
			client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
			if err != nil {
				return
			}
			_ = node.AddSubscription(ctx, "test-channel", Subscriber{Client:client, Ephemeral: false})
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
	_ = node.Run()
	transport := &capturingTransport{}
	ctx := context.Background()

	client, _, err := NewClientSession(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
	}

	channels := []string{"channel-1", "channel-2", "channel-3"}
	for _, ch := range channels {
		err = node.AddSubscription(ctx, ch, Subscriber{Client:client, Ephemeral: false})
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
	_ = node.Run()

	transport1 := &capturingTransport{}
	transport2 := &capturingTransport{}
	ctx := context.Background()

	client1, _, err := NewClientSession(ctx, node, transport1, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
	}
	client1.mu.Lock()
	client1.authenticated = true
	client1.mu.Unlock()

	client2, _, err := NewClientSession(ctx, node, transport2, JSONMarshaler{})
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
	}
	client2.mu.Lock()
	client2.authenticated = true
	client2.mu.Unlock()

	node.addClient(client1)
	node.addClient(client2)
	node.AddSubscription(ctx, "channel-1", Subscriber{Client:client1, Ephemeral: false})
	node.AddSubscription(ctx, "channel-2", Subscriber{Client:client2, Ephemeral: false})

	// Clear transport messages from subscriptions
	transport1.messages = nil
	transport2.messages = nil

	// Publish to channel-1
	_ = node.Publish("channel-1", []byte("payload-1"))

	// Only client1 should receive
	if transport1.getMessageCount() != 1 {
		t.Errorf("client1 should receive 1 message, got %d", transport1.getMessageCount())
	}
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
	_ = node.Run()

	transport := &capturingTransport{}
	ctx := context.Background()

	client, _, _ := NewClientSession(ctx, node, transport, JSONMarshaler{})
	client.mu.Lock()
	client.authenticated = true
	client.mu.Unlock()

	node.AddClient(client)
	node.AddSubscription(ctx, "test-channel", Subscriber{Client:client, Ephemeral: false})

	payload := []byte("test payload")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		node.Publish("test-channel", payload)
	}
}

func BenchmarkNode_AddSubscription(b *testing.B) {
	node := NewNode(nil)
	_ = node.Run()
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		transport := &capturingTransport{}
		client, _, _ := NewClientSession(ctx, node, transport, JSONMarshaler{})
		node.AddSubscription(ctx, "test-channel", Subscriber{Client:client, Ephemeral: false})
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
