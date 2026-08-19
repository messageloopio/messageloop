package redisbroker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/messageloopio/messageloop/config"

	"github.com/messageloopio/messageloop/internal/stream"
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

// TestRedisBroker_Publish_PubSubFailureKeepsStream verifies §7.1/§10.7: a
// failed PUBLISH must NOT roll back the stream entry (zero XDel) and Publish
// must still return the assigned offset with err=nil — the stream log already
// accepted the message (KD-K14).
func TestRedisBroker_Publish_PubSubFailureKeepsStream(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	broker := New(redisCfg).(*redisBroker)
	t.Cleanup(func() { _ = broker.client.Close() })
	broker.client.AddHook(failPublishHook{})

	ch := "publish-no-rollback"
	pub := &stream.Publication{Payload: []byte("msg"), Kind: stream.PayloadKindBinary}
	offset, err := broker.Publish(ch, pub)
	require.NoError(t, err, "a pub/sub delivery failure must not negate the publish")
	require.NotZero(t, offset, "the assigned stream offset must be reported")
	require.Equal(t, uint64(1), pub.Seq, "the dense seq must be backfilled onto the publication")

	stream := broker.opts.StreamPrefix + ch
	length, lerr := broker.client.XLen(context.Background(), stream).Result()
	require.NoError(t, lerr)
	require.Equal(t, int64(1), length, "the stream entry must be retained after a pubsub failure")

	// The retained entry carries its dense seq field (C4).
	msgs, rerr := broker.client.XRangeN(context.Background(), stream, "-", "+", 1).Result()
	require.NoError(t, rerr)
	require.Len(t, msgs, 1)
	require.Equal(t, uint64(1), streamEntrySeq(msgs[0]), "the retained stream entry must carry its dense seq")

	// The history page still serves the entry, and its retained marker was
	// written with the entry's own offset.
	page, herr := broker.History(ch, 0, 0)
	require.NoError(t, herr)
	require.Len(t, page.Pubs(), 1)
	require.Equal(t, offset, page.FirstRetained)
}

// TestRedisBroker_Publish_SeqKeyHygiene verifies C4 §7.8: Publish leaves both
// the seq counter key and the stream with a TTL, and PublishTransient never
// creates a seq key.
func TestRedisBroker_Publish_SeqKeyHygiene(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	broker := New(redisCfg).(*redisBroker)
	t.Cleanup(func() { _ = broker.client.Close() })
	ctx := context.Background()

	ch := "seq-key-hygiene"
	_, err := broker.Publish(ch, &stream.Publication{Payload: []byte("m"), Kind: stream.PayloadKindBinary})
	require.NoError(t, err)

	seqKey := broker.opts.StreamPrefix + "seq:" + ch
	streamTTL, err := broker.client.TTL(ctx, broker.opts.StreamPrefix+ch).Result()
	require.NoError(t, err)
	require.Greater(t, streamTTL, time.Duration(0), "the stream must carry a TTL")
	seqTTL, err := broker.client.TTL(ctx, seqKey).Result()
	require.NoError(t, err)
	require.Greater(t, seqTTL, time.Duration(0), "the seq counter key must carry a TTL")
	val, err := broker.client.Get(ctx, seqKey).Result()
	require.NoError(t, err)
	require.Equal(t, "1", val, "the seq counter starts at 1")

	transientCh := "seq-key-transient"
	err = broker.PublishTransient(transientCh, &stream.Publication{Payload: []byte("t"), Kind: stream.PayloadKindBinary})
	require.NoError(t, err)
	exists, err := broker.client.Exists(ctx, broker.opts.StreamPrefix+"seq:"+transientCh).Result()
	require.NoError(t, err)
	require.Zero(t, exists, "PublishTransient must never create a seq key")
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
	go func() { started <- broker.Start(ctx, func(string, *stream.Publication) error { return nil }) }()

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
		started <- brokerA.Start(ctx, func(_ string, pub *stream.Publication) error {
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
		offset, err := brokerB.Publish("catchup-ch", &stream.Publication{Payload: []byte("live"), Kind: stream.PayloadKindBinary})
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

	// Wait until the consumer has actually torn down the subscription before
	// publishing the missed messages, so they cannot be delivered live by the
	// old connection (which would make the catch-up path nondeterministic).
	require.Eventually(t, func() bool {
		brokerA.pubsubMu.Lock()
		defer brokerA.pubsubMu.Unlock()
		return brokerA.activePubSub == nil
	}, 5*time.Second, 25*time.Millisecond)

	// Publish two more messages while the consumer is disconnected.
	for i := 0; i < 2; i++ {
		offset, err := brokerB.Publish("catchup-ch", &stream.Publication{Payload: []byte("missed"), Kind: stream.PayloadKindBinary})
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
