package redisbroker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/config"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// failPublishHook injects an error for every PUBLISH command.
type failPublishHook struct{}

func (failPublishHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (failPublishHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "publish" {
			return errors.New("injected publish failure")
		}
		return next(ctx, cmd)
	}
}
func (failPublishHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// TestRedisBroker_Publish_PubSubFailureRollsBackStream verifies that a failed
// PUBLISH rolls back the stream entry (XDEL) so history stays consistent with
// what was actually delivered in real time.
func TestRedisBroker_Publish_PubSubFailureRollsBackStream(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	broker := New(redisCfg).(*redisBroker)
	t.Cleanup(func() { _ = broker.client.Close() })
	broker.client.AddHook(failPublishHook{})

	ch := "publish-rollback"
	_, err := broker.Publish(ch, []byte("msg"), false)
	require.Error(t, err, "publish must fail when the pub/sub delivery fails")

	stream := broker.opts.StreamPrefix + ch
	length, lerr := broker.client.XLen(context.Background(), stream).Result()
	require.NoError(t, lerr)
	require.Zero(t, length, "stream entry must be rolled back after a pubsub failure")
}

// requireCommandBusRedis lives in cluster_command_bus_test.go; this test file
// re-uses it via the package-level helper. Keep a compile-time reference to
// config so imports stay stable.
var _ = config.RedisConfig{}

// TestRedisBroker_Ready_ClosesAfterSubscribe verifies the Ready signal: not
// closed before Start, closed once the pub/sub subscription is confirmed.
func TestRedisBroker_Ready_ClosesAfterSubscribe(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	broker := New(redisCfg).(*redisBroker)
	t.Cleanup(func() { _ = broker.client.Close() })

	select {
	case <-broker.Ready():
		t.Fatal("Ready must not be closed before Start")
	default:
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan error, 1)
	go func() { started <- broker.Start(ctx, func(string, *messageloop.Publication) error { return nil }) }()

	select {
	case <-broker.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("Ready must close once the subscription is live")
	}

	cancel()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("broker did not stop")
	}
}
// TestRedisBroker_Reconnect_CatchesUpMissedMessages verifies that messages
// published while the pub/sub connection was down are replayed from the
// stream after the reconnect, without duplicates.
func TestRedisBroker_Reconnect_CatchesUpMissedMessages(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)

	var mu sync.Mutex
	var received []uint64
	brokerA := New(redisCfg).(*redisBroker)
	brokerB := New(redisCfg).(*redisBroker)
	t.Cleanup(func() { _ = brokerB.client.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan error, 1)
	go func() {
		started <- brokerA.Start(ctx, func(_ string, pub *messageloop.Publication) error {
			mu.Lock()
			received = append(received, pub.Offset)
			mu.Unlock()
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

	require.NoError(t, brokerA.Subscribe("catchup-ch"))
	select {
	case <-brokerA.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("broker never became ready")
	}

	waitForOffsets := func(want int) []uint64 {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			n := len(received)
			mu.Unlock()
			if n >= want {
				mu.Lock()
				snapshot := append([]uint64(nil), received...)
				mu.Unlock()
				return snapshot
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %d offsets, have %v", want, received)
		return nil
	}

	// Warm the subscription with three live messages.
	var offsets []uint64
	for i := 0; i < 3; i++ {
		offset, err := brokerB.Publish("catchup-ch", []byte("live"), false)
		require.NoError(t, err)
		offsets = append(offsets, offset)
	}
	waitForOffsets(3)

	// Simulate a disconnect: close the live pub/sub subscription. The retry
	// loop reconnects with 1s backoff.
	brokerA.pubsubMu.Lock()
	if brokerA.activePubSub != nil {
		_ = brokerA.activePubSub.Close()
	}
	brokerA.pubsubMu.Unlock()

	// Publish two more messages while the consumer is disconnected.
	for i := 0; i < 2; i++ {
		offset, err := brokerB.Publish("catchup-ch", []byte("missed"), false)
		require.NoError(t, err)
		offsets = append(offsets, offset)
	}

	// After the reconnect, all five messages must arrive exactly once, in
	// order: the two missed ones are caught up from the stream.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		snapshot := append([]uint64(nil), received...)
		mu.Unlock()
		if len(snapshot) >= 5 {
			require.Equal(t, offsets, snapshot, "every publication must arrive exactly once, in order")
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for catch-up, have %v want %v", received, offsets)
}