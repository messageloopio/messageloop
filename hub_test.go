package messageloop

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockTransport is a mock implementation of Transport for testing
type mockTransport struct {
	mu          sync.Mutex
	closed      bool
	messages    [][]byte
	closeCount  int
	closeReason Disconnect
	sendErr     error
}

func (m *mockTransport) Write(data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("transport closed")
	}
	if m.sendErr != nil {
		return m.sendErr
	}
	m.messages = append(m.messages, data)
	return nil
}

func (m *mockTransport) WriteMany(data ...[]byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("transport closed")
	}
	m.messages = append(m.messages, data...)
	return nil
}

func (m *mockTransport) Close(disconnect Disconnect) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.closeCount++
	m.closeReason = disconnect
	return nil
}

func (m *mockTransport) RemoteAddr() string {
	return "127.0.0.1:12345"
}

func (m *mockTransport) getMessageCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.messages)
}

func (m *mockTransport) getMessage(i int) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i < 0 || i >= len(m.messages) {
		return nil
	}
	return m.messages[i]
}

func newTestClient(t *testing.T, sessionID, userID string) *Client {
	return newTestClientWithTransport(t, sessionID, userID, &mockTransport{})
}

func newTestClientWithTransport(t *testing.T, sessionID, userID string, transport Transport) *Client {
	ctx := context.Background()
	node := NewNode(nil)
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	// Set the user and client fields for testing
	client.mu.Lock()
	client.session = sessionID
	client.user = userID
	client.client = "client-" + sessionID
	client.mu.Unlock()
	return client
}

func TestNewHub(t *testing.T) {
	h := newHub(0, 0)
	if h == nil {
		t.Fatal("newHub() should not return nil")
	}
	if len(h.connShards) != numHubShards {
		t.Errorf("len(connShards) = %d, want %d", len(h.connShards), numHubShards)
	}
	if len(h.subShards) != numHubShards {
		t.Errorf("len(subShards) = %d, want %d", len(h.subShards), numHubShards)
	}
	if h.sessions == nil {
		t.Error("sessions map should be initialized")
	}
}

func TestNewConnShard(t *testing.T) {
	shard := newConnShard()
	if shard == nil {
		t.Fatal("newConnShard() should not return nil")
	}
	if shard.clients == nil {
		t.Error("clients map should be initialized")
	}
	if shard.users == nil {
		t.Error("users map should be initialized")
	}
}

func TestNewSubShard(t *testing.T) {
	const maxTimeLag = 1000
	shard := newSubShard(maxTimeLag)
	if shard == nil {
		t.Fatal("newSubShard() should not return nil")
	}
	if shard.subs == nil {
		t.Error("subs map should be initialized")
	}
	if shard.maxTimeLagMilli != maxTimeLag {
		t.Errorf("maxTimeLagMilli = %d, want %d", shard.maxTimeLagMilli, maxTimeLag)
	}
}

func TestConnShard_Add(t *testing.T) {
	shard := newConnShard()
	client := newTestClient(t, "session-1", "user-1")

	_ = shard.addWithLimit(client, 0)

	shard.mu.RLock()
	defer shard.mu.RUnlock()

	if len(shard.clients) != 1 {
		t.Errorf("len(clients) = %d, want 1", len(shard.clients))
	}
	if _, ok := shard.clients["session-1"]; !ok {
		t.Error("client session-1 should be in clients map")
	}
	if len(shard.users) != 1 {
		t.Errorf("len(users) = %d, want 1", len(shard.users))
	}
	if _, ok := shard.users["user-1"]; !ok {
		t.Error("user-1 should be in users map")
	}
	if _, ok := shard.users["user-1"]["session-1"]; !ok {
		t.Error("session-1 should be in user-1's session set")
	}
}

func TestConnShard_Add_MultipleSessionsSameUser(t *testing.T) {
	shard := newConnShard()
	client1 := newTestClient(t, "session-1", "user-1")
	client2 := newTestClient(t, "session-2", "user-1")

	_ = shard.addWithLimit(client1, 0)
	_ = shard.addWithLimit(client2, 0)

	shard.mu.RLock()
	defer shard.mu.RUnlock()

	if len(shard.clients) != 2 {
		t.Errorf("len(clients) = %d, want 2", len(shard.clients))
	}
	if len(shard.users) != 1 {
		t.Errorf("len(users) = %d, want 1", len(shard.users))
	}
	if len(shard.users["user-1"]) != 2 {
		t.Errorf("user-1 should have 2 sessions, got %d", len(shard.users["user-1"]))
	}
}

func TestConnShard_Add_MultipleUsers(t *testing.T) {
	shard := newConnShard()
	client1 := newTestClient(t, "session-1", "user-1")
	client2 := newTestClient(t, "session-2", "user-2")

	_ = shard.addWithLimit(client1, 0)
	_ = shard.addWithLimit(client2, 0)

	shard.mu.RLock()
	defer shard.mu.RUnlock()

	if len(shard.clients) != 2 {
		t.Errorf("len(clients) = %d, want 2", len(shard.clients))
	}
	if len(shard.users) != 2 {
		t.Errorf("len(users) = %d, want 2", len(shard.users))
	}
}

func TestSubShard_NumSubscribers_Empty(t *testing.T) {
	shard := newSubShard(0)
	count := shard.NumSubscribers("test-channel")
	if count != 0 {
		t.Errorf("NumSubscribers() = %d, want 0", count)
	}
}

func TestSubShard_NumSubscribers(t *testing.T) {
	shard := newSubShard(0)
	client1 := newTestClient(t, "session-1", "user-1")
	client2 := newTestClient(t, "session-2", "user-2")

	_, _ = shard.addSub("test-channel", Subscriber{Client: client1, Ephemeral: false})
	_, _ = shard.addSub("test-channel", Subscriber{Client: client2, Ephemeral: true})

	count := shard.NumSubscribers("test-channel")
	if count != 2 {
		t.Errorf("NumSubscribers() = %d, want 2", count)
	}
}

func TestSubShard_AddSub_NewChannel(t *testing.T) {
	shard := newSubShard(0)
	client := newTestClient(t, "session-1", "user-1")

	first, err := shard.addSub("test-channel", Subscriber{Client: client, Ephemeral: false})
	if err != nil {
		t.Fatalf("addSub() error = %v", err)
	}
	if !first {
		t.Error("addSub() should return true for first subscriber")
	}

	count := shard.NumSubscribers("test-channel")
	if count != 1 {
		t.Errorf("NumSubscribers() = %d, want 1", count)
	}
}

func TestSubShard_AddSub_ExistingChannel(t *testing.T) {
	shard := newSubShard(0)
	client1 := newTestClient(t, "session-1", "user-1")
	client2 := newTestClient(t, "session-2", "user-2")

	_, _ = shard.addSub("test-channel", Subscriber{Client: client1, Ephemeral: false})

	first, err := shard.addSub("test-channel", Subscriber{Client: client2, Ephemeral: false})
	if err != nil {
		t.Fatalf("addSub() error = %v", err)
	}
	if first {
		t.Error("addSub() should return false for subsequent subscribers")
	}

	count := shard.NumSubscribers("test-channel")
	if count != 2 {
		t.Errorf("NumSubscribers() = %d, want 2", count)
	}
}

func TestSubShard_RemoveSub_NotFound(t *testing.T) {
	shard := newSubShard(0)
	client := newTestClient(t, "session-1", "user-1")

	empty, found := shard.removeSub("test-channel", client)
	if !empty {
		t.Error("removeSub() should return true for empty (first return value)")
	}
	if found {
		t.Error("removeSub() should return false for not found (second return value)")
	}
}

func TestSubShard_RemoveSub_Success(t *testing.T) {
	shard := newSubShard(0)
	client := newTestClient(t, "session-1", "user-1")

	_, _ = shard.addSub("test-channel", Subscriber{Client: client, Ephemeral: false})

	// Remove the subscription
	empty, found := shard.removeSub("test-channel", client)
	if !empty {
		t.Error("removeSub() should return true for empty after removing last subscriber")
	}
	if !found {
		t.Error("removeSub() should return true for found")
	}

	// Verify channel was removed
	count := shard.NumSubscribers("test-channel")
	if count != 0 {
		t.Errorf("NumSubscribers() = %d, want 0 after removal", count)
	}
}

func TestSubShard_RemoveSub_OneOfMany(t *testing.T) {
	shard := newSubShard(0)
	client1 := newTestClient(t, "session-1", "user-1")
	client2 := newTestClient(t, "session-2", "user-2")

	_, _ = shard.addSub("test-channel", Subscriber{Client: client1, Ephemeral: false})
	_, _ = shard.addSub("test-channel", Subscriber{Client: client2, Ephemeral: false})

	// Remove one subscription
	empty, found := shard.removeSub("test-channel", client1)
	if empty {
		t.Error("removeSub() should return false for empty when other subscribers remain")
	}
	if !found {
		t.Error("removeSub() should return true for found")
	}

	count := shard.NumSubscribers("test-channel")
	if count != 1 {
		t.Errorf("NumSubscribers() = %d, want 1", count)
	}
}

func TestHub_Add(t *testing.T) {
	h := newHub(0, 0)
	client := newTestClient(t, "session-1", "user-1")

	_ = h.add(client)

	h.mu.RLock()
	if len(h.sessions) != 1 {
		h.mu.RUnlock()
		t.Errorf("len(sessions) = %d, want 1", len(h.sessions))
	}
	if _, ok := h.sessions["session-1"]; !ok {
		h.mu.RUnlock()
		t.Error("session-1 should be in sessions map")
	}
	h.mu.RUnlock()

	// Check connShard
	shardIdx := index("user-1", numHubShards)
	shard := h.connShards[shardIdx]
	shard.mu.RLock()
	if len(shard.clients) != 1 {
		shard.mu.RUnlock()
		t.Errorf("len(connShard.clients) = %d, want 1", len(shard.clients))
	}
	shard.mu.RUnlock()
}

func TestHub_NumSubscribers(t *testing.T) {
	h := newHub(0, 0)
	client1 := newTestClient(t, "session-1", "user-1")
	client2 := newTestClient(t, "session-2", "user-2")

	shardIdx := index("test-channel", numHubShards)
	shard := h.subShards[shardIdx]
	_, _ = shard.addSub("test-channel", Subscriber{Client: client1, Ephemeral: false})
	_, _ = shard.addSub("test-channel", Subscriber{Client: client2, Ephemeral: false})

	count := h.NumSubscribers("test-channel")
	if count != 2 {
		t.Errorf("NumSubscribers() = %d, want 2", count)
	}
}

func TestHub_AddSub(t *testing.T) {
	h := newHub(0, 0)
	client := newTestClient(t, "session-1", "user-1")

	first, err := h.addSub("test-channel", Subscriber{Client: client, Ephemeral: false})
	if err != nil {
		t.Fatalf("addSub() error = %v", err)
	}
	if !first {
		t.Error("addSub() should return true for first subscriber")
	}

	count := h.NumSubscribers("test-channel")
	if count != 1 {
		t.Errorf("NumSubscribers() = %d, want 1", count)
	}
}

func TestHub_RemoveSub(t *testing.T) {
	h := newHub(0, 0)
	client := newTestClient(t, "session-1", "user-1")

	_, _ = h.addSub("test-channel", Subscriber{Client: client, Ephemeral: false})

	empty, found := h.removeSub("test-channel", client)
	if !empty {
		t.Error("removeSub() should return true for empty")
	}
	if !found {
		t.Error("removeSub() should return true for found")
	}

	count := h.NumSubscribers("test-channel")
	if count != 0 {
		t.Errorf("NumSubscribers() = %d, want 0", count)
	}
}

func TestSubShard_BroadcastPublication(t *testing.T) {
	shard := newSubShard(0)

	transport1 := &mockTransport{}
	transport2 := &mockTransport{}
	client1 := newTestClientWithTransport(t, "session-1", "user-1", transport1)
	client2 := newTestClientWithTransport(t, "session-2", "user-2", transport2)

	_, _ = shard.addSub("test-channel", Subscriber{Client: client1, Ephemeral: false})
	_, _ = shard.addSub("test-channel", Subscriber{Client: client2, Ephemeral: false})

	pub := &Publication{
		Channel: "test-channel",
		Offset:  1,
		Payload: []byte("test payload"),
		Time:    time.Now().UnixMilli(),
	}

	err := shard.broadcastPublication("test-channel", pub)
	if err != nil {
		t.Fatalf("broadcastPublication() error = %v", err)
	}

	// Check that both clients received a message
	if transport1.getMessageCount() != 1 {
		t.Errorf("client1 received %d messages, want 1", transport1.getMessageCount())
	}
	if transport2.getMessageCount() != 1 {
		t.Errorf("client2 received %d messages, want 1", transport2.getMessageCount())
	}
}

func TestSubShard_BroadcastPublication_NoSubscribers(t *testing.T) {
	shard := newSubShard(0)

	pub := &Publication{
		Channel: "test-channel",
		Offset:  1,
		Payload: []byte("test payload"),
		Time:    time.Now().UnixMilli(),
	}

	err := shard.broadcastPublication("test-channel", pub)
	if err != nil {
		t.Fatalf("broadcastPublication() error = %v", err)
	}
}

func TestHub_BroadcastPublication(t *testing.T) {
	h := newHub(0, 0)

	transport1 := &mockTransport{}
	transport2 := &mockTransport{}
	client1 := newTestClientWithTransport(t, "session-1", "user-1", transport1)
	client2 := newTestClientWithTransport(t, "session-2", "user-2", transport2)

	_, _ = h.addSub("test-channel", Subscriber{Client: client1, Ephemeral: false})
	_, _ = h.addSub("test-channel", Subscriber{Client: client2, Ephemeral: false})

	pub := &Publication{
		Channel: "test-channel",
		Offset:  1,
		Payload: []byte("test payload"),
		Time:    time.Now().UnixMilli(),
	}

	err := h.broadcastPublication("test-channel", pub)
	if err != nil {
		t.Fatalf("broadcastPublication() error = %v", err)
	}

	if transport1.getMessageCount() != 1 {
		t.Errorf("client1 received %d messages, want 1", transport1.getMessageCount())
	}
	if transport2.getMessageCount() != 1 {
		t.Errorf("client2 received %d messages, want 1", transport2.getMessageCount())
	}
}

func TestIndex(t *testing.T) {
	tests := []struct {
		name       string
		s          string
		numBuckets int
		wantRange  [2]int // [min, max]
	}{
		{
			name:       "single bucket",
			s:          "any-string",
			numBuckets: 1,
			wantRange:  [2]int{0, 0},
		},
		{
			name:       "64 buckets",
			s:          "test-channel",
			numBuckets: 64,
			wantRange:  [2]int{0, 63},
		},
		{
			name:       "100 buckets",
			s:          "another-channel",
			numBuckets: 100,
			wantRange:  [2]int{0, 99},
		},
		{
			name:       "16384 buckets (subLocks)",
			s:          "subscription-channel",
			numBuckets: 16384,
			wantRange:  [2]int{0, 16383},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := index(tt.s, tt.numBuckets)
			if got < tt.wantRange[0] || got > tt.wantRange[1] {
				t.Errorf("index() = %d, want in range %v", got, tt.wantRange)
			}
		})
	}
}

func TestIndex_Consistency(t *testing.T) {
	s := "test-string"
	numBuckets := 64

	// Same string should always map to same bucket
	idx1 := index(s, numBuckets)
	idx2 := index(s, numBuckets)
	if idx1 != idx2 {
		t.Errorf("index() inconsistent: %d != %d", idx1, idx2)
	}
}

func TestIndex_Distribution(t *testing.T) {
	// Test that index distributes strings reasonably well across buckets
	numBuckets := 64
	buckets := make([]int, numBuckets)

	for i := 0; i < 1000; i++ {
		s := fmt.Sprintf("channel-%d", i)
		idx := index(s, numBuckets)
		buckets[idx]++
	}

	// Check that each bucket got at least some hits
	minHits := buckets[0]
	maxHits := buckets[0]
	for _, hits := range buckets {
		if hits < minHits {
			minHits = hits
		}
		if hits > maxHits {
			maxHits = hits
		}
	}

	// With 1000 items and 64 buckets, we expect ~16 per bucket
	// Allow for some variance but check distribution isn't terrible
	if minHits == 0 {
		t.Error("Some buckets got no hits, distribution may be poor")
	}
	if maxHits > 50 {
		t.Errorf("Max hits %d seems too high for poor distribution", maxHits)
	}
}

func TestHub_ConcurrentAdd(t *testing.T) {
	h := newHub(0, 0)
	const numClients = 100
	var wg sync.WaitGroup

	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			client := newTestClient(t, fmt.Sprintf("session-%d", n), fmt.Sprintf("user-%d", n))
			_ = h.add(client)
		}(i)
	}

	wg.Wait()

	h.mu.RLock()
	if len(h.sessions) != numClients {
		h.mu.RUnlock()
		t.Errorf("len(sessions) = %d, want %d", len(h.sessions), numClients)
	}
	h.mu.RUnlock()
}

func TestHub_ConcurrentSubscribe(t *testing.T) {
	h := newHub(0, 0)
	const numSubscribers = 100
	var wg sync.WaitGroup

	for i := 0; i < numSubscribers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			client := newTestClient(t, fmt.Sprintf("session-%d", n), fmt.Sprintf("user-%d", n))
			_, _ = h.addSub("test-channel", Subscriber{Client: client, Ephemeral: false})
		}(i)
	}

	wg.Wait()

	count := h.NumSubscribers("test-channel")
	if count != numSubscribers {
		t.Errorf("NumSubscribers() = %d, want %d", count, numSubscribers)
	}
}

func TestSubShard_ConcurrentOperations(t *testing.T) {
	shard := newSubShard(0)
	const numOps = 100
	var wg sync.WaitGroup

	// Add subscribers
	for i := 0; i < numOps; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			client := newTestClient(t, fmt.Sprintf("session-%d", n), fmt.Sprintf("user-%d", n))
			_, _ = shard.addSub("test-channel", Subscriber{Client: client, Ephemeral: false})
		}(i)
	}

	wg.Wait()

	count := shard.NumSubscribers("test-channel")
	if count != numOps {
		t.Errorf("NumSubscribers() = %d, want %d", count, numOps)
	}
}

func TestHub_BroadcastPublication_MultipleChannels(t *testing.T) {
	h := newHub(0, 0)

	transport1 := &mockTransport{}
	transport2 := &mockTransport{}
	client1 := newTestClientWithTransport(t, "session-1", "user-1", transport1)
	client2 := newTestClientWithTransport(t, "session-2", "user-2", transport2)

	_, _ = h.addSub("channel-1", Subscriber{Client: client1, Ephemeral: false})
	_, _ = h.addSub("channel-2", Subscriber{Client: client2, Ephemeral: false})

	pub1 := &Publication{
		Channel: "channel-1",
		Offset:  1,
		Payload: []byte("payload-1"),
		Time:    time.Now().UnixMilli(),
	}

	err := h.broadcastPublication("channel-1", pub1)
	if err != nil {
		t.Fatalf("broadcastPublication() error = %v", err)
	}

	// Only client1 should receive the message
	if transport1.getMessageCount() != 1 {
		t.Errorf("client1 received %d messages, want 1", transport1.getMessageCount())
	}
	if transport2.getMessageCount() != 0 {
		t.Errorf("client2 received %d messages, want 0", transport2.getMessageCount())
	}
}

func TestSubShard_AddSub_EphemeralFlag(t *testing.T) {
	shard := newSubShard(0)
	client := newTestClient(t, "session-1", "user-1")

	// Add ephemeral subscription
	first, err := shard.addSub("test-channel", Subscriber{Client: client, Ephemeral: true})
	if err != nil {
		t.Fatalf("addSub() error = %v", err)
	}
	if !first {
		t.Error("addSub() should return true for first subscriber")
	}

	shard.mu.RLock()
	subs, ok := shard.subs["test-channel"]
	shard.mu.RUnlock()

	if !ok {
		t.Fatal("channel should have subscribers")
	}

	sub := subs["session-1"]
	if !sub.Ephemeral {
		t.Error("ephemeral flag should be preserved")
	}
}

func TestHub_MultipleChannels(t *testing.T) {
	h := newHub(0, 0)
	client := newTestClient(t, "session-1", "user-1")

	channels := []string{"channel-1", "channel-2", "channel-3"}
	for _, ch := range channels {
		_, _ = h.addSub(ch, Subscriber{Client: client, Ephemeral: false})
	}

	for _, ch := range channels {
		count := h.NumSubscribers(ch)
		if count != 1 {
			t.Errorf("NumSubscribers(%q) = %d, want 1", ch, count)
		}
	}
}

// --- P1-13: exact + wildcard double subscription must not double-deliver ---

func newTestPublication(channel string, offset uint64) *Publication {
	return &Publication{
		Channel: channel,
		Offset:  offset,
		Payload: []byte("test payload"),
		Time:    time.Now().UnixMilli(),
	}
}

// assertSinglePublication asserts the transport captured exactly one message
// and that it is a Publication envelope carrying exactly one message.
func assertSinglePublication(t *testing.T, transport *mockTransport, channel string) {
	t.Helper()
	require.Equal(t, 1, transport.getMessageCount())
	var out clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getMessage(0), &out))
	pub := out.GetPublication()
	require.NotNil(t, pub, "outbound message should be a publication")
	require.Len(t, pub.GetMessages(), 1)
	assert.Equal(t, channel, pub.GetMessages()[0].GetChannel())
}

func TestHub_BroadcastPublication_DedupExactAndWildcard(t *testing.T) {
	h := newHub(0, 0)
	transport := &mockTransport{}
	client := newTestClientWithTransport(t, "session-1", "user-1", transport)

	_, err := h.addSub("chat.x", Subscriber{Client: client, Ephemeral: false})
	require.NoError(t, err)
	_, err = h.addSub("chat.*", Subscriber{Client: client, Ephemeral: false})
	require.NoError(t, err)

	err = h.broadcastPublication("chat.x", newTestPublication("chat.x", 1))
	require.NoError(t, err)

	// Subscribed both exactly and via wildcard: exactly one copy is delivered.
	assertSinglePublication(t, transport, "chat.x")
}

func TestHub_BroadcastPublication_DedupExactAndWildcard_MixedSubscribers(t *testing.T) {
	h := newHub(0, 0)

	transportBoth := &mockTransport{}
	transportWildcard := &mockTransport{}
	transportExact := &mockTransport{}
	clientBoth := newTestClientWithTransport(t, "session-both", "user-both", transportBoth)
	clientWildcard := newTestClientWithTransport(t, "session-wild", "user-wild", transportWildcard)
	clientExact := newTestClientWithTransport(t, "session-exact", "user-exact", transportExact)

	for _, sub := range []struct {
		channel string
		client  *Client
	}{
		{"chat.x", clientBoth},
		{"chat.*", clientBoth},
		{"chat.*", clientWildcard},
		{"chat.x", clientExact},
	} {
		_, err := h.addSub(sub.channel, Subscriber{Client: sub.client, Ephemeral: false})
		require.NoError(t, err)
	}

	err := h.broadcastPublication("chat.x", newTestPublication("chat.x", 1))
	require.NoError(t, err)

	// Every client receives exactly one copy regardless of how it subscribed.
	assertSinglePublication(t, transportBoth, "chat.x")
	assertSinglePublication(t, transportWildcard, "chat.x")
	assertSinglePublication(t, transportExact, "chat.x")
}

func TestHub_BroadcastPublication_ExactOnly_SingleDelivery(t *testing.T) {
	h := newHub(0, 0)
	transport := &mockTransport{}
	client := newTestClientWithTransport(t, "session-1", "user-1", transport)

	_, err := h.addSub("chat.x", Subscriber{Client: client, Ephemeral: false})
	require.NoError(t, err)

	err = h.broadcastPublication("chat.x", newTestPublication("chat.x", 1))
	require.NoError(t, err)

	assertSinglePublication(t, transport, "chat.x")
}

func TestHub_BroadcastPublication_WildcardOnly_SingleDelivery(t *testing.T) {
	h := newHub(0, 0)
	transport := &mockTransport{}
	client := newTestClientWithTransport(t, "session-1", "user-1", transport)

	_, err := h.addSub("chat.*", Subscriber{Client: client, Ephemeral: false})
	require.NoError(t, err)

	err = h.broadcastPublication("chat.x", newTestPublication("chat.x", 1))
	require.NoError(t, err)

	assertSinglePublication(t, transport, "chat.x")
}

// --- P2-16: large fan-out must deliver to all subscribers via bounded concurrency ---

func TestHub_BroadcastPublication_LargeFanOut(t *testing.T) {
	h := newHub(0, 0)
	const n = 200
	transports := make([]*mockTransport, n)
	for i := 0; i < n; i++ {
		transports[i] = &mockTransport{}
		client := newTestClientWithTransport(t, fmt.Sprintf("session-%d", i), fmt.Sprintf("user-%d", i), transports[i])
		_, err := h.addSub("fan.ch", Subscriber{Client: client, Ephemeral: false})
		require.NoError(t, err)
	}

	err := h.broadcastPublication("fan.ch", newTestPublication("fan.ch", 1))
	require.NoError(t, err)

	for i, transport := range transports {
		assert.Equal(t, 1, transport.getMessageCount(), "client %d should receive exactly one message", i)
	}
}

// --- P1-3: realtime delivery message IDs must be stable (channel-offset) ---

func TestHub_BroadcastPublication_StableMessageID(t *testing.T) {
	h := newHub(0, 0)
	transport := &mockTransport{}
	client := newTestClientWithTransport(t, "session-1", "user-1", transport)

	_, err := h.addSub("stable.ch", Subscriber{Client: client, Ephemeral: false})
	require.NoError(t, err)

	err = h.broadcastPublication("stable.ch", newTestPublication("stable.ch", 42))
	require.NoError(t, err)

	var out clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getMessage(0), &out))
	pub := out.GetPublication()
	require.NotNil(t, pub)
	require.Len(t, pub.GetMessages(), 1)
	assert.Equal(t, "stable.ch-42", pub.GetMessages()[0].GetId())
}

// --- P1-1: ReplaceSession must migrate wildcard subscriptions ---

func TestHub_ReplaceSession_MigratesWildcardSubscription(t *testing.T) {
	h := newHub(0, 0)
	oldTransport := &mockTransport{}
	newTransport := &mockTransport{}
	oldClient := newTestClientWithTransport(t, "session-1", "user-1", oldTransport)
	newClient := newTestClientWithTransport(t, "session-1", "user-1", newTransport)

	require.NoError(t, h.add(oldClient))
	_, err := h.addSub("chat.*", Subscriber{Client: oldClient, Ephemeral: false})
	require.NoError(t, err)

	h.ReplaceSession("session-1", newClient)

	err = h.broadcastPublication("chat.x", newTestPublication("chat.x", 1))
	require.NoError(t, err)

	// The wildcard subscription must follow the session to the new client.
	assertSinglePublication(t, newTransport, "chat.x")
	assert.Equal(t, 0, oldTransport.getMessageCount(), "old client must not receive deliveries")

	// The matcher record must point at the new client.
	sub, ok := h.LookupSubscriber("chat.*", newClient)
	require.True(t, ok)
	assert.Same(t, newClient, sub.Client)
}
