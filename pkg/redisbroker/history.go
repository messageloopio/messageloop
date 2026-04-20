package redisbroker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/messageloopio/messageloop"
	"github.com/redis/go-redis/v9"
)

// getHistory retrieves publications from the Redis Stream for ch with
// offset >= sinceOffset. limit <= 0 returns all available entries.
func (b *redisBroker) getHistory(ch string, sinceOffset uint64, limit int) ([]*messageloop.Publication, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream := b.opts.StreamPrefix + ch

	if limit <= 0 {
		limit = messageloop.DefaultHistoryLimit
	}

	// Build start ID. Use exclusive form "(ts-seq" so we start AFTER sinceOffset.
	var start string
	if sinceOffset == 0 {
		start = "0"
	} else {
		ts := sinceOffset / 1000
		seq := sinceOffset % 1000
		start = fmt.Sprintf("(%d-%d", ts, seq)
	}

	messages, err := b.client.XRangeN(ctx, stream, start, "+", int64(limit)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}

	pubs := make([]*messageloop.Publication, 0, len(messages))
	for _, m := range messages {
		data, ok := m.Values["data"].(string)
		if !ok {
			continue
		}
		redisMsg, err := deserializeMessage([]byte(data))
		if err != nil || redisMsg.Type != messageTypePublication {
			continue
		}
		pubs = append(pubs, &messageloop.Publication{
			Channel: ch,
			Offset:  parseStreamOffset(m.ID),
			Payload: redisMsg.Payload,
			IsText:  redisMsg.IsText,
			Time:    time.Now().UnixMilli(),
		})
	}
	return pubs, nil
}

// parseStreamOffset converts a Redis Stream ID ("ts-seq") into a uint64 offset.
// Encoding: offset = ts*1000 + seq. Lossless as long as seq < 1000 per ms,
// which is guaranteed in practice since Redis resets seq per millisecond.
func parseStreamOffset(id string) uint64 {
	parts := strings.SplitN(id, "-", 2)
	if len(parts) != 2 {
		return 0
	}
	ts, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0
	}
	seq, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return 0
	}
	return ts*1000 + seq
}
