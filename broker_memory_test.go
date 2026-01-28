package messageloop

import (
	"sync"
	"testing"
	"time"
)

// mockBrokerEventHandler is a mock implementation of BrokerEventHandler for testing
type mockBrokerEventHandler struct {
	mu              sync.Mutex
	publications    []*Publication
	joins           []*mockEventInfo
	leaves          []*mockEventInfo
	handlePubError  error
	handleJoinError error
	handleLeaveError error
}

type mockEventInfo struct {
	channel string
	info    *ClientDesc
}

func (m *mockBrokerEventHandler) HandlePublication(ch string, pub *Publication) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.handlePubError != nil {
		return m.handlePubError
	}
	m.publications = append(m.publications, pub)
	return nil
}

func (m *mockBrokerEventHandler) HandleJoin(ch string, info *ClientDesc) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.handleJoinError != nil {
		return m.handleJoinError
	}
	m.joins = append(m.joins, &mockEventInfo{channel: ch, info: info})
	return nil
}

func (m *mockBrokerEventHandler) HandleLeave(ch string, info *ClientDesc) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.handleLeaveError != nil {
		return m.handleLeaveError
	}
	m.leaves = append(m.leaves, &mockEventInfo{channel: ch, info: info})
	return nil
}

func (m *mockBrokerEventHandler) getPublicationCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.publications)
}

func (m *mockBrokerEventHandler) getJoinCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.joins)
}

func (m *mockBrokerEventHandler) getLeaveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.leaves)
}

func (m *mockBrokerEventHandler) getLastPublication() *Publication {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.publications) == 0 {
		return nil
	}
	return m.publications[len(m.publications)-1]
}

func TestNewMemoryBroker(t *testing.T) {
	broker := NewMemoryBroker()
	if broker == nil {
		t.Fatal("NewMemoryBroker() should not return nil")
	}

	_, ok := broker.(*memoryBroker)
	if !ok {
		t.Error("NewMemoryBroker() should return *memoryBroker type")
	}
}

func TestMemoryBroker_RegisterEventHandler(t *testing.T) {
	broker := newMemoryBroker(nil)
	handler := &mockBrokerEventHandler{}

	err := broker.RegisterEventHandler(handler)
	if err != nil {
		t.Fatalf("RegisterEventHandler() error = %v", err)
	}

	if broker.eventHandler != handler {
		t.Error("eventHandler should be set to the registered handler")
	}
}

func TestMemoryBroker_Subscribe(t *testing.T) {
	broker := newMemoryBroker(nil)
	err := broker.Subscribe("test-channel")
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
}

func TestMemoryBroker_Unsubscribe(t *testing.T) {
	broker := newMemoryBroker(nil)
	err := broker.Unsubscribe("test-channel")
	if err != nil {
		t.Fatalf("Unsubscribe() error = %v", err)
	}
}

func TestMemoryBroker_Publish(t *testing.T) {
	broker := newMemoryBroker(nil)
	handler := &mockBrokerEventHandler{}
	broker.RegisterEventHandler(handler)

	payload := []byte("test payload")
	opts := PublishOptions{
		AsBytes: true,
	}

	pos, suppressed, err := broker.Publish("test-channel", payload, opts)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	if suppressed {
		t.Error("Publish() should not be suppressed")
	}

	// Check StreamPosition
	if pos.Offset != 0 {
		t.Errorf("Offset = %d, want 0", pos.Offset)
	}
	if pos.Epoch != "" {
		t.Errorf("Epoch = %s, want empty string", pos.Epoch)
	}

	if handler.getPublicationCount() != 1 {
		t.Errorf("handler received %d publications, want 1", handler.getPublicationCount())
	}

	pub := handler.getLastPublication()
	if pub.Channel != "test-channel" {
		t.Errorf("Channel = %s, want test-channel", pub.Channel)
	}
	if string(pub.Payload) != string(payload) {
		t.Errorf("Payload = %s, want %s", string(pub.Payload), string(payload))
	}
	if !pub.IsBlob {
		t.Error("IsBlob should be true when AsBytes option is set")
	}
}

func TestMemoryBroker_Publish_WithoutAsBytes(t *testing.T) {
	broker := newMemoryBroker(nil)
	handler := &mockBrokerEventHandler{}
	broker.RegisterEventHandler(handler)

	payload := []byte("test payload")
	opts := PublishOptions{} // AsBytes not set

	_, _, err := broker.Publish("test-channel", payload, opts)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	pub := handler.getLastPublication()
	if pub.IsBlob {
		t.Error("IsBlob should be false when AsBytes option is not set")
	}
}

func TestMemoryBroker_Publish_WithClientDesc(t *testing.T) {
	broker := newMemoryBroker(nil)
	handler := &mockBrokerEventHandler{}
	broker.RegisterEventHandler(handler)

	clientDesc := &ClientDesc{
		ClientID:  "client-1",
		SessionID: "session-1",
		UserID:    "user-1",
	}

	opts := PublishOptions{
		ClientDesc: clientDesc,
	}

	_, _, err := broker.Publish("test-channel", []byte("payload"), opts)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	// Note: memoryBroker doesn't set the ClientDesc in the Publication
	// The option is stored but not used in memory broker
}

func TestMemoryBroker_Publish_NoEventHandler(t *testing.T) {
	broker := newMemoryBroker(nil)
	// Don't register an event handler

	// The memory broker will panic if Publish is called without an event handler
	// This is a documented behavior - always register an event handler before using
	defer func() {
		if r := recover(); r == nil {
			t.Error("Publish() should panic when no event handler registered")
		}
	}()
	_, _, _ = broker.Publish("test-channel", []byte("payload"), PublishOptions{})
}

func TestMemoryBroker_Publish_HandlerError(t *testing.T) {
	broker := newMemoryBroker(nil)
	handler := &mockBrokerEventHandler{
		handlePubError: ErrBadRequest,
	}
	broker.RegisterEventHandler(handler)

	_, _, err := broker.Publish("test-channel", []byte("payload"), PublishOptions{})
	if err != ErrBadRequest {
		t.Errorf("Publish() error = %v, want %v", err, ErrBadRequest)
	}
}

func TestMemoryBroker_PublishJoin(t *testing.T) {
	broker := newMemoryBroker(nil)
	handler := &mockBrokerEventHandler{}
	broker.RegisterEventHandler(handler)

	info := &ClientDesc{
		ClientID:  "client-1",
		SessionID: "session-1",
		UserID:    "user-1",
	}

	err := broker.PublishJoin("test-channel", info)
	if err != nil {
		t.Fatalf("PublishJoin() error = %v", err)
	}

	// Memory broker no-ops on PublishJoin
	if handler.getJoinCount() != 0 {
		t.Errorf("Memory broker should not trigger joins, got %d", handler.getJoinCount())
	}
}

func TestMemoryBroker_PublishLeave(t *testing.T) {
	broker := newMemoryBroker(nil)
	handler := &mockBrokerEventHandler{}
	broker.RegisterEventHandler(handler)

	info := &ClientDesc{
		ClientID:  "client-1",
		SessionID: "session-1",
		UserID:    "user-1",
	}

	err := broker.PublishLeave("test-channel", info)
	if err != nil {
		t.Fatalf("PublishLeave() error = %v", err)
	}

	// Memory broker no-ops on PublishLeave
	if handler.getLeaveCount() != 0 {
		t.Errorf("Memory broker should not trigger leaves, got %d", handler.getLeaveCount())
	}
}

func TestMemoryBroker_History(t *testing.T) {
	broker := newMemoryBroker(nil)

	opts := HistoryOptions{}

	pubs, pos, err := broker.History("test-channel", opts)
	if err != nil {
		t.Fatalf("History() error = %v", err)
	}

	if pubs != nil {
		t.Error("History() should return nil publications for memory broker")
	}

	if pos.Offset != 0 {
		t.Errorf("Offset = %d, want 0", pos.Offset)
	}
}

func TestMemoryBroker_RemoveHistory(t *testing.T) {
	broker := newMemoryBroker(nil)

	err := broker.RemoveHistory("test-channel")
	if err != nil {
		t.Fatalf("RemoveHistory() error = %v", err)
	}
}

func TestMemoryBroker_Publish_TimeSet(t *testing.T) {
	broker := newMemoryBroker(nil)
	handler := &mockBrokerEventHandler{}
	broker.RegisterEventHandler(handler)

	before := time.Now().UnixMilli()

	_, _, err := broker.Publish("test-channel", []byte("payload"), PublishOptions{})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	after := time.Now().UnixMilli()

	pub := handler.getLastPublication()
	if pub.Time < before || pub.Time > after {
		t.Errorf("Time = %d, want between %d and %d", pub.Time, before, after)
	}
}

func TestMemoryBroker_BrokerInterface(t *testing.T) {
	// Verify that memoryBroker implements the Broker interface
	var _ Broker = new(memoryBroker)
}

func TestNewMemoryBroker_WithNode(t *testing.T) {
	node := NewNode(nil)
	broker := newMemoryBroker(node)

	if broker.node != node {
		t.Error("broker.node should be set to the provided node")
	}
}

func TestMemoryBroker_MultiplePublish(t *testing.T) {
	broker := newMemoryBroker(nil)
	handler := &mockBrokerEventHandler{}
	broker.RegisterEventHandler(handler)

	const numPubs = 10
	for i := 0; i < numPubs; i++ {
		payload := []byte(string(rune('a' + i)))
		_, _, err := broker.Publish("test-channel", payload, PublishOptions{})
		if err != nil {
			t.Fatalf("Publish() error = %v", err)
		}
	}

	if handler.getPublicationCount() != numPubs {
		t.Errorf("handler received %d publications, want %d", handler.getPublicationCount(), numPubs)
	}
}

func TestMemoryBroker_ConcurrentPublish(t *testing.T) {
	broker := newMemoryBroker(nil)
	handler := &mockBrokerEventHandler{}
	broker.RegisterEventHandler(handler)

	const numPubs = 100
	var wg sync.WaitGroup

	for i := 0; i < numPubs; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			payload := []byte(string(rune('a' + (n % 26))))
			_, _, _ = broker.Publish("test-channel", payload, PublishOptions{})
		}(i)
	}

	wg.Wait()

	count := handler.getPublicationCount()
	if count != numPubs {
		t.Errorf("handler received %d publications, want %d", count, numPubs)
	}
}

func TestMemoryBroker_Publish_EmptyPayload(t *testing.T) {
	broker := newMemoryBroker(nil)
	handler := &mockBrokerEventHandler{}
	broker.RegisterEventHandler(handler)

	_, _, err := broker.Publish("test-channel", []byte{}, PublishOptions{})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	pub := handler.getLastPublication()
	if len(pub.Payload) != 0 {
		t.Errorf("Payload = %v, want empty", pub.Payload)
	}
}

func TestMemoryBroker_Publish_NilPayload(t *testing.T) {
	broker := newMemoryBroker(nil)
	handler := &mockBrokerEventHandler{}
	broker.RegisterEventHandler(handler)

	_, _, err := broker.Publish("test-channel", nil, PublishOptions{})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	pub := handler.getLastPublication()
	if len(pub.Payload) != 0 {
		t.Errorf("Payload = %v, want empty/nil", pub.Payload)
	}
}

func TestMemoryBroker_MultipleChannels(t *testing.T) {
	broker := newMemoryBroker(nil)
	handler := &mockBrokerEventHandler{}
	broker.RegisterEventHandler(handler)

	channels := []string{"channel-1", "channel-2", "channel-3"}
	for _, ch := range channels {
		_, _, err := broker.Publish(ch, []byte("payload for "+ch), PublishOptions{})
		if err != nil {
			t.Fatalf("Publish() error for %s: %v", ch, err)
		}
	}

	if handler.getPublicationCount() != len(channels) {
		t.Errorf("handler received %d publications, want %d", handler.getPublicationCount(), len(channels))
	}
}

func TestMemoryBroker_StreamPosition_Default(t *testing.T) {
	broker := newMemoryBroker(nil)
	handler := &mockBrokerEventHandler{}
	broker.RegisterEventHandler(handler)

	pos, _, _ := broker.Publish("test-channel", []byte("payload"), PublishOptions{})

	if pos.Offset != 0 {
		t.Errorf("Offset = %d, want 0 (memory broker doesn't track offset)", pos.Offset)
	}
	if pos.Epoch != "" {
		t.Errorf("Epoch = %s, want empty string", pos.Epoch)
	}
}

// Export error for testing
var (
	ErrBadRequest = DisconnectBadRequest
)

func TestPublishOptions_WithClientDesc(t *testing.T) {
	info := &ClientDesc{
		ClientID:  "test-client",
		SessionID: "test-session",
		UserID:    "test-user",
	}

	opts := PublishOptions{}
	WithClientDesc(info)(&opts)

	if opts.ClientDesc != info {
		t.Error("WithClientDesc() should set ClientDesc")
	}
}

func TestPublishOptions_WithAsBytes(t *testing.T) {
	opts := PublishOptions{}
	WithAsBytes(true)(&opts)

	if !opts.AsBytes {
		t.Error("WithAsBytes() should set AsBytes to true")
	}

	WithAsBytes(false)(&opts)
	if opts.AsBytes {
		t.Error("WithAsBytes(false) should set AsBytes to false")
	}
}

func TestPublishOptions_Compose(t *testing.T) {
	info := &ClientDesc{
		ClientID:  "test-client",
		SessionID: "test-session",
		UserID:    "test-user",
	}

	opts := PublishOptions{}
	WithClientDesc(info)(&opts)
	WithAsBytes(true)(&opts)

	if opts.ClientDesc != info {
		t.Error("WithClientDesc() should set ClientDesc")
	}
	if !opts.AsBytes {
		t.Error("WithAsBytes() should set AsBytes to true")
	}
}

func TestHistoryOptions_Default(t *testing.T) {
	opts := HistoryOptions{}

	if opts.Filter.Limit != 0 {
		t.Errorf("Filter.Limit = %d, want 0", opts.Filter.Limit)
	}
	if opts.Filter.Reverse {
		t.Error("Filter.Reverse should be false by default")
	}
	if opts.MetaTTL != 0 {
		t.Errorf("MetaTTL = %v, want 0", opts.MetaTTL)
	}
}

func TestHistoryFilter_Since(t *testing.T) {
	pos := StreamPosition{
		Offset: 100,
		Epoch:  "test-epoch",
	}

	filter := HistoryFilter{
		Since:  &pos,
		Limit:  10,
		Reverse: false,
	}

	if filter.Since == nil {
		t.Error("Since should be set")
	}
	if filter.Since.Offset != 100 {
		t.Errorf("Since.Offset = %d, want 100", filter.Since.Offset)
	}
	if filter.Limit != 10 {
		t.Errorf("Limit = %d, want 10", filter.Limit)
	}
}

func TestStreamPosition_Default(t *testing.T) {
	var pos StreamPosition

	if pos.Offset != 0 {
		t.Errorf("Offset = %d, want 0", pos.Offset)
	}
	if pos.Epoch != "" {
		t.Errorf("Epoch = %s, want empty string", pos.Epoch)
	}
}

func TestPublication_Default(t *testing.T) {
	pub := &Publication{}

	if pub.Channel != "" {
		t.Errorf("Channel = %s, want empty", pub.Channel)
	}
	if pub.Offset != 0 {
		t.Errorf("Offset = %d, want 0", pub.Offset)
	}
	if pub.Metadata != nil {
		t.Error("Metadata should be nil by default")
	}
	if pub.IsBlob {
		t.Error("IsBlob should be false by default")
	}
	if pub.Payload != nil {
		t.Error("Payload should be nil by default")
	}
	if pub.Time != 0 {
		t.Error("Time should be 0 by default (not set)")
	}
}

func TestMemoryBroker_IntegrationWithNode(t *testing.T) {
	node := NewNode(nil)
	broker := node.Broker()

	if broker == nil {
		t.Fatal("Node should have a broker")
	}

	// The default broker should be a memoryBroker
	_, ok := broker.(*memoryBroker)
	if !ok {
		t.Error("Node's default broker should be memoryBroker")
	}
}

func TestMemoryBroker_Run(t *testing.T) {
	node := NewNode(nil)
	err := node.Run()
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// After run, the broker should have an event handler
	broker := node.Broker().(*memoryBroker)
	if broker.eventHandler == nil {
		t.Error("Broker should have an event handler after Run()")
	}
}

func BenchmarkMemoryBroker_Publish(b *testing.B) {
	broker := newMemoryBroker(nil)
	handler := &mockBrokerEventHandler{}
	broker.RegisterEventHandler(handler)

	payload := []byte("test payload")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		broker.Publish("test-channel", payload, PublishOptions{})
	}
}

func BenchmarkMemoryBroker_ConcurrentPublish(b *testing.B) {
	broker := newMemoryBroker(nil)
	handler := &mockBrokerEventHandler{}
	broker.RegisterEventHandler(handler)

	payload := []byte("test payload")

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			broker.Publish("test-channel", payload, PublishOptions{})
			i++
		}
	})
}
