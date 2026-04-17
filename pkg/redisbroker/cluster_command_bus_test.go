package redisbroker

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/config"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

const clusterCommandBusTestDB = 14

func TestClusterCommandBus_DedupesCompletedCommands(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	ctx := context.Background()

	receiver := newTestClusterCommandBus(t, redisCfg, "node-a", "inc-a")
	sender := newTestClusterCommandBus(t, redisCfg, "node-b", "inc-b")

	var handledCount atomic.Int32
	receiver.SetHandler(func(context.Context, *messageloop.ClusterCommand) (*messageloop.ClusterCommandResult, error) {
		handledCount.Add(1)
		return &messageloop.ClusterCommandResult{
			Status: messageloop.ClusterCommandStatusSucceeded,
			Metadata: map[string]string{
				"result": "ok",
			},
		}, nil
	})
	receiver.start(t, ctx)

	firstResult, err := sender.SendCommand(ctx, testClusterCommand("dedupe-complete", "node-a", "inc-a"))
	require.NoError(t, err)
	require.NotNil(t, firstResult)
	require.Equal(t, messageloop.ClusterCommandStatusSucceeded, firstResult.Status)
	require.Equal(t, map[string]string{"result": "ok"}, firstResult.Metadata)

	secondResult, err := sender.SendCommand(ctx, testClusterCommand("dedupe-complete", "node-a", "inc-a"))
	require.NoError(t, err)
	require.NotNil(t, secondResult)
	require.Equal(t, messageloop.ClusterCommandStatusSucceeded, secondResult.Status)
	require.Equal(t, map[string]string{"result": "ok"}, secondResult.Metadata)
	require.EqualValues(t, 1, handledCount.Load())
}

func TestClusterCommandBus_ReturnsInProgressForDuplicatePendingCommand(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	ctx := context.Background()

	receiver := newTestClusterCommandBus(t, redisCfg, "node-a", "inc-a")
	sender := newTestClusterCommandBus(t, redisCfg, "node-b", "inc-b")

	var handledCount atomic.Int32
	handlerStarted := make(chan struct{}, 1)
	releaseHandler := make(chan struct{})
	receiver.SetHandler(func(context.Context, *messageloop.ClusterCommand) (*messageloop.ClusterCommandResult, error) {
		handledCount.Add(1)
		select {
		case handlerStarted <- struct{}{}:
		default:
		}
		<-releaseHandler
		return &messageloop.ClusterCommandResult{Status: messageloop.ClusterCommandStatusSucceeded}, nil
	})
	receiver.start(t, ctx)

	firstResultCh := make(chan *messageloop.ClusterCommandResult, 1)
	firstErrCh := make(chan error, 1)
	go func() {
		result, err := sender.SendCommand(ctx, testClusterCommand("dedupe-pending", "node-a", "inc-a"))
		firstResultCh <- result
		firstErrCh <- err
	}()

	select {
	case <-handlerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first command to reach handler")
	}

	duplicateResult, err := sender.SendCommand(ctx, testClusterCommand("dedupe-pending", "node-a", "inc-a"))
	require.NoError(t, err)
	require.NotNil(t, duplicateResult)
	require.Equal(t, messageloop.ClusterCommandStatusInProgress, duplicateResult.Status)
	require.Equal(t, "COMMAND_IN_PROGRESS", duplicateResult.ErrorCode)
	require.EqualValues(t, 1, handledCount.Load())

	close(releaseHandler)
	require.NoError(t, <-firstErrCh)
	firstResult := <-firstResultCh
	require.NotNil(t, firstResult)
	require.Equal(t, messageloop.ClusterCommandStatusSucceeded, firstResult.Status)
}

func TestClusterCommandBus_ReturnsUnknownFinalStateAfterTimeout(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	ctx := context.Background()

	receiver := newTestClusterCommandBus(t, redisCfg, "node-a", "inc-a")
	sender := newTestClusterCommandBus(t, redisCfg, "node-b", "inc-b")

	releaseHandler := make(chan struct{})
	receiver.SetHandler(func(context.Context, *messageloop.ClusterCommand) (*messageloop.ClusterCommandResult, error) {
		<-releaseHandler
		return &messageloop.ClusterCommandResult{Status: messageloop.ClusterCommandStatusSucceeded}, nil
	})
	receiver.start(t, ctx)

	timeoutCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()

	result, err := sender.SendCommand(timeoutCtx, testClusterCommand("dedupe-timeout", "node-a", "inc-a"))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, messageloop.ClusterCommandStatusUnknownFinalState, result.Status)
	require.Equal(t, "UNKNOWN_FINAL_STATE", result.ErrorCode)

	close(releaseHandler)
}

type testClusterCommandBus struct {
	*redisClusterCommandBus
}

func newTestClusterCommandBus(t *testing.T, redisCfg config.RedisConfig, nodeID, incarnationID string) *testClusterCommandBus {
	t.Helper()
	bus, ok := NewClusterCommandBus(redisCfg, nodeID, incarnationID).(*redisClusterCommandBus)
	require.True(t, ok)
	t.Cleanup(func() {
		require.NoError(t, bus.Shutdown(context.Background()))
	})
	return &testClusterCommandBus{redisClusterCommandBus: bus}
}

func (b *testClusterCommandBus) start(t *testing.T, ctx context.Context) {
	t.Helper()
	require.NoError(t, b.Start(ctx))
}

func testClusterCommand(commandID, targetNodeID, targetIncarnationID string) *messageloop.ClusterCommand {
	return &messageloop.ClusterCommand{
		CommandID:           commandID,
		Type:                messageloop.ClusterCommandDisconnect,
		SessionID:           "sess-" + commandID,
		TargetNodeID:        targetNodeID,
		TargetIncarnationID: targetIncarnationID,
	}
}

func requireCommandBusRedis(t *testing.T) config.RedisConfig {
	t.Helper()

	redisCfg := config.RedisConfig{
		Addr:     firstNonEmpty(os.Getenv("MESSAGELOOP_TEST_REDIS_ADDR"), "127.0.0.1:6379"),
		Password: firstNonEmpty(os.Getenv("MESSAGELOOP_TEST_REDIS_PASSWORD"), os.Getenv("REDIS_PASSWORD")),
		DB:       clusterCommandBusTestDB,
	}

	client := redis.NewClient(&redis.Options{
		Addr:     redisCfg.Addr,
		Password: redisCfg.Password,
		DB:       redisCfg.DB,
	})
	t.Cleanup(func() {
		_ = client.Close()
	})

	pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		t.Skipf("redis not available for command bus integration tests: %v", err)
	}
	require.NoError(t, client.FlushDB(context.Background()).Err())
	t.Cleanup(func() {
		_ = client.FlushDB(context.Background()).Err()
	})

	return redisCfg
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func TestClusterCommandBus_ResolveTimedOutCommandPrefersTerminalResult(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	ctx := context.Background()

	bus := newTestClusterCommandBus(t, redisCfg, "node-a", "inc-a")
	storedResult := &messageloop.ClusterCommandResult{
		CommandID:     "resolve-terminal",
		SessionID:     "sess-resolve-terminal",
		NodeID:        "node-a",
		IncarnationID: "inc-a",
		Status:        messageloop.ClusterCommandStatusSucceeded,
	}
	require.NoError(t, bus.storeCommandResult(ctx, storedResult))

	result, err := bus.resolveTimedOutCommand(context.Background(), testClusterCommand("resolve-terminal", "node-a", "inc-a"))
	require.NoError(t, err)
	require.Equal(t, storedResult, result)
}

func TestClusterCommandBus_RecordsMetricsForDedupeHits(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	ctx := context.Background()

	receiver := newTestClusterCommandBus(t, redisCfg, "node-a", "inc-a")
	sender := newTestClusterCommandBus(t, redisCfg, "node-b", "inc-b")
	registry := prometheus.NewRegistry()
	metrics := messageloop.NewMetrics(registry)
	sender.SetMetrics(metrics)

	receiver.SetHandler(func(context.Context, *messageloop.ClusterCommand) (*messageloop.ClusterCommandResult, error) {
		return &messageloop.ClusterCommandResult{Status: messageloop.ClusterCommandStatusSucceeded}, nil
	})
	receiver.start(t, ctx)

	_, err := sender.SendCommand(ctx, testClusterCommand("metrics-dedupe", "node-a", "inc-a"))
	require.NoError(t, err)
	_, err = sender.SendCommand(ctx, testClusterCommand("metrics-dedupe", "node-a", "inc-a"))
	require.NoError(t, err)
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.ClusterCommandDedupeHits))
}

func TestClusterCommandBus_RecordsMetricsForTimeoutAndUnknownFinalState(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	ctx := context.Background()

	receiver := newTestClusterCommandBus(t, redisCfg, "node-a", "inc-a")
	sender := newTestClusterCommandBus(t, redisCfg, "node-b", "inc-b")
	registry := prometheus.NewRegistry()
	metrics := messageloop.NewMetrics(registry)
	sender.SetMetrics(metrics)

	releaseHandler := make(chan struct{})
	receiver.SetHandler(func(context.Context, *messageloop.ClusterCommand) (*messageloop.ClusterCommandResult, error) {
		<-releaseHandler
		return &messageloop.ClusterCommandResult{Status: messageloop.ClusterCommandStatusSucceeded}, nil
	})
	receiver.start(t, ctx)

	timeoutCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()

	result, err := sender.SendCommand(timeoutCtx, testClusterCommand("metrics-timeout", "node-a", "inc-a"))
	close(releaseHandler)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, messageloop.ClusterCommandStatusUnknownFinalState, result.Status)
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.ClusterCommandTimeouts))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.ClusterCommandUnknownFinalState))
}
