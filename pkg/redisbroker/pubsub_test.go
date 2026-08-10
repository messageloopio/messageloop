package redisbroker

import (
	"context"
	"testing"
	"time"

	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/pkg/topics"
	"github.com/stretchr/testify/require"
)

// newTestRedisBroker builds a redisBroker without any Redis connection; only
// the subscription bookkeeping (subscribed/wcCounts/wcHandles/matcher) is
// exercised by unit tests.
func newTestRedisBroker() *redisBroker {
	return &redisBroker{
		subscribed: make(map[string]int),
		wcCounts:   make(map[string]int),
		wcHandles:  make(map[string]*topics.Subscription),
		matcher:    topics.NewCSTrieMatcher(),
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
		_, err := brokerB.Publish("forex.eur", []byte("tick-1"), false)
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
	_, err := brokerB.Publish("forex.eur", []byte("tick-2"), false)
	require.NoError(t, err)
	select {
	case ch := <-received:
		require.Equal(t, "forex.eur", ch, "pattern must stay subscribed while refcount > 0")
	case <-time.After(3 * time.Second):
		t.Fatal("wildcard subscriber should still receive while refcount > 0")
	}

	// Unsubscribe again: refcount reaches zero, interest must be dropped.
	require.NoError(t, brokerA.Unsubscribe("forex.*"))
	_, err = brokerB.Publish("forex.eur", []byte("tick-3"), false)
	require.NoError(t, err)
	select {
	case ch := <-received:
		t.Fatalf("pattern must not receive after refcount reaches 0, got %s", ch)
	case <-time.After(1500 * time.Millisecond):
	}
}
