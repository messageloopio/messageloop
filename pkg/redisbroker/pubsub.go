package redisbroker

import (
	"context"
	"strings"
	"time"

	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop"
	"github.com/redis/go-redis/v9"
)

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

	// Messages published while we were disconnected were not delivered in
	// real time: replay them from the stream before resuming live delivery.
	// Catch-up only covers exact channels (wildcard patterns have no stream
	// mapping); the gap is a documented limitation.
	b.catchUpMissed(ctx)

	ch := pubsub.Channel()
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

			// Deduplicate against the catch-up window: after a reconnect the
			// live stream may deliver a message that catchUpMissed already
			// replayed (or vice versa).
			if b.offsetDelivered(channelName, redisMsg.Offset) {
				continue
			}

			pub := &messageloop.Publication{
				Channel: channelName,
				Offset:  redisMsg.Offset,
				Epoch:   redisMsg.Epoch,
				Payload: redisMsg.Payload,
				IsText:  redisMsg.IsText,
				Time:    time.Now().UnixMilli(),
			}
			if b.handler != nil {
				_ = b.handler(channelName, pub)
			}
			b.recordDeliveredOffset(channelName, redisMsg.Offset)
		}
	}
}

// catchUpMissed replays publications that were missed while the pub/sub
// connection was down: for every exact channel with a recorded last offset,
// it re-reads the stream from lastOffset+1 and feeds the handler. Wildcard
// patterns cannot be mapped to a stream and are not caught up (documented
// limitation).
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
			offset := parseStreamOffset(m.ID)
			if b.offsetDelivered(ch, offset) {
				continue
			}
			pub := &messageloop.Publication{
				Channel: ch,
				Offset:  offset,
				Epoch:   redisMsg.Epoch,
				Payload: redisMsg.Payload,
				IsText:  redisMsg.IsText,
				Time:    time.Now().UnixMilli(),
			}
			if b.handler != nil {
				_ = b.handler(ch, pub)
			}
			b.recordDeliveredOffset(ch, offset)
		}
	}
}

// offsetDelivered reports whether the given channel offset was already
// delivered (i.e. it is at or below the recorded last offset).
func (b *redisBroker) offsetDelivered(channel string, offset uint64) bool {
	if offset == 0 {
		return false
	}
	b.subMu.RLock()
	defer b.subMu.RUnlock()
	last, ok := b.lastOffsets[channel]
	return ok && last >= offset
}

// recordDeliveredOffset advances the per-channel last offset when offset is
// newer than the recorded one.
func (b *redisBroker) recordDeliveredOffset(channel string, offset uint64) {
	if offset == 0 {
		return
	}
	b.subMu.Lock()
	defer b.subMu.Unlock()
	if last, ok := b.lastOffsets[channel]; !ok || offset > last {
		b.lastOffsets[channel] = offset
	}
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
