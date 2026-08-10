package redisbroker

import (
	"testing"

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

func TestParseStreamOffsetNoCollisionAtTsBoundary(t *testing.T) {
	const (
		ts = uint64(1750000000000)
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
		offset, err := broker.Publish(ch, []byte("msg-"+string(rune('a'+i))), false)
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
