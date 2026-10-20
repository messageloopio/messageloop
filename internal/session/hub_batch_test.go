package session

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	clientpb "github.com/messageloopio/messageloop/protocol/genproto/messageloop/client/v2"
)

// TestHub_BroadcastPublications_MultiMessage pins the batching frame shape:
// N publications for one channel ride ONE frame per subscriber, messages in
// offset order with the stable channel-offset IDs, and the batch records the
// maximum offset as delivered.
func TestHub_BroadcastPublications_MultiMessage(t *testing.T) {
	h := newHub(0, 0)

	transport1 := &mockTransport{}
	transport2 := &mockTransport{}
	client1 := newTestClientWithTransport(t, "session-1", "user-1", transport1)
	client2 := newTestClientWithTransport(t, "session-2", "user-2", transport2)
	_, _ = h.AddSub("test-channel", Subscriber{Session: client1, Ephemeral: false})
	_, _ = h.AddSub("test-channel", Subscriber{Session: client2, Ephemeral: false})

	pubs := []*Publication{
		{Channel: "test-channel", Offset: 1, Payload: []byte("one"), Time: time.Now().UnixMilli()},
		{Channel: "test-channel", Offset: 2, Payload: []byte("two"), Time: time.Now().UnixMilli()},
		{Channel: "test-channel", Offset: 3, Payload: []byte("three"), Time: time.Now().UnixMilli()},
	}
	require.NoError(t, h.BroadcastPublications("test-channel", pubs))

	for name, tr := range map[string]*mockTransport{"client1": transport1, "client2": transport2} {
		require.Equal(t, 1, tr.getMessageCount(), "%s must receive exactly one frame for the whole batch", name)
		var out clientpb.OutboundMessage
		require.NoError(t, JSONMarshaler{}.Unmarshal(tr.getMessage(0), &out))
		publication := out.GetPublication()
		require.NotNil(t, publication)
		require.Len(t, publication.Messages, 3)
		for i, wantOffset := range []uint64{1, 2, 3} {
			msg := publication.Messages[i]
			assert.Equal(t, "test-channel", msg.Channel)
			assert.Equal(t, publicationID("test-channel", wantOffset), msg.Id)
		}
	}

	// The delivered-offset bookkeeping records the batch maximum: the frame
	// delivers all three messages atomically per subscriber.
	sub, ok := h.LookupSubscriber("test-channel", client1)
	require.True(t, ok)
	assert.Equal(t, uint64(3), sub.DeliveredOffset, "the batch maximum offset must be recorded as delivered")
}

// TestHub_BroadcastPublications_EmptyBatch is a cheap no-op.
func TestHub_BroadcastPublications_EmptyBatch(t *testing.T) {
	h := newHub(0, 0)
	require.NoError(t, h.BroadcastPublications("test-channel", nil))
}

// TestHub_BroadcastPublications_MultiJSONSplice pins the multi-message raw
// splice: when every publication in the batch is a JSON object, JSON-wire
// subscribers receive the stored payload bytes verbatim — big integers stay
// exact (no float64 rounding through structpb) and no placeholder leaks.
func TestHub_BroadcastPublications_MultiJSONSplice(t *testing.T) {
	h := newHub(0, 0)

	transport := &mockTransport{}
	client := newTestClientWithTransport(t, "session-json", "user-json", transport)
	_, _ = h.AddSub("test-channel", Subscriber{Session: client, Ephemeral: false})

	pubs := []*Publication{
		{Channel: "test-channel", Offset: 1, Kind: PayloadKindJSON, Payload: []byte(`{"n":123456789012345678}`)},
		{Channel: "test-channel", Offset: 2, Kind: PayloadKindJSON, Payload: []byte(`{"s":"hi","nested":{"json":{}}}`)},
	}
	require.NoError(t, h.BroadcastPublications("test-channel", pubs))

	require.Equal(t, 1, transport.getMessageCount())
	frame := transport.getMessage(0)
	assert.True(t, bytes.Contains(frame, []byte("123456789012345678")),
		"the big integer must be spliced verbatim, not float64-rounded")
	assert.True(t, bytes.Contains(frame, []byte(`"json":{"n":123456789012345678}`)),
		"the splice replaces only the {} inside the json key")
	assert.True(t, bytes.Contains(frame, []byte(`"nested":{"json":{}}`)),
		"a payload-internal splice-point lookalike must survive untouched")

	// The frame stays a well-formed client envelope carrying both messages.
	var out clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(frame, &out))
	require.Len(t, out.GetPublication().GetMessages(), 2)
}

// TestHub_BroadcastPublications_MixedJSONFallsBackToProtojson pins the
// all-or-nothing rule: one non-object payload in the batch disables the
// splice for the whole frame; every message takes the structpb path and the
// binary kind survives round trip.
func TestHub_BroadcastPublications_MixedJSONFallsBackToProtojson(t *testing.T) {
	h := newHub(0, 0)

	transport := &mockTransport{}
	client := newTestClientWithTransport(t, "session-json", "user-json", transport)
	_, _ = h.AddSub("test-channel", Subscriber{Session: client, Ephemeral: false})

	pubs := []*Publication{
		{Channel: "test-channel", Offset: 1, Kind: PayloadKindJSON, Payload: []byte(`{"n":123456789012345678}`)},
		{Channel: "test-channel", Offset: 2, Kind: PayloadKindJSON, Payload: []byte(`[1,2,3]`)}, // array: not an object
	}
	require.NoError(t, h.BroadcastPublications("test-channel", pubs))

	require.Equal(t, 1, transport.getMessageCount())
	frame := transport.getMessage(0)
	// structpb cannot hold an array: the whole frame degrades per-kind (the
	// array degrades to text) and the object goes through the round trip —
	// the integer is float64-rounded, which is exactly the pre-batching
	// behavior for non-spliceable payloads.
	assert.False(t, bytes.Contains(frame, []byte("123456789012345678")),
		"no verbatim splice may happen for a mixed batch")

	var out clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(frame, &out))
	msgs := out.GetPublication().GetMessages()
	require.Len(t, msgs, 2)
	assert.NotNil(t, msgs[1].GetPayload().GetText(), "the array payload must degrade to text")
}

// TestSpliceRawJSONPayloads covers the multi-point splice helper: in-order
// replacement across several placeholders, immunity to splice-point
// lookalikes inside already-spliced raw payloads (searches resume after the
// inserted bytes), and the defensive failure path.
func TestSpliceRawJSONPayloads(t *testing.T) {
	frame := []byte(`{"m":[{"p":{"json":{}},"t":"a"},{"p":{"json":{}},"t":"b"}]}`)
	raws := [][]byte{
		[]byte(`{"first":1}`),
		[]byte(`{"second":{"json":{}}}`), // contains the literal splice point
	}
	out, ok := spliceRawJSONPayloads(frame, raws)
	require.True(t, ok)
	// `"json":` is kept from the placeholder; only the `{}` is replaced.
	want := []byte(`{"m":[{"p":{"json":{"first":1}},"t":"a"},{"p":{"json":{"second":{"json":{}}}},"t":"b"}]}`)
	assert.Equal(t, string(want), string(out))

	// More raw payloads than placeholders: the defensive contract matches
	// the single form — report failure, hand back no frame.
	out, ok = spliceRawJSONPayloads(frame, [][]byte{raws[0], raws[1], []byte(`{}`)})
	assert.False(t, ok)
	assert.Nil(t, out)
}
