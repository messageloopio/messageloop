package redisbroker

import (
	"context"
	"fmt"
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

	// deliverMu serializes the check+record of each offset so live delivery
	// and catch-up can never double-deliver, no matter the interleaving
	// between the two paths. The handler runs on the worker pool outside
	// this lock (see deliverOnce/dispatch).
	deliverMu sync.Mutex

	// deliveryActive is true once the handler worker pool is running (see
	// startDeliveryWorkers). Before Start, deliverOnce dispatches inline.
	deliveryActive atomic.Bool
	// deliverChans routes publications to the per-channel workers.
	deliverChans [deliveryWorkers]chan delivery

	// handlerFailures counts publication handler errors and panics (see
	// deliver). Guarded by atomic ops; wiring into Prometheus would require
	// a metrics hook on the broker.
	handlerFailures atomic.Uint64
	// catchUpGaps counts reconnect catch-up ranges that could not be
	// replayed in full (see checkCatchUpGap).
	catchUpGaps atomic.Uint64

	// activePubSub is the live pub/sub subscription; tests close it to
	// simulate a disconnect. Guarded by pubsubMu.
	pubsubMu     sync.Mutex
	activePubSub *redis.PubSub
}

// New creates a new Redis-backed Broker.
// Call go broker.Start(ctx, handler) to start processing events.
func New(cfg config.RedisConfig) messageloop.Broker {
	opts := NewOptions(cfg)
	return &redisBroker{
		client:      newRedisClient(opts),
		opts:        opts,
		subscribed:  make(map[string]int),
		wcCounts:    make(map[string]int),
		wcHandles:   make(map[string]*topics.Subscription),
		matcher:     topics.NewCSTrieMatcher(),
		readyCh:     make(chan struct{}),
		lastOffsets: make(map[string]uint64),
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
	if _, err := b.client.SetNX(c, b.opts.EpochKey, uuid.NewString(), 0).Result(); err != nil {
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
// interest is kept until every subscriber has left.
func (b *redisBroker) Subscribe(ch string) error {
	b.subMu.Lock()
	defer b.subMu.Unlock()
	if isWildcardChannel(ch) {
		b.wcCounts[ch]++
		if b.wcCounts[ch] == 1 {
			sub, err := b.matcher.Subscribe(ch, ch)
			if err != nil {
				delete(b.wcCounts, ch)
				return err
			}
			b.wcHandles[ch] = sub
		}
		return nil
	}
	b.subscribed[ch]++
	return nil
}

// Unsubscribe removes interest in ch on this node, keeping the interest
// while the reference count is still above zero.
func (b *redisBroker) Unsubscribe(ch string) error {
	b.subMu.Lock()
	defer b.subMu.Unlock()
	if isWildcardChannel(ch) {
		if b.wcCounts[ch] > 0 {
			b.wcCounts[ch]--
			if b.wcCounts[ch] == 0 {
				delete(b.wcCounts, ch)
				if sub, ok := b.wcHandles[ch]; ok {
					b.matcher.Unsubscribe(sub)
					delete(b.wcHandles, ch)
				}
			}
		}
		return nil
	}
	if b.subscribed[ch] > 0 {
		b.subscribed[ch]--
		if b.subscribed[ch] == 0 {
			delete(b.subscribed, ch)
			// The delivery baseline is meaningless without subscribers:
			// drop it so the map cannot grow without bound, and a fresh
			// subscription starts from its own baseline instead of
			// replaying history the previous subscriber already consumed.
			delete(b.lastOffsets, ch)
		}
	}
	return nil
}

// interested reports whether this node wants messages for the given concrete
// channel: exact subscriptions or any wildcard pattern that matches it.
func (b *redisBroker) interested(channel string) bool {
	b.subMu.RLock()
	defer b.subMu.RUnlock()
	if b.subscribed[channel] > 0 {
		return true
	}
	return len(b.matcher.Lookup(channel)) > 0
}

// Publish writes payload to the Redis Stream (for history) and broadcasts via
// Pub/Sub (for real-time delivery). Returns the stream offset assigned.
func (b *redisBroker) Publish(ch string, pub *messageloop.Publication) (uint64, error) {
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

	// First, write to stream to get the offset.
	streamData, err := serializeMessage(msg)
	if err != nil {
		return 0, err
	}
	stream := b.opts.StreamPrefix + ch
	id, err := b.client.XAdd(ctx, &redis.XAddArgs{
		Stream: stream,
		MaxLen: b.opts.StreamMaxLength,
		Approx: b.opts.StreamApproximate,
		Values: map[string]interface{}{"data": streamData},
	}).Result()
	if err != nil {
		return 0, err
	}
	if err := b.client.Expire(ctx, stream, b.opts.HistoryTTL).Err(); err != nil {
		log.WarnContext(ctx, "failed to set stream TTL", "stream", stream, "error", err)
	}

	offset := parseStreamOffset(id)
	msg.Offset = offset
	msg.Time = time.Now().UnixMilli()
	pub.Offset = offset
	pub.Time = msg.Time

	// Serialize again with offset included for pub/sub delivery.
	pubSubData, err := serializeMessage(msg)
	if err != nil {
		return 0, err
	}
	if err := b.client.Publish(ctx, b.opts.PubSubPrefix+ch, pubSubData).Err(); err != nil {
		// Roll back the stream entry so history never contains a message
		// that was not actually delivered in real time. XADD and PUBLISH are
		// not atomic; a leftover entry is only acceptable when the rollback
		// itself fails (clients can still recover it from history).
		if delErr := b.client.XDel(ctx, stream, id).Err(); delErr != nil {
			log.ErrorContext(ctx, "failed to roll back stream entry after pubsub failure",
				delErr, "stream", stream, "id", id)
		}
		return 0, err
	}

	return offset, nil
}

// PublishTransient broadcasts via Pub/Sub only, without writing to the Redis
// Stream, so the publication never appears in History. The offset is always
// 0: no stream entry means there is no history offset to report.
func (b *redisBroker) PublishTransient(ch string, pub *messageloop.Publication) error {
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

// History returns publications stored for ch with offset >= sinceOffset.
func (b *redisBroker) History(ch string, sinceOffset uint64, limit int) ([]*messageloop.Publication, error) {
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
