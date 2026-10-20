package runtime

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	clientpb "github.com/messageloopio/messageloop/protocol/genproto/messageloop/client/v2"

	"github.com/messageloopio/messageloop/internal/config"
)

// batchRecorder captures the batcher's two delivery paths and their order.
type batchRecorder struct {
	mu        sync.Mutex
	immediate []uint64   // offsets sent unbatched, in call order
	batches   [][]uint64 // offsets per batched broadcast, in call order
	events    []string   // "imm:<off>" / "batch:<off>,<off>,..." in order
	block     chan struct{}
	unblocked sync.Once     // unblock is idempotent (test + cleanup)
	batchDone chan struct{} // signaled after each recorded batch
}

func (r *batchRecorder) recordImmediate(offset uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.immediate = append(r.immediate, offset)
	r.events = append(r.events, fmt.Sprintf("imm:%d", offset))
}

func (r *batchRecorder) recordBatch(offsets []uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]uint64, len(offsets))
	copy(cp, offsets)
	r.batches = append(r.batches, cp)
	r.events = append(r.events, fmt.Sprintf("batch:%v", cp))
	if r.batchDone != nil {
		select {
		case r.batchDone <- struct{}{}:
		default:
		}
	}
}

func (r *batchRecorder) unblock() {
	r.mu.Lock()
	block := r.block
	r.mu.Unlock()
	if block != nil {
		r.unblocked.Do(func() { close(block) })
	}
}

// total returns the recorded event list snapshot.
func (r *batchRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// newBatcherHarness builds a batcher over a bare node with stubbed
// delivery/count/policy seams. The channel is at the subscriber threshold by
// default; tests move it via rec.subs.
func newBatcherHarness(t *testing.T, s config.BatchingSettings) (*publicationBatcher, *batchRecorder) {
	t.Helper()
	node := NewNode(nil)
	b := newPublicationBatcher(node, s)
	rec := &batchRecorder{batchDone: make(chan struct{}, 64)}
	b.immediate = func(ch string, pub *Publication) error {
		rec.recordImmediate(pub.Offset)
		return nil
	}
	b.broadcast = func(ch string, pubs []*Publication) error {
		if rec.block != nil {
			<-rec.block
		}
		offsets := make([]uint64, len(pubs))
		for i, pub := range pubs {
			offsets[i] = pub.Offset
		}
		rec.recordBatch(offsets)
		return nil
	}
	b.subCount = func(ch string) int { return s.MinSubscribers }
	b.policyFor = func(ch string) ChannelPolicy { return ChannelPolicy{} }
	t.Cleanup(func() {
		rec.unblock()
		b.Close()
	})
	return b, rec
}

func pub(offset uint64, payload byte) *Publication {
	return &Publication{
		Channel: "game.room1",
		Offset:  offset,
		Payload: make([]byte, payload),
	}
}

// TestBatcher_TransientBypassesBatching pins the offset-0 rule: presence
// join/leave events keep the immediate path regardless of thresholds.
func TestBatcher_TransientBypassesBatching(t *testing.T) {
	b, rec := newBatcherHarness(t, config.BatchingSettings{
		MaxDelay: 20 * time.Millisecond, MaxMessages: 4, MaxBatchBytes: 64 * 1024,
		MinSubscribers: 1, IdleReap: time.Second,
	})
	require.NoError(t, b.Deliver("game.room1", pub(0, 8)))
	assert.Equal(t, []string{"imm:0"}, rec.snapshot())
}

// TestBatcher_BelowThresholdImmediate: a channel under min_subscribers pays
// no batching latency.
func TestBatcher_BelowThresholdImmediate(t *testing.T) {
	b, rec := newBatcherHarness(t, config.BatchingSettings{
		MaxDelay: 20 * time.Millisecond, MaxMessages: 4, MaxBatchBytes: 64 * 1024,
		MinSubscribers: 256, IdleReap: time.Second,
	})
	b.subCount = func(ch string) int { return 255 }
	require.NoError(t, b.Deliver("game.room1", pub(1, 8)))
	assert.Equal(t, []string{"imm:1"}, rec.snapshot())
}

// TestBatcher_PolicyOverride pins the channel-policy switch: batching=false
// keeps the channel unbatched even above the threshold; batching=true (with
// the threshold met) batches.
func TestBatcher_PolicyOverride(t *testing.T) {
	off := false
	on := true
	b, rec := newBatcherHarness(t, config.BatchingSettings{
		MaxDelay: 15 * time.Millisecond, MaxMessages: 4, MaxBatchBytes: 64 * 1024,
		MinSubscribers: 1, IdleReap: time.Second,
	})
	b.policyFor = func(ch string) ChannelPolicy { return ChannelPolicy{Batching: &off} }
	require.NoError(t, b.Deliver("game.room1", pub(1, 8)))
	assert.Equal(t, []string{"imm:1"}, rec.snapshot(), "policy off must force the immediate path")

	b.policyFor = func(ch string) ChannelPolicy { return ChannelPolicy{Batching: &on} }
	require.NoError(t, b.Deliver("game.room1", pub(2, 8)))
	waitForBatch(t, rec, 1, time.Second)
	assert.Equal(t, []string{"batch:[2]"}, rec.snapshot()[1:], "policy on must batch at the threshold")
}

// TestBatcher_WindowFlush: a trickle below the message cap flushes within
// max_delay of the first message, in offset order.
func TestBatcher_WindowFlush(t *testing.T) {
	b, rec := newBatcherHarness(t, config.BatchingSettings{
		MaxDelay: 20 * time.Millisecond, MaxMessages: 8, MaxBatchBytes: 64 * 1024,
		MinSubscribers: 1, IdleReap: time.Second,
	})
	start := time.Now()
	require.NoError(t, b.Deliver("game.room1", pub(1, 8)))
	time.Sleep(5 * time.Millisecond)
	require.NoError(t, b.Deliver("game.room1", pub(2, 8)))
	waitForBatch(t, rec, 1, time.Second)
	assert.Less(t, time.Since(start), time.Second, "window flush must follow max_delay, not the idle reap")
	assert.Equal(t, []string{"batch:[1 2]"}, rec.snapshot(), "both publications must ride one frame in offset order")
}

// TestBatcher_ThresholdFlush: hitting max_messages flushes immediately
// without waiting out the (long) window.
func TestBatcher_ThresholdFlush(t *testing.T) {
	b, rec := newBatcherHarness(t, config.BatchingSettings{
		MaxDelay: 10 * time.Second, MaxMessages: 3, MaxBatchBytes: 64 * 1024,
		MinSubscribers: 1, IdleReap: time.Second,
	})
	start := time.Now()
	for i := uint64(1); i <= 3; i++ {
		require.NoError(t, b.Deliver("game.room1", pub(i, 8)))
	}
	waitForBatch(t, rec, 1, time.Second)
	assert.Less(t, time.Since(start), time.Second, "threshold flush must not wait out the window")
	assert.Equal(t, []string{"batch:[1 2 3]"}, rec.snapshot())
}

// TestBatcher_BackpressureBlocks pins the memory contract: when the buffer
// is at both caps and the flusher is stuck in a blocked broadcast, the next
// append blocks the delivery worker instead of growing the buffer. It
// resumes once the drain makes space.
func TestBatcher_BackpressureBlocks(t *testing.T) {
	b, rec := newBatcherHarness(t, config.BatchingSettings{
		MaxDelay: 20 * time.Millisecond, MaxMessages: 2, MaxBatchBytes: 64 * 1024,
		MinSubscribers: 1, IdleReap: time.Second,
	})
	rec.block = make(chan struct{})
	b.broadcast = func(ch string, pubs []*Publication) error {
		if rec.block != nil {
			<-rec.block
		}
		offsets := make([]uint64, len(pubs))
		for i, p := range pubs {
			offsets[i] = p.Offset
		}
		rec.recordBatch(offsets)
		return nil
	}

	// p1+p2 fill the first buffer; the flusher swaps and parks in the
	// blocked broadcast. p3+p4 fill the second; p5 must block.
	for i := uint64(1); i <= 4; i++ {
		require.NoError(t, b.Deliver("game.room1", pub(i, 8)))
	}
	appended := make(chan error, 1)
	go func() { appended <- b.Deliver("game.room1", pub(5, 8)) }()

	select {
	case err := <-appended:
		require.NoError(t, err)
		t.Fatal("append must block while the buffer is full and the flusher is parked")
	case <-time.After(150 * time.Millisecond):
		// expected: the delivery worker is parked in cond.Wait
	}

	rec.unblock()
	select {
	case err := <-appended:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("append must resume after the drain")
	}
	waitForBatchCount(t, rec, 3, 2*time.Second)
	assert.Equal(t, [][]uint64{{1, 2}, {3, 4}, {5}}, rec.batchesSnapshot(), "order must be preserved across the backpressure window")
}

// TestBatcher_OversizeBypassPreservesOrder pins the oversize rule: the
// unbatchable publication waits for the buffered batch's broadcast to
// RETURN (inFlight, not just the swap) before going out unbatched —
// otherwise the immediate send races the batch fan-out on another
// goroutine and the channel reorders on the wire.
func TestBatcher_OversizeBypassPreservesOrder(t *testing.T) {
	b, rec := newBatcherHarness(t, config.BatchingSettings{
		MaxDelay: 10 * time.Second, MaxMessages: 4, MaxBatchBytes: 64,
		MinSubscribers: 1, IdleReap: time.Second,
	})
	// p1 fits the budget and buffers (window is long, threshold far).
	require.NoError(t, b.Deliver("game.room1", pub(1, 10)))
	// p2 exceeds the per-publication budget: drain then immediate.
	require.NoError(t, b.Deliver("game.room1", pub(2, 128)))
	assert.Equal(t, []string{"batch:[1]", "imm:2"}, rec.snapshot(),
		"the buffered batch must complete its broadcast before the oversize publication goes out")
}

// TestBatcher_IdleReap: after the window drains a channel and idle_reap
// elapses with no traffic, the accumulator is unregistered; the next
// threshold-passing publication builds a fresh one.
func TestBatcher_IdleReap(t *testing.T) {
	b, rec := newBatcherHarness(t, config.BatchingSettings{
		MaxDelay: 10 * time.Millisecond, MaxMessages: 4, MaxBatchBytes: 64 * 1024,
		MinSubscribers: 1, IdleReap: 60 * time.Millisecond,
	})
	require.NoError(t, b.Deliver("game.room1", pub(1, 8)))
	waitForBatch(t, rec, 1, time.Second)

	deadline := time.Now().Add(2 * time.Second)
	for b.lookup("game.room1") != nil {
		if time.Now().After(deadline) {
			t.Fatal("accumulator must be reaped after idle_reap")
		}
		time.Sleep(5 * time.Millisecond)
	}
	require.NoError(t, b.Deliver("game.room1", pub(2, 8)))
	waitForBatch(t, rec, 2, time.Second)
	assert.Equal(t, [][]uint64{{1}, {2}}, rec.batchesSnapshot(), "the reaped channel must batch again from scratch")
}

// TestBatcher_CloseFlushesPending: the final flush on Close drains whatever
// the window still held.
func TestBatcher_CloseFlushesPending(t *testing.T) {
	b, rec := newBatcherHarness(t, config.BatchingSettings{
		MaxDelay: 10 * time.Second, MaxMessages: 8, MaxBatchBytes: 64 * 1024,
		MinSubscribers: 1, IdleReap: time.Second,
	})
	require.NoError(t, b.Deliver("game.room1", pub(1, 8)))
	require.NoError(t, b.Deliver("game.room1", pub(2, 8)))
	b.Close()
	assert.Equal(t, [][]uint64{{1, 2}}, rec.batchesSnapshot(), "Close must flush the pending window")
}

// TestBatcher_ClosedFallsThroughToImmediate: after Close drained and closed
// a channel's accumulator, Deliver degrades to the immediate path instead of
// dropping or blocking.
func TestBatcher_ClosedFallsThroughToImmediate(t *testing.T) {
	b, rec := newBatcherHarness(t, config.BatchingSettings{
		MaxDelay: 10 * time.Second, MaxMessages: 8, MaxBatchBytes: 64 * 1024,
		MinSubscribers: 1, IdleReap: time.Second,
	})
	// Establish the accumulator, then Close: its flusher exits and marks it
	// closed deterministically.
	require.NoError(t, b.Deliver("game.room1", pub(1, 8)))
	b.Close()
	require.NoError(t, b.Deliver("game.room1", pub(2, 8)))
	assert.Equal(t, [][]uint64{{1}}, rec.batchesSnapshot(), "Close must flush the pending window")
	assert.Equal(t, []string{"imm:2"}, rec.snapshot()[1:], "Deliver after Close must take the immediate path")
}

// TestBatcher_EndToEndThroughBroker pins the production wiring: NewNode with
// server.broadcast constructs the batcher, Node.Run routes broker deliveries
// through it, and a subscriber receives one multi-message frame per window
// — the real memory broker → batcher → hub seam.
func TestBatcher_EndToEndThroughBroker(t *testing.T) {
	node := NewNode(&config.Server{
		Broadcast: &config.BatchingConfig{MaxDelay: "20ms", MinSubscribers: 1},
	})
	require.NotNil(t, node.batcher, "server.broadcast must construct the batcher")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, node.Run(ctx))

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.Attach(client.Attachment()))
	_, err = node.hub.AddSub("game.room1", NewSubscriber(client, false))
	require.NoError(t, err)
	// The subscribe saga registers broker interest alongside the hub entry;
	// without it the memory broker skips delivery to interest-less channels.
	require.NoError(t, node.Broker().Subscribe("game.room1"))

	// Publish through the memory broker: offsets are assigned by the broker.
	for i := 0; i < 2; i++ {
		_, err := node.Broker().Publish("game.room1", &Publication{
			Channel: "game.room1",
			Payload: []byte(fmt.Sprintf(`{"i":%d}`, i)),
			Kind:    PayloadKindJSON,
		})
		require.NoError(t, err)
	}

	waitForBatchFrames(t, transport, 1, 2*time.Second)
	transport.mu.Lock()
	frames := append([][]byte(nil), transport.messages...)
	transport.mu.Unlock()
	require.Len(t, frames, 1, "both publications must ride one frame")

	var out clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(frames[0], &out))
	msgs := out.GetPublication().GetMessages()
	require.Len(t, msgs, 2)
	assert.Equal(t, "game.room1-1", msgs[0].GetId())
	assert.Equal(t, "game.room1-2", msgs[1].GetId())
}

// TestBatcher_DisabledByDefault pins the opt-in contract: without
// server.broadcast the batcher stays nil and Node.Run wires the direct hub
// path.
func TestBatcher_DisabledByDefault(t *testing.T) {
	assert.Nil(t, NewNode(nil).batcher)
	assert.Nil(t, NewNode(&config.Server{}).batcher)
}

// waitForBatchFrames polls the transport until at least n frames arrived.
func waitForBatchFrames(t *testing.T, tr *capturingTransport, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		tr.mu.Lock()
		got := len(tr.messages)
		tr.mu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d frames (have %d)", n, got)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitForBatch waits for the next recorded batch.
func waitForBatch(t *testing.T, rec *batchRecorder, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for len(rec.batchesSnapshot()) < want {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for batch %d (have %d)", want, len(rec.batchesSnapshot()))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitForBatchCount waits until the recorder holds at least n batches.
func waitForBatchCount(t *testing.T, rec *batchRecorder, n int, timeout time.Duration) {
	waitForBatch(t, rec, n, timeout)
}

// batchesSnapshot returns a copy of the recorded batches.
func (r *batchRecorder) batchesSnapshot() [][]uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]uint64, len(r.batches))
	for i, b := range r.batches {
		out[i] = append([]uint64(nil), b...)
	}
	return out
}
