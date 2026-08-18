package redisbroker

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/messageloopio/messageloop/internal/stream"
)

func TestParseStreamOffsetRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want uint64
	}{
		{name: "zero", id: "0-0", want: 0},
		{name: "seq zero", id: "123456789-0", want: 123456789 << 20},
		{name: "seq above 1000", id: "123456789-1000", want: 123456789<<20 | 1000},
		{name: "seq near cap", id: "123456789-1048575", want: 123456789<<20 | 1048575},
		{name: "large ts", id: "1750000000000-1048575", want: 1750000000000<<20 | 1048575},
		{name: "malformed no dash", id: "123456789", want: 0},
		{name: "malformed empty", id: "", want: 0},
		{name: "malformed bad ts", id: "abc-1", want: 0},
		{name: "malformed bad seq", id: "1-abc", want: 0},
		{name: "malformed extra dash", id: "1-2-3", want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, parseStreamOffset(tc.id))
		})
	}
}

// TestRedisBroker_PerPublishHistorySize verifies the per-publication
// HistorySize override (channel policy): the XADD MAXLEN uses
// pub.HistorySize when set, so a small cap keeps only the last N entries.
// Exact trimming is forced so the assertion is deterministic.
func TestRedisBroker_PerPublishHistorySize(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	broker := New(redisCfg).(*redisBroker)
	t.Cleanup(func() { _ = broker.client.Close() })
	broker.opts.StreamApproximate = false

	ch := "per-pub-cap"
	for i := 0; i < 5; i++ {
		_, err := broker.Publish(ch, &stream.Publication{
			Payload:     []byte{byte('a' + i)},
			Kind:        stream.PayloadKindBinary,
			HistorySize: 3,
		})
		require.NoError(t, err)
	}
	page, err := broker.History(ch, 0, 0)
	require.NoError(t, err)
	pubs := page.Pubs()
	require.Len(t, pubs, 3, "per-publication HistorySize=3 must cap the stream to the last 3 entries")
	require.Equal(t, "c", string(pubs[0].Payload))
	require.Equal(t, "e", string(pubs[2].Payload))
}

// TestRedisBroker_PerPublishHistoryTTL verifies the per-publication
// HistoryTTL override (channel policy): the EXPIRE after XADD uses
// pub.HistoryTTL when set.
func TestRedisBroker_PerPublishHistoryTTL(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	broker := New(redisCfg).(*redisBroker)
	t.Cleanup(func() { _ = broker.client.Close() })

	ch := "per-pub-ttl"
	_, err := broker.Publish(ch, &stream.Publication{
		Payload:    []byte("msg"),
		Kind:       stream.PayloadKindBinary,
		HistoryTTL: 10 * time.Second,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ttl, err := broker.client.TTL(ctx, broker.opts.StreamPrefix+ch).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, 5*time.Second, "stream TTL must reflect the per-publication override")
	require.LessOrEqual(t, ttl, 10*time.Second)
}

func TestParseStreamOffsetNoCollisionAtTsBoundary(t *testing.T) {
	const (
		ts     = uint64(1750000000000)
		maxSeq = uint64(0xFFFFF)
	)

	// Last message of ts must sort before the first message of ts+1:
	// seq overflow across the millisecond boundary must not collide.
	require.Less(t, ts<<20|maxSeq, (ts+1)<<20)

	// Offsets within the same millisecond stay monotonic in seq.
	for seq := uint64(0); seq < maxSeq; seq += 100000 {
		require.Less(t, ts<<20|seq, ts<<20|seq+1)
	}
}

func TestStreamStartID(t *testing.T) {
	const ts = uint64(1750000000000)

	tests := []struct {
		name        string
		sinceOffset uint64
		want        string
	}{
		{name: "zero offset", sinceOffset: 0, want: "0"},
		{name: "first message", sinceOffset: ts << 20, want: "1750000000000-0"},
		{name: "seq above 1000", sinceOffset: ts<<20 | 1000, want: "1750000000000-1000"},
		{name: "seq near cap", sinceOffset: ts<<20 | 0xFFFFF, want: "1750000000000-1048575"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, streamStartID(tc.sinceOffset))
		})
	}
}

func TestStreamOffsetFullRoundTrip(t *testing.T) {
	ids := []string{
		"123456789-0",
		"123456789-1000",
		"123456789-1048575",
		"1750000000000-1000",
		"1750000000000-1048575",
	}
	for _, id := range ids {
		offset := parseStreamOffset(id)
		// The inclusive start ID must reconstruct the same (ts-seq) pair.
		require.Equal(t, id, streamStartID(offset), "id %q", id)
	}
}

// TestRedisBroker_History_InclusiveSinceOffset verifies that History honors
// the Broker contract (offset >= sinceOffset) against real Redis: recovering
// from o2 must return o2 and o3, not just o3.
func TestRedisBroker_History_InclusiveSinceOffset(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	broker := New(redisCfg).(*redisBroker)
	t.Cleanup(func() { _ = broker.client.Close() })

	ch := "history-inclusive"
	offsets := make([]uint64, 0, 3)
	for i := 0; i < 3; i++ {
		offset, err := broker.Publish(ch, &stream.Publication{Payload: []byte("msg-" + string(rune('a'+i))), Kind: stream.PayloadKindBinary})
		require.NoError(t, err)
		require.NotZero(t, offset)
		offsets = append(offsets, offset)
	}
	o1, o2, o3 := offsets[0], offsets[1], offsets[2]
	require.Less(t, o1, o2)
	require.Less(t, o2, o3)

	// Recovery from o2 must include o2 itself and o3 (exclusive behavior
	// would only return o3).
	page, err := broker.History(ch, o2, 0)
	require.NoError(t, err)
	pubs := page.Pubs()
	require.Len(t, pubs, 2, "History(ch, o2) must return o2 and o3")
	require.Equal(t, o2, pubs[0].Offset)
	require.Equal(t, o3, pubs[1].Offset)
	require.False(t, page.Gap, "sinceOffset within the retained range is not a gap")
	require.True(t, page.FirstRetained <= o2, "FirstRetained must be the stream head")

	// Full scan regression: from the beginning all three must be returned.
	all, err := broker.History(ch, 0, 0)
	require.NoError(t, err)
	require.Len(t, all.Pubs(), 3, "History(ch, 0) must return all entries")
	require.False(t, all.Gap, "reading from the head is never a gap")
}

// TestRedisBroker_History_EmptyExpiredSince verifies §10.8: History on a
// channel that never had entries with sinceOffset > 0 reports an empty page
// with GapReason=EmptyExpired, never None.
func TestRedisBroker_History_EmptyExpiredSince(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	broker := New(redisCfg).(*redisBroker)
	t.Cleanup(func() { _ = broker.client.Close() })

	page, err := broker.History("never-published", 42, 0)
	require.NoError(t, err)
	require.Empty(t, page.Pubs())
	require.True(t, page.Gap, "since>0 with no retained entries must never be HistoryGapNone")
	require.Equal(t, stream.HistoryGapEmptyExpired, page.GapReason)
	require.Zero(t, page.FirstRetained)
}

// TestRedisBroker_History_HeadTrimmed verifies the retained marker drives
// gap detection: with MAXLEN=2 the first entry is trimmed, so a sinceOffset
// below the retained head reports GapReason=HeadTrimmed.
func TestRedisBroker_History_HeadTrimmed(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	broker := New(redisCfg).(*redisBroker)
	t.Cleanup(func() { _ = broker.client.Close() })
	broker.opts.StreamApproximate = false

	ch := "history-head-trimmed"
	offsets := make([]uint64, 0, 3)
	for i := 0; i < 3; i++ {
		offset, err := broker.Publish(ch, &stream.Publication{
			Payload:     []byte{byte('a' + i)},
			Kind:        stream.PayloadKindBinary,
			HistorySize: 2,
		})
		require.NoError(t, err)
		offsets = append(offsets, offset)
	}
	o1, o3 := offsets[0], offsets[2]

	page, err := broker.History(ch, o1, 0)
	require.NoError(t, err)
	require.Len(t, page.Pubs(), 2, "entries o2 and o3 remain")
	require.True(t, page.Gap, "the first entry was trimmed")
	require.Equal(t, stream.HistoryGapHeadTrimmed, page.GapReason)
	require.Greater(t, page.FirstRetained, o1, "FirstRetained must point past the trimmed entry")
	require.LessOrEqual(t, page.FirstRetained, o3)
}

// TestRedisBroker_History_NoGapAtHeadWithMarker verifies the marker keeps
// reporting no gap for an untrimmed stream even after a fresh channel.
func TestRedisBroker_History_NoGapAtHeadWithMarker(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	broker := New(redisCfg).(*redisBroker)
	t.Cleanup(func() { _ = broker.client.Close() })

	ch := "history-no-gap"
	offsets := make([]uint64, 0, 2)
	for i := 0; i < 2; i++ {
		offset, err := broker.Publish(ch, &stream.Publication{Payload: []byte("m"), Kind: stream.PayloadKindBinary})
		require.NoError(t, err)
		offsets = append(offsets, offset)
	}
	page, err := broker.History(ch, offsets[0], 0)
	require.NoError(t, err)
	require.Len(t, page.Pubs(), 2)
	require.False(t, page.Gap, "an untrimmed stream covering sinceOffset is not a gap")
	require.Equal(t, offsets[0], page.FirstRetained)
}

// Task 12: old stream entries written without the kind field must deserialize
// with the kind inferred from isText (rolling-upgrade safety).
func TestRedisBroker_Message_BackwardCompat(t *testing.T) {
	textMsg, err := deserializeMessage([]byte(`{"t":"pub","ch":"x","p":"aGVsbG8=","isText":true,"off":5}`))
	require.NoError(t, err)
	require.Equal(t, stream.PayloadKindText, textMsg.Kind)

	binaryMsg, err := deserializeMessage([]byte(`{"t":"pub","ch":"x","p":"Ymlu","off":6}`))
	require.NoError(t, err)
	require.Equal(t, stream.PayloadKindBinary, binaryMsg.Kind)

	// New-format entries carry the kind explicitly.
	jsonMsg, err := deserializeMessage([]byte(`{"t":"pub","ch":"x","p":"eyJhIjoxfQ==","kind":2,"ct":"application/json","id":"m-1","ts":1700000000000,"off":7}`))
	require.NoError(t, err)
	require.Equal(t, stream.PayloadKindJSON, jsonMsg.Kind)
	require.Equal(t, "application/json", jsonMsg.ContentType)
	require.Equal(t, "m-1", jsonMsg.Id)
	require.Equal(t, int64(1700000000000), jsonMsg.Time)
}

// TestRedisBroker_Publish_AtomicDenseSeq verifies C4 §7.1: 50 concurrent
// publishers on one channel produce stream entries whose dense seq ("s"
// field) is exactly 1..50 with no duplicates, offsets stay monotonic, and
// the data JSON envelope carries no "seq" key (the dense seq lives only in
// the entry field and the live pub/sub payload).
func TestRedisBroker_Publish_AtomicDenseSeq(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	broker := New(redisCfg).(*redisBroker)
	t.Cleanup(func() { _ = broker.client.Close() })

	const n = 50
	ch := "dense-seq-atomic"
	seqs := make([]uint64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pub := &stream.Publication{Payload: []byte(fmt.Sprintf("m-%d", i)), Kind: stream.PayloadKindBinary}
			offset, err := broker.Publish(ch, pub)
			if err != nil {
				t.Errorf("publish %d failed: %v", i, err)
				return
			}
			if offset == 0 {
				t.Errorf("publish %d returned zero offset", i)
				return
			}
			if pub.Seq == 0 {
				t.Errorf("publish %d did not backfill pub.Seq", i)
				return
			}
			seqs[i] = pub.Seq
		}(i)
	}
	wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msgs, err := broker.client.XRangeN(ctx, broker.opts.StreamPrefix+ch, "-", "+", n).Result()
	require.NoError(t, err)
	require.Len(t, msgs, n)

	// INCR and XADD commit in the same Lua script, so stream ID order is seq
	// order: entries must carry exactly 1..n, and offsets stay monotonic.
	var prevOffset uint64
	for i, m := range msgs {
		require.Equal(t, uint64(i+1), streamEntrySeq(m), "entry %d (id %s)", i, m.ID)
		offset := parseStreamOffset(m.ID)
		require.Greater(t, offset, prevOffset, "offsets must be monotonic (id %s)", m.ID)
		prevOffset = offset

		data, ok := m.Values["data"].(string)
		require.True(t, ok, "entry %d must carry a data field", i)
		var envelope map[string]any
		require.NoError(t, json.Unmarshal([]byte(data), &envelope))
		require.NotContains(t, envelope, "seq", "the stream data JSON must not carry the dense seq")
		require.NotContains(t, envelope, "off", "the stream data JSON must not carry the offset")
	}

	// Every publisher observed a distinct seq in 1..n.
	seen := make(map[uint64]bool, n)
	for _, s := range seqs {
		require.NotZero(t, s)
		require.False(t, seen[s], "seq %d handed out twice", s)
		seen[s] = true
	}
	require.Len(t, seen, n)
}

// TestRedisBroker_History_MiddleGapDetected verifies C4 §7.2: deleting one
// entry from the middle of the retained range (XDEL) is detected via the
// dense seq and reported as HistoryGapMiddle, while the surviving entries
// are still returned.
func TestRedisBroker_History_MiddleGapDetected(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	broker := New(redisCfg).(*redisBroker)
	t.Cleanup(func() { _ = broker.client.Close() })

	ch := "history-middle-gap"
	offsets := make([]uint64, 0, 5)
	for i := 0; i < 5; i++ {
		pub := &stream.Publication{Payload: []byte{byte('a' + i)}, Kind: stream.PayloadKindBinary}
		offset, err := broker.Publish(ch, pub)
		require.NoError(t, err)
		require.Equal(t, uint64(i+1), pub.Seq)
		offsets = append(offsets, offset)
	}

	// XDEL the third entry (seq 3).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msgs, err := broker.client.XRangeN(ctx, broker.opts.StreamPrefix+ch, "-", "+", 10).Result()
	require.NoError(t, err)
	require.Len(t, msgs, 5)
	deleted, err := broker.client.XDel(ctx, broker.opts.StreamPrefix+ch, msgs[2].ID).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted)

	// Reading from the first entry's offset exposes the hole in the middle.
	page, err := broker.History(ch, offsets[0], 0)
	require.NoError(t, err)
	require.Len(t, page.Pubs(), 4, "the surviving entries must still be returned")
	require.True(t, page.Gap)
	require.Equal(t, stream.HistoryGapMiddle, page.GapReason)

	// A from-head read exposes the same hole.
	head, err := broker.History(ch, 0, 0)
	require.NoError(t, err)
	require.True(t, head.Gap)
	require.Equal(t, stream.HistoryGapMiddle, head.GapReason)

	// A contiguous page (the first two entries) reports no gap.
	page2, err := broker.History(ch, offsets[0], 2)
	require.NoError(t, err)
	require.Len(t, page2.Pubs(), 2)
	require.False(t, page2.Gap, "a page with consecutive dense seqs is not a middle gap")
}

// TestRedisBroker_History_LegacyEntriesBreakChain verifies C4 §7.4: entries
// without the dense seq field (written before C4) break the evidence chain —
// a legacy entry sandwiched between sequenced entries, and an all-legacy
// stream, must never be reported as HistoryGapMiddle.
func TestRedisBroker_History_LegacyEntriesBreakChain(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	broker := New(redisCfg).(*redisBroker)
	t.Cleanup(func() { _ = broker.client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	legacyData := func(ch, payload string) string {
		data, err := serializeMessage(&redisMessage{
			Type:    messageTypePublication,
			Channel: ch,
			Payload: []byte(payload),
			Kind:    stream.PayloadKindBinary,
			Time:    time.Now().UnixMilli(),
		})
		require.NoError(t, err)
		return string(data)
	}

	// seq 1, then a legacy entry (no "s"), then seq 2: neither adjacent pair
	// has both seqs known, so no middle gap may be asserted.
	mixed := "history-legacy-mixed"
	_, err := broker.Publish(mixed, &stream.Publication{Payload: []byte("s1"), Kind: stream.PayloadKindBinary})
	require.NoError(t, err)
	_, err = broker.client.XAdd(ctx, &redis.XAddArgs{
		Stream: broker.opts.StreamPrefix + mixed,
		Values: map[string]interface{}{"data": legacyData(mixed, "legacy")},
	}).Result()
	require.NoError(t, err)
	_, err = broker.Publish(mixed, &stream.Publication{Payload: []byte("s2"), Kind: stream.PayloadKindBinary})
	require.NoError(t, err)

	page, err := broker.History(mixed, 0, 0)
	require.NoError(t, err)
	require.Len(t, page.Pubs(), 3)
	require.False(t, page.Gap, "a legacy entry breaks the evidence chain; never libel a middle gap")
	require.Equal(t, uint64(1), page.Pubs()[0].Seq)
	require.Zero(t, page.Pubs()[1].Seq, "the legacy entry has no dense seq")
	require.Equal(t, uint64(2), page.Pubs()[2].Seq)

	// An all-legacy stream: no seqs at all, no gap.
	legacy := "history-legacy-only"
	for i := 0; i < 2; i++ {
		_, err = broker.client.XAdd(ctx, &redis.XAddArgs{
			Stream: broker.opts.StreamPrefix + legacy,
			Values: map[string]interface{}{"data": legacyData(legacy, fmt.Sprintf("l%d", i))},
		}).Result()
		require.NoError(t, err)
	}
	page, err = broker.History(legacy, 0, 0)
	require.NoError(t, err)
	require.Len(t, page.Pubs(), 2)
	require.False(t, page.Gap, "an all-legacy page must not report a middle gap")
}
