package redisbroker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop"
	"github.com/redis/go-redis/v9"
)

// pubsubBufferSize is the go-redis Go channel buffer for live publications.
// The go-redis default (100) can silently drop messages when a burst arrives
// while the consumer is busy (e.g. replaying catch-up after a reconnect).
const pubsubBufferSize = 1024

// deliveryWorkers is the number of publication handler goroutines. Channels
// are hashed onto workers so a slow handler (e.g. a blocked client network
// write) cannot stall the pub/sub consumer loop, catch-up replay, or delivery
// on other channels; the same channel always lands on the same worker, so
// per-channel delivery order is preserved.
const deliveryWorkers = 16

// deliveryQueueSize bounds the per-worker buffered queue. When a worker's
// queue is full, the producer (consumer loop or catch-up) blocks, applying
// backpressure instead of buffering without bound.
const deliveryQueueSize = 256

// liveControlChannel is the stable control subscription on every pub/sub
// connection: its subscribe ack confirms liveness so Ready() can close even
// before any real interest exists, and it keeps the connection subscribed
// when the node has no interests at all. Nothing publishes to it; inbound
// messages on it are ignored. Client keys pass ValidateTopic, so the name is
// never special-cased in topic validation (A3 §5.2).
const liveControlChannel = "__live__"

// liveOpsBufferSize bounds the serial live-subscription change queue. The
// consumer (runPubSub) applies changes on the active connection; while the
// connection is down, the queue fills and further changes are dropped and
// counted — the reconnect rebuild in runPubSub re-derives the full desired
// set, so drops never lose interest (A3 §5.3).
const liveOpsBufferSize = 256

// liveOpAckTimeout bounds how long a Subscribe caller waits for the live
// subscription add to be confirmed on the active connection before giving up.
// Confirmation means Redis processed the subscribe: publications after that
// point are delivered in real time. On timeout the interest is still kept
// locally (and rebuilt on the next connection), so delivery is eventually
// consistent even when the ack is lost.
const liveOpAckTimeout = 5 * time.Second

// liveOp is one queued Redis live-subscription change: subscribe/unsubscribe
// an exact channel or a compiled glob pattern (full name, pubsub prefix
// included). done is closed by the consumer once the change is applied and
// confirmed (adds) or applied (removes); nil means fire-and-forget.
type liveOp struct {
	add     bool
	pattern bool // true → PSubscribe/PUnsubscribe, false → Subscribe/Unsubscribe
	channel string
	done    chan struct{}
}

// enqueueLiveOps queues live-subscription changes for the pub/sub consumer.
// Adds are acknowledged before returning when a live connection exists: the
// caller can then rely on real-time delivery for the new interest (e.g. a
// presence join published right after Subscribe returns). The queue is
// bounded; when it is full (the consumer is stuck on a dead connection),
// changes are dropped and counted instead of blocking the Subscribe /
// Unsubscribe caller — the reconnect rebuild recovers the desired set, so a
// dropped op never loses interest permanently.
func (b *redisBroker) enqueueLiveOps(ops []liveOp) {
	for _, op := range ops {
		b.pubsubMu.Lock()
		live := b.activePubSub != nil
		b.pubsubMu.Unlock()
		if op.add && live {
			op.done = make(chan struct{})
		}
		select {
		case b.liveOps <- op:
		default:
			b.liveOpsDropped.Add(1)
			log.WarnContext(context.Background(), "live subscription change dropped: queue full",
				"channel", op.channel, "add", op.add)
			continue
		}
		if op.done != nil {
			select {
			case <-op.done:
			case <-time.After(liveOpAckTimeout):
				log.WarnContext(context.Background(), "live subscription add not confirmed in time",
					"channel", op.channel)
			}
		}
	}
}

// completeLiveOp closes the op's confirmation channel, if any. Safe to call
// from the consumer goroutine exactly once per op.
func completeLiveOp(op liveOp) {
	if op.done != nil {
		close(op.done)
	}
}

// applyLiveOp applies one queued change on the given pub/sub connection and
// confirms it. Called only from the runPubSub goroutine. While disconnected
// (nil pubsub) the change is skipped: the reconnect rebuild subscribes the
// full desired set anyway. Adds already covered by the current connection
// (e.g. subscribed during the rebuild) are confirmed without a redundant
// write.
func (b *redisBroker) applyLiveOp(ctx context.Context, pubsub *redis.PubSub, op liveOp) {
	if pubsub == nil {
		completeLiveOp(op)
		return
	}
	if op.add && b.isLiveActive(op.channel) {
		completeLiveOp(op)
		return
	}

	opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	var err error
	switch {
	case op.pattern && op.add:
		err = pubsub.PSubscribe(opCtx, op.channel)
	case op.pattern && !op.add:
		err = pubsub.PUnsubscribe(opCtx, op.channel)
	case !op.pattern && op.add:
		err = pubsub.Subscribe(opCtx, op.channel)
	default:
		err = pubsub.Unsubscribe(opCtx, op.channel)
	}
	cancel()
	if err != nil {
		log.WarnContext(ctx, "live subscription change failed",
			"channel", op.channel, "add", op.add, "error", err)
		completeLiveOp(op)
		return
	}
	if op.add {
		// The Redis ack (subscribe/psubscribe) is the confirmation point: it
		// is emitted right after Redis processed the command, so every
		// publication after that is guaranteed to reach this connection.
		b.setLiveActive(op.channel)
		b.pendingLiveOps[op.channel] = append(b.pendingLiveOps[op.channel], op.done)
		return
	}
	b.clearLiveActive(op.channel)
	completeLiveOp(op)
}

// completePendingLiveOps confirms every add op still awaiting its Redis ack,
// then forgets them. Called from the runPubSub goroutine when the connection
// goes away: the reconnect rebuild re-subscribes the desired set.
func (b *redisBroker) completePendingLiveOps() {
	for name, dones := range b.pendingLiveOps {
		for _, done := range dones {
			if done != nil {
				close(done)
			}
		}
		delete(b.pendingLiveOps, name)
	}
}

// isLiveActive reports whether name is currently subscribed on the active
// connection. Guarded by pubsubMu.
func (b *redisBroker) isLiveActive(name string) bool {
	b.pubsubMu.Lock()
	defer b.pubsubMu.Unlock()
	_, ok := b.liveActive[name]
	return ok
}

// setLiveActive records name as subscribed on the active connection. Guarded
// by pubsubMu.
func (b *redisBroker) setLiveActive(name string) {
	b.pubsubMu.Lock()
	b.liveActive[name] = struct{}{}
	b.pubsubMu.Unlock()
}

// clearLiveActive forgets name on the active connection. Guarded by pubsubMu.
func (b *redisBroker) clearLiveActive(name string) {
	b.pubsubMu.Lock()
	delete(b.liveActive, name)
	b.pubsubMu.Unlock()
}

// liveDesiredLocked returns the full set of Redis channels/patterns (pubsub
// prefix included) this node currently needs, derived from the compiled
// interest of every subscribed key. Caller must hold subMu (read or write).
func (b *redisBroker) liveDesiredLocked() map[string]struct{} {
	desired := make(map[string]struct{})
	add := func(name string) { desired[name] = struct{}{} }
	for ch := range b.subscribed {
		ci, err := messageloop.CompileInterest(ch)
		if err != nil {
			continue
		}
		if ci.Exact != "" {
			add(b.opts.PubSubPrefix + ci.Exact)
		}
	}
	for key := range b.wcCounts {
		ci, err := messageloop.CompileInterest(key)
		if err != nil {
			continue
		}
		if ci.Pattern != "" {
			add(b.opts.PubSubPrefix + ci.Pattern)
		}
		if ci.AlsoExact != "" {
			add(b.opts.PubSubPrefix + ci.AlsoExact)
		}
	}
	return desired
}

// liveDiffLocked computes the desired live-sub set, diffs it against the last
// enqueued set, and returns the ops to apply (add for newly desired names,
// remove for names that lost all interest — multiple keys sharing one
// compiled name keep it desired until every key is gone). Caller must hold
// subMu (write).
func (b *redisBroker) liveDiffLocked() []liveOp {
	desired := b.liveDesiredLocked()
	var ops []liveOp
	for name := range desired {
		if _, ok := b.liveDesired[name]; !ok {
			ops = append(ops, liveOp{
				add:     true,
				pattern: strings.Contains(name, "*"),
				channel: name,
			})
		}
	}
	for name := range b.liveDesired {
		if _, ok := desired[name]; !ok {
			ops = append(ops, liveOp{
				add:     false,
				pattern: strings.Contains(name, "*"),
				channel: name,
			})
		}
	}
	b.liveDesired = desired
	return ops
}

// rebuildLiveSubs subscribes the current compiled interest on a fresh pub/sub
// connection: exact channels via Subscribe, patterns via PSubscribe, the
// trailing-** zero-segment channel via Subscribe (A3 §5.2). Only
// CompileInterest results are used; there is no default PSubscribe(prefix+"*")
// anymore. Each subscribed name is recorded as active so queued add ops for
// it are confirmed without a redundant write.
func (b *redisBroker) rebuildLiveSubs(ctx context.Context, pubsub *redis.PubSub) error {
	b.subMu.RLock()
	desired := b.liveDesiredLocked()
	b.subMu.RUnlock()

	for name := range desired {
		opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		var err error
		if strings.Contains(name, "*") {
			err = pubsub.PSubscribe(opCtx, name)
		} else {
			err = pubsub.Subscribe(opCtx, name)
		}
		cancel()
		if err != nil {
			return fmt.Errorf("redis broker: rebuild live subscription %q: %w", name, err)
		}
		b.setLiveActive(name)
	}
	return nil
}

// delivery is one queued handler invocation.
type delivery struct {
	channel string
	pub     *messageloop.Publication
}

// startDeliveryWorkers launches the bounded handler pool; workers exit when
// ctx is done. Safe to call once (idempotent).
func (b *redisBroker) startDeliveryWorkers(ctx context.Context) {
	if !b.deliveryActive.CompareAndSwap(false, true) {
		return
	}
	for i := range b.deliverChans {
		queue := make(chan delivery, deliveryQueueSize)
		b.deliverChans[i] = queue
		go func(q chan delivery) {
			for {
				select {
				case <-ctx.Done():
					return
				case d := <-q:
					b.deliver(d.channel, d.pub)
				}
			}
		}(queue)
	}
}

// deliveryWorkerIndex hashes a channel name onto the worker pool, preserving
// per-channel ordering (the same channel always maps to the same worker).
func deliveryWorkerIndex(channel string) int {
	var h uint32 = 2166136261
	for i := 0; i < len(channel); i++ {
		h ^= uint32(channel[i])
		h *= 16777619
	}
	return int(h % deliveryWorkers)
}

// dispatch hands the publication to the worker owning its channel. Before
// Start (unit tests, no worker pool) the handler runs inline.
func (b *redisBroker) dispatch(channel string, pub *messageloop.Publication) {
	if b.deliveryActive.Load() {
		b.deliverChans[deliveryWorkerIndex(channel)] <- delivery{channel: channel, pub: pub}
		return
	}
	b.deliver(channel, pub)
}

// runPubSubWithRetry wraps runPubSub with exponential backoff reconnection.
func (b *redisBroker) runPubSubWithRetry(ctx context.Context) error {
	backoff := 1 * time.Second
	const maxBackoff = 30 * time.Second

	for {
		err := b.runPubSub(ctx)
		if ctx.Err() != nil {
			return nil
		}
		log.WarnContext(ctx, "redis pubsub disconnected, retrying", "error", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// runPubSub runs the live consumer for one pub/sub connection: it subscribes
// the control channel, rebuilds the compiled interest, and dispatches inbound
// publications to the handler. Blocks until ctx is done or the connection
// fails. There is no default PSubscribe(PubSubPrefix+"*"): Redis subscriptions
// are exactly the CompileInterest results of the current interest (A3 §5.2).
func (b *redisBroker) runPubSub(ctx context.Context) error {
	pubsub := b.client.Subscribe(ctx, b.opts.PubSubPrefix+liveControlChannel)
	b.setActivePubSub(pubsub)
	defer func() {
		b.completePendingLiveOps()
		b.clearActivePubSub(pubsub)
		_ = pubsub.Close()
	}()

	// Wait for the subscription confirmation: the control channel's subscribe
	// ack proves the connection is live (it is always the first subscription,
	// so its ack arrives first), and Ready() must only close once the
	// connection is actually live (which also guarantees the epoch is
	// initialized, see Start).
	if _, err := pubsub.Receive(ctx); err != nil {
		return err
	}
	b.readyOnce.Do(func() { close(b.readyCh) })

	// Subscribe the current compiled interest before consuming messages so a
	// publication arriving right after connect is not missed in real time
	// (exact channels are additionally covered by the stream catch-up below).
	if err := b.rebuildLiveSubs(ctx, pubsub); err != nil {
		return err
	}

	// Create the delivery channel before the catch-up: publications arriving
	// while history is replayed are buffered and delivered live afterwards
	// instead of overflowing go-redis's default 100-message buffer and being
	// silently dropped. ChannelWithSubscriptions also surfaces the
	// subscribe/psubscribe acks used to confirm dynamic live-subscription
	// adds (see applyLiveOp).
	ch := pubsub.ChannelWithSubscriptions(redis.WithChannelSize(pubsubBufferSize))

	// Messages published while we were disconnected were not delivered in
	// real time: replay them from the stream before resuming live delivery.
	// Catch-up only covers exact channels (wildcard patterns have no stream
	// mapping); the gap is a documented limitation.
	b.catchUpMissed(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case op := <-b.liveOps:
			// Serialized live-subscription changes: applied on this
			// connection while it is alive; queued changes are recovered by
			// the rebuild on the next connection.
			b.applyLiveOp(ctx, pubsub, op)
		case item, ok := <-ch:
			if !ok {
				return nil
			}
			switch m := item.(type) {
			case *redis.Subscription:
				// Confirms every dynamic add awaiting this channel's ack
				// (kind subscribe/psubscribe); other kinds (unsubscribe
				// acks, reconnect resubscribes) match no pending op and are
				// ignored.
				if dones, ok := b.pendingLiveOps[m.Channel]; ok {
					for _, done := range dones {
						if done != nil {
							close(done)
						}
					}
					delete(b.pendingLiveOps, m.Channel)
				}
			case *redis.Message:
				if len(m.Channel) <= len(b.opts.PubSubPrefix) {
					continue
				}
				channelName := strings.TrimPrefix(m.Channel, b.opts.PubSubPrefix)
				if channelName == liveControlChannel {
					continue
				}

				// Exact interest or a segment-level pattern match (the Redis
				// glob over-match is discarded here, see interested).
				if !b.interested(channelName) {
					continue
				}

				redisMsg, err := deserializeMessage([]byte(m.Payload))
				if err != nil || redisMsg.Type != messageTypePublication {
					continue
				}

				b.deliverOnce(channelName, messageToPublication(channelName, redisMsg, redisMsg.Offset))
			default:
				// *redis.Pong (go-redis health pings) and anything else.
			}
		}
	}
}

// catchUpMissed replays publications that were missed while the pub/sub
// connection was down: for every exact channel with a recorded last offset,
// it re-reads the stream from lastOffset+1 and feeds the handler. Wildcard
// patterns cannot be mapped to a stream and are not caught up (documented
// limitation). When the replayed range is truncated or the stream no longer
// holds the baseline, the missing entries cannot be replayed: the gap is
// detected and surfaced (warning + counter) instead of failing silently.
func (b *redisBroker) catchUpMissed(ctx context.Context) {
	b.subMu.RLock()
	channels := make([]string, 0, len(b.subscribed))
	offsets := make(map[string]uint64, len(b.subscribed))
	for ch := range b.subscribed {
		channels = append(channels, ch)
	}
	for ch, off := range b.lastOffsets {
		offsets[ch] = off
	}
	b.subMu.RUnlock()

	for _, ch := range channels {
		last := offsets[ch]
		if last == 0 {
			// No known delivery baseline: the gap cannot be bounded, skip.
			continue
		}
		msgs, err := b.client.XRangeN(ctx, b.opts.StreamPrefix+ch, streamStartID(last+1), "+", int64(b.opts.StreamMaxLength)).Result()
		if err != nil {
			log.WarnContext(ctx, "catch-up failed", err, "channel", ch)
			continue
		}
		for _, m := range msgs {
			data, ok := m.Values["data"].(string)
			if !ok {
				continue
			}
			redisMsg, err := deserializeMessage([]byte(data))
			if err != nil || redisMsg.Type != messageTypePublication {
				continue
			}
			b.deliverOnce(ch, messageToPublication(ch, redisMsg, parseStreamOffset(m.ID)))
		}
		b.checkCatchUpGap(ctx, ch, msgs, last)
	}
}

// checkCatchUpGap detects catch-up ranges that could not be replayed in
// full. Under approximate trimming the stream can hold slightly more than
// StreamMaxLength entries, so a full XRangeN batch is not proof that the
// newest entry was reached: when the newest stream offset is beyond the last
// replayed one, the truncated tail cannot be replayed and the gap is
// surfaced as a warning + counter instead of failing silently. (A trimmed
// stream head is not detectable this way: offsets are millisecond-based, so
// a normal pause between publications is indistinguishable from missing
// entries.) A client-facing gap envelope is future work (see the Broker
// contract in broker.go).
func (b *redisBroker) checkCatchUpGap(ctx context.Context, ch string, msgs []redis.XMessage, last uint64) {
	if len(msgs) < int(b.opts.StreamMaxLength) {
		// The range was not truncated: nothing newer was missed.
		return
	}
	deliveredTail := last
	if n := len(msgs); n > 0 {
		deliveredTail = parseStreamOffset(msgs[n-1].ID)
	}
	newest, err := b.newestStreamOffset(ctx, b.opts.StreamPrefix+ch)
	if err != nil {
		log.WarnContext(ctx, "catch-up gap check failed", err, "channel", ch)
		return
	}
	if newest > deliveredTail {
		b.catchUpGaps.Add(1)
		log.WarnContext(ctx, "catch-up gap detected: stream entries newer than the replayed tail were not delivered",
			"channel", ch, "last_replayed_offset", deliveredTail, "newest_stream_offset", newest)
	}
}

// newestStreamOffset returns the offset of the newest entry in the stream
// (0 when the stream is empty or missing).
func (b *redisBroker) newestStreamOffset(ctx context.Context, stream string) (uint64, error) {
	msgs, err := b.client.XRevRangeN(ctx, stream, "+", "-", 1).Result()
	if err != nil {
		return 0, err
	}
	if len(msgs) == 0 {
		return 0, nil
	}
	return parseStreamOffset(msgs[0].ID), nil
}

// deliverOnce hands a publication to the handler exactly once per channel
// offset. The duplicate check and the lastOffset advance run inside one
// critical section, so live delivery and reconnect catch-up can never
// double-deliver the same offset: the second deliverer always observes the
// offset as already recorded. The handler invocation itself runs outside the
// critical section, on a per-channel worker (see dispatch), so a slow
// handler cannot serialize delivery across all channels or stall catch-up.
func (b *redisBroker) deliverOnce(channel string, pub *messageloop.Publication) {
	if pub.Offset == 0 {
		// Transient publications have no stream offset and cannot be
		// deduplicated; deliver unconditionally.
		b.dispatch(channel, pub)
		return
	}

	b.deliverMu.Lock()
	b.subMu.RLock()
	last, ok := b.lastOffsets[channel]
	b.subMu.RUnlock()
	if ok && last >= pub.Offset {
		b.deliverMu.Unlock()
		return
	}
	b.subMu.Lock()
	b.lastOffsets[channel] = pub.Offset
	b.subMu.Unlock()
	b.deliverMu.Unlock()

	b.dispatch(channel, pub)
}

// deliver invokes the publication handler, converting a panic into a logged
// error so a misbehaving handler cannot take down the pub/sub consumer
// goroutine. Delivery errors are logged and counted, never propagated to
// Publish callers (see the Broker contract in broker.go).
func (b *redisBroker) deliver(channel string, pub *messageloop.Publication) {
	if b.handler == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			b.handlerFailures.Add(1)
			log.ErrorContext(context.Background(), "panic in publication handler",
				fmt.Errorf("panic: %v, channel: %s", r, channel))
		}
	}()
	if err := b.handler(channel, pub); err != nil {
		b.handlerFailures.Add(1)
		log.ErrorContext(context.Background(), "publication handler failed", err, "channel", channel)
	}
}

// messageToPublication converts a deserialized redisMessage into a
// Publication for the given channel. The offset is passed explicitly because
// live pub/sub payloads carry it inside the envelope while stream entries
// (catch-up) must derive it from the stream ID instead.
func messageToPublication(channelName string, redisMsg *redisMessage, offset uint64) *messageloop.Publication {
	pub := &messageloop.Publication{
		Channel:     channelName,
		Offset:      offset,
		Epoch:       redisMsg.Epoch,
		Payload:     redisMsg.Payload,
		Kind:        redisMsg.Kind,
		ContentType: redisMsg.ContentType,
		Id:          redisMsg.Id,
		Metadata:    redisMsg.Metadata,
		Time:        redisMsg.Time,
	}
	if pub.Time == 0 {
		pub.Time = time.Now().UnixMilli()
	}
	return pub
}

// setActivePubSub records the live pub/sub subscription (used by tests to
// simulate a disconnect) and resets the active-name mirror: every connection
// starts from an empty subscription set, rebuilt in runPubSub.
func (b *redisBroker) setActivePubSub(pubsub *redis.PubSub) {
	b.pubsubMu.Lock()
	b.activePubSub = pubsub
	b.liveActive = make(map[string]struct{})
	b.pubsubMu.Unlock()
}

// clearActivePubSub clears the recorded subscription when it is no longer
// active.
func (b *redisBroker) clearActivePubSub(pubsub *redis.PubSub) {
	b.pubsubMu.Lock()
	if b.activePubSub == pubsub {
		b.activePubSub = nil
		b.liveActive = make(map[string]struct{})
	}
	b.pubsubMu.Unlock()
}
