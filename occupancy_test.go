package messageloop

import (
	"context"
	"os"
	"testing"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	"github.com/stretchr/testify/require"
)

// TestOccupancy_GenZeroOrNilEventDropped pins B2 §4/§5.2: gen==0 and nil
// Event are invalid occupancy events and must be dropped on the receiver
// without delivery and without failing Join/Leave.
func TestOccupancy_GenZeroOrNilEventDropped(t *testing.T) {
	node := NewNode(nil)
	require.NoError(t, node.Run(context.Background()))
	const ch = "invalid.occ.ch"
	_, transport := connectAndSubscribe(t, node, "client-a", &clientpb.Subscription{Channel: ch})
	transport.messages = nil

	for _, evt := range []OccupancyEvent{
		{Gen: 0, Event: &clientpb.PresenceEvent{Action: "join", Info: &clientpb.PresenceInfo{SessionId: "s"}}},
		{Gen: 1, Event: nil},
		{Gen: 0, Event: nil},
		{Gen: 1, Event: &clientpb.PresenceEvent{Action: "join", Info: &clientpb.PresenceInfo{SessionId: ""}}},
	} {
		require.NoError(t, node.onOccupancy(ch, evt),
			"an invalid or session-less occupancy must be dropped, never delivered")
	}
	require.Empty(t, presenceEventsOf(t, transport),
		"gen==0, nil-event and empty-session occupancy events must not reach clients")
}

// TestOccupancy_ErrLateOccupancyIsASentinel pins the exported sentinel used
// to count late events on the receiver.
func TestOccupancy_ErrLateOccupancyIsASentinel(t *testing.T) {
	node := NewNode(nil)

	const ch = "late.sentinel.ch"
	base := OccupancyEvent{
		Event: &clientpb.PresenceEvent{Channel: ch, Action: "join",
			Info: &clientpb.PresenceInfo{SessionId: "sess-late"}},
	}
	require.NoError(t, node.onOccupancy(ch, OccupancyEvent{Event: base.Event, Gen: 11}))
	err := node.onOccupancy(ch, OccupancyEvent{Event: base.Event, Gen: 10})
	require.ErrorIs(t, err, ErrLateOccupancy)
}

// TestOccupancy_NoForbiddenProductionRemnants pins B2 §8.1 at source level:
// Hub has no Node back-reference, broadcastPublication and the live path no
// longer recognize an "ml.type" presence frame, and node.go has no
// presenceClusterEmit / emitPresence remnant.
func TestOccupancy_NoForbiddenProductionRemnants(t *testing.T) {
	hubSrc := readSource(t, "hub.go")
	require.NotContains(t, hubSrc, "node *Node", "Hub must not hold a Node back-reference")
	require.NotContains(t, hubSrc, "ml.type", "broadcastPublication must not recognize an ml.type frame")
	require.NotContains(t, hubSrc, "PresenceMetaType", "the ml.type constants must be gone")

	nodeSrc := readSource(t, "node.go")
	require.NotContains(t, nodeSrc, "presenceClusterEmit", "no cluster_emit helper may remain")
	require.NotContains(t, nodeSrc, "func (n *Node) emitPresence", "emitPresence must be gone")

	eventSrc := readSource(t, "presence_event.go")
	require.NotContains(t, eventSrc, "ml.type", "the live path must not attach ml.type")
	require.NotContains(t, eventSrc, "presencePublication", "the transient presence frame helper must be gone")
}

func readSource(t *testing.T, file string) string {
	t.Helper()
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	return string(data)
}