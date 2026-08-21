// Package redisbroker provides Redis-backed implementations of the broker,
// presence store, and cluster control-plane adapters.
//
// Delivery: cluster command requests travel to the target incarnation's
// Redis Stream inbox (ml2:cluster:cmd:stream:{nodeID}:{incarnationID}) and are
// consumed through a per-stream consumer group (XREADGROUP + XAUTOCLAIM +
// XACK), giving at-least-once delivery with command-id dedupe (PR-KA-C3).
// Command results still travel over Redis Pub/Sub reply channels. Both
// directions are signed with HMAC-SHA256 (PR-KA-B4, see
// internal/cluster/hmac). The
// key comes from node configuration only (cluster.hmac_key or
// cluster.hmac_key_file) and is never written to Redis. Being able to write
// to the Redis instance is NOT enough to inject disconnect/takeover/publish
// commands: unsigned, badly signed, or skewed (±30s) envelopes are rejected
// before claiming or handling, and forged replies are discarded by the
// waiting sender. Commands still carry an IssuedBy audit field (sender
// NodeID); it is forgeable, sits outside the signed bytes, and is
// informational only. Redis network isolation remains defense in depth, but
// it is no longer the only boundary.
package redisbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"sync/atomic"

	"github.com/google/uuid"
	"github.com/lynx-go/x/log"
	"github.com/redis/go-redis/v9"

	"github.com/messageloopio/messageloop/config"
	"github.com/messageloopio/messageloop/internal/cluster"
	clusterhmac "github.com/messageloopio/messageloop/internal/cluster/hmac"
	"github.com/messageloopio/messageloop/internal/metrics"
)

const (
	clusterCommandStreamPrefix = "ml2:cluster:cmd:stream:"
	clusterCommandReplyPrefix  = "ml2:cluster:cmd:reply:"
	clusterCommandStatePrefix  = "ml2:cluster:cmd:state:"
	clusterCommandReplyKey     = "reply_channel"
	// clusterCommandGroupName is the fixed consumer group each inbox stream
	// carries; the consumer name is the incarnation ID (one live consumer per
	// incarnation).
	clusterCommandGroupName = "inbox"
	// clusterCommandStreamPayloadField is the single stream entry field
	// holding the command envelope JSON (including its Signature).
	clusterCommandStreamPayloadField = "payload"
	defaultCommandTimeout            = 5 * time.Second
	defaultCommandStateTTL           = 10 * time.Minute
)

// Command bus tunables are variables (not constants) so tests can shorten
// them; the documented production defaults are set below.
var (
	// clusterCommandHandlerConcurrency bounds the number of cluster command
	// handlers running concurrently per node. The reader loop blocks on this
	// semaphore before dispatching, so at most this many commands run at
	// once and no command is dropped when the bus is saturated.
	clusterCommandHandlerConcurrency = 128
	// clusterCommandClaimLeaseTTL is the TTL of a pending command claim.
	// A crashed owner's claim expires within this window, after which a
	// later sender can re-claim the command instead of being locked in
	// pending for the full terminal-state TTL (defaultCommandStateTTL).
	clusterCommandClaimLeaseTTL = 30 * time.Second
	// clusterCommandClaimRenewInterval is how often an in-flight owner
	// renews its claim lease while the handler is still running.
	clusterCommandClaimRenewInterval = 10 * time.Second
	// clusterCommandHandlerTimeout bounds each handler execution. A stuck
	// handler (e.g. a blocked survey write) produces a terminal
	// CLUSTER_COMMAND_TIMEOUT result instead of pinning the command in
	// pending forever. NOTE: a handler that ignores its context keeps
	// occupying its concurrency slot until it returns; handlers must honor
	// ctx cancellation.
	clusterCommandHandlerTimeout = 10 * time.Second
	// clusterCommandReconnectBaseBackoff is the initial delay before the
	// first cluster command bus reconnection attempt. Variable so tests can
	// shorten it.
	clusterCommandReconnectBaseBackoff = 1 * time.Second
	// clusterCommandStreamMaxLen bounds each per-incarnation inbox stream via
	// approximate MAXLEN trimming on XADD, so an inbox cannot grow without
	// bound. Variable so tests can tune it.
	clusterCommandStreamMaxLen int64 = 10000
	// clusterCommandReadBlockTimeout is the XREADGROUP block timeout. When it
	// elapses with no new entries the reader runs an XAUTOCLAIM pass for
	// crash redelivery, so this also bounds the redelivery cadence. Variable
	// so tests can shorten it.
	clusterCommandReadBlockTimeout = 2 * time.Second
	// clusterCommandReadCount bounds the entries fetched per XREADGROUP or
	// XAUTOCLAIM call.
	clusterCommandReadCount int64 = 32
)

type redisClusterCommandBus struct {
	client        *redis.Client
	opts          *Options
	nodeID        string
	incarnationID string
	// hmacKey signs and verifies every command/result envelope. It is copied
	// at construction and never written to Redis, logs, or metrics.
	hmacKey []byte
	// now is the clock used for IssuedAt stamping and skew checks; tests may
	// replace it. Nil means time.Now.
	now func() time.Time

	mu        sync.RWMutex
	handler   cluster.ClusterCommandHandler
	cancel    context.CancelFunc
	readerWG  sync.WaitGroup
	handlerWG sync.WaitGroup
	metrics   *metrics.Metrics
	start     bool
	stop      bool

	// disconnects counts unexpected stream-reader failures (reconnect
	// attempts); exposed for tests and operators.
	disconnects atomic.Uint64
}

// minClusterCommandHMACKeyBytes is the minimum HMAC key length accepted by
// the bus. config.Validate rejects shorter keys first; the bus re-checks so a
// misconfigured process can never start an unprotected bus.
const minClusterCommandHMACKeyBytes = 32

// NewClusterCommandBus returns a Redis-backed request/reply ClusterCommandBus.
// hmacKey (at least 32 bytes) signs every outgoing envelope and gates every
// incoming one; Start fails when it is missing or too short.
func NewClusterCommandBus(cfg config.RedisConfig, nodeID, incarnationID string, hmacKey []byte) cluster.ClusterCommandBus {
	opts := NewOptions(cfg)
	return &redisClusterCommandBus{
		client:        newRedisClient(opts),
		opts:          opts,
		nodeID:        nodeID,
		incarnationID: incarnationID,
		hmacKey:       append([]byte(nil), hmacKey...),
		now:           time.Now,
	}
}

// nowTime returns the current time on the bus clock.
func (b *redisClusterCommandBus) nowTime() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b *redisClusterCommandBus) SetHandler(handler cluster.ClusterCommandHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handler = handler
}

func (b *redisClusterCommandBus) SetMetrics(metrics *metrics.Metrics) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.metrics = metrics
}

func (b *redisClusterCommandBus) Start(ctx context.Context) error {
	b.mu.Lock()
	if b.start {
		b.mu.Unlock()
		return nil
	}
	if len(b.hmacKey) < minClusterCommandHMACKeyBytes {
		b.mu.Unlock()
		return fmt.Errorf("cluster command bus requires an HMAC key of at least %d bytes", minClusterCommandHMACKeyBytes)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if err := b.client.Ping(pingCtx).Err(); err != nil {
		cancel()
		b.mu.Unlock()
		return err
	}
	cancel()

	busCtx, busCancel := context.WithCancel(ctx)
	b.cancel = busCancel
	b.start = true
	b.mu.Unlock()

	// Bound concurrent command handling: the reader blocks on the semaphore
	// before dispatching, so at most clusterCommandHandlerConcurrency
	// commands run at once and none is dropped under load.
	sem := make(chan struct{}, clusterCommandHandlerConcurrency)
	confirmed := make(chan error, 1)

	b.readerWG.Add(1)
	go func() {
		defer b.readerWG.Done()
		// runCommandReaderWithRetry reports the first consumer-group creation
		// outcome on confirmed itself; later outcomes are absorbed by the
		// retry loop. Its only non-nil return is that same first-creation
		// error, already delivered on confirmed and acted on by the Start
		// select below, so the return value carries nothing new here.
		_ = b.runCommandReaderWithRetry(busCtx, sem, confirmed)
	}()

	// The reader reports the outcome of the first consumer-group creation so
	// Start can surface setup failures synchronously (cluster startup rolls
	// back on component start errors); later failures are absorbed by the
	// retry loop.
	select {
	case err := <-confirmed:
		if err != nil {
			busCancel()
			b.mu.Lock()
			b.start = false
			b.cancel = nil
			b.mu.Unlock()
			b.readerWG.Wait()
			return err
		}
	case <-ctx.Done():
		busCancel()
		return ctx.Err()
	}
	return nil
}

// runCommandReaderWithRetry keeps the inbox stream reader alive, re-creating
// the consumer group and reconnecting with exponential backoff after
// unexpected failures. The outcome of the first consumer-group creation is
// reported on confirmed (nil once the group is live, or an error when the
// first attempt fails) so Start can fail synchronously. It returns nil once
// the bus is stopped.
func (b *redisClusterCommandBus) runCommandReaderWithRetry(ctx context.Context, sem chan struct{}, confirmed chan<- error) error {
	backoff := clusterCommandReconnectBaseBackoff
	const maxBackoff = 30 * time.Second
	first := true
	for {
		err := b.ensureConsumerGroup(ctx)
		if err == nil {
			// The first successful group creation unblocks Start; later
			// reconnects must not touch the confirmed channel again (Start
			// has returned and nobody drains it).
			if first {
				first = false
				confirmed <- nil
			}
			err = b.runCommandReader(ctx, sem)
			if err == nil {
				// runCommandReader returns nil only on intentional shutdown.
				return nil
			}
		} else if first {
			confirmed <- err
			return err
		}
		if ctx.Err() != nil || b.stopped() {
			return nil
		}
		log.WarnContext(ctx, "cluster command stream reader failed, retrying",
			"error", err, "backoff", backoff, "node_id", b.nodeID)
		b.recordCommandBusDisconnect()
		if !b.waitReconnectBackoff(ctx, &backoff, maxBackoff) {
			return nil
		}
	}
}

// ensureConsumerGroup creates the inbox stream's consumer group (and the
// stream itself via MKSTREAM) starting at the beginning of the stream, so
// entries XADDed before the group existed are still delivered. An existing
// group (BUSYGROUP) is not an error.
func (b *redisClusterCommandBus) ensureConsumerGroup(ctx context.Context) error {
	err := b.client.XGroupCreateMkStream(ctx, b.streamKey(b.nodeID, b.incarnationID), clusterCommandGroupName, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return err
	}
	return nil
}

// runCommandReader drains the inbox stream and dispatches each command to a
// bounded handler goroutine, XACKing the entry once handling returns. It
// returns nil when ctx is cancelled (the bus is shutting down) and an error
// when the stream read fails unexpectedly, so the caller can reconnect.
func (b *redisClusterCommandBus) runCommandReader(ctx context.Context, sem chan struct{}) error {
	streamKey := b.streamKey(b.nodeID, b.incarnationID)
	consumer := b.consumerName()
	for {
		// Crash redelivery: pull entries pending longer than the claim lease
		// TTL back to this consumer and run them through the same handling
		// path; command-id dedupe bounds side effects to one execution.
		if err := b.autoClaimPending(ctx, sem, streamKey, consumer); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		streams, err := b.client.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    clusterCommandGroupName,
			Consumer: consumer,
			Streams:  []string{streamKey, ">"},
			Count:    clusterCommandReadCount,
			Block:    clusterCommandReadBlockTimeout,
		}).Result()
		if err != nil {
			if errors.Is(err, redis.Nil) {
				continue
			}
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for _, stream := range streams {
			for _, message := range stream.Messages {
				b.dispatchStreamMessage(ctx, sem, streamKey, message)
			}
		}
	}
}

// autoClaimPending transfers entries idle in the pending list for at least
// clusterCommandClaimLeaseTTL to this consumer and dispatches them.
func (b *redisClusterCommandBus) autoClaimPending(ctx context.Context, sem chan struct{}, streamKey, consumer string) error {
	start := "0-0"
	for {
		messages, next, err := b.client.XAutoClaim(ctx, &redis.XAutoClaimArgs{
			Stream:   streamKey,
			Group:    clusterCommandGroupName,
			Consumer: consumer,
			MinIdle:  clusterCommandClaimLeaseTTL,
			Start:    start,
			Count:    clusterCommandReadCount,
		}).Result()
		if err != nil {
			return err
		}
		for _, message := range messages {
			b.dispatchStreamMessage(ctx, sem, streamKey, message)
		}
		if next == "0" || next == "0-0" {
			return nil
		}
		start = next
	}
}

// dispatchStreamMessage runs one stream entry through handleMessage on a
// bounded handler goroutine and XACKs it afterwards. The ACK happens for
// every outcome — success, handler failure, missing handler, and HMAC
// rejection alike — so a poison message is not redelivered forever; entries
// whose payload was trimmed by the approximate MAXLEN are ACKed without
// handling.
func (b *redisClusterCommandBus) dispatchStreamMessage(ctx context.Context, sem chan struct{}, streamKey string, message redis.XMessage) {
	sem <- struct{}{}
	b.handlerWG.Add(1)
	go func() {
		defer b.handlerWG.Done()
		defer func() { <-sem }()
		if payload, ok := message.Values[clusterCommandStreamPayloadField].(string); ok {
			b.handleMessage(ctx, payload)
		}
		if err := b.client.XAck(ctx, streamKey, clusterCommandGroupName, message.ID).Err(); err != nil && ctx.Err() == nil {
			log.WarnContext(ctx, "failed to ack cluster command stream entry",
				"stream", streamKey, "entry_id", message.ID, "error", err)
		}
	}()
}

// recordCommandBusDisconnect counts an unexpected bus disconnect (the Warn
// log lives in the retry loop).
func (b *redisClusterCommandBus) recordCommandBusDisconnect() {
	b.disconnects.Add(1)
}

// stopped reports whether Shutdown has been called, i.e. reconnection must
// stop even though ctx may not be cancelled yet.
func (b *redisClusterCommandBus) stopped() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.stop
}

// waitReconnectBackoff sleeps for the current backoff and doubles it (capped)
// for the next attempt. Returns false when ctx is done, i.e. the caller must
// stop retrying.
func (b *redisClusterCommandBus) waitReconnectBackoff(ctx context.Context, backoff *time.Duration, maxBackoff time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(*backoff):
	}
	*backoff *= 2
	if *backoff > maxBackoff {
		*backoff = maxBackoff
	}
	return true
}

func (b *redisClusterCommandBus) Shutdown(ctx context.Context) error {
	b.mu.Lock()
	if b.stop {
		b.mu.Unlock()
		return nil
	}
	b.stop = true
	if b.cancel != nil {
		b.cancel()
	}
	b.mu.Unlock()

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		b.readerWG.Wait()
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-readerDone:
	}
	handlersDone := make(chan struct{})
	go func() {
		defer close(handlersDone)
		b.handlerWG.Wait()
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-handlersDone:
	}
	return b.client.Close()
}

func (b *redisClusterCommandBus) SendCommand(ctx context.Context, cmd *cluster.ClusterCommand) (*cluster.ClusterCommandResult, error) {
	if cmd == nil || cmd.TargetNodeID == "" || cmd.TargetIncarnationID == "" {
		return nil, nil
	}

	// Stamp the sender identity for audit; see the package trust-boundary
	// comment — this is not a security mechanism.
	cmd.IssuedBy = b.nodeID

	commandCtx, cancel := ensureCommandTimeout(ctx)
	defer cancel()

	if cmd.CommandID == "" {
		cmd.CommandID = uuid.NewString()
	}
	cmd.IssuedAt = b.nowTime()
	if cmd.Metadata == nil {
		cmd.Metadata = make(map[string]string)
	}
	if resolvedResult, err := b.resolveExistingCommand(commandCtx, cmd.CommandID); err != nil {
		return nil, err
	} else if resolvedResult != nil {
		b.recordDedupeHit(commandCtx, cmd, "send")
		return resolvedResult, nil
	}

	// Fast-fail when the target incarnation holds no live node lease: a dead
	// target would otherwise leave the sender waiting for the full command
	// deadline (defaultCommandTimeout) before resolving UnknownFinalState.
	alive, err := b.targetAlive(commandCtx, cmd.TargetNodeID, cmd.TargetIncarnationID)
	if err != nil {
		return nil, err
	}
	if !alive {
		return &cluster.ClusterCommandResult{
			CommandID:     cmd.CommandID,
			SessionID:     cmd.SessionID,
			NodeID:        cmd.TargetNodeID,
			IncarnationID: cmd.TargetIncarnationID,
			Status:        cluster.ClusterCommandStatusFailed,
			ErrorCode:     "TARGET_NODE_NOT_ALIVE",
			ErrorMessage:  "target node incarnation has no live lease",
		}, nil
	}

	replyChannel := b.replyChannel(uuid.NewString())
	cmd.Metadata[clusterCommandReplyKey] = replyChannel

	pubsub := b.client.Subscribe(commandCtx, replyChannel)
	defer func() { _ = pubsub.Close() }()
	if _, err := pubsub.Receive(commandCtx); err != nil {
		return nil, err
	}

	// Sign before enqueueing: an envelope that cannot be signed must never
	// reach the wire, and the signature must cover the final CommandID and
	// IssuedAt stamped above.
	if err := clusterhmac.SignCommand(b.hmacKey, cmd); err != nil {
		return nil, fmt.Errorf("sign cluster command: %w", err)
	}

	payload, err := json.Marshal(cmd)
	if err != nil {
		return nil, err
	}
	// Requests are appended to the target incarnation's inbox stream (never
	// published): the stream plus its consumer group gives at-least-once
	// delivery, and command-id dedupe bounds side effects to one execution.
	if err := b.client.XAdd(commandCtx, &redis.XAddArgs{
		Stream: b.streamKey(cmd.TargetNodeID, cmd.TargetIncarnationID),
		MaxLen: clusterCommandStreamMaxLen,
		Approx: true,
		Values: map[string]any{clusterCommandStreamPayloadField: string(payload)},
	}).Err(); err != nil {
		return nil, err
	}

	return b.waitForReply(commandCtx, cmd, pubsub.Channel())
}

// targetAlive reports whether the target node incarnation currently holds a
// live node lease. SendCommand calls it before subscribing for the reply so
// a dead target fails fast instead of burning the full command deadline.
// The lease key layout matches redisSessionDirectory.nodeLeaseKey.
func (b *redisClusterCommandBus) targetAlive(ctx context.Context, nodeID, incarnationID string) (bool, error) {
	data, err := b.client.Get(ctx, b.opts.ClusterNodePrefix+nodeID+":"+incarnationID).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	lease := &cluster.ClusterNodeLease{}
	if err := json.Unmarshal([]byte(data), lease); err != nil {
		// Unreadable lease: be permissive and let the normal send path run
		// (the publish/reply machinery reports the real failure).
		return true, nil
	}
	return lease.ExpiresAt.IsZero() || lease.ExpiresAt.After(time.Now()), nil
}

// waitForReply drains the reply channel until a result whose CommandID matches
// the command arrives. Mismatched replies are logged and skipped as defense in
// depth; the wait still ends at the command deadline.
func (b *redisClusterCommandBus) waitForReply(ctx context.Context, cmd *cluster.ClusterCommand, replies <-chan *redis.Message) (*cluster.ClusterCommandResult, error) {
	for {
		select {
		case <-ctx.Done():
			return b.resolveTimeout(ctx, cmd)
		case reply, ok := <-replies:
			if !ok {
				// The reply channel can close at the exact moment the command
				// deadline fires (go-redis closes the connection when the
				// deadline passes). Prefer the timeout resolution in that case
				// so the caller observes UnknownFinalState instead of a hard
				// channel-closed error.
				if ctx.Err() != nil {
					return b.resolveTimeout(ctx, cmd)
				}
				return nil, fmt.Errorf("cluster command reply channel closed")
			}
			result := &cluster.ClusterCommandResult{}
			if err := json.Unmarshal([]byte(reply.Payload), result); err != nil {
				return nil, err
			}
			// A forged or unsigned reply is not a reply: record the rejection
			// and keep waiting until the command deadline (which resolves to
			// unknown_final_state), never to a forged succeeded.
			if err := clusterhmac.VerifyResult(b.hmacKey, result, b.nowTime()); err != nil {
				b.recordHMACReject(ctx, "reply", result.CommandID, err)
				continue
			}
			if result.CommandID == cmd.CommandID {
				return result, nil
			}
			log.WarnContext(ctx, "cluster command reply command id mismatch, continuing to wait",
				"expected_command_id", cmd.CommandID,
				"received_command_id", result.CommandID,
			)
		}
	}
}

// resolveTimeout records a reply timeout (when the deadline actually expired)
// and resolves the command through the timed-out path.
func (b *redisClusterCommandBus) resolveTimeout(ctx context.Context, cmd *cluster.ClusterCommand) (*cluster.ClusterCommandResult, error) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		b.recordCommandTimeout(ctx, cmd)
	}
	return b.resolveTimedOutCommand(ctx, cmd)
}

func (b *redisClusterCommandBus) BroadcastCommand(ctx context.Context, cmd *cluster.ClusterCommand) ([]*cluster.ClusterCommandResult, error) {
	if cmd == nil {
		return nil, nil
	}
	// Stamp the sender identity for audit; the per-goroutine copies below
	// inherit it. See the package trust-boundary comment.
	cmd.IssuedBy = b.nodeID
	if cmd.TargetNodeID != "" && cmd.TargetIncarnationID != "" {
		result, err := b.SendCommand(ctx, cmd)
		if result == nil || err != nil {
			return nil, err
		}
		return []*cluster.ClusterCommandResult{result}, nil
	}

	keys, err := scanKeys(ctx, b.client, b.opts.ClusterNodePrefix+"*")
	if err != nil {
		return nil, err
	}

	results := make([]*cluster.ClusterCommandResult, 0, len(keys))
	type broadcastOutcome struct {
		result *cluster.ClusterCommandResult
	}
	outcomes := make(chan broadcastOutcome, len(keys))
	var wg sync.WaitGroup
	for _, key := range keys {
		payload, getErr := b.client.Get(ctx, key).Result()
		if getErr != nil {
			continue
		}
		lease := &cluster.ClusterNodeLease{}
		if err := json.Unmarshal([]byte(payload), lease); err != nil {
			continue
		}
		if cmd.Metadata[cluster.ClusterCommandMetaExcludeSelf] == "true" && lease.NodeID == b.nodeID && lease.IncarnationID == b.incarnationID {
			continue
		}
		wg.Add(1)
		go func(lease *cluster.ClusterNodeLease) {
			defer wg.Done()
			// Deep-copy Metadata: every goroutine mutates its own copy inside
			// SendCommand (e.g. the reply_channel key), and sharing the map
			// between goroutines would race with concurrent map writes.
			copyCommand := *cmd
			copyCommand.Metadata = cloneCommandMetadata(cmd.Metadata)
			copyCommand.CommandID = uuid.NewString()
			copyCommand.TargetNodeID = lease.NodeID
			copyCommand.TargetIncarnationID = lease.IncarnationID
			result, sendErr := b.SendCommand(ctx, &copyCommand)
			if sendErr != nil {
				outcomes <- broadcastOutcome{result: &cluster.ClusterCommandResult{
					CommandID:     copyCommand.CommandID,
					SessionID:     copyCommand.SessionID,
					NodeID:        lease.NodeID,
					IncarnationID: lease.IncarnationID,
					Status:        cluster.ClusterCommandStatusFailed,
					ErrorCode:     "CLUSTER_COMMAND_SEND_FAILED",
					ErrorMessage:  sendErr.Error(),
				}}
				return
			}
			outcomes <- broadcastOutcome{result: result}
		}(lease)
	}
	go func() {
		wg.Wait()
		close(outcomes)
	}()
	for outcome := range outcomes {
		if outcome.result != nil {
			results = append(results, outcome.result)
		}
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].NodeID != results[j].NodeID {
			return results[i].NodeID < results[j].NodeID
		}
		return results[i].IncarnationID < results[j].IncarnationID
	})
	return results, nil
}

// streamKey returns the per-incarnation inbox stream for cluster command
// requests. Note it deliberately lives outside the ml2:cluster:node: prefix so
// the C2 node-lease SCAN never touches it.
func (b *redisClusterCommandBus) streamKey(nodeID, incarnationID string) string {
	return clusterCommandStreamPrefix + nodeID + ":" + incarnationID
}

// consumerName returns this process's consumer name inside the inbox group:
// the incarnation ID, so only one live consumer exists per incarnation.
func (b *redisClusterCommandBus) consumerName() string {
	if b.incarnationID != "" {
		return b.incarnationID
	}
	return clusterCommandGroupName
}

func (b *redisClusterCommandBus) replyChannel(commandID string) string {
	return clusterCommandReplyPrefix + commandID
}

func (b *redisClusterCommandBus) commandStateKey(commandID string) string {
	return clusterCommandStatePrefix + commandID
}

func (b *redisClusterCommandBus) handleMessage(ctx context.Context, payload string) {
	command := &cluster.ClusterCommand{}
	if err := json.Unmarshal([]byte(payload), command); err != nil {
		return
	}

	// HMAC hard gate: unsigned, badly signed, skewed, or id-less commands are
	// rejected BEFORE claiming — no handler run, no dedupe state write (an
	// attacker must not poison a victim's command id), no reply.
	if err := clusterhmac.VerifyCommand(b.hmacKey, command, b.nowTime()); err != nil {
		b.recordHMACReject(ctx, "command", command.CommandID, err)
		return
	}

	// Audit trail: record who issued the command. IssuedBy is filled by the
	// sending bus and is informational only (see the package trust-boundary
	// comment); it is forgeable and not part of the signed bytes.
	log.DebugContext(ctx, "cluster command received",
		"command_id", command.CommandID,
		"command_type", command.Type,
		"issued_by", command.IssuedBy,
		"node_id", b.nodeID,
	)

	result := &cluster.ClusterCommandResult{
		CommandID:     command.CommandID,
		SessionID:     command.SessionID,
		NodeID:        b.nodeID,
		IncarnationID: b.incarnationID,
		Status:        cluster.ClusterCommandStatusFailed,
		ErrorCode:     "CLUSTER_COMMAND_HANDLER_NOT_CONFIGURED",
		ErrorMessage:  "cluster command handler is not configured",
	}

	claimed, storedResult, err := b.claimCommandExecution(ctx, command)
	if err != nil {
		result.ErrorCode = "CLUSTER_COMMAND_DEDUPE_FAILED"
		result.ErrorMessage = err.Error()
		b.publishCommandResult(ctx, command, result)
		return
	}
	if !claimed {
		b.recordDedupeHit(ctx, command, "owner")
		result = storedResult
		if result == nil {
			result = &cluster.ClusterCommandResult{
				CommandID:     command.CommandID,
				SessionID:     command.SessionID,
				NodeID:        b.nodeID,
				IncarnationID: b.incarnationID,
				Status:        cluster.ClusterCommandStatusInProgress,
				ErrorCode:     "COMMAND_IN_PROGRESS",
				ErrorMessage:  "cluster command is already in progress",
			}
		}
		if result.Status == cluster.ClusterCommandStatusPending {
			result = cloneClusterCommandResult(result)
			result.Status = cluster.ClusterCommandStatusInProgress
			result.ErrorCode = "COMMAND_IN_PROGRESS"
			result.ErrorMessage = "cluster command is already in progress"
		}
		b.publishCommandResult(ctx, command, result)
		return
	}

	// Claimed. Hold a short owner lease on the pending state and renew it
	// while the handler runs. If this process dies mid-handling, the lease
	// expires within clusterCommandClaimLeaseTTL and a later sender can
	// re-claim the command instead of being locked in pending for the
	// 10-minute terminal-state TTL.
	renewCtx, stopRenew := context.WithCancel(ctx)
	defer stopRenew()
	go b.renewClaimLease(renewCtx, b.commandStateKey(command.CommandID))

	b.mu.RLock()
	handler := b.handler
	b.mu.RUnlock()
	if handler != nil {
		// Bound each handler execution with a per-command deadline: a
		// handler that blocks (e.g. a stuck survey write) must not pin the
		// command in pending forever.
		handlerCtx, cancel := context.WithTimeout(ctx, clusterCommandHandlerTimeout)
		handledResult, err := b.executeHandlerBounded(handlerCtx, handler, command)
		cancel()
		if err != nil {
			result.Status = cluster.ClusterCommandStatusFailed
			if errors.Is(err, context.DeadlineExceeded) {
				result.ErrorCode = "CLUSTER_COMMAND_TIMEOUT"
				result.ErrorMessage = "cluster command handler exceeded its execution deadline"
			} else {
				result.ErrorCode = "CLUSTER_COMMAND_HANDLER_FAILED"
				result.ErrorMessage = err.Error()
			}
		} else if handledResult != nil {
			result = handledResult
		}
	}
	if result.CommandID == "" {
		result.CommandID = command.CommandID
	}
	if result.SessionID == "" {
		result.SessionID = command.SessionID
	}
	if result.NodeID == "" {
		result.NodeID = b.nodeID
	}
	if result.IncarnationID == "" {
		result.IncarnationID = b.incarnationID
	}
	if result.Status == "" || result.Status == cluster.ClusterCommandStatusPending {
		result.Status = cluster.ClusterCommandStatusSucceeded
	}
	if storeErr := b.storeCommandResult(ctx, result); storeErr != nil {
		result = cloneClusterCommandResult(result)
		result.Status = cluster.ClusterCommandStatusUnknownFinalState
		result.ErrorCode = "UNKNOWN_FINAL_STATE"
		result.ErrorMessage = fmt.Sprintf("cluster command completed but terminal result could not be persisted: %v", storeErr)
		b.recordUnknownFinalState(ctx, command, result.ErrorMessage)
	}
	// The terminal state is persisted with its own long TTL; stop renewing
	// the pending claim lease so it never shortens the terminal result's
	// lifetime.
	stopRenew()
	b.publishCommandResult(ctx, command, result)
}

func ensureCommandTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, defaultCommandTimeout)
}

func (b *redisClusterCommandBus) resolveExistingCommand(ctx context.Context, commandID string) (*cluster.ClusterCommandResult, error) {
	if commandID == "" {
		return nil, nil
	}
	storedResult, err := b.loadCommandResult(ctx, commandID)
	if err != nil || storedResult == nil {
		return storedResult, err
	}
	if storedResult.Status != cluster.ClusterCommandStatusPending {
		return storedResult, nil
	}
	resolved := cloneClusterCommandResult(storedResult)
	resolved.Status = cluster.ClusterCommandStatusInProgress
	resolved.ErrorCode = "COMMAND_IN_PROGRESS"
	resolved.ErrorMessage = "cluster command is already in progress"
	return resolved, nil
}

func (b *redisClusterCommandBus) claimCommandExecution(ctx context.Context, command *cluster.ClusterCommand) (bool, *cluster.ClusterCommandResult, error) {
	if command == nil || command.CommandID == "" {
		return false, nil, nil
	}
	pendingResult := &cluster.ClusterCommandResult{
		CommandID:     command.CommandID,
		SessionID:     command.SessionID,
		NodeID:        b.nodeID,
		IncarnationID: b.incarnationID,
		Status:        cluster.ClusterCommandStatusPending,
	}
	encodedPending, err := json.Marshal(pendingResult)
	if err != nil {
		return false, nil, err
	}
	// Claim with a short lease TTL (not the 10-minute terminal TTL): the
	// owner renews the lease while handling, so a crash is recovered once
	// the lease expires instead of locking the command in pending.
	claimed, err := b.client.SetNX(ctx, b.commandStateKey(command.CommandID), encodedPending, clusterCommandClaimLeaseTTL).Result() //nolint:staticcheck // SetNX is the clearest API for this pattern
	if err != nil {
		return false, nil, err
	}
	if claimed {
		return true, pendingResult, nil
	}
	storedResult, err := b.loadCommandResult(ctx, command.CommandID)
	if err != nil {
		return false, nil, err
	}
	if storedResult != nil {
		return false, storedResult, nil
	}
	claimed, err = b.client.SetNX(ctx, b.commandStateKey(command.CommandID), encodedPending, clusterCommandClaimLeaseTTL).Result() //nolint:staticcheck
	if err != nil {
		return false, nil, err
	}
	if claimed {
		return true, pendingResult, nil
	}
	storedResult, err = b.loadCommandResult(ctx, command.CommandID)
	return false, storedResult, err
}

func (b *redisClusterCommandBus) loadCommandResult(ctx context.Context, commandID string) (*cluster.ClusterCommandResult, error) {
	if commandID == "" {
		return nil, nil
	}
	data, err := b.client.Get(ctx, b.commandStateKey(commandID)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result := &cluster.ClusterCommandResult{}
	if err := json.Unmarshal([]byte(data), result); err != nil {
		return nil, err
	}
	return result, nil
}

func (b *redisClusterCommandBus) storeCommandResult(ctx context.Context, result *cluster.ClusterCommandResult) error {
	if result == nil || result.CommandID == "" {
		return nil
	}
	encodedResult, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return b.client.Set(ctx, b.commandStateKey(result.CommandID), encodedResult, defaultCommandStateTTL).Err()
}

func (b *redisClusterCommandBus) publishCommandResult(ctx context.Context, command *cluster.ClusterCommand, result *cluster.ClusterCommandResult) {
	if command == nil || result == nil {
		return
	}
	replyChannel := command.Metadata[clusterCommandReplyKey]
	if replyChannel == "" {
		return
	}
	if result.IssuedAt.IsZero() {
		result.IssuedAt = b.nowTime()
	}
	if err := clusterhmac.SignResult(b.hmacKey, result); err != nil {
		log.WarnContext(ctx, "failed to sign cluster command result", "command_id", command.CommandID, "error", err)
		return
	}
	encodedResult, err := json.Marshal(result)
	if err != nil {
		return
	}
	_ = b.client.Publish(ctx, replyChannel, encodedResult).Err()
}

// recordHMACReject counts and logs a rejected envelope (command or reply).
// The reason label comes from the verification error; the key is never
// logged or labeled.
func (b *redisClusterCommandBus) recordHMACReject(ctx context.Context, stage, commandID string, err error) {
	reason := string(clusterhmac.RejectBad)
	var verifyErr *clusterhmac.VerifyError
	if errors.As(err, &verifyErr) {
		reason = string(verifyErr.Reason)
	}
	if metrics := b.getMetrics(); metrics != nil {
		metrics.ClusterCommandHMACRejects.WithLabelValues(reason).Inc()
	}
	log.WarnContext(ctx, "cluster envelope rejected by HMAC verification",
		"stage", stage,
		"reason", reason,
		"command_id", commandID,
		"node_id", b.nodeID,
	)
}

func (b *redisClusterCommandBus) executeHandler(ctx context.Context, handler cluster.ClusterCommandHandler, command *cluster.ClusterCommand) (result *cluster.ClusterCommandResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = nil
			err = fmt.Errorf("panic in cluster command handler: %v", recovered)
		}
	}()
	return handler(ctx, command)
}

// executeHandlerBounded runs handler under a per-command deadline. A handler
// that does not return before ctx expires (e.g. it ignores its context) is
// abandoned: a terminal context.DeadlineExceeded error is returned and the
// still-running goroutine's eventual result is discarded, so a stuck handler
// cannot wedge the command bus. Handlers MUST respond to ctx cancellation: a
// handler that keeps running past its deadline continues to occupy its
// clusterCommandHandlerConcurrency slot until it returns.
func (b *redisClusterCommandBus) executeHandlerBounded(ctx context.Context, handler cluster.ClusterCommandHandler, command *cluster.ClusterCommand) (result *cluster.ClusterCommandResult, err error) {
	type outcome struct {
		result *cluster.ClusterCommandResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := b.executeHandler(ctx, handler, command)
		done <- outcome{result: result, err: err}
	}()
	select {
	case o := <-done:
		return o.result, o.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// renewClaimLease extends the pending claim's TTL while the handler runs so
// a long-running command is not stolen by a duplicate sender. It stops when
// ctx is cancelled, which happens once the terminal state has been persisted
// or handleMessage exits.
func (b *redisClusterCommandBus) renewClaimLease(ctx context.Context, key string) {
	ticker := time.NewTicker(clusterCommandClaimRenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := b.client.Expire(ctx, key, clusterCommandClaimLeaseTTL).Err(); err != nil {
				log.WarnContext(ctx, "failed to renew cluster command claim lease", "key", key, "error", err)
			}
		}
	}
}

func (b *redisClusterCommandBus) resolveTimedOutCommand(ctx context.Context, command *cluster.ClusterCommand) (*cluster.ClusterCommandResult, error) {
	if command == nil {
		return nil, ctx.Err()
	}
	storedResult, err := b.loadCommandResult(context.Background(), command.CommandID)
	if err != nil {
		return nil, err
	}
	if storedResult != nil && storedResult.Status != cluster.ClusterCommandStatusPending {
		return storedResult, nil
	}
	b.recordUnknownFinalState(ctx, command, "cluster command timed out before a terminal result was observed")
	return &cluster.ClusterCommandResult{
		CommandID:     command.CommandID,
		SessionID:     command.SessionID,
		NodeID:        command.TargetNodeID,
		IncarnationID: command.TargetIncarnationID,
		Status:        cluster.ClusterCommandStatusUnknownFinalState,
		ErrorCode:     "UNKNOWN_FINAL_STATE",
		ErrorMessage:  "cluster command timed out before a terminal result was observed",
	}, nil
}

func (b *redisClusterCommandBus) getMetrics() *metrics.Metrics {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.metrics
}

func (b *redisClusterCommandBus) recordDedupeHit(ctx context.Context, cmd *cluster.ClusterCommand, stage string) {
	if metrics := b.getMetrics(); metrics != nil {
		metrics.ClusterCommandDedupeHits.Inc()
	}
	if cmd == nil {
		return
	}
	log.DebugContext(ctx, "cluster command dedupe hit",
		"command_id", cmd.CommandID,
		"command_type", cmd.Type,
		"stage", stage,
		"target_node_id", cmd.TargetNodeID,
		"target_incarnation_id", cmd.TargetIncarnationID,
	)
}

func (b *redisClusterCommandBus) recordCommandTimeout(ctx context.Context, cmd *cluster.ClusterCommand) {
	if metrics := b.getMetrics(); metrics != nil {
		metrics.ClusterCommandTimeouts.Inc()
	}
	if cmd == nil {
		return
	}
	log.WarnContext(ctx, "cluster command timed out waiting for reply",
		"command_id", cmd.CommandID,
		"command_type", cmd.Type,
		"target_node_id", cmd.TargetNodeID,
		"target_incarnation_id", cmd.TargetIncarnationID,
	)
}

func (b *redisClusterCommandBus) recordUnknownFinalState(ctx context.Context, cmd *cluster.ClusterCommand, reason string) {
	if metrics := b.getMetrics(); metrics != nil {
		metrics.ClusterCommandUnknownFinalState.Inc()
	}
	if cmd == nil {
		return
	}
	log.WarnContext(ctx, "cluster command entered unknown final state",
		"command_id", cmd.CommandID,
		"command_type", cmd.Type,
		"target_node_id", cmd.TargetNodeID,
		"target_incarnation_id", cmd.TargetIncarnationID,
		"reason", reason,
	)
}

func cloneCommandMetadata(metadata map[string]string) map[string]string {
	if metadata == nil {
		return nil
	}
	clone := make(map[string]string, len(metadata))
	for key, value := range metadata {
		clone[key] = value
	}
	return clone
}

func cloneClusterCommandResult(result *cluster.ClusterCommandResult) *cluster.ClusterCommandResult {
	if result == nil {
		return nil
	}
	clone := *result
	if result.Metadata != nil {
		clone.Metadata = make(map[string]string, len(result.Metadata))
		for key, value := range result.Metadata {
			clone.Metadata[key] = value
		}
	}
	return &clone
}

var _ cluster.ClusterCommandBus = (*redisClusterCommandBus)(nil)
