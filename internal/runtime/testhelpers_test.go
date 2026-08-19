package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

// testProtocolVersion is the Connect version every test uses: the server-side
// version gate (PR-KA-D2) only accepts protocol generation 2.
const testProtocolVersion = "2.0.0"

// publishPub builds a Publication from the legacy (payload, isText) tuple so
// tests keep their intent after the Publication model extension (Task 12).
// newTestClient is the root copy of the helper that moved with hub_test.go
// (PR-KA-D14). Leave-root tests keep the same constructor name.
func newTestClient(t *testing.T, sessionID, userID string) *Session {
	t.Helper()
	ctx := context.Background()
	client, _, err := NewClient(ctx, NewNode(nil), &capturingTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs(sessionID, userID, "client-"+sessionID)
	return client
}

func publishPub(payload []byte, isText bool) *Publication {
	kind := PayloadKindBinary
	if isText {
		kind = PayloadKindText
	}
	return &Publication{Payload: payload, Kind: kind}
}

// posOffset reads the optional offset of a shared.v2 Position.
func posOffset(p *sharedv2.Position) (uint64, bool) {
	if p == nil || p.Offset == nil {
		return 0, false
	}
	return p.GetOffset(), true
}

// cursorOf builds a client cursor Position carrying offset (set=true).
func cursorOf(epoch string, offset uint64) *sharedv2.Position {
	off := offset
	return &sharedv2.Position{StreamEpoch: epoch, Offset: &off}
}

// outboundMessages decodes every frame captured on the transport in write
// order. Because Session.Send is synchronous (B1 §7), the frames of one
// request — bare ack first, then the replay stream — are exactly the
// transport's write sequence.
func outboundMessages(t *testing.T, transport *capturingTransport) []*clientpb.OutboundMessage {
	t.Helper()
	var out []*clientpb.OutboundMessage
	for _, data := range transport.snapshotMessages() {
		var msg clientpb.OutboundMessage
		require.NoError(t, JSONMarshaler{}.Unmarshal(data, &msg))
		out = append(out, &msg)
	}
	return out
}

// recoverCompletes returns every RecoverComplete envelope in wire order.
func recoverCompletes(msgs []*clientpb.OutboundMessage) []*clientpb.RecoverComplete {
	var out []*clientpb.RecoverComplete
	for _, m := range msgs {
		if rc := m.GetRecoverComplete(); rc != nil {
			out = append(out, rc)
		}
	}
	return out
}

// replayPublications returns the replay Publications (every message carries
// replay=true) in wire order; live publications are filtered out.
func replayPublications(msgs []*clientpb.OutboundMessage) []*clientpb.Publication {
	var out []*clientpb.Publication
	for _, m := range msgs {
		pub := m.GetPublication()
		if pub == nil {
			continue
		}
		replay := true
		for _, msg := range pub.GetMessages() {
			if msg == nil || !msg.GetReplay() {
				replay = false
			}
		}
		if replay {
			out = append(out, pub)
		}
	}
	return out
}

// publicationOffsets flattens the messages of a publication batch into their
// position offsets. Unset offsets are omitted (transient frames are expected
// to be absent from recovery lists anyway).
func publicationOffsets(pubs []*clientpb.Publication) []uint64 {
	var offsets []uint64
	for _, pub := range pubs {
		for _, m := range pub.GetMessages() {
			if off, ok := posOffset(m.GetPosition()); ok {
				offsets = append(offsets, off)
			}
		}
	}
	return offsets
}

// assertSubscriptionChannels asserts the ack subscription list contains
// exactly the given channels, in order.
func assertSubscriptionChannels(t *testing.T, subs []*clientpb.Subscription, channels []string) {
	t.Helper()
	got := make([]string, 0, len(subs))
	for _, s := range subs {
		got = append(got, s.GetChannel())
	}
	require.Equal(t, channels, got)
}

// findReplayComplete returns the RecoverComplete for channel, or nil.
func findReplayComplete(msgs []*clientpb.OutboundMessage, channel string) *clientpb.RecoverComplete {
	for _, m := range msgs {
		if rc := m.GetRecoverComplete(); rc != nil && rc.GetChannel() == channel {
			return rc
		}
	}
	return nil
}
