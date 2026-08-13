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

// runPubSub subscribes to the wildcard Redis Pub/Sub pattern and dispatches
// incoming publication messages to the handler. Blocks until ctx is done.
func (b *redisBroker) runPubSub(ctx context.Context) error {
	pubsub := b.client.PSubscribe(ctx, b.opts.PubSubPrefix+"*")
	b.setActivePubSub(pubsub)
	defer func() {
		b.clearActivePubSub(pubsub)
		_ = pubsub.Close()
	}()

	// Wait for the subscription confirmation: PSubscribe is asynchronous, and
	// Ready() must only close once the subscription is actually live (which
	// also guarantees the epoch is initialized, see Start).
	if _, err := pubsub.Receive(ctx); err != nil {
		return err
	}
	b.readyOnce.Do(func() { close(b.readyCh) })

	// Create the delivery channel before the catch-up: publications arriving
	// while history is replayed are buffered and delivered live afterwards
	// instead of overflowing go-redis's default 100-message buffer and being
	// silently dropped.
	ch := pubsub.ChannelSize(pubsubBufferSize)

	// Messages published while we were disconnected were not delivered in
	// real time: replay them from the stream before resuming live delivery.
	// Catch-up only covers exact channels (wildcard patterns have no stream
	// mapping); the gap is a documented limitation.
	b.catchUpMissed(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			if len(msg.Channel) <= len(b.opts.PubSubPrefix) {
				continue
			}
			channelName := strings.TrimPrefix(msg.Channel, b.opts.PubSubPrefix)

			if !b.interested(channelName) {
				continue
			}

			redisMsg, err := deserializeMessage([]byte(msg.Payload))
			if err != nil || redisMsg.Type != messageTypePublication {
				continue
			}

			b.deliverOnce(channelName, messageToPublication(channelName, redisMsg, redisMsg.Offset))
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
// simulate a disconnect).
func (b *redisBroker) setActivePubSub(pubsub *redis.PubSub) {
	b.pubsubMu.Lock()
	b.activePubSub = pubsub
	b.pubsubMu.Unlock()
}

// clearActivePubSub clears the recorded subscription when it is no longer
// active.
func (b *redisBroker) clearActivePubSub(pubsub *redis.PubSub) {
	b.pubsubMu.Lock()
	if b.activePubSub == pubsub {
		b.activePubSub = nil
	}
	b.pubsubMu.Unlock()
}
