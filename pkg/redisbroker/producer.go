package redisbroker

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/deeplooplabs/messageloop"
	"github.com/redis/go-redis/v9"
)

// publishToRedis publishes a message to Redis Stream and Pub/Sub.
func (b *redisBroker) publishToRedis(ch string, msg *redisMessage) (messageloop.StreamPosition, bool, error) {
	ctx, cancel := context.WithTimeout(b.ctx, 5*time.Second)
	defer cancel()

	payload, err := serializeMessage(msg)
	if err != nil {
		return messageloop.StreamPosition{}, false, err
	}

	stream := b.options.StreamPrefix + ch

	// Add to stream for history
	var id string
	if b.options.StreamApproximate {
		id, err = b.client.XAdd(ctx, &redis.XAddArgs{
			Stream: stream,
			MaxLen: b.options.StreamMaxLength,
			Approx: true,
			Values: map[string]interface{}{"data": payload},
		}).Result()
	} else {
		id, err = b.client.XAdd(ctx, &redis.XAddArgs{
			Stream: stream,
			MaxLen: b.options.StreamMaxLength,
			Values: map[string]interface{}{"data": payload},
		}).Result()
	}
	if err != nil {
		return messageloop.StreamPosition{}, false, err
	}

	// Set expiration on the stream key
	_ = b.client.Expire(ctx, stream, b.options.HistoryTTL).Err()

	// Publish to pub/sub for real-time delivery
	pubSubCh := b.options.PubSubPrefix + ch
	if err := b.client.Publish(ctx, pubSubCh, payload).Err(); err != nil {
		return messageloop.StreamPosition{}, false, err
	}

	// Parse stream ID to get offset and epoch
	position := b.parseStreamID(id)

	return position, false, nil
}

// parseStreamID parses a Redis Stream ID into StreamPosition.
// Redis Stream IDs have the format "timestamp-sequence" (e.g., "1634567890123-0")
func (b *redisBroker) parseStreamID(id string) messageloop.StreamPosition {
	parts := strings.Split(id, "-")
	if len(parts) != 2 {
		return messageloop.StreamPosition{Epoch: id}
	}

	// The timestamp part serves as our epoch/offset
	timestamp, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return messageloop.StreamPosition{Epoch: id}
	}

	sequence, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return messageloop.StreamPosition{Epoch: id}
	}

	// Combine timestamp and sequence into offset
	offset := timestamp*1000 + sequence

	return messageloop.StreamPosition{
		Offset: offset,
		Epoch:  id,
	}
}
