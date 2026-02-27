package redisbroker

import (
	"context"
	"strings"
	"time"

	"github.com/messageloopio/messageloop"
)

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
				Offset:  0, // real-time pub/sub messages don't carry stream offset
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
