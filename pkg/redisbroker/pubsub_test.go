package redisbroker

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/config"
	"github.com/messageloopio/messageloop/pkg/topics"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	"github.com/stretchr/testify/require"
)

// newTestRedisBroker builds a redisBroker without any Redis connection; only
// the subscription bookkeeping (subscribed/wcCounts/wcHandles/matcher) is
// exercised by unit tests.
func newTestRedisBroker() *redisBroker {
	return &redisBroker{
		opts:           NewOptions(config.RedisConfig{}),
		subscribed:     make(map[string]int),
		wcCounts:       make(map[string]int),
		wcHandles:      make(map[string]*topics.Subscription),
		matcher:        topics.NewCSTrieMatcher(),
		lastOffsets:    make(map[string]uint64),
		lastSeqs:       make(map[string]uint64),
		liveOps:        make(chan liveOp, liveOpsBufferSize),
		liveDesired:    make(map[string]struct{}),
		liveActive:     make(map[string]struct{}),
		pendingLiveOps: make(map[string][]chan struct{}),
	}
}

func TestRedisBroker_Interested_Wildcard(t *testing.T) {
	b := newTestRedisBroker()
	require.NoError(t, b.Subscribe("forex.*"))
	if !b.interested("forex.eur") {
		t.Fatal("wildcard pattern should match concrete channel")
	}
	if b.interested("stocks.us") {
		t.Fatal("unrelated channel should not match")
	}
}

func TestRedisBroker_Unsubscribe_RefCount(t *testing.T) {
	b := newTestRedisBroker()
	require.NoError(t, b.Subscribe("forex.*"))
	require.NoError(t, b.Subscribe("forex.*")) // second subscriber
	require.NoError(t, b.Unsubscribe("forex.*"))
	if !b.interested("forex.eur") {
		t.Fatal("pattern must stay subscribed while refcount > 0")
	}
	require.NoError(t, b.Unsubscribe("forex.*"))
	if b.interested("forex.eur") {
		t.Fatal("pattern must be removed when refcount reaches 0")
	}
}

func TestRedisBroker_Subscribe_ExactRefCount(t *testing.T) {
	b := newTestRedisBroker()
	require.NoError(t, b.Subscribe("forex.eur"))
	require.NoError(t, b.Subscribe("forex.eur"))
	require.NoError(t, b.Unsubscribe("forex.eur"))
	if !b.interested("forex.eur") {
		t.Fatal("exact channel must stay subscribed while refcount > 0")
	}
	require.NoError(t, b.Unsubscribe("forex.eur"))
	if b.interested("forex.eur") {
		t.Fatal("exact channel must be removed when refcount reaches 0")
	}
	if b.interested("forex.usd") {
		t.Fatal("sibling channel must not match an exact subscription")
	}
}

// TestRedisBroker_DeliverOnce_DeduplicatesByOffset verifies that the same
// offset delivered twice (live delivery racing reconnect catch-up) reaches
// the handler exactly once, while a new offset always goes through.
func TestRedisBroker_DeliverOnce_DeduplicatesByOffset(t *testing.T) {
	b := newTestRedisBroker()
	var delivered []uint64
	b.handler = func(_ string, pub *messageloop.Publication) error {
		delivered = append(delivered, pub.Offset)
		return nil
	}

	b.deliverOnce("ch", &messageloop.Publication{Offset: 10})
	b.deliverOnce("ch", &messageloop.Publication{Offset: 10})
	b.deliverOnce("ch", &messageloop.Publication{Offset: 11})
	b.deliverOnce("other", &messageloop.Publication{Offset: 10})

	require.Equal(t, []uint64{10, 11, 10}, delivered)
	require.Equal(t, map[string]uint64{"ch": 11, "other": 10}, b.lastOffsets)
}

// TestRedisBroker_DeliverOnce_TransientDeliversUnconditionally verifies that
// offset-0 (transient) publications bypass deduplication.
func TestRedisBroker_DeliverOnce_TransientDeliversUnconditionally(t *testing.T) {
	b := newTestRedisBroker()
	var delivered int
	b.handler = func(string, *messageloop.Publication) error {
		delivered++
		return nil
	}
	for i := 0; i < 3; i++ {
		b.deliverOnce("ch", &messageloop.Publication{Offset: 0})
	}
	require.Equal(t, 3, delivered)
	require.NotContains(t, b.lastOffsets, "ch")
}

// TestRedisBroker_DeliverOnce_HandlerPanicIsContained verifies P1-C3: a
// panicking handler must be recovered into a logged error instead of taking
// down the pub/sub consumer goroutine.
func TestRedisBroker_DeliverOnce_HandlerPanicIsContained(t *testing.T) {
	b := newTestRedisBroker()
	b.handler = func(string, *messageloop.Publication) error {
		panic("injected panic")
	}

	require.NotPanics(t, func() {
		b.deliverOnce("ch", &messageloop.Publication{Offset: 1})
	})
	require.EqualValues(t, 1, b.handlerFailures.Load())

	// The broker keeps working after the panic: the offset is recorded and
	// the next delivery still goes through.
	require.Equal(t, map[string]uint64{"ch": 1}, b.lastOffsets)
}

// TestRedisBroker_DeliverOnce_HandlerErrorCountedNotPropagated verifies
// P1-C3: handler errors are counted and logged, never propagated to Publish
// callers (the Redis broker is an asynchronous delivery implementation).
func TestRedisBroker_DeliverOnce_HandlerErrorCountedNotPropagated(t *testing.T) {
	b := newTestRedisBroker()
	b.handler = func(string, *messageloop.Publication) error {
		return errors.New("injected delivery error")
	}

	require.NotPanics(t, func() {
		b.deliverOnce("ch", &messageloop.Publication{Offset: 5})
	})
	require.EqualValues(t, 1, b.handlerFailures.Load())
}

// TestRedisBroker_DeliverOnce_NilHandlerNoOp verifies the nil-handler guard.
func TestRedisBroker_DeliverOnce_NilHandlerNoOp(t *testing.T) {
	b := newTestRedisBroker()
	require.NotPanics(t, func() {
		b.deliverOnce("ch", &messageloop.Publication{Offset: 7})
		b.deliverOnce("ch", &messageloop.Publication{Offset: 0})
	})
	require.EqualValues(t, 0, b.handlerFailures.Load())
}

// TestRedisBroker_CrossChannelDeliveryNotSerialized verifies P1-C2: a slow
// handler on one channel must not block real-time delivery on other channels
// (previously the handler ran inside the global deliverMu critical section).
func TestRedisBroker_CrossChannelDeliveryNotSerialized(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)

	brokerA := New(redisCfg).(*redisBroker)
	brokerB := New(redisCfg).(*redisBroker)
	t.Cleanup(func() { _ = brokerB.client.Close() })

	const slowCh = "serialization-slow"
	const fastCh = "serialization-fast"
	// The two channels must land on different workers, otherwise the fast
	// channel shares the slow handler's worker and legitimately queues
	// behind it.
	require.NotEqual(t, deliveryWorkerIndex(slowCh), deliveryWorkerIndex(fastCh))
	slowEntered := make(chan struct{}, 1)
	releaseSlow := make(chan struct{})
	delivered := make(chan string, 8)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan error, 1)
	go func() {
		started <- brokerA.Start(ctx, func(ch string, _ *messageloop.Publication) error {
			if ch == slowCh {
				select {
				case slowEntered <- struct{}{}:
				default:
				}
				<-releaseSlow
			}
			delivered <- ch
			return nil
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-started:
		case <-time.After(3 * time.Second):
		}
	})

	require.NoError(t, brokerA.Subscribe(slowCh))
	require.NoError(t, brokerA.Subscribe(fastCh))
	select {
	case <-brokerA.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("broker never became ready")
	}

	// Enter a slow handler on the first channel.
	_, err := brokerB.Publish(slowCh, &messageloop.Publication{Payload: []byte("slow"), Kind: messageloop.PayloadKindBinary})
	require.NoError(t, err)
	select {
	case <-slowEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("slow handler never entered")
	}

	// A publication on a different channel must be delivered while the slow
	// handler is still inside (previously it queued behind deliverMu).
	startTime := time.Now()
	_, err = brokerB.Publish(fastCh, &messageloop.Publication{Payload: []byte("fast"), Kind: messageloop.PayloadKindBinary})
	require.NoError(t, err)
	select {
	case ch := <-delivered:
		require.Equal(t, fastCh, ch)
		require.Less(t, time.Since(startTime), 2*time.Second,
			"fast-channel delivery must not wait for the slow handler")
	case <-time.After(2 * time.Second):
		t.Fatal("fast channel delivery blocked behind slow handler")
	}

	close(releaseSlow)
}

// TestRedisBroker_CatchUpGapDetected verifies P1-C4: when the stream holds
// more entries than XRangeN's cap since the delivery baseline, the truncated
// tail is surfaced as a detected gap instead of failing silently.
func TestRedisBroker_CatchUpGapDetected(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)

	brokerA := New(redisCfg).(*redisBroker)
	brokerB := New(redisCfg).(*redisBroker)
	t.Cleanup(func() { _ = brokerB.client.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan error, 1)
	go func() {
		started <- brokerA.Start(ctx, func(string, *messageloop.Publication) error { return nil })
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-started:
		case <-time.After(3 * time.Second):
		}
	})

	require.NoError(t, brokerA.Subscribe("gap-ch"))
	select {
	case <-brokerA.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("broker never became ready")
	}

	// Seed a delivery baseline.
	_, err := brokerB.Publish("gap-ch", &messageloop.Publication{Payload: []byte("seed"), Kind: messageloop.PayloadKindBinary})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		brokerA.subMu.RLock()
		last := brokerA.lastOffsets["gap-ch"]
		brokerA.subMu.RUnlock()
		return last > 0
	}, 5*time.Second, 25*time.Millisecond)

	// Shrink the catch-up window so the replay cap is smaller than the
	// number of missed messages.
	originalMaxLen := brokerA.opts.StreamMaxLength
	brokerA.opts.StreamMaxLength = 2
	t.Cleanup(func() { brokerA.opts.StreamMaxLength = originalMaxLen })

	// Publish more messages than the capped window while disconnected.
	brokerA.pubsubMu.Lock()
	if brokerA.activePubSub != nil {
		_ = brokerA.activePubSub.Close()
	}
	brokerA.pubsubMu.Unlock()
	require.Eventually(t, func() bool {
		brokerA.pubsubMu.Lock()
		defer brokerA.pubsubMu.Unlock()
		return brokerA.activePubSub == nil
	}, 5*time.Second, 25*time.Millisecond)

	for i := 0; i < 5; i++ {
		_, err := brokerB.Publish("gap-ch", &messageloop.Publication{Payload: []byte("missed"), Kind: messageloop.PayloadKindBinary})
		require.NoError(t, err)
	}

	// The reconnect catch-up replays at most 2 entries; the newest stream
	// entries cannot be replayed, so the gap must be detected and counted.
	require.Eventually(t, func() bool {
		return brokerA.catchUpGaps.Load() >= 1
	}, 15*time.Second, 50*time.Millisecond)
}

// TestRedisBroker_WildcardReceivesPublication_Redis verifies wildcard
// subscriptions receive real-time publications over Redis Pub/Sub, and that
// the interest disappears only when the reference count reaches zero.
func TestRedisBroker_WildcardReceivesPublication_Redis(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)

	brokerA := New(redisCfg).(*redisBroker)
	brokerB := New(redisCfg).(*redisBroker)
	t.Cleanup(func() { _ = brokerB.client.Close() })

	received := make(chan string, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startErr := make(chan error, 1)
	go func() {
		startErr <- brokerA.Start(ctx, func(ch string, _ *messageloop.Publication) error {
			received <- ch
			return nil
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-startErr:
		case <-time.After(3 * time.Second):
		}
	})

	require.NoError(t, brokerA.Subscribe("forex.*"))
	require.NoError(t, brokerA.Subscribe("forex.*")) // second subscriber

	// Wait until the pub/sub subscription is live: subscription setup is
	// asynchronous, so retry publishes until the consumer confirms delivery.
	var first string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, err := brokerB.Publish("forex.eur", &messageloop.Publication{Payload: []byte("tick-1"), Kind: messageloop.PayloadKindBinary})
		require.NoError(t, err)
		select {
		case first = <-received:
		case <-time.After(300 * time.Millisecond):
			continue
		}
		break
	}
	if first == "" {
		t.Fatal("wildcard subscriber did not receive publication within timeout")
	}
	require.Equal(t, "forex.eur", first)

	// Unsubscribe once: refcount stays above zero, interest must remain.
	require.NoError(t, brokerA.Unsubscribe("forex.*"))
	_, err := brokerB.Publish("forex.eur", &messageloop.Publication{Payload: []byte("tick-2"), Kind: messageloop.PayloadKindBinary})
	require.NoError(t, err)
	select {
	case ch := <-received:
		require.Equal(t, "forex.eur", ch, "pattern must stay subscribed while refcount > 0")
	case <-time.After(3 * time.Second):
		t.Fatal("wildcard subscriber should still receive while refcount > 0")
	}

	// Unsubscribe again: refcount reaches zero, interest must be dropped.
	require.NoError(t, brokerA.Unsubscribe("forex.*"))
	_, err = brokerB.Publish("forex.eur", &messageloop.Publication{Payload: []byte("tick-3"), Kind: messageloop.PayloadKindBinary})
	require.NoError(t, err)
	select {
	case ch := <-received:
		t.Fatalf("pattern must not receive after refcount reaches 0, got %s", ch)
	case <-time.After(1500 * time.Millisecond):
	}
}

// liveActiveNames returns a sorted snapshot of the names currently subscribed
// on the broker's active pub/sub connection.
func liveActiveNames(b *redisBroker) []string {
	b.pubsubMu.Lock()
	defer b.pubsubMu.Unlock()
	names := make([]string, 0, len(b.liveActive))
	for name := range b.liveActive {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// waitLiveActive waits until the broker's active connection subscribes
// exactly want (sorted) and returns the last observed set.
func waitLiveActive(t *testing.T, b *redisBroker, want []string) []string {
	t.Helper()
	var got []string
	require.Eventually(t, func() bool {
		got = liveActiveNames(b)
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}, 5*time.Second, 25*time.Millisecond)
	return got
}

// startLiveTestBrokers starts a consumer broker A plus a publisher broker B
// sharing the test Redis; publications received by A's handler are pushed to
// the returned channel.
func startLiveTestBrokers(t *testing.T) (*redisBroker, *redisBroker, chan string) {
	t.Helper()
	redisCfg := requireCommandBusRedis(t)
	brokerA := New(redisCfg).(*redisBroker)
	brokerB := New(redisCfg).(*redisBroker)
	t.Cleanup(func() { _ = brokerB.client.Close() })

	received := make(chan string, 32)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan error, 1)
	go func() {
		started <- brokerA.Start(ctx, func(ch string, _ *messageloop.Publication) error {
			received <- ch
			return nil
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-started:
		case <-time.After(3 * time.Second):
		}
	})
	require.NoError(t, brokerA.Subscribe("__probe__.ready"))
	waitLiveActive(t, brokerA, []string{brokerA.opts.PubSubPrefix + "__probe__.ready"})
	require.NoError(t, brokerA.Unsubscribe("__probe__.ready"))
	waitLiveActive(t, brokerA, nil)
	return brokerA, brokerB, received
}

// expectReceived asserts the next handler delivery is channel want (draining
// any earlier deliveries first).
func expectReceived(t *testing.T, received <-chan string, want string) {
	t.Helper()
	select {
	case ch := <-received:
		require.Equal(t, want, ch)
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for delivery on %q", want)
	}
}

// expectNoDelivery asserts no handler delivery arrives within wait.
func expectNoDelivery(t *testing.T, received <-chan string, wait time.Duration) {
	t.Helper()
	select {
	case ch := <-received:
		t.Fatalf("unexpected delivery on %q", ch)
	case <-time.After(wait):
	}
}

// TestRedisBroker_LiveSubscription_CompiledOnly pins A3 §8-3: the live
// subscription set is exactly the compiled interest — after subscribing only
// chat.1 the connection holds no glob pattern at all (no PSubscribe(prefix+*)
// fallback), and publications on unrelated channels never reach the handler.
func TestRedisBroker_LiveSubscription_CompiledOnly(t *testing.T) {
	brokerA, brokerB, received := startLiveTestBrokers(t)

	require.NoError(t, brokerA.Subscribe("chat.1"))
	names := waitLiveActive(t, brokerA, []string{brokerA.opts.PubSubPrefix + "chat.1"})
	for _, name := range names {
		require.NotContains(t, name, "*", "no glob subscription may exist for an exact-only interest")
	}

	// chat.1 is delivered...
	_, err := brokerB.Publish("chat.1", &messageloop.Publication{Payload: []byte("m1"), Kind: messageloop.PayloadKindBinary})
	require.NoError(t, err)
	expectReceived(t, received, "chat.1")

	// ...while a publish on an unsubscribed channel never reaches the handler
	// (previously it arrived via PSubscribe(prefix+"*") and was dropped only
	// after receipt).
	_, err = brokerB.Publish("stocks.1", &messageloop.Publication{Payload: []byte("m2"), Kind: messageloop.PayloadKindBinary})
	require.NoError(t, err)
	expectNoDelivery(t, received, 1500*time.Millisecond)
}

// TestRedisBroker_LiveSubscription_ImDoubleStar pins A3 §8-3: Subscribe("im.**")
// compiles to the pattern im.* plus the exact channel im (zero-segment case),
// so publications on "im" and "im.x" reach the handler while unrelated
// channels never do.
func TestRedisBroker_LiveSubscription_ImDoubleStar(t *testing.T) {
	brokerA, brokerB, received := startLiveTestBrokers(t)

	require.NoError(t, brokerA.Subscribe("im.**"))
	waitLiveActive(t, brokerA, []string{
		brokerA.opts.PubSubPrefix + "im",
		brokerA.opts.PubSubPrefix + "im.*",
	})

	_, err := brokerB.Publish("im", &messageloop.Publication{Payload: []byte("z"), Kind: messageloop.PayloadKindBinary})
	require.NoError(t, err)
	expectReceived(t, received, "im")

	_, err = brokerB.Publish("im.x", &messageloop.Publication{Payload: []byte("x"), Kind: messageloop.PayloadKindBinary})
	require.NoError(t, err)
	expectReceived(t, received, "im.x")

	_, err = brokerB.Publish("im.a.b.c", &messageloop.Publication{Payload: []byte("d"), Kind: messageloop.PayloadKindBinary})
	require.NoError(t, err)
	expectReceived(t, received, "im.a.b.c")

	_, err = brokerB.Publish("stocks", &messageloop.Publication{Payload: []byte("s"), Kind: messageloop.PayloadKindBinary})
	require.NoError(t, err)
	expectNoDelivery(t, received, 1500*time.Millisecond)
}

// TestRedisBroker_LiveSubscription_ImRoomStarLocalMatch pins hard constraint
// 4: Subscribe("im.room.*") must deliver im.room.a but NOT im.room.a.b — the
// Redis glob matches the deeper channel too, so the local segment-level Match
// must discard it.
func TestRedisBroker_LiveSubscription_ImRoomStarLocalMatch(t *testing.T) {
	brokerA, brokerB, received := startLiveTestBrokers(t)

	require.NoError(t, brokerA.Subscribe("im.room.*"))
	waitLiveActive(t, brokerA, []string{brokerA.opts.PubSubPrefix + "im.room.*"})

	_, err := brokerB.Publish("im.room.a", &messageloop.Publication{Payload: []byte("a"), Kind: messageloop.PayloadKindBinary})
	require.NoError(t, err)
	expectReceived(t, received, "im.room.a")

	_, err = brokerB.Publish("im.room.a.b", &messageloop.Publication{Payload: []byte("b"), Kind: messageloop.PayloadKindBinary})
	require.NoError(t, err)
	expectNoDelivery(t, received, 1500*time.Millisecond)

	_, err = brokerB.Publish("im.other.a", &messageloop.Publication{Payload: []byte("o"), Kind: messageloop.PayloadKindBinary})
	require.NoError(t, err)
	expectNoDelivery(t, received, 500*time.Millisecond)
}

// TestRedisBroker_LiveSubscription_ReconnectRebuildsInterest pins A3 §8-4:
// after the active pub/sub connection is dropped, the reconnect rebuilds
// exactly the compiled interest (still no glob for unrelated traffic) and
// only interested channels are delivered.
func TestRedisBroker_LiveSubscription_ReconnectRebuildsInterest(t *testing.T) {
	brokerA, brokerB, received := startLiveTestBrokers(t)

	require.NoError(t, brokerA.Subscribe("im.**"))
	waitLiveActive(t, brokerA, []string{
		brokerA.opts.PubSubPrefix + "im",
		brokerA.opts.PubSubPrefix + "im.*",
	})

	// Drop the connection and wait for the teardown to complete.
	brokerA.pubsubMu.Lock()
	if brokerA.activePubSub != nil {
		_ = brokerA.activePubSub.Close()
	}
	brokerA.pubsubMu.Unlock()
	require.Eventually(t, func() bool {
		brokerA.pubsubMu.Lock()
		defer brokerA.pubsubMu.Unlock()
		return brokerA.activePubSub == nil
	}, 5*time.Second, 25*time.Millisecond)

	// After the reconnect the same compiled interest must be rebuilt: the
	// pattern plus the zero-segment exact channel, and no bare prefix+"*".
	waitLiveActive(t, brokerA, []string{
		brokerA.opts.PubSubPrefix + "im",
		brokerA.opts.PubSubPrefix + "im.*",
	})
	for _, name := range liveActiveNames(brokerA) {
		require.NotEqual(t, brokerA.opts.PubSubPrefix+"*", name,
			"rebuilt live set must never contain the bare wildcard subscription")
	}

	_, err := brokerB.Publish("im.x", &messageloop.Publication{Payload: []byte("x"), Kind: messageloop.PayloadKindBinary})
	require.NoError(t, err)
	expectReceived(t, received, "im.x")

	_, err = brokerB.Publish("stocks", &messageloop.Publication{Payload: []byte("s"), Kind: messageloop.PayloadKindBinary})
	require.NoError(t, err)
	expectNoDelivery(t, received, 1500*time.Millisecond)
}

// TestRedisBroker_LiveSubscription_DynamicRemove pins A3 §5.3: when the last
// subscriber of a pattern leaves, the compiled Redis subscription is removed
// from the live connection.
func TestRedisBroker_LiveSubscription_DynamicRemove(t *testing.T) {
	brokerA, brokerB, received := startLiveTestBrokers(t)

	require.NoError(t, brokerA.Subscribe("im.**"))
	waitLiveActive(t, brokerA, []string{
		brokerA.opts.PubSubPrefix + "im",
		brokerA.opts.PubSubPrefix + "im.*",
	})

	require.NoError(t, brokerA.Unsubscribe("im.**"))
	waitLiveActive(t, brokerA, nil)

	_, err := brokerB.Publish("im.x", &messageloop.Publication{Payload: []byte("x"), Kind: messageloop.PayloadKindBinary})
	require.NoError(t, err)
	expectNoDelivery(t, received, 1500*time.Millisecond)
}

// TestRedisBroker_LiveSubscription_OccupancyFollowsInterest pins B2 §5.2:
// an occupancy publish on im.room.1 reaches the occupancy handler of a node
// whose compiled interest covers it (im.**), never the publication handler,
// and carries its gen unchanged. Occupancy has no stream offset, so it is
// not deduplicated by deliverOnce.
func TestRedisBroker_LiveSubscription_OccupancyFollowsInterest(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	brokerA := New(redisCfg).(*redisBroker)
	brokerB := New(redisCfg).(*redisBroker)
	t.Cleanup(func() { _ = brokerB.client.Close() })

	occA := make(chan messageloop.OccupancyEvent, 8)
	pubA := make(chan string, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan error, 1)
	require.NoError(t, brokerA.SetOccupancyHandler(func(_ string, evt messageloop.OccupancyEvent) error {
		occA <- evt
		return nil
	}))
	go func() {
		started <- brokerA.Start(ctx, func(ch string, _ *messageloop.Publication) error {
			pubA <- ch
			return nil
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-started:
		case <-time.After(3 * time.Second):
		}
	})

	require.NoError(t, brokerA.Subscribe("im.**"))
	waitLiveActive(t, brokerA, []string{brokerA.opts.PubSubPrefix + "im", brokerA.opts.PubSubPrefix + "im.*"})

	// A real publication still reaches the publication handler.
	_, err := brokerB.Publish("im.room.1", &messageloop.Publication{Payload: []byte("m1"), Kind: messageloop.PayloadKindBinary})
	require.NoError(t, err)
	expectReceived(t, pubA, "im.room.1")

	// A live occupancy event reaches only the occupancy handler, gen intact.
	require.NoError(t, brokerB.PublishOccupancy("im.room.1", messageloop.OccupancyEvent{
		Gen:   7,
		Event: &clientpb.PresenceEvent{Action: "join", Info: &clientpb.PresenceInfo{SessionId: "sess-x"}},
	}))
	select {
	case evt := <-occA:
		require.Equal(t, uint64(7), evt.Gen, "the gen must survive the live bus untouched")
		require.Equal(t, "join", evt.Event.GetAction())
	case <-time.After(5 * time.Second):
		t.Fatal("the interested node's occupancy handler did not receive im.room.1 join")
	}
	expectNoDelivery(t, pubA, 300*time.Millisecond)
}

// TestRedisBroker_LiveSubscription_OccupancyNotInterested pins B2 §8.3: a
// node subscribed only to chat.1 never invokes its occupancy handler for an
// im.room.1 event, and its publication handler stays untouched.
func TestRedisBroker_LiveSubscription_OccupancyNotInterested(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	brokerA := New(redisCfg).(*redisBroker)
	brokerB := New(redisCfg).(*redisBroker)
	t.Cleanup(func() { _ = brokerB.client.Close() })

	occA := make(chan messageloop.OccupancyEvent, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan error, 1)
	require.NoError(t, brokerA.SetOccupancyHandler(func(_ string, evt messageloop.OccupancyEvent) error {
		occA <- evt
		return nil
	}))
	go func() { started <- brokerA.Start(ctx, func(string, *messageloop.Publication) error { return nil }) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-started:
		case <-time.After(3 * time.Second):
		}
	})

	require.NoError(t, brokerA.Subscribe("chat.1"))
	waitLiveActive(t, brokerA, []string{brokerA.opts.PubSubPrefix + "chat.1"})

	require.NoError(t, brokerB.PublishOccupancy("im.room.1", messageloop.OccupancyEvent{
		Gen:   3,
		Event: &clientpb.PresenceEvent{Action: "join", Info: &clientpb.PresenceInfo{SessionId: "sess-y"}},
	}))
	select {
	case evt := <-occA:
		t.Fatalf("a node without im-tree interest must not receive im.room.1 occupancy (got gen %d)", evt.Gen)
	case <-time.After(1 * time.Second):
	}
}

// TestRedisBroker_Subscribe_RejectsUnroutable pins A3 §8-2 on the Redis side:
// unroutable patterns and bare wildcards are refused up front and leave no
// live subscription behind.
func TestRedisBroker_Subscribe_RejectsUnroutable(t *testing.T) {
	b := newTestRedisBroker()
	for _, ch := range []string{"*.room", "**", "*", "im.*.tick"} {
		err := b.Subscribe(ch)
		require.ErrorIs(t, err, messageloop.ErrPatternNotRoutable, "channel %q", ch)
	}
	err := b.Subscribe("a..b")
	require.ErrorIs(t, err, topics.ErrBadTopic)
	require.Empty(t, liveActiveNames(b))
}

// TestRedisBroker_DeliverOnce_RecordsDenseSeqBaseline verifies C4: deliverOnce
// advances lastSeqs in parallel with lastOffsets for sequenced publications,
// legacy (Seq=0) publications leave the seq baseline untouched, and
// Unsubscribe drops both baselines.
func TestRedisBroker_DeliverOnce_RecordsDenseSeqBaseline(t *testing.T) {
	b := newTestRedisBroker()
	require.NoError(t, b.Subscribe("ch"))

	b.deliverOnce("ch", &messageloop.Publication{Offset: 10, Seq: 3})
	b.deliverOnce("ch", &messageloop.Publication{Offset: 11, Seq: 4})
	// A legacy publication (no dense seq) advances only the offset baseline.
	b.deliverOnce("ch", &messageloop.Publication{Offset: 12})
	require.Equal(t, map[string]uint64{"ch": 12}, b.lastOffsets)
	require.Equal(t, map[string]uint64{"ch": 4}, b.lastSeqs)

	require.NoError(t, b.Unsubscribe("ch"))
	require.NotContains(t, b.lastOffsets, "ch")
	require.NotContains(t, b.lastSeqs, "ch", "Unsubscribe must drop the dense seq baseline too")
}

// TestRedisBroker_CatchUpMissed_MiddleGapCounted verifies C4 §7.6: with a
// dense-seq delivery baseline recorded, an entry deleted (XDEL) from the
// missed range is detected during catch-up via the seq discontinuity and
// counted in catchUpGaps. The catch-up is invoked directly — no sleeps.
func TestRedisBroker_CatchUpMissed_MiddleGapCounted(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	broker := New(redisCfg).(*redisBroker)
	t.Cleanup(func() { _ = broker.client.Close() })

	ch := "catchup-middle-gap"
	require.NoError(t, broker.Subscribe(ch))

	// Four entries with dense seqs 1..4.
	var firstPub *messageloop.Publication
	for i := 0; i < 4; i++ {
		pub := &messageloop.Publication{Payload: []byte{byte('a' + i)}, Kind: messageloop.PayloadKindBinary}
		_, err := broker.Publish(ch, pub)
		require.NoError(t, err)
		if i == 0 {
			firstPub = pub
		}
	}

	// Delivery baseline: the first entry was delivered live (offset + seq 1).
	var delivered []uint64
	broker.handler = func(_ string, pub *messageloop.Publication) error {
		delivered = append(delivered, pub.Seq)
		return nil
	}
	broker.deliverOnce(ch, &messageloop.Publication{Channel: ch, Offset: firstPub.Offset, Seq: firstPub.Seq})
	require.Equal(t, []uint64{1}, delivered)

	// XDEL the seq=3 entry: the replay reads seq 2 and 4, a middle hole.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msgs, err := broker.client.XRangeN(ctx, broker.opts.StreamPrefix+ch, "-", "+", 10).Result()
	require.NoError(t, err)
	require.Len(t, msgs, 4)
	deleted, err := broker.client.XDel(ctx, broker.opts.StreamPrefix+ch, msgs[2].ID).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted)

	broker.catchUpMissed(context.Background())
	require.Equal(t, []uint64{1, 2, 4}, delivered, "the surviving missed entries are replayed")
	require.EqualValues(t, 1, broker.catchUpGaps.Load(), "the XDEL'd middle entry must be detected via the dense seq")
}

// TestRedisBroker_CatchUpMissed_LegacyBaselineNoFalsePositive verifies C4:
// without a dense-seq baseline (legacy node / pre-C4 traffic), catch-up
// skips the middle-gap check entirely — missing entries are not libeled.
func TestRedisBroker_CatchUpMissed_LegacyBaselineNoFalsePositive(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	broker := New(redisCfg).(*redisBroker)
	t.Cleanup(func() { _ = broker.client.Close() })

	ch := "catchup-legacy-baseline"
	require.NoError(t, broker.Subscribe(ch))

	var firstOffset uint64
	for i := 0; i < 3; i++ {
		offset, err := broker.Publish(ch, &messageloop.Publication{Payload: []byte{byte('a' + i)}, Kind: messageloop.PayloadKindBinary})
		require.NoError(t, err)
		if i == 0 {
			firstOffset = offset
		}
	}

	// Baseline with an offset only, no dense seq (legacy bookkeeping).
	broker.deliverOnce(ch, &messageloop.Publication{Channel: ch, Offset: firstOffset})

	// Delete the middle missed entry; without a seq baseline the hole is not
	// detectable and must not be reported.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msgs, err := broker.client.XRangeN(ctx, broker.opts.StreamPrefix+ch, "-", "+", 10).Result()
	require.NoError(t, err)
	require.Len(t, msgs, 3)
	_, err = broker.client.XDel(ctx, broker.opts.StreamPrefix+ch, msgs[1].ID).Result()
	require.NoError(t, err)

	broker.catchUpMissed(context.Background())
	require.EqualValues(t, 0, broker.catchUpGaps.Load(), "a legacy (seq-less) baseline must never produce a middle-gap report")
}
