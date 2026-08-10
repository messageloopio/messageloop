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
// offset >= sinceOffset, matching the Broker.History contract (broker.go).
// limit <= 0 returns all available entries.
func (b *redisBroker) getHistory(ch string, sinceOffset uint64, limit int) ([]*messageloop.Publication, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream := b.opts.StreamPrefix + ch

	if limit <= 0 {
		limit = messageloop.DefaultHistoryLimit
	}

	// Build start ID. Use the inclusive form "ts-seq" so the range starts AT
	// sinceOffset: the Broker contract is offset >= sinceOffset.
	start := streamStartID(sinceOffset)

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
		pubTime := redisMsg.Time
		if pubTime == 0 {
			pubTime = time.Now().UnixMilli()
		}
		pubs = append(pubs, &messageloop.Publication{
			Channel:     ch,
			Offset:      parseStreamOffset(m.ID),
			Payload:     redisMsg.Payload,
			Kind:        redisMsg.Kind,
			ContentType: redisMsg.ContentType,
			Id:          redisMsg.Id,
			Metadata:    redisMsg.Metadata,
			Time:        pubTime,
			Epoch:       redisMsg.Epoch,
		})
	}
	return pubs, nil
}

// streamStartID builds the inclusive Redis Stream start ID ("ts-seq") for the
// given offset, so the range starts AT the message at sinceOffset (matching
// the Broker.History contract: offset >= sinceOffset).
// The zero offset maps to "0", i.e. the beginning of the stream.
func streamStartID(sinceOffset uint64) string {
	if sinceOffset == 0 {
		return "0"
	}
	ts := sinceOffset >> 20
	seq := sinceOffset & 0xFFFFF
	return fmt.Sprintf("%d-%d", ts, seq)
}

// parseStreamOffset converts a Redis Stream ID ("ts-seq") into a uint64 offset.
// Encoding: offset = ts<<20 | seq, i.e. ts = offset>>20 and seq = offset&0xFFFFF.
// The seq field occupies the low 20 bits, so up to 2^20-1 (~1M) messages per
// millisecond are representable without colliding with the next millisecond.
// NOTE: this encoding is NOT compatible with the previous offset = ts*1000 + seq
// scheme; offsets persisted under the old encoding do not decode correctly.
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
	return ts<<20 | seq
}
