package messageloop

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/messageloopio/messageloop/config"
	"github.com/messageloopio/messageloop/pkg/topics"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// connectAndSubscribe wires a fresh client with the given client ID and
// returns it together with its transport.
func connectAndSubscribe(t *testing.T, node *Node, clientID string, chs ...*clientpb.Subscription) (*Client, *capturingTransport) {
	t.Helper()
	ctx := context.Background()
	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	connectMsg := &clientpb.InboundMessage{
		Id:       "connect-" + clientID,
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: clientID}},
	}
	require.NoError(t, client.HandleMessage(ctx, connectMsg))
	if len(chs) > 0 {
		subMsg := &clientpb.InboundMessage{
			Id:       "subscribe-" + clientID,
			Envelope: &clientpb.InboundMessage_Subscribe{Subscribe: &clientpb.Subscribe{Subscriptions: chs}},
		}
		require.NoError(t, client.HandleMessage(ctx, subMsg))
	}
	return client, transport
}

// presenceEventsOf decodes every presence_event envelope captured by the
// transport (publications are ignored).
func presenceEventsOf(t *testing.T, transport *capturingTransport) []*clientpb.PresenceEvent {
	t.Helper()
	var events []*clientpb.PresenceEvent
	for i := 0; i < transport.getMessageCount(); i++ {
		var out clientpb.OutboundMessage
		require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getMessage(i), &out))
		if evt := out.GetPresenceEvent(); evt != nil {
			events = append(events, evt)
		}
	}
	return events
}

func publicationsOf(t *testing.T, transport *capturingTransport) int {
	t.Helper()
	count := 0
	for i := 0; i < transport.getMessageCount(); i++ {
		var out clientpb.OutboundMessage
		require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getMessage(i), &out))
		if out.GetPublication() != nil {
			count++
		}
	}
	return count
}

// lastSubscribeAckPresence returns the presence snapshots of the last
// SubscribeAck captured by the transport.
func lastSubscribeAckPresence(t *testing.T, transport *capturingTransport) []*clientpb.PresenceSnapshot {
	t.Helper()
	var ack *clientpb.SubscribeAck
	for i := 0; i < transport.getMessageCount(); i++ {
		var out clientpb.OutboundMessage
		require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getMessage(i), &out))
		if got := out.GetSubscribeAck(); got != nil {
			ack = got
		}
	}
	require.NotNil(t, ack, "a SubscribeAck must have been received")
	return ack.GetPresence()
}

func TestPresence_JoinEventAndSnapshot(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	require.NoError(t, node.Run(ctx))

	const ch = "chat.room.1"
	clientA, transportA := connectAndSubscribe(t, node, "client-a", &clientpb.Subscription{Channel: ch})
	transportA.messages = nil

	clientB, transportB := connectAndSubscribe(t, node, "client-b", &clientpb.Subscription{Channel: ch})

	// A receives exactly one first-class join event, not a publication.
	events := presenceEventsOf(t, transportA)
	require.Len(t, events, 1, "A must receive exactly one join event")
	require.Equal(t, "join", events[0].GetAction())
	require.Equal(t, ch, events[0].GetChannel())
	require.Equal(t, clientB.SessionID(), events[0].GetInfo().GetSessionId())
	require.Equal(t, "client-b", events[0].GetInfo().GetClientId(), "info.client_id is Connect.client_id")
	require.Zero(t, publicationsOf(t, transportA), "presence must not arrive as a publication")

	// B's SubscribeAck snapshot contains A and B.
	snapshots := lastSubscribeAckPresence(t, transportB)
	require.Len(t, snapshots, 1)
	require.Equal(t, ch, snapshots[0].GetChannel())
	require.False(t, snapshots[0].GetTruncated())
	require.Equal(t, int32(2), snapshots[0].GetOccupancy())
	bySession := make(map[string]*clientpb.PresenceInfo, len(snapshots[0].GetClients()))
	for _, info := range snapshots[0].GetClients() {
		bySession[info.GetSessionId()] = info
	}
	require.Contains(t, bySession, clientA.SessionID())
	require.Equal(t, "client-a", bySession[clientA.SessionID()].GetClientId())
	require.Contains(t, bySession, clientB.SessionID())
	require.Equal(t, "client-b", bySession[clientB.SessionID()].GetClientId())

	// B's own transport never saw a self-join.
	require.Empty(t, presenceEventsOf(t, transportB), "the joiner must not receive its own join")
}

func TestPresence_WildcardSubscriberReceivesExactJoin(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	require.NoError(t, node.Run(ctx))

	_, transportA := connectAndSubscribe(t, node, "client-a", &clientpb.Subscription{Channel: "chat.**"})
	transportA.messages = nil

	clientB, _ := connectAndSubscribe(t, node, "client-b", &clientpb.Subscription{Channel: "chat.room.1"})

	events := presenceEventsOf(t, transportA)
	require.Len(t, events, 1)
	require.Equal(t, "chat.room.1", events[0].GetChannel(), "events on a wildcard subscription always carry the exact channel")
	require.Equal(t, "join", events[0].GetAction())
	require.Equal(t, clientB.SessionID(), events[0].GetInfo().GetSessionId())

	// The wildcard pattern itself never entered the store.
	present, err := node.Presence(ctx, "chat.**")
	require.NoError(t, err)
	require.Empty(t, present, "wildcard patterns must not be presence store keys")
	present, err = node.Presence(ctx, "chat.room.1")
	require.NoError(t, err)
	require.Contains(t, present, clientB.SessionID())
}

func TestPresence_EphemeralNoStoreOrEvent(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	require.NoError(t, node.Run(ctx))

	const ch = "ephemeral.presence.ch"
	observer := newPresenceEventObserver(t, node, ch)

	client, _ := connectAndSubscribe(t, node, "client-eph", &clientpb.Subscription{Channel: ch, Ephemeral: true})

	require.Equal(t, 2, node.Hub().NumSubscribers(ch))
	present, err := node.Presence(ctx, ch)
	require.NoError(t, err)
	require.Len(t, present, 1, "only the observer is present; the ephemeral session must not be")
	require.Contains(t, present, observer.client.SessionID())
	assert.Zero(t, observer.eventCount(), "ephemeral join must not reach other members")
	require.NotNil(t, client)
}

func TestPresence_QueryRequiresCoverage(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	require.NoError(t, node.Run(ctx))

	const ch = "chat.room.1"
	// A may query: covered by chat.**, gets a snapshot containing B.
	clientA, transportA := connectAndSubscribe(t, node, "client-a", &clientpb.Subscription{Channel: "chat.**"})
	// B is a real member of the exact channel.
	_, _ = connectAndSubscribe(t, node, "client-b", &clientpb.Subscription{Channel: ch})
	// C is connected but covers nothing (the default ACL would allow it).
	clientC, transportC := connectAndSubscribe(t, node, "client-c")

	// A may query: covered by chat.**, gets a snapshot containing B.
	queryA := &clientpb.InboundMessage{
		Id:       "query-a",
		Envelope: &clientpb.InboundMessage_PresenceQuery{PresenceQuery: &clientpb.PresenceQuery{Channel: ch}},
	}
	require.NoError(t, clientA.HandleMessage(ctx, queryA))
	last := transportA.getLastMessage()
	var outA clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(last, &outA))
	snapshot := outA.GetPresence()
	require.NotNil(t, snapshot, "covered query must return a presence snapshot")
	require.Equal(t, ch, snapshot.GetChannel())
	require.Equal(t, int32(1), snapshot.GetOccupancy(), "only B is tracked in the exact channel")

	// C is not covered: PERMISSION_DENIED / acl_error, connection stays up.
	queryC := &clientpb.InboundMessage{
		Id:       "query-c",
		Envelope: &clientpb.InboundMessage_PresenceQuery{PresenceQuery: &clientpb.PresenceQuery{Channel: ch}},
	}
	require.NoError(t, clientC.HandleMessage(ctx, queryC))
	last = transportC.getLastMessage()
	var outC clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(last, &outC))
	require.Nil(t, outC.GetPresence(), "uncovered query must not return a snapshot")
	errObj := outC.GetError()
	require.NotNil(t, errObj)
	require.Equal(t, "PERMISSION_DENIED", errObj.GetCode())
	require.Equal(t, "acl_error", errObj.GetType())
	require.False(t, transportC.isClosed(), "a denied query must not disconnect")
}

func TestPresence_QueryWildcardRejected(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	require.NoError(t, node.Run(ctx))

	client, transport := connectAndSubscribe(t, node, "client-a", &clientpb.Subscription{Channel: "chat.**"})

	query := &clientpb.InboundMessage{
		Id:       "query-wc",
		Envelope: &clientpb.InboundMessage_PresenceQuery{PresenceQuery: &clientpb.PresenceQuery{Channel: "chat.**"}},
	}
	require.NoError(t, client.HandleMessage(ctx, query))
	last := transport.getLastMessage()
	var out clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(last, &out))
	errObj := out.GetError()
	require.NotNil(t, errObj)
	require.Equal(t, "BAD_REQUEST", errObj.GetCode())
	require.Equal(t, "request_error", errObj.GetType())
	require.False(t, transport.isClosed(), "a rejected query must not disconnect")
}

func TestPresence_PolicyPresenceFalse(t *testing.T) {
	ctx := context.Background()
	node := NewNode(&config.Server{Channels: config.ChannelConfig{
		Policies: []config.ChannelPolicyRule{{
			Pattern:           "nopres.**",
			ChannelPolicySpec: config.ChannelPolicySpec{Presence: policyBoolPtr(false)},
		}},
	}})
	require.NoError(t, node.Run(ctx))

	const ch = "nopres.chat"
	observer := newPresenceEventObserver(t, node, ch)

	client, transport := connectAndSubscribe(t, node, "client-nopres", &clientpb.Subscription{Channel: ch})

	// Subscription succeeds, but nothing is tracked and no event is emitted.
	// The observer is subject to the same policy: with presence=false nobody
	// — not even the observer — is stored.
	require.Equal(t, 2, node.Hub().NumSubscribers(ch))
	present, err := node.Presence(ctx, ch)
	require.NoError(t, err)
	require.Empty(t, present, "presence=false channel: no member may be stored")
	assert.Zero(t, observer.eventCount(), "presence=false: no join event may be emitted")

	// Query is rejected by policy.
	query := &clientpb.InboundMessage{
		Id:       "query-nopres",
		Envelope: &clientpb.InboundMessage_PresenceQuery{PresenceQuery: &clientpb.PresenceQuery{Channel: ch}},
	}
	require.NoError(t, client.HandleMessage(ctx, query))
	var out clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getLastMessage(), &out))
	errObj := out.GetError()
	require.NotNil(t, errObj)
	require.Equal(t, "POLICY_DENIED", errObj.GetCode())
	require.Equal(t, "policy_error", errObj.GetType())
}

func TestPresence_SnapshotTruncated(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	require.NoError(t, node.Run(ctx))

	const ch = "crowded.ch"
	for i := 0; i < 300; i++ {
		session := fmt.Sprintf("preset-%03d", i)
		require.NoError(t, node.presence.Add(ctx, ch, &PresenceInfo{
			ClientID:  session,
			SessionID: session,
			UserID:    "user-preset",
			ConnectedAt: int64(i),
		}))
	}

	_, transport := connectAndSubscribe(t, node, "client-crowd", &clientpb.Subscription{Channel: ch})

	snapshots := lastSubscribeAckPresence(t, transport)
	require.Len(t, snapshots, 1)
	snapshot := snapshots[0]
	require.True(t, snapshot.GetTruncated(), "301 members must exceed the 256 snapshot cap")
	require.Equal(t, int32(301), snapshot.GetOccupancy(), "occupancy counts every member including the joiner")
	require.LessOrEqual(t, len(snapshot.GetClients()), 256)
	require.Equal(t, 256, len(snapshot.GetClients()))
	require.Empty(t, presenceEventsOf(t, transport), "the joiner must not receive its own join")
}

// TestPresence_ConnectedSnapshotFilled verifies §9.9: a Connect carrying
// subscriptions returns one presence snapshot per tracked channel in
// connected.presence, while the PR-03 recovery fields stay intact.
func TestPresence_ConnectedSnapshotFilled(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	require.NoError(t, node.Run(ctx))

	const ch = "connect.presence.ch"
	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	connectMsg := &clientpb.InboundMessage{
		Id: "connect-with-subs",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId:      "client-c",
				Subscriptions: []*clientpb.Subscription{{Channel: ch}},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, connectMsg))

	var connected *clientpb.Connected
	for i := 0; i < transport.getMessageCount(); i++ {
		var out clientpb.OutboundMessage
		require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getMessage(i), &out))
		if got := out.GetConnected(); got != nil {
			connected = got
		}
	}
	require.NotNil(t, connected)
	require.Len(t, connected.GetPresence(), 1, "connected.presence must carry the tracked channel snapshot")
	require.Equal(t, ch, connected.GetPresence()[0].GetChannel())
	require.Equal(t, int32(1), connected.GetPresence()[0].GetOccupancy())
	require.Equal(t, 1, len(connected.GetPresence()[0].GetClients()))
	require.Equal(t, client.SessionID(), connected.GetPresence()[0].GetClients()[0].GetSessionId())
	// PR-03 recovery fields remain present.
	require.NotNil(t, connected.GetRecoverResults())
	require.Equal(t, connected.GetSessionId(), client.SessionID())
}

// countingBroker records every PublishTransient channel so tests can prove
// whether companion presence frames were written.
type countingBroker struct {
	mu        sync.Mutex
	transient []string
}

func (b *countingBroker) Start(context.Context, PublicationHandler) error { return nil }
func (b *countingBroker) Subscribe(string) error                          { return nil }
func (b *countingBroker) Unsubscribe(string) error                        { return nil }
func (b *countingBroker) Publish(string, *Publication) (uint64, error)    { return 0, nil }
func (b *countingBroker) PublishTransient(ch string, _ *Publication) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.transient = append(b.transient, ch)
	return nil
}
func (b *countingBroker) History(string, uint64, int) ([]*Publication, error) { return nil, nil }

func (b *countingBroker) transientChannels() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.transient...)
}

func (b *countingBroker) publishedTo(ch string) bool {
	for _, got := range b.transientChannels() {
		if got == ch {
			return true
		}
	}
	return false
}

func TestPresence_NoCompanionByDefault(t *testing.T) {
	ctx := context.Background()
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	node := NewNode(nil)
	node.SetMetrics(metrics)
	broker := &countingBroker{}
	node.SetBroker(broker)
	require.NoError(t, node.Run(ctx))

	const ch = "companion.ch"
	clientA, _ := connectAndSubscribe(t, node, "client-a", &clientpb.Subscription{Channel: ch})
	_ = clientA
	_, _ = connectAndSubscribe(t, node, "client-b", &clientpb.Subscription{Channel: ch})
	require.NoError(t, clientA.Close(Disconnect{}))

	require.Eventually(t, func() bool { return node.Hub().NumSubscribers(ch) == 1 }, time.Second, 10*time.Millisecond)
	assert.False(t, broker.publishedTo(presenceChannel(ch)),
		"default policy must not write the companion channel")
	assert.Zero(t, testutil.ToFloat64(metrics.PresencePublishFailures),
		"join/leave must not fail companion publishes that never run")
}

func TestPresence_LegacyCompanionExactOnly(t *testing.T) {
	ctx := context.Background()
	node := NewNode(&config.Server{Channels: config.ChannelConfig{
		Default: config.ChannelPolicySpec{LegacyPresenceChannel: policyBoolPtr(true)},
	}})
	broker := &countingBroker{}
	node.SetBroker(broker)
	require.NoError(t, node.Run(ctx))

	_, _ = connectAndSubscribe(t, node, "client-a", &clientpb.Subscription{Channel: "legacy.ch"})
	require.Eventually(t, func() bool {
		return broker.publishedTo(presenceChannel("legacy.ch"))
	}, time.Second, 10*time.Millisecond, "legacy_presence_channel=true must write the exact companion channel")

	_, _ = connectAndSubscribe(t, node, "client-b", &clientpb.Subscription{Channel: "im.**"})
	// Give any (wrong) async writes a chance to land.
	time.Sleep(50 * time.Millisecond)
	assert.False(t, broker.publishedTo(presenceChannel("im.**")),
		"wildcard subscriptions must never write a companion channel")
}

func TestPresence_ValidateTopicCompanionStillRejected(t *testing.T) {
	require.ErrorIs(t, topics.ValidateTopic("a.**/__presence"), topics.ErrBadTopic)
	require.ErrorIs(t, topics.ValidateTopic("a.**.b/__presence"), topics.ErrBadTopic)
}

func TestPresence_BroadcastPresenceNotPublication(t *testing.T) {
	ctx := context.Background()
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	node := NewNode(nil)
	node.SetMetrics(metrics)
	require.NoError(t, node.Run(ctx))

	const ch = "rewrite.ch"
	client, transport := connectAndSubscribe(t, node, "client-a", &clientpb.Subscription{Channel: ch})
	transport.messages = nil

	evt := &clientpb.PresenceEvent{
		Action: "join",
		Info:   &clientpb.PresenceInfo{SessionId: "sess-x", UserId: "user-x", ClientId: "client-x"},
	}
	require.NoError(t, node.hub.broadcastPublication(ch, presencePublication(evt)))

	events := presenceEventsOf(t, transport)
	require.Len(t, events, 1)
	require.Equal(t, ch, events[0].GetChannel(), "an empty event channel is filled from the frame channel")
	require.Equal(t, "sess-x", events[0].GetInfo().GetSessionId())
	require.Zero(t, publicationsOf(t, transport), "a presence frame must never become a publication")
	require.Zero(t, testutil.ToFloat64(metrics.MessagesDelivered), "presence delivery must not count MessagesDelivered")

	// A non-presence publication still flows unchanged.
	require.NoError(t, node.hub.broadcastPublication(ch, &Publication{
		Payload: []byte("hello"),
		Kind:    PayloadKindText,
		Offset:  7,
	}))
	require.Equal(t, 1, publicationsOf(t, transport))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.MessagesDelivered))
	require.NotNil(t, client)
}

func TestPresence_BroadcastUnparseablePresenceDropped(t *testing.T) {
	ctx := context.Background()
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	node := NewNode(nil)
	node.SetMetrics(metrics)
	require.NoError(t, node.Run(ctx))

	const ch = "drop.ch"
	_, transport := connectAndSubscribe(t, node, "client-a", &clientpb.Subscription{Channel: ch})
	transport.messages = nil

	badPub := &Publication{
		Payload:  []byte("not-json"),
		Kind:     PayloadKindJSON,
		Metadata: map[string]string{PresenceMetaTypeKey: PresenceMetaTypeValue},
	}
	require.NoError(t, node.hub.broadcastPublication(ch, badPub))
	require.Zero(t, publicationsOf(t, transport), "an unparseable presence frame must be dropped, not forwarded")
	require.Empty(t, presenceEventsOf(t, transport))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.PresenceFailures.WithLabelValues("rewrite")))
}

func TestPresence_RestoreWildcardSkipsStore(t *testing.T) {
	node := NewNode(nil)
	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-restore-wc", "user-restore", "client-restore")

	subscriptions := []ClusterSubscriptionSnapshot{
		{Channel: "chat.**", Ephemeral: false},
		{Channel: "normal.ch"},
	}
	require.NoError(t, node.restoreSessionSubscriptions(context.Background(), client, subscriptions))

	present, err := node.presence.Get(context.Background(), "chat.**")
	require.NoError(t, err)
	require.Empty(t, present, "wildcard patterns must not register presence on restore")

	present, err = node.presence.Get(context.Background(), "normal.ch")
	require.NoError(t, err)
	require.Contains(t, present, "sess-restore-wc", "tracked channels still register presence on restore")
}

func TestPresence_ResubscribeSnapshotNoSecondJoin(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	require.NoError(t, node.Run(ctx))

	const ch = "resub.ch"
	clientA, transportA := connectAndSubscribe(t, node, "client-a", &clientpb.Subscription{Channel: ch})
	transportA.messages = nil

	clientB, transportB := connectAndSubscribe(t, node, "client-b", &clientpb.Subscription{Channel: ch})

	// B re-subscribes the same channel: no second join, snapshot still sent.
	transportB.messages = nil
	resub := &clientpb.InboundMessage{
		Id: "resub-b",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{
				Subscriptions: []*clientpb.Subscription{{Channel: ch}},
			},
		},
	}
	require.NoError(t, clientB.HandleMessage(ctx, resub))

	events := presenceEventsOf(t, transportA)
	require.Len(t, events, 1, "a re-subscribe must not emit a second join")
	require.Equal(t, clientB.SessionID(), events[0].GetInfo().GetSessionId())

	snapshots := lastSubscribeAckPresence(t, transportB)
	require.Len(t, snapshots, 1)
	require.Equal(t, int32(2), snapshots[0].GetOccupancy(), "the re-subscribe ack still carries the catch-up snapshot")
	require.Equal(t, 2, len(snapshots[0].GetClients()))
	require.NotNil(t, clientA)
}
