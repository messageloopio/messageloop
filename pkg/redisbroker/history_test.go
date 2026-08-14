package redisbroker

import (
	"context"
	"testing"
	"time"

	"github.com/messageloopio/messageloop"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		_, err := broker.Publish(ch, &messageloop.Publication{
			Payload:     []byte{byte('a' + i)},
			Kind:        messageloop.PayloadKindBinary,
			HistorySize: 3,
		})
		require.NoError(t, err)
	}
	pubs, err := broker.History(ch, 0, 0)
	require.NoError(t, err)
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
	_, err := broker.Publish(ch, &messageloop.Publication{
		Payload:    []byte("msg"),
		Kind:       messageloop.PayloadKindBinary,
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
		offset, err := broker.Publish(ch, &messageloop.Publication{Payload: []byte("msg-" + string(rune('a'+i))), Kind: messageloop.PayloadKindBinary})
		require.NoError(t, err)
		require.NotZero(t, offset)
		offsets = append(offsets, offset)
	}
	o1, o2, o3 := offsets[0], offsets[1], offsets[2]
	require.Less(t, o1, o2)
	require.Less(t, o2, o3)

	// Recovery from o2 must include o2 itself and o3 (exclusive behavior
	// would only return o3).
	pubs, err := broker.History(ch, o2, 0)
	require.NoError(t, err)
	require.Len(t, pubs, 2, "History(ch, o2) must return o2 and o3")
	require.Equal(t, o2, pubs[0].Offset)
	require.Equal(t, o3, pubs[1].Offset)

	// Full scan regression: from the beginning all three must be returned.
	all, err := broker.History(ch, 0, 0)
	require.NoError(t, err)
	require.Len(t, all, 3, "History(ch, 0) must return all entries")
}

// Task 12: old stream entries written without the kind field must deserialize
// with the kind inferred from isText (rolling-upgrade safety).
func TestRedisBroker_Message_BackwardCompat(t *testing.T) {
	textMsg, err := deserializeMessage([]byte(`{"t":"pub","ch":"x","p":"aGVsbG8=","isText":true,"off":5}`))
	require.NoError(t, err)
	require.Equal(t, messageloop.PayloadKindText, textMsg.Kind)

	binaryMsg, err := deserializeMessage([]byte(`{"t":"pub","ch":"x","p":"Ymlu","off":6}`))
	require.NoError(t, err)
	require.Equal(t, messageloop.PayloadKindBinary, binaryMsg.Kind)

	// New-format entries carry the kind explicitly.
	jsonMsg, err := deserializeMessage([]byte(`{"t":"pub","ch":"x","p":"eyJhIjoxfQ==","kind":2,"ct":"application/json","id":"m-1","ts":1700000000000,"off":7}`))
	require.NoError(t, err)
	require.Equal(t, messageloop.PayloadKindJSON, jsonMsg.Kind)
	require.Equal(t, "application/json", jsonMsg.ContentType)
	require.Equal(t, "m-1", jsonMsg.Id)
	require.Equal(t, int64(1700000000000), jsonMsg.Time)
}
