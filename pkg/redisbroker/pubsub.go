package redisbroker

import (
	"context"
	"strings"
	"time"

	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop"
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
	defer pubsub.Close()

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

			b.subMu.RLock()
			_, interested := b.subscribed[channelName]
			b.subMu.RUnlock()
			if !interested {
				continue
			}

			redisMsg, err := deserializeMessage([]byte(msg.Payload))
			if err != nil || redisMsg.Type != messageTypePublication {
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
		}
	}
}
