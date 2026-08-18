package redisbroker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/config"
	"github.com/messageloopio/messageloop/pkg/topics"
	"github.com/redis/go-redis/v9"
)

// redisBroker implements messageloop.Broker using Redis Streams (history)
// and Redis Pub/Sub (real-time fan-out).
type redisBroker struct {
	client  *redis.Client
	opts    *Options
	handler messageloop.PublicationHandler
	// occHandler receives live occupancy events (B2). Set via
	// SetOccupancyHandler before Start; never the publication handler.
	occHandler messageloop.OccupancyHandler
	// gapHandler receives catch-up gap notifications (C6). Set via
	// SetGapHandler; nil disables client notification while detection
	// counters/logging keep running. Written before Start and read on the
	// catch-up path, so it is guarded by gapHandlerMu for tests that
	// register it on a running broker.
	gapHandlerMu sync.RWMutex
	gapHandler   messageloop.GapHandler
	// epoch is set by initEpoch during Start and read concurrently by
	// Publish/PublishTransient/Epoch, so it is guarded by atomic.Value.
	epoch atomic.Value

	// Subscription bookkeeping. Exact channels and wildcard patterns are
	// reference counted: the hub removes a broker subscription whenever any
	// wildcard subscriber leaves (hub.removeWildcardSub always reports
	// last=true), so the broker must keep the interest until the count
	// reaches zero.
	subMu      sync.RWMutex
	subscribed map[string]int                 // exact channel -> refcount
	wcCounts   map[string]int                 // wildcard pattern -> refcount
	wcHandles  map[string]*topics.Subscription // pattern -> matcher handle
	matcher    topics.Matcher                 // wildcard pattern matching

	// readyCh is closed once the pub/sub subscription has been confirmed
	// (Ready semantics, aligned with the memory broker).
	readyCh   chan struct{}
	readyOnce sync.Once

	// lastOffsets tracks the highest delivered stream offset per exact
	// channel; it seeds the reconnect catch-up and deduplicates live vs
	// replayed delivery. Guarded by subMu.
	lastOffsets map[string]uint64

	// lastSeqs tracks the highest delivered dense per-channel seq per exact
	// channel, in parallel with lastOffsets (C4). It is the baseline for the
	// reconnect catch-up middle-gap check; only publications with Seq > 0
	// advance it. Guarded by subMu.
	lastSeqs map[string]uint64

	// deliverMu serializes the check+record of each offset so live delivery
	// and catch-up can never double-deliver, no matter the interleaving
	// between the two paths. The handler runs on the worker pool outside
	// this lock (see deliverOnce/dispatch).
	deliverMu sync.Mutex

	// degraded tracks the channels with live delivery-pressure evidence
	// (D4): an occupancy event dropped because its worker queue was full, or
	// a publication dense-seq jump detected by noteLiveSeqGap. A channel is
	// flagged on the transition only, cleared on its next successful enqueue
	// (dispatch/dispatchOccupancy), and the whole set is reset on every
	// reconnect (setActivePubSub). The set has no consumers beyond the
	// live_degraded_channels gauge and logs: it never feeds back into
	// interest or publish decisions. Guarded by degradedMu, a leaf lock: it
	// is never held while acquiring deliverMu/subMu/pubsubMu (at most
	// metricsMu, which itself never reaches back), so the established
	// deliverMu → subMu order is untouched.
	degradedMu sync.Mutex
	degraded   map[string]struct{}

	// deliveryActive is true once the handler worker pool is running (see
	// startDeliveryWorkers). Before Start, deliverOnce dispatches inline.
	deliveryActive atomic.Bool
	// deliverChans routes publications to the per-channel workers.
	deliverChans [deliveryWorkers]chan delivery

	// handlerFailures counts publication handler errors and panics (see
	// deliver). Guarded by atomic ops; wiring into Prometheus would require
	// a metrics hook on the broker.
	handlerFailures atomic.Uint64
	// occupancyFailures counts occupancy handler errors, panics and
	// malformed occupancy envelopes that were dropped.
	occupancyFailures atomic.Uint64
	// catchUpGaps counts reconnect catch-up ranges that could not be
	// replayed in full (see checkCatchUpGap).
	catchUpGaps atomic.Uint64

	// metrics is the shared Prometheus metrics object (D3), wired via
	// SetMetrics after construction. Nil disables counting (nil-tolerant,
	// same paradigm as the cluster command bus).
	metricsMu sync.RWMutex
	metrics   *messageloop.Metrics

	// activePubSub is the live pub/sub subscription; tests close it to
	// simulate a disconnect. Guarded by pubsubMu.
	pubsubMu     sync.Mutex
	activePubSub *redis.PubSub
	// liveActive mirrors the names (full Redis channel/pattern names, pubsub
	// prefix included) currently subscribed on the active connection; it is
	// rebuilt from scratch on every connection. Guarded by pubsubMu.
	liveActive map[string]struct{}

	// liveOps is the serial queue of Redis live-subscription changes
	// (add/remove exact channels and patterns) derived from CompileInterest.
	// runPubSub applies them on the active pub/sub connection so Subscribe /
	// Unsubscribe never touch Redis while holding subMu (A3 §5.3).
	liveOps chan liveOp
	// liveDesired tracks the desired live-sub set at the last enqueue: the
	// diff between it and the recomputed set yields the ops to apply. Guarded
	// by subMu.
	liveDesired map[string]struct{}
	// liveOpsDropped counts live-subscription changes dropped while the
	// serial queue was full (the consumer was stuck on a dead connection);
	// the reconnect rebuild recovers the desired set.
	liveOpsDropped atomic.Uint64
	// pendingLiveOps maps full Redis channel/pattern names to the
	// confirmation channels of add ops whose subscribe/psubscribe ack has
	// not arrived yet (a name can accumulate several waiters). Consumer-
	// goroutine-only (runPubSub), no lock needed.
	pendingLiveOps map[string][]chan struct{}
}

// New creates a new Redis-backed Broker.
// Call go broker.Start(ctx, handler) to start processing events.
func New(cfg config.RedisConfig) messageloop.Broker {
	opts := NewOptions(cfg)
	return &redisBroker{
		client:         newRedisClient(opts),
		opts:           opts,
		subscribed:     make(map[string]int),
		wcCounts:       make(map[string]int),
		wcHandles:      make(map[string]*topics.Subscription),
		matcher:        topics.NewCSTrieMatcher(),
		readyCh:        make(chan struct{}),
		lastOffsets:    make(map[string]uint64),
		lastSeqs:       make(map[string]uint64),
		degraded:       make(map[string]struct{}),
		liveOps:        make(chan liveOp, liveOpsBufferSize),
		liveDesired:    make(map[string]struct{}),
		liveActive:     make(map[string]struct{}),
		pendingLiveOps: make(map[string][]chan struct{}),
	}
}

// Ready returns a channel that is closed once the pub/sub subscription is
// live. It closes exactly once; reconnections after the initial ready do not
// reset it.
func (b *redisBroker) Ready() <-chan struct{} {
	return b.readyCh
}

// Start verifies the Redis connection, initializes the cluster-wide epoch,
// starts the bounded delivery worker pool, and then runs the Pub/Sub
// consumer loop until ctx is cancelled. Intended to be called as:
// go broker.Start(ctx, handler).
func (b *redisBroker) Start(ctx context.Context, handler messageloop.PublicationHandler) error {
	b.handler = handler

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := b.client.Ping(pingCtx).Err(); err != nil {
		return fmt.Errorf("redis broker: connect: %w", err)
	}
	if err := b.initEpoch(ctx); err != nil {
		return fmt.Errorf("redis broker: init epoch: %w", err)
	}

	b.startDeliveryWorkers(ctx)
	defer func() { _ = b.client.Close() }()
	return b.runPubSubWithRetry(ctx)
}

// initEpoch derives the cluster-wide epoch from a fixed Redis key: the first
// node to start creates it (SET NX) and every node (including restarts of the
// same deployment) reuses the stored value, so a node restart does not
// invalidate client offsets and force a full recovery.
func (b *redisBroker) initEpoch(ctx context.Context) error {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Set with NX replaces the deprecated SetNX; a Nil reply means the key
	// already existed (another node won the race), which is not a failure.
	if _, err := b.client.SetArgs(c, b.opts.EpochKey, uuid.NewString(), redis.SetArgs{Mode: "NX"}).Result(); err != nil && !errors.Is(err, redis.Nil) {
		return err
	}
	epoch, err := b.client.Get(c, b.opts.EpochKey).Result()
	if err != nil {
		return err
	}
	b.epoch.Store(epoch)
	return nil
}

// isWildcardChannel reports whether ch is a wildcard pattern, consistent
// with the hub's isWildcard (strings.Contains(ch, "*")).
func isWildcardChannel(ch string) bool {
	return strings.Contains(ch, "*")
}

// Subscribe registers interest in ch on this node. Wildcard patterns are
// matched against incoming pub/sub channels via the topic matcher; both
// exact channels and patterns are reference counted so the underlying
// interest is kept until every subscriber has left. Keys that
// CompileInterest rejects (unroutable patterns like "*.room", bare "*"/"**",
// malformed topics) are refused with ErrPatternNotRoutable / ErrBadTopic
// before any state changes (A3). First subscriptions enqueue the compiled
// Redis live-subscription change; the pub/sub consumer applies it without
// holding subMu.
func (b *redisBroker) Subscribe(ch string) error {
	if _, err := messageloop.CompileInterest(ch); err != nil {
		return err
	}
	var ops []liveOp
	b.subMu.Lock()
	first := false
	if isWildcardChannel(ch) {
		b.wcCounts[ch]++
		if b.wcCounts[ch] == 1 {
			sub, err := b.matcher.Subscribe(ch, ch)
			if err != nil {
				delete(b.wcCounts, ch)
				b.subMu.Unlock()
				return err
			}
			b.wcHandles[ch] = sub
			first = true
		}
	} else {
		b.subscribed[ch]++
		first = b.subscribed[ch] == 1
	}
	if first {
		ops = b.liveDiffLocked()
	}
	b.subMu.Unlock()
	b.enqueueLiveOps(ops)
	return nil
}

// Unsubscribe removes interest in ch on this node, keeping the interest
// while the reference count is still above zero. Last unsubscriptions enqueue
// the compiled Redis live-subscription removal (only when no other key shares
// the same compiled channel/pattern, see liveDiffLocked).
func (b *redisBroker) Unsubscribe(ch string) error {
	var ops []liveOp
	b.subMu.Lock()
	last := false
	if isWildcardChannel(ch) {
		if b.wcCounts[ch] > 0 {
			b.wcCounts[ch]--
			if b.wcCounts[ch] == 0 {
				delete(b.wcCounts, ch)
				if sub, ok := b.wcHandles[ch]; ok {
					b.matcher.Unsubscribe(sub)
					delete(b.wcHandles, ch)
				}
				last = true
			}
		}
	} else {
		if b.subscribed[ch] > 0 {
			b.subscribed[ch]--
			if b.subscribed[ch] == 0 {
				delete(b.subscribed, ch)
				// The delivery baseline is meaningless without subscribers:
				// drop it so the map cannot grow without bound, and a fresh
				// subscription starts from its own baseline instead of
				// replaying history the previous subscriber already consumed.
				delete(b.lastOffsets, ch)
				delete(b.lastSeqs, ch)
				last = true
			}
		}
	}
	if last {
		ops = b.liveDiffLocked()
	}
	b.subMu.Unlock()
	b.enqueueLiveOps(ops)
	return nil
}

// interested reports whether this node wants messages for the given concrete
// channel: exact subscriptions or any wildcard pattern that matches it.
// Patterns are additionally verified with MatchAfterCompile so the Redis
// glob over-match (Redis "*" crosses dots: "im.room.*" also covers
// "im.room.a.b") is discarded before delivery (A3 §4).
func (b *redisBroker) interested(channel string) bool {
	b.subMu.RLock()
	defer b.subMu.RUnlock()
	if b.subscribed[channel] > 0 {
		return true
	}
	for _, pattern := range b.matcher.Lookup(channel) {
		key, ok := pattern.(string)
		if ok && messageloop.MatchAfterCompile(key, channel) {
			return true
		}
	}
	return false
}

// publishScript atomically assigns the per-channel dense seq and appends the
// history stream entry (C4). A two-step Go-side "INCR then XADD" is forbidden:
// a crash between the steps would leave a "seq issued, no entry" phantom
// middle gap. Numbering therefore happens inside the script together with the
// XADD, so either both succeed or both fail. The TTL refresh of the seq
// counter and the stream folds in here as an atomic side effect (previously
// two standalone best-effort EXPIREs).
// KEYS: 1 = seq counter key, 2 = stream key.
// ARGV: 1 = maxLen, 2 = stream data, 3 = TTL milliseconds (PEXPIRE), 4 = "1"
// for approximate MAXLEN trimming, anything else for exact trimming.
// Returns {seq, streamID}.
var publishScript = redis.NewScript(`
local seq = redis.call('INCR', KEYS[1])
local id
if ARGV[4] == '1' then
  id = redis.call('XADD', KEYS[2], 'MAXLEN', '~', ARGV[1], '*', 's', seq, 'data', ARGV[2])
else
  id = redis.call('XADD', KEYS[2], 'MAXLEN', ARGV[1], '*', 's', seq, 'data', ARGV[2])
end
redis.call('PEXPIRE', KEYS[1], ARGV[3])
redis.call('PEXPIRE', KEYS[2], ARGV[3])
return {seq, id}
`)

// Publish writes payload to the Redis Stream (for history) and broadcasts via
// Pub/Sub (for real-time delivery). Returns the stream offset assigned.
func (b *redisBroker) Publish(ch string, pub *messageloop.Publication) (uint64, error) {
	// Channels with explicit empty segments ("a.", ".a", "a..b") and the
	// empty channel are rejected up front so malformed channels never reach
	// Redis (B1).
	if err := topics.ValidateTopic(ch); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	msg := &redisMessage{
		Type:        messageTypePublication,
		Channel:     ch,
		Payload:     pub.Payload,
		Kind:        pub.Kind,
		ContentType: pub.ContentType,
		Id:          pub.Id,
		Metadata:    pub.Metadata,
		Time:        time.Now().UnixMilli(),
		Epoch:       b.epochString(),
	}

	// First, write to stream to get the offset and the dense seq.
	streamData, err := serializeMessage(msg)
	if err != nil {
		return 0, err
	}
	// Per-publication history cap and TTL overrides (from channel policy):
	// a non-zero value wins over the broker global.
	maxLen := b.opts.StreamMaxLength
	if pub.HistorySize > 0 {
		maxLen = int64(pub.HistorySize)
	}
	ttl := b.opts.HistoryTTL
	if pub.HistoryTTL > 0 {
		ttl = pub.HistoryTTL
	}
	stream := b.opts.StreamPrefix + ch
	seqKey := b.opts.StreamPrefix + "seq:" + ch
	approx := "0"
	if b.opts.StreamApproximate {
		approx = "1"
	}
	res, err := publishScript.Run(ctx, b.client, []string{seqKey, stream},
		maxLen, streamData, ttl.Milliseconds(), approx).Result()
	if err != nil {
		return 0, err
	}
	values, ok := res.([]interface{})
	if !ok || len(values) != 2 {
		return 0, fmt.Errorf("redis broker: unexpected publish script result %T %v", res, res)
	}
	seq, ok := values[0].(int64)
	if !ok {
		return 0, fmt.Errorf("redis broker: unexpected seq type %T", values[0])
	}
	id, ok := values[1].(string)
	if !ok {
		return 0, fmt.Errorf("redis broker: unexpected stream id type %T", values[1])
	}

	offset := parseStreamOffset(id)
	msg.Offset = offset
	msg.Seq = uint64(seq)
	msg.Time = time.Now().UnixMilli()
	pub.Offset = offset
	pub.Seq = uint64(seq)
	pub.Time = msg.Time

	// Serialize again with offset and seq included for pub/sub delivery.
	pubSubData, err := serializeMessage(msg)
	if err != nil {
		return 0, err
	}
	b.updateFirstRetained(ctx, ch, offset, ttl)
	if err := b.client.Publish(ctx, b.opts.PubSubPrefix+ch, pubSubData).Err(); err != nil {
		// Publish success means the log accepted the message (KD-K14): a
		// pub/sub delivery failure is a log/metrics event, never a reason to
		// roll back the stream entry (no XDel) or to fail the caller.
		log.WarnContext(ctx, "pub/sub delivery failed; stream entry retained",
			"stream", stream, "id", id, "error", err)
	}
	return offset, nil
}

// updateFirstRetained refreshes the first_retained marker for ch after a
// successful XADD: the stream's first-entry ID read via XINFO, falling back
// to the just-assigned offset when XINFO is unavailable and no marker exists
// yet. The marker expires with the same TTL as the stream, so a fully
// expired channel cannot claim retained coverage. Best effort: failures are
// logged, the marker stays stale at worst.
func (b *redisBroker) updateFirstRetained(ctx context.Context, ch string, fallback uint64, ttl time.Duration) {
	retainedKey := b.opts.StreamPrefix + "retained:" + ch
	if info, err := b.client.XInfoStream(ctx, b.opts.StreamPrefix+ch).Result(); err == nil && info.FirstEntry.ID != "" {
		if first := parseStreamOffset(info.FirstEntry.ID); first > 0 {
			if err := b.client.Set(ctx, retainedKey, strconv.FormatUint(first, 10), ttl).Err(); err != nil {
				log.WarnContext(ctx, "failed to write first_retained marker", "channel", ch, "error", err)
			}
			return
		}
	}
	// XINFO unavailable or the stream reports no first entry: only write the
	// fallback when no marker exists, so a stale marker never claims a wider
	// retained range than reality.
	exists, err := b.client.Exists(ctx, retainedKey).Result()
	if err != nil {
		log.WarnContext(ctx, "failed to check first_retained marker", "channel", ch, "error", err)
		return
	}
	if exists == 0 {
		if err := b.client.Set(ctx, retainedKey, strconv.FormatUint(fallback, 10), ttl).Err(); err != nil {
			log.WarnContext(ctx, "failed to write first_retained marker", "channel", ch, "error", err)
		}
	}
}

// PublishTransient broadcasts via Pub/Sub only, without writing to the Redis
// Stream, so the publication never appears in History. The offset is always
// 0: no stream entry means there is no history offset to report.
func (b *redisBroker) PublishTransient(ch string, pub *messageloop.Publication) error {
	if err := topics.ValidateTopic(ch); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	msg := &redisMessage{
		Type:        messageTypePublication,
		Channel:     ch,
		Payload:     pub.Payload,
		Kind:        pub.Kind,
		ContentType: pub.ContentType,
		Id:          pub.Id,
		Metadata:    pub.Metadata,
		Time:        time.Now().UnixMilli(),
		Epoch:       b.epochString(),
	}
	pubSubData, err := serializeMessage(msg)
	if err != nil {
		return err
	}
	return b.client.Publish(ctx, b.opts.PubSubPrefix+ch, pubSubData).Err()
}

// SetOccupancyHandler registers the live occupancy handler; it must be called
// before Start. Occupancy events never reach the publication handler (B2).
func (b *redisBroker) SetOccupancyHandler(handler messageloop.OccupancyHandler) error {
	b.occHandler = handler
	return nil
}

// SetGapHandler registers the catch-up gap handler (C6); nil disables client
// notification while detection counters and warnings keep running.
func (b *redisBroker) SetGapHandler(handler messageloop.GapHandler) {
	b.gapHandlerMu.Lock()
	b.gapHandler = handler
	b.gapHandlerMu.Unlock()
}

// SetMetrics wires the shared Prometheus metrics object (D3). Nil is
// tolerated and disables counting, so an unwired broker never panics.
func (b *redisBroker) SetMetrics(metrics *messageloop.Metrics) {
	b.metricsMu.Lock()
	defer b.metricsMu.Unlock()
	b.metrics = metrics
}

// getMetrics returns the wired metrics object, or nil when none was set.
func (b *redisBroker) getMetrics() *messageloop.Metrics {
	b.metricsMu.RLock()
	defer b.metricsMu.RUnlock()
	return b.metrics
}

// PublishOccupancy broadcasts an occupancy event on the exact channel's
// pub/sub name. It never writes a Stream entry, so the event is never
// replayed by catch-up and never appears in History. The live envelope type
// is not "pub": the consumer routes it to the occupancy handler instead of
// the publication handler (B2 §5.2).
func (b *redisBroker) PublishOccupancy(ch string, evt messageloop.OccupancyEvent) error {
	if err := topics.ValidateTopic(ch); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	payload, err := serializeOccupancy(evt)
	if err != nil {
		return err
	}
	return b.client.Publish(ctx, b.opts.PubSubPrefix+ch, payload).Err()
}

// History returns a page of publications stored for ch with offset >=
// sinceOffset, plus gap metadata.
func (b *redisBroker) History(ch string, sinceOffset uint64, limit int) (*messageloop.HistoryPage, error) {
	return b.getHistory(ch, sinceOffset, limit)
}

var _ messageloop.Broker = (*redisBroker)(nil)

// Epoch returns the broker's epoch identifier. It is empty until Start has
// initialized it; consumers treat an empty epoch conservatively (full
// recovery).
func (b *redisBroker) Epoch() string {
	if v := b.epoch.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// epochString returns the current epoch (empty before Start initializes it).
func (b *redisBroker) epochString() string {
	if v := b.epoch.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// Ping verifies connectivity to the backing Redis instance. It is exposed
// for the node's health endpoint, which probes Redis in cluster mode.
func (b *redisBroker) Ping(ctx context.Context) error {
	return b.client.Ping(ctx).Err()
}
