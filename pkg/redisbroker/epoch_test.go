package redisbroker

import (
	"context"
	"testing"
	"time"

	"github.com/messageloopio/messageloop"
	"github.com/stretchr/testify/require"
)

// TestRedisBroker_Epoch_SharedAcrossNodes verifies that brokers connected to
// the same Redis agree on one cluster-wide epoch.
func TestRedisBroker_Epoch_SharedAcrossNodes(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)

	brokerA := New(redisCfg).(*redisBroker)
	brokerB := New(redisCfg).(*redisBroker)
	t.Cleanup(func() { _ = brokerB.client.Close() })

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	startA := make(chan error, 1)
	go func() { startA <- brokerA.Start(ctxA, func(string, *messageloop.Publication) error { return nil }) }()
	defer func() { cancelA() }()

	// Wait until broker A has initialized its epoch (Start orders initEpoch
	// before runPubSub; once the pub/sub loop runs the epoch is set).
	waitForEpoch(t, brokerA)

	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	startB := make(chan error, 1)
	go func() { startB <- brokerB.Start(ctxB, func(string, *messageloop.Publication) error { return nil }) }()
	defer func() { cancelB() }()
	waitForEpoch(t, brokerB)

	require.NotEmpty(t, brokerA.Epoch())
	require.Equal(t, brokerA.Epoch(), brokerB.Epoch(), "epoch must be shared across nodes")
}

// TestRedisBroker_Epoch_PersistedAcrossRestart verifies the epoch survives a
// broker restart: restarting a node must not trigger a full recovery.
func TestRedisBroker_Epoch_PersistedAcrossRestart(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)

	ctxA, cancelA := context.WithCancel(context.Background())
	brokerA := New(redisCfg).(*redisBroker)
	startA := make(chan error, 1)
	go func() { startA <- brokerA.Start(ctxA, func(string, *messageloop.Publication) error { return nil }) }()
	waitForEpoch(t, brokerA)
	epochA := brokerA.Epoch()
	require.NotEmpty(t, epochA)

	// "Restart": stop broker A (Start returns on ctx cancel and closes the
	// client) and start a fresh broker B against the same Redis.
	cancelA()
	select {
	case <-startA:
	case <-time.After(3 * time.Second):
		t.Fatal("broker A did not stop")
	}

	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	brokerB := New(redisCfg).(*redisBroker)
	startB := make(chan error, 1)
	go func() { startB <- brokerB.Start(ctxB, func(string, *messageloop.Publication) error { return nil }) }()
	defer func() { cancelB() }()
	waitForEpoch(t, brokerB)

	require.Equal(t, epochA, brokerB.Epoch(), "epoch must persist across restart")
}

func waitForEpoch(t *testing.T, b *redisBroker) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b.Epoch() != "" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("broker epoch was not initialized in time")
}

// TestRedisBroker_Epoch_ConcurrentInit verifies that concurrent epoch
// initialization converges on a single value (SET NX semantics) and that all
// readers observe the same epoch.
func TestRedisBroker_Epoch_ConcurrentInit(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)

	const nodes = 8
	brokers := make([]*redisBroker, nodes)
	for i := range brokers {
		brokers[i] = New(redisCfg).(*redisBroker)
	}
	t.Cleanup(func() {
		for _, b := range brokers {
			_ = b.client.Close()
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan error, nodes)
	for _, b := range brokers {
		go func(b *redisBroker) {
			started <- b.Start(ctx, func(string, *messageloop.Publication) error { return nil })
		}(b)
	}
	for _, b := range brokers {
		waitForEpoch(t, b)
	}

	epoch := brokers[0].Epoch()
	require.NotEmpty(t, epoch)
	for _, b := range brokers[1:] {
		require.Equal(t, epoch, b.Epoch(), "all nodes must observe the same epoch")
	}
}
