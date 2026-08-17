package redisbroker

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/config"
	clusterhmac "github.com/messageloopio/messageloop/internal/cluster/hmac"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

const clusterCommandBusTestDB = 14

// testClusterCommandBusKey is the 32-byte HMAC key shared by every bus in
// these tests (spec allows a 32-byte literal in tests).
var testClusterCommandBusKey = []byte("0123456789abcdef0123456789abcdef")

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

// TestClusterCommandBus_SendCommandFillsIssuedBy verifies P1-9: the command
// bus stamps each command with the sender's NodeID for audit purposes before
// delivering it to the target node's handler.
func TestClusterCommandBus_SendCommandFillsIssuedBy(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	ctx := context.Background()

	receiver := newTestClusterCommandBus(t, redisCfg, "node-a", "inc-a")
	sender := newTestClusterCommandBus(t, redisCfg, "node-b", "inc-b")

	var issuedBy atomic.Value
	receiver.SetHandler(func(_ context.Context, cmd *messageloop.ClusterCommand) (*messageloop.ClusterCommandResult, error) {
		issuedBy.Store(cmd.IssuedBy)
		return &messageloop.ClusterCommandResult{Status: messageloop.ClusterCommandStatusSucceeded}, nil
	})
	receiver.start(t, ctx)

	result, err := sender.SendCommand(ctx, testClusterCommand("issued-by", "node-a", "inc-a"))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, messageloop.ClusterCommandStatusSucceeded, result.Status)
	require.Equal(t, "node-b", issuedBy.Load(),
		"the target handler must observe the sender's NodeID in IssuedBy")
}

type testClusterCommandBus struct {
	*redisClusterCommandBus
}

func newTestClusterCommandBus(t *testing.T, redisCfg config.RedisConfig, nodeID, incarnationID string) *testClusterCommandBus {
	t.Helper()
	bus, ok := NewClusterCommandBus(redisCfg, nodeID, incarnationID, testClusterCommandBusKey).(*redisClusterCommandBus)
	require.True(t, ok)
	t.Cleanup(func() {
		require.NoError(t, bus.Shutdown(context.Background()))
	})
	// Register a live node lease so SendCommand's target-alive pre-check
	// passes: in production the node lease manager writes it on startup.
	registerTestNodeLease(t, bus, nodeID, incarnationID)
	return &testClusterCommandBus{redisClusterCommandBus: bus}
}

// registerTestNodeLease writes a node lease for the given incarnation using
// the same key layout as redisSessionDirectory.nodeLeaseKey.
func registerTestNodeLease(t *testing.T, bus *redisClusterCommandBus, nodeID, incarnationID string) {
	t.Helper()
	lease := &messageloop.ClusterNodeLease{
		NodeID:        nodeID,
		IncarnationID: incarnationID,
		StartedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(time.Hour),
	}
	data, err := json.Marshal(lease)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, bus.client.Set(ctx, bus.opts.ClusterNodePrefix+nodeID+":"+incarnationID, data, time.Hour).Err())
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

// TestClusterCommandBus_CloneCommandMetadataIsIndependent verifies that the
// metadata map copied for each BroadcastCommand goroutine is not shared with
// the caller's map.
func TestClusterCommandBus_CloneCommandMetadataIsIndependent(t *testing.T) {
	source := map[string]string{"exclude_self": "true", "k": "v"}
	clone := cloneCommandMetadata(source)

	clone["reply_channel"] = "ml:cluster:cmd:reply:test"
	require.Len(t, source, 2)
	require.NotContains(t, source, "reply_channel")
	require.Equal(t, "ml:cluster:cmd:reply:test", clone["reply_channel"])
	require.Nil(t, cloneCommandMetadata(nil))
}

// TestClusterCommandBus_WaitsForMatchingReply verifies that SendCommand's reply
// wait skips results whose CommandID does not match and keeps waiting for the
// matching result instead of returning a cross-wired reply.
func TestClusterCommandBus_WaitsForMatchingReply(t *testing.T) {
	bus := &redisClusterCommandBus{hmacKey: testClusterCommandBusKey, now: time.Now}
	cmd := &messageloop.ClusterCommand{CommandID: "expected-command"}

	mismatchedResult := &messageloop.ClusterCommandResult{
		CommandID: "other-command",
		IssuedAt:  time.Now(),
	}
	require.NoError(t, clusterhmac.SignResult(testClusterCommandBusKey, mismatchedResult))
	mismatched, err := json.Marshal(mismatchedResult)
	require.NoError(t, err)
	matchingResult := &messageloop.ClusterCommandResult{
		CommandID: "expected-command",
		Status:    messageloop.ClusterCommandStatusSucceeded,
		IssuedAt:  time.Now(),
	}
	require.NoError(t, clusterhmac.SignResult(testClusterCommandBusKey, matchingResult))
	matching, err := json.Marshal(matchingResult)
	require.NoError(t, err)

	replies := make(chan *redis.Message, 2)
	replies <- &redis.Message{Payload: string(mismatched)}
	replies <- &redis.Message{Payload: string(matching)}
	close(replies)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := bus.waitForReply(ctx, cmd, replies)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "expected-command", result.CommandID)
	require.Equal(t, messageloop.ClusterCommandStatusSucceeded, result.Status)
}

// TestClusterCommandBus_ReplyWaitTimesOutOnOnlyMismatchedReplies verifies the
// reply wait ends at the command deadline when no matching reply arrives.
func TestClusterCommandBus_ReplyWaitTimesOutOnOnlyMismatchedReplies(t *testing.T) {
	bus := &redisClusterCommandBus{
		client:  redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"}),
		hmacKey: testClusterCommandBusKey,
		now:     time.Now,
	}
	cmd := &messageloop.ClusterCommand{CommandID: "expected-command"}

	mismatchedResult := &messageloop.ClusterCommandResult{CommandID: "other-command", IssuedAt: time.Now()}
	require.NoError(t, clusterhmac.SignResult(testClusterCommandBusKey, mismatchedResult))
	mismatched, err := json.Marshal(mismatchedResult)
	require.NoError(t, err)

	replies := make(chan *redis.Message, 1)
	replies <- &redis.Message{Payload: string(mismatched)}
	close(replies)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err = bus.waitForReply(ctx, cmd, replies)
	require.Error(t, err, "expected the reply wait to end with an error at the deadline")
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

// TestClusterCommandBus_ReclaimsAfterClaimLeaseExpiry verifies P2-2 fix 2:
// a command whose owner died mid-handling (simulated by a hung handler with
// lease renewal disabled) is re-claimable once the claim lease expires,
// instead of being locked in pending for the full terminal-state TTL.
func TestClusterCommandBus_ReclaimsAfterClaimLeaseExpiry(t *testing.T) {
	originalLeaseTTL := clusterCommandClaimLeaseTTL
	originalRenewInterval := clusterCommandClaimRenewInterval
	clusterCommandClaimLeaseTTL = 300 * time.Millisecond
	clusterCommandClaimRenewInterval = time.Hour // disabled: simulate a crashed owner
	t.Cleanup(func() {
		clusterCommandClaimLeaseTTL = originalLeaseTTL
		clusterCommandClaimRenewInterval = originalRenewInterval
	})

	redisCfg := requireCommandBusRedis(t)
	ctx := context.Background()

	receiver := newTestClusterCommandBus(t, redisCfg, "node-a", "inc-a")
	sender := newTestClusterCommandBus(t, redisCfg, "node-b", "inc-b")

	var handledCount atomic.Int32
	releaseHandler := make(chan struct{})
	receiver.SetHandler(func(context.Context, *messageloop.ClusterCommand) (*messageloop.ClusterCommandResult, error) {
		handledCount.Add(1)
		<-releaseHandler
		return &messageloop.ClusterCommandResult{Status: messageloop.ClusterCommandStatusSucceeded}, nil
	})
	receiver.start(t, ctx)

	firstCtx, firstCancel := context.WithTimeout(ctx, 1*time.Second)
	defer firstCancel()
	firstResult, err := sender.SendCommand(firstCtx, testClusterCommand("lease-reclaim", "node-a", "inc-a"))
	require.NoError(t, err)
	require.NotNil(t, firstResult)
	require.Equal(t, messageloop.ClusterCommandStatusUnknownFinalState, firstResult.Status,
		"sender must observe the pending command timing out")

	// Wait for the claim lease to expire (no renewal, so the pending state
	// vanishes instead of persisting for the 10-minute terminal TTL).
	time.Sleep(500 * time.Millisecond)

	secondResultCh := make(chan *messageloop.ClusterCommandResult, 1)
	secondErrCh := make(chan error, 1)
	go func() {
		result, err := sender.SendCommand(ctx, testClusterCommand("lease-reclaim", "node-a", "inc-a"))
		secondResultCh <- result
		secondErrCh <- err
	}()

	// The re-sent command must be re-claimed and reach the handler again.
	deadline := time.Now().Add(2 * time.Second)
	for handledCount.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	require.EqualValues(t, 2, handledCount.Load(),
		"the command must be re-claimed and handled again after the lease expires")

	close(releaseHandler)
	require.NoError(t, <-secondErrCh)
	secondResult := <-secondResultCh
	require.NotNil(t, secondResult)
	require.Equal(t, messageloop.ClusterCommandStatusSucceeded, secondResult.Status)
}

// TestClusterCommandBus_BoundedHandlerConcurrency verifies P2-2 fix 1: at
// most clusterCommandHandlerConcurrency commands are handled concurrently,
// and a saturated bus queues commands instead of dropping them.
func TestClusterCommandBus_BoundedHandlerConcurrency(t *testing.T) {
	originalConcurrency := clusterCommandHandlerConcurrency
	clusterCommandHandlerConcurrency = 1
	t.Cleanup(func() { clusterCommandHandlerConcurrency = originalConcurrency })

	redisCfg := requireCommandBusRedis(t)
	ctx := context.Background()

	receiver := newTestClusterCommandBus(t, redisCfg, "node-a", "inc-a")
	sender := newTestClusterCommandBus(t, redisCfg, "node-b", "inc-b")

	var handledCount atomic.Int32
	firstStarted := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	receiver.SetHandler(func(context.Context, *messageloop.ClusterCommand) (*messageloop.ClusterCommandResult, error) {
		handledCount.Add(1)
		if handledCount.Load() == 1 {
			select {
			case firstStarted <- struct{}{}:
			default:
			}
			<-releaseFirst
		}
		return &messageloop.ClusterCommandResult{Status: messageloop.ClusterCommandStatusSucceeded}, nil
	})
	receiver.start(t, ctx)

	firstResultCh := make(chan *messageloop.ClusterCommandResult, 1)
	firstErrCh := make(chan error, 1)
	go func() {
		result, err := sender.SendCommand(ctx, testClusterCommand("sem-1", "node-a", "inc-a"))
		firstResultCh <- result
		firstErrCh <- err
	}()

	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first command to reach handler")
	}

	secondResultCh := make(chan *messageloop.ClusterCommandResult, 1)
	secondErrCh := make(chan error, 1)
	go func() {
		result, err := sender.SendCommand(ctx, testClusterCommand("sem-2", "node-a", "inc-a"))
		secondResultCh <- result
		secondErrCh <- err
	}()

	// With concurrency 1, the second command must not be dispatched while
	// the first handler is still running.
	time.Sleep(300 * time.Millisecond)
	require.EqualValues(t, 1, handledCount.Load(),
		"second command must wait for the semaphore slot")

	close(releaseFirst)
	require.NoError(t, <-firstErrCh)
	firstResult := <-firstResultCh
	require.NotNil(t, firstResult)
	require.Equal(t, messageloop.ClusterCommandStatusSucceeded, firstResult.Status)

	require.NoError(t, <-secondErrCh)
	secondResult := <-secondResultCh
	require.NotNil(t, secondResult)
	require.Equal(t, messageloop.ClusterCommandStatusSucceeded, secondResult.Status)
	require.EqualValues(t, 2, handledCount.Load())
}

// TestClusterCommandBus_HandlerTimeoutWritesTerminalError verifies P2-3
// fix 2: a handler that exceeds its execution deadline produces a terminal
// CLUSTER_COMMAND_TIMEOUT result instead of pinning the command pending.
func TestClusterCommandBus_HandlerTimeoutWritesTerminalError(t *testing.T) {
	originalTimeout := clusterCommandHandlerTimeout
	clusterCommandHandlerTimeout = 200 * time.Millisecond
	t.Cleanup(func() { clusterCommandHandlerTimeout = originalTimeout })

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

	sendCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	result, err := sender.SendCommand(sendCtx, testClusterCommand("handler-timeout", "node-a", "inc-a"))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, messageloop.ClusterCommandStatusFailed, result.Status)
	require.Equal(t, "CLUSTER_COMMAND_TIMEOUT", result.ErrorCode)

	close(releaseHandler)
}

// TestClusterCommandBus_ReconnectsAfterDisconnect verifies P1-C1: when the
// request-channel subscription dies unexpectedly, the reader reconnects with
// backoff and the node keeps processing cluster commands instead of
// silently stopping until restart.
func TestClusterCommandBus_ReconnectsAfterDisconnect(t *testing.T) {
	originalBackoff := clusterCommandReconnectBaseBackoff
	clusterCommandReconnectBaseBackoff = 100 * time.Millisecond
	t.Cleanup(func() { clusterCommandReconnectBaseBackoff = originalBackoff })

	redisCfg := requireCommandBusRedis(t)
	ctx := context.Background()

	receiver := newTestClusterCommandBus(t, redisCfg, "node-a", "inc-a")
	sender := newTestClusterCommandBus(t, redisCfg, "node-b", "inc-b")

	var handled atomic.Int32
	receiver.SetHandler(func(context.Context, *messageloop.ClusterCommand) (*messageloop.ClusterCommandResult, error) {
		handled.Add(1)
		return &messageloop.ClusterCommandResult{Status: messageloop.ClusterCommandStatusSucceeded}, nil
	})
	receiver.start(t, ctx)

	sendOnce := func(commandID string) {
		t.Helper()
		result, err := sender.SendCommand(ctx, testClusterCommand(commandID, "node-a", "inc-a"))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, messageloop.ClusterCommandStatusSucceeded, result.Status)
	}

	// First round-trip works.
	sendOnce("reconnect-1")
	require.EqualValues(t, 1, handled.Load())

	// Simulate an unexpected disconnect by closing the reader's subscription.
	receiver.mu.RLock()
	oldPubSub := receiver.pubsub
	receiver.mu.RUnlock()
	require.NotNil(t, oldPubSub)
	require.NoError(t, oldPubSub.Close())

	// Wait until the reader has torn down the old subscription and reconnected
	// with a fresh one.
	require.Eventually(t, func() bool {
		receiver.mu.RLock()
		current := receiver.pubsub
		receiver.mu.RUnlock()
		return current != nil && current != oldPubSub
	}, 10*time.Second, 25*time.Millisecond)
	require.GreaterOrEqual(t, receiver.disconnects.Load(), uint64(1),
		"the unexpected disconnect must be counted")

	// A command sent after the reconnect must be handled again.
	sendOnce("reconnect-2")
	require.EqualValues(t, 2, handled.Load())

	// A graceful Shutdown must not reconnect.
	require.NoError(t, receiver.Shutdown(context.Background()))
	require.NoError(t, sender.Shutdown(context.Background()))
}

// TestClusterCommandBus_DeadlineAndClosedReplyChannelYieldsUnknownFinalState
// verifies P1-C5: when the command deadline fires at the exact moment the
// reply channel closes (both select arms ready), the timeout resolution must
// win and the caller observes UnknownFinalState — never a hard error.
func TestClusterCommandBus_DeadlineAndClosedReplyChannelYieldsUnknownFinalState(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)

	bus := newTestClusterCommandBus(t, redisCfg, "node-a", "inc-a")
	cmd := testClusterCommand("deadline-race", "node-a", "inc-a")

	expired, cancel := context.WithTimeout(context.Background(), -time.Millisecond)
	defer cancel()
	replies := make(chan *redis.Message)
	close(replies)

	result, err := bus.waitForReply(expired, cmd, replies)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, messageloop.ClusterCommandStatusUnknownFinalState, result.Status,
		"an expired deadline with a simultaneously closed reply channel must resolve as unknown final state")
	require.Equal(t, "UNKNOWN_FINAL_STATE", result.ErrorCode)
}

// TestClusterCommandBus_ReplyChannelClosedWithoutDeadlineReturnsError verifies
// the non-deadline counterpart of the P1-C5 race: a reply channel that closes
// while the context is still live is still a hard error (the caller is not
// inside a timeout).
func TestClusterCommandBus_ReplyChannelClosedWithoutDeadlineReturnsError(t *testing.T) {
	bus := &redisClusterCommandBus{}
	cmd := &messageloop.ClusterCommand{CommandID: "closed-without-deadline"}

	replies := make(chan *redis.Message)
	close(replies)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := bus.waitForReply(ctx, cmd, replies)
	require.Error(t, err)
	require.Contains(t, err.Error(), "reply channel closed")
}

// TestClusterCommandBus_SendCommandFailsFastWhenTargetNotAlive verifies
// P2-12: SendCommand fails immediately with TARGET_NODE_NOT_ALIVE when the
// target incarnation holds no live node lease, instead of burning the full
// command deadline.
func TestClusterCommandBus_SendCommandFailsFastWhenTargetNotAlive(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	ctx := context.Background()

	// Sender only: no lease is registered for "node-dead"/"inc-dead".
	sender := newTestClusterCommandBus(t, redisCfg, "node-b", "inc-b")

	start := time.Now()
	result, err := sender.SendCommand(ctx, testClusterCommand("dead-target", "node-dead", "inc-dead"))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, messageloop.ClusterCommandStatusFailed, result.Status)
	require.Equal(t, "TARGET_NODE_NOT_ALIVE", result.ErrorCode)
	require.Less(t, time.Since(start), 2*time.Second,
		"the dead-target failure must be immediate, not after the command deadline")
}

// --- HMAC hard gate (PR-KA-B4) ---

// publishRawCommand injects a command envelope directly onto the receiver's
// request channel, bypassing the sending bus (and its signing).
func publishRawCommand(t *testing.T, redisCfg config.RedisConfig, receiver *testClusterCommandBus, cmd *messageloop.ClusterCommand) {
	t.Helper()
	payload, err := json.Marshal(cmd)
	require.NoError(t, err)
	client := newRedisClient(NewOptions(redisCfg))
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, client.Publish(ctx, receiver.requestChannel(receiver.nodeID, receiver.incarnationID), payload).Err())
}

func TestClusterCommandBus_StartRejectsShortHMACKey(t *testing.T) {
	for name, key := range map[string][]byte{
		"missing": nil,
		"short":   []byte("sixteen-bytes!!!"),
	} {
		t.Run(name, func(t *testing.T) {
			bus := NewClusterCommandBus(config.RedisConfig{Addr: "127.0.0.1:1"}, "node-a", "inc-a", key)
			err := bus.Start(context.Background())
			require.Error(t, err)
			require.Contains(t, err.Error(), "at least 32 bytes")
			_ = bus.Shutdown(context.Background())
		})
	}
}

// An unsigned command injected straight onto the request channel must be
// dropped before claiming: no handler run, no dedupe state key, no reply.
func TestClusterCommandBus_RejectsUnsignedCommand(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	ctx := context.Background()

	receiver := newTestClusterCommandBus(t, redisCfg, "node-a", "inc-a")
	sender := newTestClusterCommandBus(t, redisCfg, "node-b", "inc-b")
	metrics := messageloop.NewMetrics(prometheus.NewRegistry())
	receiver.SetMetrics(metrics)

	var handledCount atomic.Int32
	receiver.SetHandler(func(context.Context, *messageloop.ClusterCommand) (*messageloop.ClusterCommandResult, error) {
		handledCount.Add(1)
		return &messageloop.ClusterCommandResult{Status: messageloop.ClusterCommandStatusSucceeded}, nil
	})
	receiver.start(t, ctx)

	rogue := testClusterCommand("unsigned-1", "node-a", "inc-a") // no Signature
	publishRawCommand(t, redisCfg, receiver, rogue)

	// Positive control: the signed command published after the rogue one is
	// handled, proving the reader already saw and dropped the rogue message
	// (pub/sub delivers a channel's messages in publish order).
	result, err := sender.SendCommand(ctx, testClusterCommand("signed-control-1", "node-a", "inc-a"))
	require.NoError(t, err)
	require.Equal(t, messageloop.ClusterCommandStatusSucceeded, result.Status)
	require.EqualValues(t, 1, handledCount.Load(), "only the signed command may run the handler")

	exists, err := receiver.client.Exists(ctx, receiver.commandStateKey("unsigned-1")).Result()
	require.NoError(t, err)
	require.Zero(t, exists, "a rejected command must not create a dedupe state key")
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.ClusterCommandHMACRejects.WithLabelValues("missing")))
}

// A well-formed envelope whose signature's last hex char is flipped must be
// rejected as "bad"; a MAC made with the wrong key is equally "bad".
func TestClusterCommandBus_RejectsBadSignature(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	ctx := context.Background()

	receiver := newTestClusterCommandBus(t, redisCfg, "node-a", "inc-a")
	sender := newTestClusterCommandBus(t, redisCfg, "node-b", "inc-b")
	metrics := messageloop.NewMetrics(prometheus.NewRegistry())
	receiver.SetMetrics(metrics)

	var handledCount atomic.Int32
	receiver.SetHandler(func(context.Context, *messageloop.ClusterCommand) (*messageloop.ClusterCommandResult, error) {
		handledCount.Add(1)
		return &messageloop.ClusterCommandResult{Status: messageloop.ClusterCommandStatusSucceeded}, nil
	})
	receiver.start(t, ctx)

	flipped := testClusterCommand("bad-sig-flip", "node-a", "inc-a")
	flipped.IssuedAt = time.Now()
	require.NoError(t, clusterhmac.SignCommand(testClusterCommandBusKey, flipped))
	last := flipped.Signature[len(flipped.Signature)-1]
	replacement := byte('0')
	if last == '0' {
		replacement = '1'
	}
	flipped.Signature = flipped.Signature[:len(flipped.Signature)-1] + string(replacement)
	publishRawCommand(t, redisCfg, receiver, flipped)

	wrongKey := testClusterCommand("bad-sig-key", "node-a", "inc-a")
	wrongKey.IssuedAt = time.Now()
	require.NoError(t, clusterhmac.SignCommand([]byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), wrongKey))
	publishRawCommand(t, redisCfg, receiver, wrongKey)

	result, err := sender.SendCommand(ctx, testClusterCommand("signed-control-2", "node-a", "inc-a"))
	require.NoError(t, err)
	require.Equal(t, messageloop.ClusterCommandStatusSucceeded, result.Status)
	require.EqualValues(t, 1, handledCount.Load(), "badly signed commands must never run the handler")
	require.Equal(t, float64(2), testutil.ToFloat64(metrics.ClusterCommandHMACRejects.WithLabelValues("bad")))
}

// Commands signed with an IssuedAt more than 30s off the receiver's clock are
// rejected as "skew"; ±29s still passes. Uses real timestamps relative to
// now — no sleeping.
func TestClusterCommandBus_ClockSkewGate(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	ctx := context.Background()

	receiver := newTestClusterCommandBus(t, redisCfg, "node-a", "inc-a")
	metrics := messageloop.NewMetrics(prometheus.NewRegistry())
	receiver.SetMetrics(metrics)

	var handledCount atomic.Int32
	receiver.SetHandler(func(context.Context, *messageloop.ClusterCommand) (*messageloop.ClusterCommandResult, error) {
		handledCount.Add(1)
		return &messageloop.ClusterCommandResult{Status: messageloop.ClusterCommandStatusSucceeded}, nil
	})
	receiver.start(t, ctx)

	for _, tc := range []struct {
		commandID string
		issuedAt  time.Time
	}{
		{"skew-future-31", time.Now().Add(31 * time.Second)},
		{"skew-past-31", time.Now().Add(-31 * time.Second)},
	} {
		rogue := testClusterCommand(tc.commandID, "node-a", "inc-a")
		rogue.IssuedAt = tc.issuedAt
		require.NoError(t, clusterhmac.SignCommand(testClusterCommandBusKey, rogue))
		publishRawCommand(t, redisCfg, receiver, rogue)
	}

	within := testClusterCommand("skew-ok-29", "node-a", "inc-a")
	within.IssuedAt = time.Now().Add(29 * time.Second)
	require.NoError(t, clusterhmac.SignCommand(testClusterCommandBusKey, within))
	publishRawCommand(t, redisCfg, receiver, within)

	require.Eventually(t, func() bool { return handledCount.Load() == 1 },
		2*time.Second, 10*time.Millisecond, "the in-window command must be handled")
	require.Equal(t, float64(2), testutil.ToFloat64(metrics.ClusterCommandHMACRejects.WithLabelValues("skew")))
}

// An envelope with an empty CommandID is rejected even though its MAC was
// computed correctly over the empty id.
func TestClusterCommandBus_RejectsEmptyCommandID(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	ctx := context.Background()

	receiver := newTestClusterCommandBus(t, redisCfg, "node-a", "inc-a")
	sender := newTestClusterCommandBus(t, redisCfg, "node-b", "inc-b")
	metrics := messageloop.NewMetrics(prometheus.NewRegistry())
	receiver.SetMetrics(metrics)

	var handledCount atomic.Int32
	receiver.SetHandler(func(context.Context, *messageloop.ClusterCommand) (*messageloop.ClusterCommandResult, error) {
		handledCount.Add(1)
		return &messageloop.ClusterCommandResult{Status: messageloop.ClusterCommandStatusSucceeded}, nil
	})
	receiver.start(t, ctx)

	rogue := testClusterCommand("", "node-a", "inc-a")
	rogue.CommandID = ""
	require.NoError(t, clusterhmac.SignCommand(testClusterCommandBusKey, rogue))
	publishRawCommand(t, redisCfg, receiver, rogue)

	result, err := sender.SendCommand(ctx, testClusterCommand("signed-control-3", "node-a", "inc-a"))
	require.NoError(t, err)
	require.Equal(t, messageloop.ClusterCommandStatusSucceeded, result.Status)
	require.EqualValues(t, 1, handledCount.Load(), "an id-less command must never run the handler")
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.ClusterCommandHMACRejects.WithLabelValues("id")))
}

// A signed round trip: the receiver's reply carries a signature the sender
// verifies, and IssuedAt is stamped.
func TestClusterCommandBus_RoundTripResultIsSigned(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	ctx := context.Background()

	receiver := newTestClusterCommandBus(t, redisCfg, "node-a", "inc-a")
	sender := newTestClusterCommandBus(t, redisCfg, "node-b", "inc-b")
	receiver.SetHandler(func(context.Context, *messageloop.ClusterCommand) (*messageloop.ClusterCommandResult, error) {
		return &messageloop.ClusterCommandResult{Status: messageloop.ClusterCommandStatusSucceeded}, nil
	})
	receiver.start(t, ctx)

	result, err := sender.SendCommand(ctx, testClusterCommand("signed-roundtrip", "node-a", "inc-a"))
	require.NoError(t, err)
	require.Equal(t, messageloop.ClusterCommandStatusSucceeded, result.Status)
	require.NotEmpty(t, result.Signature, "replies must be signed")
	require.False(t, result.IssuedAt.IsZero(), "replies must carry IssuedAt")
	require.NoError(t, clusterhmac.VerifyResult(testClusterCommandBusKey, result, time.Now()))
}

// waitForReply must skip forged replies (unsigned or badly signed) and accept
// the properly signed one that arrives after them.
func TestClusterCommandBus_ForgedRepliesAreSkipped(t *testing.T) {
	bus := &redisClusterCommandBus{hmacKey: testClusterCommandBusKey, now: time.Now}
	cmd := &messageloop.ClusterCommand{CommandID: "victim"}

	unsigned, err := json.Marshal(&messageloop.ClusterCommandResult{
		CommandID: "victim",
		Status:    messageloop.ClusterCommandStatusSucceeded,
	})
	require.NoError(t, err)
	forgedSigned := &messageloop.ClusterCommandResult{
		CommandID: "victim",
		Status:    messageloop.ClusterCommandStatusSucceeded,
		IssuedAt:  time.Now(),
	}
	require.NoError(t, clusterhmac.SignResult([]byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), forgedSigned))
	badMAC, err := json.Marshal(forgedSigned)
	require.NoError(t, err)
	genuine := &messageloop.ClusterCommandResult{
		CommandID: "victim",
		Status:    messageloop.ClusterCommandStatusSucceeded,
		IssuedAt:  time.Now(),
	}
	require.NoError(t, clusterhmac.SignResult(testClusterCommandBusKey, genuine))
	genuinePayload, err := json.Marshal(genuine)
	require.NoError(t, err)

	replies := make(chan *redis.Message, 3)
	replies <- &redis.Message{Payload: string(unsigned)}
	replies <- &redis.Message{Payload: string(badMAC)}
	replies <- &redis.Message{Payload: string(genuinePayload)}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := bus.waitForReply(ctx, cmd, replies)
	require.NoError(t, err)
	require.Equal(t, genuine.Signature, result.Signature, "the signed reply must win over the forged ones")
	require.Equal(t, messageloop.ClusterCommandStatusSucceeded, result.Status)
}

// When every reply is forged, the wait ends at the deadline with
// unknown_final_state — a forged succeeded is never returned.
func TestClusterCommandBus_ForgedReplyTimeoutYieldsUnknownFinalState(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	bus := newTestClusterCommandBus(t, redisCfg, "node-a", "inc-a")

	forged, err := json.Marshal(&messageloop.ClusterCommandResult{
		CommandID: "victim-timeout",
		Status:    messageloop.ClusterCommandStatusSucceeded,
	})
	require.NoError(t, err)
	replies := make(chan *redis.Message, 1)
	replies <- &redis.Message{Payload: string(forged)}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result, err := bus.waitForReply(ctx, &messageloop.ClusterCommand{CommandID: "victim-timeout"}, replies)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, messageloop.ClusterCommandStatusUnknownFinalState, result.Status,
		"a forged reply must not count as success; the command resolves via the timeout path")
}

// The HMAC key must never appear in any Redis value or published payload.
func TestClusterCommandBus_KeyNeverWrittenToRedis(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	ctx := context.Background()

	receiver := newTestClusterCommandBus(t, redisCfg, "node-a", "inc-a")
	sender := newTestClusterCommandBus(t, redisCfg, "node-b", "inc-b")
	receiver.SetHandler(func(context.Context, *messageloop.ClusterCommand) (*messageloop.ClusterCommandResult, error) {
		return &messageloop.ClusterCommandResult{Status: messageloop.ClusterCommandStatusSucceeded}, nil
	})
	receiver.start(t, ctx)

	spy := newRedisClient(NewOptions(redisCfg))
	defer func() { _ = spy.Close() }()
	pubsub := spy.PSubscribe(ctx, "ml:cluster:cmd:*")
	defer func() { _ = pubsub.Close() }()
	_, err := pubsub.Receive(ctx)
	require.NoError(t, err)

	result, err := sender.SendCommand(ctx, testClusterCommand("key-hygiene", "node-a", "inc-a"))
	require.NoError(t, err)
	require.Equal(t, messageloop.ClusterCommandStatusSucceeded, result.Status)

	// Drain every pub/sub payload the round trip produced (request + reply).
	drainCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	seen := 0
	for {
		msg, err := pubsub.ReceiveMessage(drainCtx)
		if err != nil {
			break
		}
		seen++
		require.NotContains(t, msg.Payload, string(testClusterCommandBusKey),
			"no published payload may contain the HMAC key")
	}
	require.GreaterOrEqual(t, seen, 2, "expected at least the request and the reply payload")

	// Every string value stored under this test DB must be key-free too.
	keys, err := scanKeys(ctx, spy, "*")
	require.NoError(t, err)
	require.NotEmpty(t, keys)
	for _, key := range keys {
		keyType, err := spy.Type(ctx, key).Result()
		require.NoError(t, err)
		if keyType != "string" {
			continue
		}
		value, err := spy.Get(ctx, key).Result()
		require.NoError(t, err)
		require.False(t, strings.Contains(value, string(testClusterCommandBusKey)),
			"redis value %s must not contain the HMAC key", key)
	}
}
