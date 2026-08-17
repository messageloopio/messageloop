package redisbroker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop"
	"github.com/redis/go-redis/v9"
)

// getHistory retrieves publications from the Redis Stream for ch with
// offset >= sinceOffset, matching the Broker.History contract (broker.go):
// limit <= 0 uses DefaultHistoryLimit. Gap detection follows the §5 table:
// sinceOffset 0 reads from the head (no gap); a positive sinceOffset with no
// entries is HistoryGapEmptyExpired (the stream may have expired or never
// existed — false positives allowed); retained entries starting after
// sinceOffset are HistoryGapHeadTrimmed. FirstRetained comes from the
// first_retained marker, falling back to the first entry of this batch.
func (b *redisBroker) getHistory(ch string, sinceOffset uint64, limit int) (*messageloop.HistoryPage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream := b.opts.StreamPrefix + ch
	page := &messageloop.HistoryPage{}

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
			Seq:         streamEntrySeq(m),
			Payload:     redisMsg.Payload,
			Kind:        redisMsg.Kind,
			ContentType: redisMsg.ContentType,
			Id:          redisMsg.Id,
			Metadata:    redisMsg.Metadata,
			Time:        pubTime,
			Epoch:       redisMsg.Epoch,
		})
	}
	page.Publications = pubs

	// FirstRetained: the marker is best effort — a read failure falls back
	// to the first entry of this batch instead of failing the whole page.
	firstRetained := uint64(0)
	if raw, err := b.client.Get(ctx, b.opts.StreamPrefix+"retained:"+ch).Result(); err == nil {
		firstRetained, _ = strconv.ParseUint(raw, 10, 64)
	} else if !errors.Is(err, redis.Nil) {
		log.WarnContext(ctx, "failed to read first_retained marker; falling back to the first batch entry",
			"channel", ch, "error", err)
	}
	if firstRetained == 0 && len(messages) > 0 {
		firstRetained = parseStreamOffset(messages[0].ID)
	}
	page.FirstRetained = firstRetained

	if sinceOffset > 0 {
		if len(pubs) == 0 {
			// An empty batch with a positive cursor cannot be proven covered:
			// the stream may have expired or never existed. Never claim None.
			page.Gap = true
			page.GapReason = messageloop.HistoryGapEmptyExpired
		} else if firstRetained > sinceOffset {
			page.Gap = true
			page.GapReason = messageloop.HistoryGapHeadTrimmed
		}
	}
	if !page.Gap {
		// True middle-gap detection (C4): adjacent entries whose dense seqs
		// are both known and not consecutive prove at least one entry was
		// deleted from the middle of the retained range. Entries without a
		// dense seq (legacy) break the evidence chain — never assert across
		// an unknown pair (rather miss than libel), so a single-entry or
		// all-legacy page never reports Middle.
		for i := 1; i < len(pubs); i++ {
			prev, cur := pubs[i-1].Seq, pubs[i].Seq
			if prev > 0 && cur > 0 && cur != prev+1 {
				page.Gap = true
				page.GapReason = messageloop.HistoryGapMiddle
				break
			}
		}
	}
	page.Truncated = limit > 0 && len(pubs) == limit
	return page, nil
}

// streamEntrySeq parses the dense per-channel seq ("s" field) of a history
// stream entry (C4). 0 means the entry carries no seq (written before C4).
func streamEntrySeq(m redis.XMessage) uint64 {
	s, ok := m.Values["s"].(string)
	if !ok {
		return 0
	}
	seq, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return seq
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
