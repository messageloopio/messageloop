package messageloop

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/messageloopio/messageloop/pkg/topics"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestBroker creates a started broker with a handler that collects publications.
func newTestBroker(t *testing.T, opts MemoryBrokerOptions) (Broker, *collectedPubs, context.CancelFunc) {
	t.Helper()
	b := NewMemoryBroker(opts)
	cp := &collectedPubs{}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = b.Start(ctx, cp.handle) }()
	time.Sleep(time.Millisecond)
	return b, cp, cancel
}

type collectedPubs struct {
	mu   sync.Mutex
	pubs []*Publication
	err  error
}

func (c *collectedPubs) handle(_ string, pub *Publication) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.pubs = append(c.pubs, pub)
	return nil
}

func (c *collectedPubs) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pubs)
}

func (c *collectedPubs) last() *Publication {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pubs) == 0 {
		return nil
	}
	return c.pubs[len(c.pubs)-1]
}

// --- interface / lifecycle ---

func TestNewMemoryBroker(t *testing.T) {
	b := NewMemoryBroker(MemoryBrokerOptions{})
	if b == nil {
		t.Fatal("NewMemoryBroker() returned nil")
	}
	if _, ok := b.(*memoryBroker); !ok {
		t.Error("NewMemoryBroker() should return *memoryBroker")
	}
}

func TestMemoryBroker_BrokerInterface(t *testing.T) {
	var _ Broker = (*memoryBroker)(nil)
}

func TestMemoryBroker_Subscribe_Unsubscribe(t *testing.T) {
	b := NewMemoryBroker(MemoryBrokerOptions{})
	if err := b.Subscribe("ch"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := b.Unsubscribe("ch"); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
}

// --- P2-18: empty channel history entries must be reclaimed ---

func TestMemoryBroker_History_RetainedAfterLastUnsubscribe(t *testing.T) {
	b := NewMemoryBroker(MemoryBrokerOptions{})
	require.NoError(t, b.Subscribe("ch"))
	_, err := b.Publish("ch", publishPub([]byte("x"), false))
	require.NoError(t, err)

	pubs, err := b.History("ch", 0, 0)
	require.NoError(t, err)
	require.Len(t, pubs, 1)

	require.NoError(t, b.Unsubscribe("ch"))

	// History is intentionally retained while the last subscriber is away so
	// that reconnect with recovery still works.
	pubs, err = b.History("ch", 0, 0)
	require.NoError(t, err)
	assert.Len(t, pubs, 1, "history must be retained for recovery after the last unsubscribe")
}

func TestMemoryBroker_History_EmptyChannelEntryReclaimedAfterUnsubscribe(t *testing.T) {
	b := NewMemoryBroker(MemoryBrokerOptions{})
	require.NoError(t, b.Subscribe("ch"))
	require.NoError(t, b.Unsubscribe("ch"))

	mb := b.(*memoryBroker)
	mb.mu.RLock()
	_, ok := mb.history["ch"]
	mb.mu.RUnlock()
	assert.False(t, ok, "empty history entry should be removed from the map")
}

func TestMemoryBroker_History_KeptWhileSubscribersRemain(t *testing.T) {
	b := NewMemoryBroker(MemoryBrokerOptions{})
	require.NoError(t, b.Subscribe("ch"))
	require.NoError(t, b.Subscribe("ch"))
	_, err := b.Publish("ch", publishPub([]byte("x"), false))
	require.NoError(t, err)

	require.NoError(t, b.Unsubscribe("ch"))
	pubs, err := b.History("ch", 0, 0)
	require.NoError(t, err)
	assert.Len(t, pubs, 1, "history must remain while subscribers are still present")

	require.NoError(t, b.Unsubscribe("ch"))
	pubs, err = b.History("ch", 0, 0)
	require.NoError(t, err)
	assert.Len(t, pubs, 1, "history remains for recovery even after the last subscriber leaves")
}

func TestMemoryBroker_ConcurrentPublishUnsubscribe(t *testing.T) {
	b := NewMemoryBroker(MemoryBrokerOptions{})
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		ch := fmt.Sprintf("ch-%d", i)
		require.NoError(t, b.Subscribe(ch))
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = b.Publish(ch, publishPub([]byte("x"), false))
		}()
		go func() {
			defer wg.Done()
			_ = b.Unsubscribe(ch)
		}()
	}
	wg.Wait()
}

func TestMemoryBroker_Start_BlocksUntilCtxDone(t *testing.T) {
	b := NewMemoryBroker(MemoryBrokerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Start(ctx, func(_ string, _ *Publication) error { return nil }) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Error("Start did not return after context cancel")
	}
}

// --- publish / handler ---

func TestMemoryBroker_Publish_CallsHandler(t *testing.T) {
	b, cp, cancel := newTestBroker(t, MemoryBrokerOptions{})
	defer cancel()

	offset, err := b.Publish("ch", publishPub([]byte("hello"), false))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if offset == 0 {
		t.Error("expected non-zero offset")
	}
	if cp.count() != 1 {
		t.Fatalf("handler called %d times, want 1", cp.count())
	}
	pub := cp.last()
	if pub.Channel != "ch" {
		t.Errorf("Channel = %q, want \"ch\"", pub.Channel)
	}
	if string(pub.Payload) != "hello" {
		t.Errorf("Payload = %q", pub.Payload)
	}
	if pub.Offset != offset {
		t.Errorf("pub.Offset = %d, want %d", pub.Offset, offset)
	}
}

func TestMemoryBroker_Publish_Kind(t *testing.T) {
	b, cp, cancel := newTestBroker(t, MemoryBrokerOptions{})
	defer cancel()

	_, _ = b.Publish("ch", publishPub([]byte("text"), true))
	if pub := cp.last(); pub.Kind != PayloadKindText {
		t.Error("Kind should be Text")
	}
	_, _ = b.Publish("ch", publishPub([]byte("bin"), false))
	if pub := cp.last(); pub.Kind != PayloadKindBinary {
		t.Error("Kind should be Binary")
	}
}

func TestMemoryBroker_Publish_TimeSet(t *testing.T) {
	b, cp, cancel := newTestBroker(t, MemoryBrokerOptions{})
	defer cancel()

	before := time.Now().UnixMilli()
	_, _ = b.Publish("ch", publishPub([]byte("x"), false))
	after := time.Now().UnixMilli()

	pub := cp.last()
	if pub.Time < before || pub.Time > after {
		t.Errorf("Time %d not in [%d, %d]", pub.Time, before, after)
	}
}

func TestMemoryBroker_Publish_NoHandler(t *testing.T) {
	b := NewMemoryBroker(MemoryBrokerOptions{})
	offset, err := b.Publish("ch", publishPub([]byte("x"), false))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if offset == 0 {
		t.Error("expected non-zero offset even without handler")
	}
}

func TestMemoryBroker_Publish_HandlerError(t *testing.T) {
	b, cp, cancel := newTestBroker(t, MemoryBrokerOptions{})
	defer cancel()

	cp.err = DisconnectBadRequest
	_, err := b.Publish("ch", publishPub([]byte("x"), false))
	if err != DisconnectBadRequest {
		t.Errorf("expected DisconnectBadRequest, got %v", err)
	}
}

func TestMemoryBroker_Publish_MultipleChannels(t *testing.T) {
	b, cp, cancel := newTestBroker(t, MemoryBrokerOptions{})
	defer cancel()

	for _, ch := range []string{"a", "b", "c"} {
		_, _ = b.Publish(ch, publishPub([]byte(ch), false))
	}
	if cp.count() != 3 {
		t.Errorf("got %d publications, want 3", cp.count())
	}
}

// TestMemoryBroker_Publish_RejectsMalformedChannel pins B1: the publish entry
// must reject channels with explicit empty segments ("a.", ".a", "a..b") with
// ErrBadTopic instead of recording history or invoking the handler.
func TestMemoryBroker_Publish_RejectsMalformedChannel(t *testing.T) {
	b, cp, cancel := newTestBroker(t, MemoryBrokerOptions{})
	defer cancel()

	for _, ch := range []string{"a.", ".a", "a..b", ""} {
		_, err := b.Publish(ch, publishPub([]byte("x"), false))
		assert.ErrorIs(t, err, topics.ErrBadTopic, "Publish(%q)", ch)
		err = b.PublishTransient(ch, publishPub([]byte("x"), false))
		assert.ErrorIs(t, err, topics.ErrBadTopic, "PublishTransient(%q)", ch)
	}

	assert.Zero(t, cp.count(), "no publication may reach the handler for malformed channels")
	pubs, err := b.History("a.", 0, 0)
	assert.NoError(t, err)
	assert.Empty(t, pubs, "no history may be recorded for malformed channels")
}

func TestMemoryBroker_Publish_ConcurrentSafe(t *testing.T) {
	b, cp, cancel := newTestBroker(t, MemoryBrokerOptions{})
	defer cancel()

	const n = 100
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = b.Publish("ch", publishPub([]byte("x"), false))
		}()
	}
	wg.Wait()

	if cp.count() != n {
		t.Errorf("got %d publications, want %d", cp.count(), n)
	}
}

// --- offset monotonicity ---

func TestMemoryBroker_Offset_Monotonic(t *testing.T) {
	b := NewMemoryBroker(MemoryBrokerOptions{})
	var prev uint64
	for i := 0; i < 10; i++ {
		off, _ := b.Publish("ch", publishPub([]byte("x"), false))
		if off <= prev {
			t.Errorf("offset[%d]=%d is not > prev=%d", i, off, prev)
		}
		prev = off
	}
}

func TestMemoryBroker_Offset_PerChannel(t *testing.T) {
	b := NewMemoryBroker(MemoryBrokerOptions{})
	offA, _ := b.Publish("a", publishPub([]byte("x"), false))
	offB, _ := b.Publish("b", publishPub([]byte("x"), false))
	if offA != 1 {
		t.Errorf("channel a: offset = %d, want 1", offA)
	}
	if offB != 1 {
		t.Errorf("channel b: offset = %d, want 1", offB)
	}
}

// --- history ---

func TestMemoryBroker_History_Empty(t *testing.T) {
	b := NewMemoryBroker(MemoryBrokerOptions{})
	pubs, err := b.History("ch", 0, 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(pubs) != 0 {
		t.Errorf("expected 0 pubs, got %d", len(pubs))
	}
}

func TestMemoryBroker_History_All(t *testing.T) {
	b := NewMemoryBroker(MemoryBrokerOptions{})
	for i := 0; i < 5; i++ {
		_, _ = b.Publish("ch", publishPub([]byte{byte(i)}, false))
	}
	pubs, err := b.History("ch", 0, 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(pubs) != 5 {
		t.Fatalf("expected 5 pubs, got %d", len(pubs))
	}
	for i, p := range pubs {
		if p.Offset != uint64(i+1) {
			t.Errorf("pubs[%d].Offset = %d, want %d", i, p.Offset, i+1)
		}
	}
}

func TestMemoryBroker_History_SinceOffset(t *testing.T) {
	b := NewMemoryBroker(MemoryBrokerOptions{})
	for i := 0; i < 6; i++ {
		_, _ = b.Publish("ch", publishPub([]byte{byte(i)}, false))
	}
	// offsets 1-6; since=4 returns 4,5,6
	pubs, err := b.History("ch", 4, 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(pubs) != 3 {
		t.Fatalf("expected 3 pubs since offset 4, got %d", len(pubs))
	}
	if pubs[0].Offset != 4 {
		t.Errorf("first offset = %d, want 4", pubs[0].Offset)
	}
}

func TestMemoryBroker_History_Limit(t *testing.T) {
	b := NewMemoryBroker(MemoryBrokerOptions{})
	for i := 0; i < 10; i++ {
		_, _ = b.Publish("ch", publishPub([]byte{byte(i)}, false))
	}
	pubs, err := b.History("ch", 0, 3)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(pubs) != 3 {
		t.Fatalf("expected 3 pubs with limit=3, got %d", len(pubs))
	}
}

func TestMemoryBroker_History_RingBuffer(t *testing.T) {
	const size = 4
	b := NewMemoryBroker(MemoryBrokerOptions{HistorySize: size})
	for i := 0; i < 7; i++ {
		_, _ = b.Publish("ch", publishPub([]byte{byte(i)}, false))
	}
	// offsets 1-7; ring of 4 retains 4,5,6,7
	pubs, err := b.History("ch", 0, 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(pubs) != size {
		t.Fatalf("expected %d pubs (ring size), got %d", size, len(pubs))
	}
	if pubs[0].Offset != 4 {
		t.Errorf("oldest retained offset = %d, want 4", pubs[0].Offset)
	}
	if pubs[size-1].Offset != 7 {
		t.Errorf("newest offset = %d, want 7", pubs[size-1].Offset)
	}
}

func TestMemoryBroker_History_MultiChannel_Isolated(t *testing.T) {
	b := NewMemoryBroker(MemoryBrokerOptions{})
	_, _ = b.Publish("a", publishPub([]byte("a1"), false))
	_, _ = b.Publish("b", publishPub([]byte("b1"), false))
	_, _ = b.Publish("a", publishPub([]byte("a2"), false))

	pufsA, _ := b.History("a", 0, 0)
	pufsB, _ := b.History("b", 0, 0)
	if len(pufsA) != 2 {
		t.Errorf("channel a: expected 2 pubs, got %d", len(pufsA))
	}
	if len(pufsB) != 1 {
		t.Errorf("channel b: expected 1 pub, got %d", len(pufsB))
	}
}

// TestMemoryBroker_PerChannelHistorySize verifies PR-02: a ring allocated on
// the channel's first publish honors the publication's HistorySize, keeping
// only the last N entries.
func TestMemoryBroker_PerChannelHistorySize(t *testing.T) {
	b := NewMemoryBroker(MemoryBrokerOptions{})
	for i := 0; i < 5; i++ {
		pub := publishPub([]byte{'a' + byte(i)}, false)
		pub.HistorySize = 3
		_, err := b.Publish("cap-ch", pub)
		require.NoError(t, err)
	}
	pubs, err := b.History("cap-ch", 0, 0)
	require.NoError(t, err)
	require.Len(t, pubs, 3, "ring with per-channel HistorySize=3 must keep only the last 3 entries")
	require.Equal(t, "c", string(pubs[0].Payload))
	require.Equal(t, "e", string(pubs[2].Payload))
}

// TestMemoryBroker_ExistingRingKeepsCap verifies the design rule: an already
// allocated ring is never resized by a later HistorySize; it keeps its
// original capacity until it is reclaimed.
func TestMemoryBroker_ExistingRingKeepsCap(t *testing.T) {
	b := NewMemoryBroker(MemoryBrokerOptions{})
	_, err := b.Publish("keep-cap", publishPub([]byte("first"), false))
	require.NoError(t, err)

	for i := 0; i < defaultMemoryHistorySize+5; i++ {
		pub := publishPub([]byte(fmt.Sprintf("m-%d", i)), false)
		pub.HistorySize = 2
		_, err := b.Publish("keep-cap", pub)
		require.NoError(t, err)
	}
	pubs, err := b.History("keep-cap", 0, 0)
	require.NoError(t, err)
	require.Len(t, pubs, defaultMemoryHistorySize,
		"an existing ring must keep the broker default cap, not the later HistorySize=2")
}

// TestMemoryBroker_ReclaimedRingUsesNewSize verifies that after the ring is
// reclaimed (last subscriber left, ring empty) the next publish allocates
// with the new size. Rings are retained while subscribers may return for
// recovery, so the only way to reclaim a live ring is the empty-ring path in
// Unsubscribe; here we exercise the same white-box state.
func TestMemoryBroker_ReclaimedRingUsesNewSize(t *testing.T) {
	b := NewMemoryBroker(MemoryBrokerOptions{})
	_, err := b.Publish("reclaim-ch", publishPub([]byte("first"), false))
	require.NoError(t, err)
	mb := b.(*memoryBroker)
	mb.mu.Lock()
	delete(mb.history, "reclaim-ch")
	mb.mu.Unlock()

	pub := publishPub([]byte("second"), false)
	pub.HistorySize = 2
	_, err = b.Publish("reclaim-ch", pub)
	require.NoError(t, err)
	_, err = b.Publish("reclaim-ch", publishPub([]byte("third"), false))
	require.NoError(t, err)
	_, err = b.Publish("reclaim-ch", publishPub([]byte("fourth"), false))
	require.NoError(t, err)

	pubs, err := b.History("reclaim-ch", 0, 0)
	require.NoError(t, err)
	require.Len(t, pubs, 2, "reclaimed ring must be reallocated with the new HistorySize=2")
	require.Equal(t, "third", string(pubs[0].Payload))
	require.Equal(t, "fourth", string(pubs[1].Payload))
}

// TestMemoryBroker_HistoryTTLIgnored verifies the memory broker accepts a
// history_ttl override without failing (it warns and ignores it).
func TestMemoryBroker_HistoryTTLIgnored(t *testing.T) {
	b := NewMemoryBroker(MemoryBrokerOptions{})
	pub := publishPub([]byte("msg"), false)
	pub.HistoryTTL = time.Hour
	offset, err := b.Publish("ttl-ch", pub)
	require.NoError(t, err)
	require.NotZero(t, offset)
}

// --- node integration ---

func TestMemoryBroker_IntegrationWithNode(t *testing.T) {
	node := NewNode(nil)
	if node.Broker() == nil {
		t.Fatal("Node should have a default broker")
	}
	if _, ok := node.Broker().(*memoryBroker); !ok {
		t.Error("Node default broker should be *memoryBroker")
	}
}

func TestMemoryBroker_Node_Run(t *testing.T) {
	node := NewNode(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := node.Run(ctx); err != nil {
		t.Fatalf("node.Run: %v", err)
	}
}

// --- benchmarks ---

func BenchmarkMemoryBroker_Publish(b *testing.B) {
	broker := NewMemoryBroker(MemoryBrokerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = broker.Start(ctx, func(_ string, _ *Publication) error { return nil }) }()
	time.Sleep(time.Millisecond)
	payload := []byte("bench payload")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = broker.Publish("ch", publishPub(payload, false))
	}
}

func BenchmarkMemoryBroker_ConcurrentPublish(b *testing.B) {
	broker := NewMemoryBroker(MemoryBrokerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = broker.Start(ctx, func(_ string, _ *Publication) error { return nil }) }()
	time.Sleep(time.Millisecond)
	payload := []byte("bench payload")
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = broker.Publish("ch", publishPub(payload, false))
		}
	})
}

// Task 12: the Publication model must preserve kind/content_type/id/metadata
// through publish and history reads.
func TestMemoryBroker_Publish_PreservesKindAndMetadata(t *testing.T) {
	b, _, cancel := newTestBroker(t, MemoryBrokerOptions{})
	defer cancel()

	pub := &Publication{
		Payload:     []byte(`{"k":"v"}`),
		Kind:        PayloadKindJSON,
		ContentType: "application/json",
		Id:          "m-1",
		Metadata:    map[string]string{"a": "b"},
	}
	offset, err := b.Publish("ch", pub)
	require.NoError(t, err)
	require.NotZero(t, offset)

	history, err := b.History("ch", 0, 0)
	require.NoError(t, err)
	require.Len(t, history, 1)
	h := history[0]
	require.Equal(t, PayloadKindJSON, h.Kind)
	require.Equal(t, "application/json", h.ContentType)
	require.Equal(t, "m-1", h.Id)
	require.Equal(t, map[string]string{"a": "b"}, h.Metadata)
	require.Greater(t, h.Time, int64(0))
	require.Equal(t, offset, h.Offset)
}
