package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/messageloopio/messageloop/config"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- B4: ClusterSessionSnapshot.ChannelOffsets ---

// --- B4: per-session per-channel last-delivered offset bookkeeping ---

// TestHub_Broadcast_RecordsDeliveredOffset verifies that the broadcast path
// records the last successfully delivered offset per exact subscription.
func TestHub_Broadcast_RecordsDeliveredOffset(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	_ = node.Run(ctx)

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-off", "user-off", "client-off")
	require.NoError(t, node.AddClient(client))
	require.NoError(t, node.AddSubscription(ctx, "off.ch", NewSubscriber(client, false)))

	offset, err := node.Publish("off.ch", publishPub([]byte("m1"), false))
	require.NoError(t, err)
	require.Equal(t, uint64(1), offset)
	// Delivery (and with it the offset bookkeeping) is asynchronous.
	require.Eventually(t, func() bool {
		sub, ok := node.hub.LookupSubscriber("off.ch", client)
		return ok && sub.DeliveredOffset == 1
	}, 2*time.Second, time.Millisecond, "broadcast must record the delivered offset")

	offset, err = node.Publish("off.ch", publishPub([]byte("m2"), false))
	require.NoError(t, err)
	require.Equal(t, uint64(2), offset)
	require.Eventually(t, func() bool {
		sub, ok := node.hub.LookupSubscriber("off.ch", client)
		return ok && sub.DeliveredOffset == 2
	}, 2*time.Second, time.Millisecond, "offsets must keep advancing")

	// Transient publications (offset 0) never update the bookkeeping.
	require.NoError(t, node.PublishTransient("off.ch", publishPub([]byte("evt"), false)))
	sub, ok := node.hub.LookupSubscriber("off.ch", client)
	require.True(t, ok)
	require.Equal(t, uint64(2), sub.DeliveredOffset, "transient publications must not move the offset")
}

// TestHub_Broadcast_WildcardDeliveryDoesNotRecordOffset verifies that
// wildcard pattern subscriptions are never offset-tracked: only exact
// channels carry resume offsets.
func TestHub_Broadcast_WildcardDeliveryDoesNotRecordOffset(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	_ = node.Run(ctx)

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-wc", "user-wc", "client-wc")
	require.NoError(t, node.AddClient(client))
	require.NoError(t, node.AddSubscription(ctx, "wc.*", NewSubscriber(client, false)))

	offset, err := node.Publish("wc.room", publishPub([]byte("m1"), false))
	require.NoError(t, err)
	require.Equal(t, uint64(1), offset)

	sub, ok := node.hub.LookupSubscriber("wc.*", client)
	require.True(t, ok)
	require.Zero(t, sub.DeliveredOffset, "wildcard patterns must not record delivered offsets")
}

// TestNode_RemoveSubscription_ClearsDeliveredOffset verifies the cleanup
// symmetry: unsubscribing removes the offset record, and re-subscribing
// starts fresh (no stale offset survives).
func TestNode_RemoveSubscription_ClearsDeliveredOffset(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	_ = node.Run(ctx)

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-clean", "user-clean", "client-clean")
	require.NoError(t, node.AddClient(client))
	require.NoError(t, node.AddSubscription(ctx, "clean.ch", NewSubscriber(client, false)))

	_, err = node.Publish("clean.ch", publishPub([]byte("m1"), false))
	require.NoError(t, err)
	// Delivery (and with it the offset bookkeeping) is asynchronous.
	require.Eventually(t, func() bool {
		sub, ok := node.hub.LookupSubscriber("clean.ch", client)
		return ok && sub.DeliveredOffset == 1
	}, 2*time.Second, time.Millisecond)

	require.NoError(t, node.RemoveSubscription("clean.ch", client))
	_, ok := node.hub.LookupSubscriber("clean.ch", client)
	require.False(t, ok, "unsubscribe must remove the subscription record")

	require.NoError(t, node.AddSubscription(ctx, "clean.ch", NewSubscriber(client, false)))
	sub, ok := node.hub.LookupSubscriber("clean.ch", client)
	require.True(t, ok)
	require.Zero(t, sub.DeliveredOffset, "re-subscription must not inherit the old offset")
}

// TestNode_Close_ClearsDeliveredOffset verifies that closing a session
// removes the subscription (and with it the offset bookkeeping).
func TestNode_Close_ClearsDeliveredOffset(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	_ = node.Run(ctx)

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-close", "user-close", "client-close")
	require.NoError(t, node.AddClient(client))
	require.NoError(t, node.AddSubscription(ctx, "close.ch", NewSubscriber(client, false)))

	_, err = node.Publish("close.ch", publishPub([]byte("m1"), false))
	require.NoError(t, err)

	require.NoError(t, client.Close(Disconnect{}))
	_, ok := node.hub.LookupSubscriber("close.ch", client)
	require.False(t, ok, "close must remove the subscription and its offset record")

	snapshot := node.clusterSessionSnapshot(client)
	require.NotContains(t, snapshot.ChannelOffsets, "close.ch")
}

// TestNode_EvictSessionForTakeover_RollbackClearsDeliveredOffsets verifies
// the takeover-eviction rollback symmetry: a rolled-back subscription starts
// with a fresh offset record, mirroring the ephemeral-flag re-read pattern.
func TestNode_EvictSessionForTakeover_RollbackClearsDeliveredOffsets(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	_ = node.Run(ctx) // wire the memory broker handler for real deliveries

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-evict-off", "user-evict", "client-evict")
	require.NoError(t, node.AddClient(client))
	require.NoError(t, node.AddSubscription(ctx, "evict.off", NewSubscriber(client, false)))

	_, err = node.Publish("evict.off", publishPub([]byte("m1"), false))
	require.NoError(t, err)
	// Delivery (and with it the offset bookkeeping) is asynchronous.
	require.Eventually(t, func() bool {
		sub, ok := node.hub.LookupSubscriber("evict.off", client)
		return ok && sub.DeliveredOffset == 1
	}, 2*time.Second, time.Millisecond)

	// Swap in a broker that fails Unsubscribe so the fence rolls back.
	node.SetBroker(&evictTestBroker{failUnsubCh: "evict.off", subscribed: make(map[string]bool)})
	err = client.Fence(DisconnectStale)
	require.Error(t, err)

	sub, ok := node.hub.LookupSubscriber("evict.off", client)
	require.True(t, ok, "eviction rollback must restore the subscription")
	require.Zero(t, sub.DeliveredOffset, "rollback must clear the delivered offset bookkeeping")
}

// TestNode_ClusterSessionSnapshot_IncludesChannelOffsets verifies that the
// snapshot carries the per-channel last-delivered offset recorded by the hub
// broadcast path, so a cross-node resume can recover from
// ChannelOffsets[ch]+1 instead of trusting the client-reported offset.
func TestNode_ClusterSessionSnapshot_IncludesChannelOffsets(t *testing.T) {
	node := NewNode(nil)
	_ = node.Run(context.Background())

	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-off-snap", "user-off", "client-off")
	require.NoError(t, node.AddClient(client))
	require.NoError(t, node.AddSubscription(context.Background(), "snap.ch", NewSubscriber(client, false)))

	offset, err := node.Publish("snap.ch", publishPub([]byte("m1"), false))
	require.NoError(t, err)
	require.Equal(t, uint64(1), offset)

	// Delivery (and with it the offset bookkeeping) is asynchronous.
	require.Eventually(t, func() bool {
		snapshot := node.clusterSessionSnapshot(client)
		return snapshot.ChannelOffsets["snap.ch"] == 1
	}, 2*time.Second, time.Millisecond, "snapshot must carry per-channel delivered offsets")
}

// TestClusterSessionSnapshot_ChannelOffsets_RoundTrip verifies that
// ChannelOffsets survives the JSON snapshot round trip used by the session
// directory (Redis).
func TestClusterSessionSnapshot_ChannelOffsets_RoundTrip(t *testing.T) {
	snapshot := &ClusterSessionSnapshot{
		SessionID:     "sess-rt",
		UserID:        "user-1",
		ClientID:      "client-1",
		Protocol:      "ws",
		Subscriptions: []ClusterSubscriptionSnapshot{{Channel: "news"}},
		ChannelOffsets: map[string]uint64{
			"news":   42,
			"sports": 7,
		},
		BrokerEpoch: "epoch-1",
	}
	data, err := json.Marshal(snapshot)
	require.NoError(t, err)

	var decoded ClusterSessionSnapshot
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Equal(t, snapshot.ChannelOffsets, decoded.ChannelOffsets, "channel offsets must survive the JSON round trip")
}

// makeOffsetPubs builds history publications with offsets first..last.
func makeOffsetPubs(first, last uint64) []*Publication {
	pubs := make([]*Publication, 0, last-first+1)
	for i := first; i <= last; i++ {
		pubs = append(pubs, &Publication{Offset: i, Payload: []byte("m")})
	}
	return pubs
}

// remoteResumeTestNode wires a node that can remote-resume "sess-off-resume"
// with the given snapshot and broker history.
func remoteResumeTestNode(t *testing.T, snapshot *ClusterSessionSnapshot, history []*Publication, epoch string) *Node {
	t.Helper()
	directory := &fakeSessionDirectory{
		lease: &ClusterSessionLease{
			SessionID:     "sess-off-resume",
			NodeID:        "node-b",
			IncarnationID: "inc-b",
			LeaseVersion:  3,
			ExpiresAt:     time.Now().Add(time.Hour),
		},
		snapshot: snapshot,
	}
	bus := &fakeClusterCommandBus{result: &ClusterCommandResult{Status: ClusterCommandStatusSucceeded}}
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       bus,
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := NewNode(&config.Server{RequireAuth: true})
	node.SetCluster(runtime)
	node.SetBroker(&fakeEpochHistoryBroker{epoch: epoch, pubs: history})
	authProxy := &connectAuthProxyStub{userID: "user-1"}
	require.NoError(t, node.AddProxy(authProxy, "", SystemMethodAuthenticate))
	return node
}

// connectOffsets returns the offsets of the replayed publications of a remote
// resume Connect, read from the full replay stream.
func connectOffsets(t *testing.T, node *Node, transport *capturingTransport, clientOffset uint64, clientEpoch string) []uint64 {
	t.Helper()
	ctx := context.Background()
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	msg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				Version: testProtocolVersion,
				ClientId:  "client-1",
				Token:     "t",
				SessionId: "sess-off-resume",
				Subscriptions: []*clientpb.Subscription{
					{Channel: "off.news", Recover: true, Cursor: cursorOf(clientEpoch, clientOffset)},
				},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, msg))
	return publicationOffsets(replayPublications(outboundMessages(t, transport)))
}

// TestClient_RemoteResume_ServerOffsetWinsOverClientOffset verifies that a
// cross-node resume recovers from the server-recorded ChannelOffsets[ch]+1
// even when the client reports a different (lower, stale) offset.
func TestClient_RemoteResume_ServerOffsetWinsOverClientOffset(t *testing.T) {
	snapshot := &ClusterSessionSnapshot{
		SessionID:     "sess-off-resume",
		UserID:        "user-1",
		ClientID:      "client-1",
		Subscriptions: []ClusterSubscriptionSnapshot{{Channel: "off.news"}},
		ChannelOffsets: map[string]uint64{
			"off.news": 5,
		},
		BrokerEpoch: "v2",
	}
	node := remoteResumeTestNode(t, snapshot, makeOffsetPubs(1, 10), "v2")
	offsets := connectOffsets(t, node, &capturingTransport{}, 2, "v2")
	assert.Equal(t, []uint64{6, 7, 8, 9, 10}, offsets,
		"server-recorded offset (5) must win over the client-reported offset (2)")
}

// TestClient_RemoteResume_SnapshotEpochMismatchForcesFullRecovery verifies
// that a snapshot whose BrokerEpoch differs from the current broker epoch
// invalidates the server-recorded offsets: recovery starts over.
func TestClient_RemoteResume_SnapshotEpochMismatchForcesFullRecovery(t *testing.T) {
	snapshot := &ClusterSessionSnapshot{
		SessionID:     "sess-off-resume",
		UserID:        "user-1",
		ClientID:      "client-1",
		Subscriptions: []ClusterSubscriptionSnapshot{{Channel: "off.news"}},
		ChannelOffsets: map[string]uint64{
			"off.news": 5,
		},
		BrokerEpoch: "v1", // broker restarted since the snapshot was taken
	}
	node := remoteResumeTestNode(t, snapshot, makeOffsetPubs(1, 10), "v2")
	offsets := connectOffsets(t, node, &capturingTransport{}, 2, "v2")
	assert.Equal(t, []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, offsets,
		"a snapshot epoch mismatch must force full recovery")
}

// TestClient_RemoteResume_MissingOffsetSkipped verifies the resume rule
// (PR-03, §5.1): when the snapshot carries no ChannelOffsets for the channel,
// recovery is skipped entirely — the client-reported offset is never used to
// replay history, and nothing is replayed from the beginning.
func TestClient_RemoteResume_MissingOffsetSkipped(t *testing.T) {
	snapshot := &ClusterSessionSnapshot{
		SessionID:     "sess-off-resume",
		UserID:        "user-1",
		ClientID:      "client-1",
		Subscriptions: []ClusterSubscriptionSnapshot{{Channel: "off.news"}},
		BrokerEpoch:   "v2",
	}
	node := remoteResumeTestNode(t, snapshot, makeOffsetPubs(1, 10), "v2")
	offsets := connectOffsets(t, node, &capturingTransport{}, 2, "v2")
	assert.Empty(t, offsets,
		"without a server-recorded offset the resume must skip recovery, not replay from the client offset")
}
