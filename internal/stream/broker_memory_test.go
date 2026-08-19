package stream

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"

	"github.com/messageloopio/messageloop/internal/channel"
	"github.com/messageloopio/messageloop/internal/occupancy"
	"github.com/messageloopio/messageloop/internal/protocol"
	"github.com/messageloopio/messageloop/pkg/topics"
)

// publishPub builds a Publication from the legacy (payload, isText) tuple so
// tests keep their intent after the Publication model extension (Task 12).
// Local copy of the root testhelpers_test.go helper: this file moved to
// internal/stream in PR-KA-D11 and cannot import the root package.
func publishPub(payload []byte, isText bool) *Publication {
	kind := PayloadKindBinary
	if isText {
		kind = PayloadKindText
	}
	return &Publication{Payload: payload, Kind: kind}
}

// historyPubs fetches a history page and returns its publications, failing
// the test on a storage error.
func historyPubs(t *testing.T, b Broker, ch string, since uint64, limit int) []*Publication {
	t.Helper()
	page, err := b.History(ch, since, limit)
	require.NoError(t, err)
	return page.Pubs()
}

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

// waitCount waits for n handler invocations. Delivery is asynchronous
// (per-channel dispatch shards), so tests must wait instead of asserting
// counts right after Publish returns.
func (c *collectedPubs) waitCount(t *testing.T, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return c.count() == n }, 2*time.Second, time.Millisecond,
		"handler invocations: got %d, want %d", c.count(), n)
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

	pubs := historyPubs(t, b, "ch", 0, 0)
	require.Len(t, pubs, 1)

	require.NoError(t, b.Unsubscribe("ch"))

	// History is intentionally retained while the last subscriber is away so
	// that reconnect with recovery still works.
	pubs = historyPubs(t, b, "ch", 0, 0)
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
	pubs := historyPubs(t, b, "ch", 0, 0)
	assert.Len(t, pubs, 1, "history must remain while subscribers are still present")

	require.NoError(t, b.Unsubscribe("ch"))
	pubs = historyPubs(t, b, "ch", 0, 0)
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
	require.NoError(t, b.Subscribe("ch"))

	offset, err := b.Publish("ch", publishPub([]byte("hello"), false))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if offset == 0 {
		t.Error("expected non-zero offset")
	}
	cp.waitCount(t, 1)
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
	require.NoError(t, b.Subscribe("ch"))

	_, _ = b.Publish("ch", publishPub([]byte("text"), true))
	cp.waitCount(t, 1)
	if pub := cp.last(); pub.Kind != PayloadKindText {
		t.Error("Kind should be Text")
	}
	_, _ = b.Publish("ch", publishPub([]byte("bin"), false))
	cp.waitCount(t, 2)
	if pub := cp.last(); pub.Kind != PayloadKindBinary {
		t.Error("Kind should be Binary")
	}
}

func TestMemoryBroker_Publish_TimeSet(t *testing.T) {
	b, cp, cancel := newTestBroker(t, MemoryBrokerOptions{})
	defer cancel()
	require.NoError(t, b.Subscribe("ch"))

	before := time.Now().UnixMilli()
	_, _ = b.Publish("ch", publishPub([]byte("x"), false))
	after := time.Now().UnixMilli()

	cp.waitCount(t, 1)
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
	require.NoError(t, b.Subscribe("ch"))

	cp.err = protocol.DisconnectBadRequest
	offset, err := b.Publish("ch", publishPub([]byte("x"), false))
	require.NoError(t, err, "a handler error must not negate the publish")
	require.NotZero(t, offset, "the assigned offset must still be reported")
}

func TestMemoryBroker_Publish_HandlerPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := NewMemoryBroker(MemoryBrokerOptions{})
	require.NoError(t, b.Subscribe("ch"))
	go func() { _ = b.Start(ctx, func(_ string, _ *Publication) error { panic("boom") }) }()
	select {
	case <-b.(*memoryBroker).ready:
	case <-time.After(time.Second):
		t.Fatal("broker never became ready")
	}

	offset, err := b.Publish("ch", publishPub([]byte("x"), false))
	require.NoError(t, err, "a handler panic must not negate the publish")
	require.NotZero(t, offset, "the assigned offset must still be reported")
	require.Len(t, historyPubs(t, b, "ch", 0, 0), 1, "the publication must still be in history")
}

func TestMemoryBroker_Publish_MultipleChannels(t *testing.T) {
	b, cp, cancel := newTestBroker(t, MemoryBrokerOptions{})
	defer cancel()
	for _, ch := range []string{"a", "b", "c"} {
		require.NoError(t, b.Subscribe(ch))
		_, _ = b.Publish(ch, publishPub([]byte(ch), false))
	}
	cp.waitCount(t, 3)
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
	pubs := historyPubs(t, b, "a.", 0, 0)
	assert.Empty(t, pubs, "no history may be recorded for malformed channels")
}

func TestMemoryBroker_Publish_ConcurrentSafe(t *testing.T) {
	b, cp, cancel := newTestBroker(t, MemoryBrokerOptions{})
	defer cancel()
	require.NoError(t, b.Subscribe("ch"))

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

	cp.waitCount(t, n)
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
	pubs := historyPubs(t, b, "ch", 0, 0)
	if len(pubs) != 0 {
		t.Errorf("expected 0 pubs, got %d", len(pubs))
	}
}

func TestMemoryBroker_History_All(t *testing.T) {
	b := NewMemoryBroker(MemoryBrokerOptions{})
	for i := 0; i < 5; i++ {
		_, _ = b.Publish("ch", publishPub([]byte{byte(i)}, false))
	}
	pubs := historyPubs(t, b, "ch", 0, 0)
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
	pubs := historyPubs(t, b, "ch", 4, 0)
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
	pubs := historyPubs(t, b, "ch", 0, 3)
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
	pubs := historyPubs(t, b, "ch", 0, 0)
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

	pufsA := historyPubs(t, b, "a", 0, 0)
	pufsB := historyPubs(t, b, "b", 0, 0)
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
	pubs := historyPubs(t, b, "cap-ch", 0, 0)
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
	pubs := historyPubs(t, b, "keep-cap", 0, 0)
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

	pubs := historyPubs(t, b, "reclaim-ch", 0, 0)
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

// --- A2: publish contract, interest gating, history gap page ---

// TestMemoryBroker_Publish_HandlerErrorKeepsHistory verifies §10.1: when the
// handler returns an error, Publish still reports the assigned offset with
// err=nil and the publication is readable from History.
func TestMemoryBroker_Publish_HandlerErrorKeepsHistory(t *testing.T) {
	b, cp, cancel := newTestBroker(t, MemoryBrokerOptions{})
	defer cancel()
	require.NoError(t, b.Subscribe("ch"))

	cp.err = protocol.DisconnectBadRequest
	offset, err := b.Publish("ch", publishPub([]byte("x"), false))
	require.NoError(t, err, "handler failure must not negate the publish")
	require.NotZero(t, offset, "the assigned offset must be reported")
	pubs := historyPubs(t, b, "ch", 0, 0)
	require.Len(t, pubs, 1, "the publication must be in history")
	require.Equal(t, offset, pubs[0].Offset)
}

// TestMemoryBroker_NoInterestSkipsHandler verifies §10.2: without a
// Subscribe, the handler is never called, while a history-writing Publish
// still lands in the ring.
func TestMemoryBroker_NoInterestSkipsHandler(t *testing.T) {
	b, cp, cancel := newTestBroker(t, MemoryBrokerOptions{})
	defer cancel()

	offset, err := b.Publish("ch", publishPub([]byte("x"), false))
	require.NoError(t, err)
	require.NotZero(t, offset)
	require.Zero(t, cp.count(), "no interest means no handler invocation")

	require.NoError(t, b.PublishTransient("ch", publishPub([]byte("t"), false)))
	require.Zero(t, cp.count(), "transient publish without interest must skip the handler")
}

// TestMemoryBroker_WildcardInterest verifies §10.3: Subscribe("forex.*")
// matches Publish("forex.eur") but not Publish("stocks.us").
func TestMemoryBroker_WildcardInterest(t *testing.T) {
	b, cp, cancel := newTestBroker(t, MemoryBrokerOptions{})
	defer cancel()
	require.NoError(t, b.Subscribe("forex.*"))

	_, err := b.Publish("forex.eur", publishPub([]byte("eur"), false))
	require.NoError(t, err)
	_, err = b.Publish("stocks.us", publishPub([]byte("us"), false))
	require.NoError(t, err)

	cp.waitCount(t, 1)
	require.Equal(t, "forex.eur", cp.last().Channel,
		"only the wildcard-matched publish may reach the handler")
}

// TestMemoryBroker_WildcardUnsubscribeToZero verifies a wildcard pattern is
// removed from the matcher when its refcount drops to zero, so interest is
// no longer reported.
func TestMemoryBroker_WildcardUnsubscribeToZero(t *testing.T) {
	b, cp, cancel := newTestBroker(t, MemoryBrokerOptions{})
	defer cancel()
	require.NoError(t, b.Subscribe("forex.*"))
	require.NoError(t, b.Subscribe("forex.*"))
	require.NoError(t, b.Unsubscribe("forex.*"))
	_, err := b.Publish("forex.eur", publishPub([]byte("eur"), false))
	require.NoError(t, err)
	cp.waitCount(t, 1)
	require.NoError(t, b.Unsubscribe("forex.*"))
	_, err = b.Publish("forex.eur", publishPub([]byte("eur2"), false))
	require.NoError(t, err)
	// Interest is gone, so the second publish never reaches a dispatch queue;
	// an immediate check is safe.
	require.Equal(t, 1, cp.count(), "interest must be gone after the last wildcard unsubscribe")
}

// TestMemoryBroker_ExactAndWildcardInterest verifies §10.3 + hard constraint
// 4: Subscribe("im.**") alone makes Publish("im.room.1") reach the handler,
// and an exact subscription counts as interest too.
func TestMemoryBroker_ExactAndWildcardInterest(t *testing.T) {
	b, cp, cancel := newTestBroker(t, MemoryBrokerOptions{})
	defer cancel()
	require.NoError(t, b.Subscribe("im.**"))

	_, err := b.Publish("im.room.1", publishPub([]byte("m1"), false))
	require.NoError(t, err)
	cp.waitCount(t, 1)
	require.Equal(t, "im.room.1", cp.last().Channel,
		"Publish(\"im.room.1\") must reach the handler with only Subscribe(\"im.**\")")
}

// TestMemoryBroker_History_HeadTrimmed verifies §10.4: with a size-2 ring
// and offsets 1,2,3, History(ch, 1) excludes offset 1, reports
// GapReason=HeadTrimmed and FirstRetained=2.
func TestMemoryBroker_History_HeadTrimmed(t *testing.T) {
	b := NewMemoryBroker(MemoryBrokerOptions{HistorySize: 2})
	for i := 0; i < 3; i++ {
		_, err := b.Publish("ch", publishPub([]byte{byte('a' + i)}, false))
		require.NoError(t, err)
	}

	page, err := b.History("ch", 1, 0)
	require.NoError(t, err)
	require.Equal(t, []uint64{2, 3}, offsetsOf(page.Pubs()), "offset 1 must be trimmed")
	require.True(t, page.Gap)
	require.Equal(t, HistoryGapHeadTrimmed, page.GapReason)
	require.Equal(t, uint64(2), page.FirstRetained)
	require.False(t, page.Truncated)
}

// TestMemoryBroker_History_EmptyExpiredSince verifies §10.5: a positive
// sinceOffset with no retained entries (never published) reports
// EmptyExpired with an empty batch — never None.
func TestMemoryBroker_History_EmptyExpiredSince(t *testing.T) {
	b := NewMemoryBroker(MemoryBrokerOptions{})
	page, err := b.History("never.published", 5, 0)
	require.NoError(t, err)
	require.Empty(t, page.Pubs())
	require.True(t, page.Gap, "since>0 with no retained entries must never be HistoryGapNone")
	require.Equal(t, HistoryGapEmptyExpired, page.GapReason)
	require.Zero(t, page.FirstRetained)

	// A channel that published but whose ring was emptied behaves the same.
	require.NoError(t, b.Subscribe("emptied.ch"))
	_, err = b.Publish("emptied.ch", publishPub([]byte("x"), false))
	require.NoError(t, err)
	require.NoError(t, b.Unsubscribe("emptied.ch"))
	mb := b.(*memoryBroker)
	mb.mu.Lock()
	if h, ok := mb.history["emptied.ch"]; ok {
		h.mu.Lock()
		h.count = 0
		h.mu.Unlock()
	}
	mb.mu.Unlock()
	page, err = b.History("emptied.ch", 1, 0)
	require.NoError(t, err)
	require.Empty(t, page.Pubs())
	require.Equal(t, HistoryGapEmptyExpired, page.GapReason)
}

// TestMemoryBroker_History_ZeroSinceUnpublished verifies §10.6: reading from
// the beginning of a never-published channel is a clean empty page with
// GapReason=None.
func TestMemoryBroker_History_ZeroSinceUnpublished(t *testing.T) {
	b := NewMemoryBroker(MemoryBrokerOptions{})
	page, err := b.History("ch", 0, 0)
	require.NoError(t, err)
	require.Empty(t, page.Pubs())
	require.False(t, page.Gap, "since=0 reads from the head: no gap")
	require.Equal(t, HistoryGapNone, page.GapReason)
	require.Zero(t, page.FirstRetained)
}

// TestMemoryBroker_History_TruncatedFlag verifies Truncated mirrors
// len(Publications)==limit when limit > 0.
func TestMemoryBroker_History_TruncatedFlag(t *testing.T) {
	b := NewMemoryBroker(MemoryBrokerOptions{})
	for i := 0; i < 5; i++ {
		_, err := b.Publish("ch", publishPub([]byte{byte(i)}, false))
		require.NoError(t, err)
	}
	page, err := b.History("ch", 0, 3)
	require.NoError(t, err)
	require.Len(t, page.Pubs(), 3)
	require.True(t, page.Truncated)
	require.False(t, page.Gap)

	page, err = b.History("ch", 0, 0)
	require.NoError(t, err)
	require.Len(t, page.Pubs(), 5)
	require.False(t, page.Truncated)
}

func offsetsOf(pubs []*Publication) []uint64 {
	offsets := make([]uint64, 0, len(pubs))
	for _, p := range pubs {
		offsets = append(offsets, p.Offset)
	}
	return offsets
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

	history := historyPubs(t, b, "ch", 0, 0)
	require.Len(t, history, 1)
	h := history[0]
	require.Equal(t, PayloadKindJSON, h.Kind)
	require.Equal(t, "application/json", h.ContentType)
	require.Equal(t, "m-1", h.Id)
	require.Equal(t, map[string]string{"a": "b"}, h.Metadata)
	require.Greater(t, h.Time, int64(0))
	require.Equal(t, offset, h.Offset)
}

// TestMemoryBroker_Subscribe_RejectsUnroutablePatterns pins A3 §8-2: patterns
// the live bus cannot route ("*.room", bare "**"/"*") must fail with
// ErrPatternNotRoutable and leave no state behind, while "im.**" subscribes
// successfully and still delivers "im.room.1".
func TestMemoryBroker_Subscribe_RejectsUnroutablePatterns(t *testing.T) {
	b, cp, cancel := newTestBroker(t, MemoryBrokerOptions{})
	defer cancel()

	for _, ch := range []string{"*.room", "**", "*", "im.*.tick"} {
		err := b.Subscribe(ch)
		require.ErrorIs(t, err, channel.ErrPatternNotRoutable, "channel %q", ch)
	}

	err := b.Subscribe("a..b")
	require.ErrorIs(t, err, topics.ErrBadTopic)

	// The rejected keys must leave no interest behind.
	_, err = b.Publish("im.room.1", publishPub([]byte("m1"), false))
	require.NoError(t, err)
	require.Equal(t, 0, cp.count(), "rejected patterns must not create interest")

	// "im.**" still compiles and routes (A2 semantics preserved).
	require.NoError(t, b.Subscribe("im.**"))
	_, err = b.Publish("im.room.1", publishPub([]byte("m2"), false))
	require.NoError(t, err)
	cp.waitCount(t, 1)
	require.Equal(t, "im.room.1", cp.last().Channel, "Subscribe(\"im.**\") must deliver im.room.1")

	// And the zero-segment case still matches: Publish("im") reaches the
	// handler under the "im.**" interest.
	_, err = b.Publish("im", publishPub([]byte("m3"), false))
	require.NoError(t, err)
	cp.waitCount(t, 2)
}

// TestMemoryBroker_PublishOccupancy_InterestGate pins B2 §5.2: the memory
// broker invokes the occupancy handler only when this node is interested in
// the exact channel (exact or compiled pattern), never when it is not.
func TestMemoryBroker_PublishOccupancy_InterestGate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := NewMemoryBroker(MemoryBrokerOptions{})

	var occMu sync.Mutex
	var occ []occupancy.OccupancyEvent
	require.NoError(t, b.SetOccupancyHandler(func(_ string, evt occupancy.OccupancyEvent) error {
		occMu.Lock()
		defer occMu.Unlock()
		occ = append(occ, evt)
		return nil
	}))
	go func() { _ = b.Start(ctx, func(string, *Publication) error { return nil }) }()
	<-b.(interface{ Ready() <-chan struct{} }).Ready()

	require.NoError(t, b.Subscribe("im.**"))
	require.NoError(t, b.PublishOccupancy("im.room.1", occupancy.OccupancyEvent{Gen: 1, Event: &clientpb.PresenceEvent{Action: "join"}}))
	occMu.Lock()
	require.Len(t, occ, 1, "an im.** interest covers im.room.1 occupancy")
	occMu.Unlock()

	require.NoError(t, b.PublishOccupancy("stocks.1", occupancy.OccupancyEvent{Gen: 2, Event: &clientpb.PresenceEvent{Action: "join"}}))
	occMu.Lock()
	require.Len(t, occ, 1, "an unrelated channel must not reach the occupancy handler")
	occMu.Unlock()
}

// TestMemoryBroker_PublishOccupancy_NeverPublication pins B2 §8.2: occupancy
// events must never reach the publication handler.
func TestMemoryBroker_PublishOccupancy_NeverPublication(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := NewMemoryBroker(MemoryBrokerOptions{})
	mp := &collectedPubs{}
	require.NoError(t, b.SetOccupancyHandler(func(string, occupancy.OccupancyEvent) error { return nil }))
	go func() { _ = b.Start(ctx, mp.handle) }()
	<-b.(interface{ Ready() <-chan struct{} }).Ready()

	require.NoError(t, b.Subscribe("chat.1"))
	require.NoError(t, b.PublishOccupancy("chat.1", occupancy.OccupancyEvent{Gen: 1, Event: &clientpb.PresenceEvent{Action: "join"}}))
	require.Zero(t, mp.count(), "the publication handler must never see occupancy")
}
