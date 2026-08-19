package runtime

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/messageloopio/messageloop/config"
	"github.com/messageloopio/messageloop/pkg/topics"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
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
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{Version: testProtocolVersion, ClientId: clientID}},
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
	node := NewNode(&config.Server{Authorizer: config.AuthorizerConfig{
		Rules: []config.AuthorizerRule{{
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
			ClientID:    session,
			SessionID:   session,
			UserID:      "user-preset",
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
// subscriptions returns one presence snapshot per tracked channel as a
// standalone presence envelope right after Connection (client.v2 has no
// presence list on Connected, so snapshots ride their own envelope).
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
				Version: testProtocolVersion,
				ClientId:      "client-c",
				Subscriptions: []*clientpb.Subscription{{Channel: ch}},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, connectMsg))

	msgs := outboundMessages(t, transport)
	// The first frame is the bare Connected (no presence embedded).
	require.NotNil(t, msgs[0].GetConnected())
	require.Len(t, msgs[0].GetConnected().GetSubscriptions(), 1)

	var snapshots []*clientpb.PresenceSnapshot
	for _, m := range msgs {
		if s := m.GetPresence(); s != nil {
			snapshots = append(snapshots, s)
		}
	}
	require.Len(t, snapshots, 1, "a tracked channel must deliver one presence snapshot envelope")
	require.Equal(t, ch, snapshots[0].GetChannel())
	require.Equal(t, int32(1), snapshots[0].GetOccupancy())
	require.Equal(t, 1, len(snapshots[0].GetClients()))
	require.Equal(t, client.SessionID(), snapshots[0].GetClients()[0].GetSessionId())
	require.Equal(t, msgs[0].GetConnected().GetSessionId(), client.SessionID())
}

// countingBroker records every PublishTransient / PublishOccupancy channel
// so tests can prove whether companion presence frames or occupancy events
// were written, without delivering them.
type countingBroker struct {
	mu        sync.Mutex
	transient []string
	occupancy []string
}

func (b *countingBroker) Start(context.Context, PublicationHandler) error { return nil }
func (b *countingBroker) Subscribe(string) error                          { return nil }
func (b *countingBroker) Unsubscribe(string) error                        { return nil }
func (b *countingBroker) Publish(string, *Publication) (uint64, error)    { return 0, nil }
func (b *countingBroker) SetOccupancyHandler(OccupancyHandler) error      { return nil }
func (b *countingBroker) SetGapHandler(GapHandler)                        {}
func (b *countingBroker) PublishTransient(ch string, _ *Publication) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.transient = append(b.transient, ch)
	return nil
}
func (b *countingBroker) PublishOccupancy(ch string, evt OccupancyEvent) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if evt.Event != nil {
		b.occupancy = append(b.occupancy, fmt.Sprintf("%s|%s|%d", ch, evt.Event.GetAction(), evt.Gen))
	}
	return nil
}
func (b *countingBroker) History(string, uint64, int) (*HistoryPage, error) { return nil, nil }

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

// occupancyEmits returns the recorded (channel|action|gen) occupancy
// publishes, in record order.
func (b *countingBroker) occupancyEmits() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.occupancy...)
}

// clearOccupancy drops the recorded occupancy publishes (to focus later
// assertions on a single session's events).
func (b *countingBroker) clearOccupancy() {
	b.mu.Lock()
	b.occupancy = nil
	b.mu.Unlock()
}

// publishedOccupancyTo reports whether an occupancy with the given action was
// published to ch.
func (b *countingBroker) publishedOccupancyTo(ch, action string) bool {
	return lastGenOf(b.occupancyEmits(), ch, action) > 0
}

// lastGenOf returns the largest gen recorded for (ch, action), 0 if none.
func lastGenOf(emits []string, ch, action string) uint64 {
	prefix := ch + "|" + action + "|"
	var maxGen uint64
	for _, emit := range emits {
		if strings.HasPrefix(emit, prefix) {
			if n, err := strconv.ParseUint(emit[len(prefix):], 10, 64); err == nil && n > maxGen {
				maxGen = n
			}
		}
	}
	return maxGen
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
	node := NewNode(&config.Server{Authorizer: config.AuthorizerConfig{
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
	require.Never(t, func() bool {
		return broker.publishedTo(presenceChannel("im.**"))
	}, 500*time.Millisecond, 25*time.Millisecond,
		"wildcard subscriptions must never write a companion channel")
}

func TestPresence_ValidateTopicCompanionStillRejected(t *testing.T) {
	require.ErrorIs(t, topics.ValidateTopic("a.**/__presence"), topics.ErrBadTopic)
	require.ErrorIs(t, topics.ValidateTopic("a.**.b/__presence"), topics.ErrBadTopic)
}

// TestPresence_BroadcastIgnoresMlTypeAnnotations pins B2 §5.4/§8.1: the hub
// no longer recognizes an "ml.type=presence" publication annotation — such a
// frame (a legacy/rogue chat publication) is delivered as a plain
// publication, never rewritten into a presence event. Occupancy events have
// their own live-bus type and never reach broadcastPublication.
func TestPresence_BroadcastIgnoresMlTypeAnnotations(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	require.NoError(t, node.Run(ctx))

	const ch = "plain.ch"
	_, transport := connectAndSubscribe(t, node, "client-a", &clientpb.Subscription{Channel: ch})
	transport.messages = nil

	legacyFrame := &Publication{
		Payload:  []byte(`{"__type":"presence","action":"join"}`),
		Kind:     PayloadKindJSON,
		Offset:   7,
		Metadata: map[string]string{"ml.type": "presence"},
	}
	require.NoError(t, node.hub.BroadcastPublication(ch, legacyFrame))

	require.Equal(t, 1, publicationsOf(t, transport),
		"with the ml.type rewrite gone the frame must flow as a publication")
	require.Empty(t, presenceEventsOf(t, transport),
		"the hub must never rewrite a publication into a presence event")

	// A normal publication still flows unchanged.
	require.NoError(t, node.hub.BroadcastPublication(ch, &Publication{
		Payload: []byte("hello"),
		Kind:    PayloadKindText,
		Offset:  8,
	}))
	require.Equal(t, 2, publicationsOf(t, transport))
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
	require.Empty(t, node.restoreSessionSubscriptions(context.Background(), client, subscriptions))

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

// TestPresence_EphemeralExactDoesNotHideWildcardCoverage verifies that a
// session subscribed both ephemerally to the exact channel and persistently
// via a wildcard still receives join events (dedupe prefers non-ephemeral).
func TestPresence_EphemeralExactDoesNotHideWildcardCoverage(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	require.NoError(t, node.Run(ctx))

	const ch = "chat.room.mix"
	clientA, transportA := connectAndSubscribe(t, node, "client-a",
		&clientpb.Subscription{Channel: "chat.**"},
		&clientpb.Subscription{Channel: ch, Ephemeral: true},
	)
	transportA.messages = nil

	clientB, _ := connectAndSubscribe(t, node, "client-b", &clientpb.Subscription{Channel: ch})

	events := presenceEventsOf(t, transportA)
	require.Len(t, events, 1, "wildcard coverage must still deliver the exact-channel join")
	require.Equal(t, ch, events[0].GetChannel())
	require.Equal(t, clientB.SessionID(), events[0].GetInfo().GetSessionId())
	require.NotNil(t, clientA)
}

// TestPresence_ClusterUnsubscribeEphemeralEmitsNoLeave verifies Admin
// unsubscribe of a locally created ephemeral subscription does not emit leave.
func TestPresence_ClusterUnsubscribeEphemeralEmitsNoLeave(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	require.NoError(t, node.Run(ctx))

	const ch = "admin.eph.ch"
	_, transportA := connectAndSubscribe(t, node, "client-a", &clientpb.Subscription{Channel: ch})
	clientB, _ := connectAndSubscribe(t, node, "client-b", &clientpb.Subscription{Channel: ch, Ephemeral: true})
	transportA.messages = nil

	result := node.handleClusterUnsubscribeCommand(ctx, &ClusterCommand{
		Type:      ClusterCommandUnsubscribe,
		SessionID: clientB.SessionID(),
		Channel:   ch,
	}, &ClusterCommandResult{})
	require.NotEqual(t, ClusterCommandStatusFailed, result.Status)

	require.Empty(t, presenceEventsOf(t, transportA),
		"unsubscribing an ephemeral member via Admin must not emit leave")
	present, err := node.Presence(ctx, ch)
	require.NoError(t, err)
	require.NotContains(t, present, clientB.SessionID())
}

// TestPresence_OccupancySinglePathExactlyOne proves the B2 single path: a
// Join writes the store and issues exactly ONE occupancy publish on the live
// bus (never a transient publication), and the joining peer's covered
// subscribers each receive exactly one PresenceEvent join — no double
// delivery from a stacked local+bus path.
func TestPresence_OccupancySinglePathExactlyOne(t *testing.T) {
	ctx := context.Background()

	// Broker spy: one occupancy publish, zero transient publications.
	broker := &countingBroker{}
	spyReg := prometheus.NewRegistry()
	spyMetrics := NewMetrics(spyReg)
	spyNode := NewNode(nil)
	spyNode.SetMetrics(spyMetrics)
	spyNode.SetBroker(broker)
	require.NoError(t, spyNode.Run(ctx))
	const ch = "single.path.ch"
	_, _ = connectAndSubscribe(t, spyNode, "client-a", &clientpb.Subscription{Channel: ch})
	// Focus on B's join: client-a's own join already hit the spy.
	broker.clearOccupancy()
	_, _ = connectAndSubscribe(t, spyNode, "client-b", &clientpb.Subscription{Channel: ch})
	require.Len(t, broker.occupancyEmits(), 1,
		"joining must emit exactly one occupancy join on the live bus")
	require.True(t, broker.publishedOccupancyTo(ch, "join"))
	require.False(t, broker.publishedTo(ch), "occupancy must never use PublishTransient")

	// Real memory broker: clients receive exactly one join each, no
	// publication, joiner receives none; neither MessagesPublished nor
	// MessagesDelivered are touched by occupancy.
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	node := NewNode(nil)
	node.SetMetrics(metrics)
	require.NoError(t, node.Run(ctx))
	_, transportA := connectAndSubscribe(t, node, "client-a", &clientpb.Subscription{Channel: ch})
	_, transportC := connectAndSubscribe(t, node, "client-c", &clientpb.Subscription{Channel: ch})
	transportA.messages = nil
	transportC.messages = nil
	clientB, transportB := connectAndSubscribe(t, node, "client-b", &clientpb.Subscription{Channel: ch})

	eventsA := presenceEventsOf(t, transportA)
	require.Len(t, eventsA, 1)
	require.Equal(t, "join", eventsA[0].GetAction())
	require.Equal(t, ch, eventsA[0].GetChannel())
	require.Equal(t, clientB.SessionID(), eventsA[0].GetInfo().GetSessionId())
	require.Len(t, presenceEventsOf(t, transportC), 1, "C must receive exactly one join")
	require.Empty(t, presenceEventsOf(t, transportB), "the joiner must not receive its own join")
	require.Zero(t, publicationsOf(t, transportA), "presence frames must never become publications")
	require.Zero(t, testutil.ToFloat64(metrics.MessagesPublished),
		"occupancy emit must not count MessagesPublished")
	require.Zero(t, testutil.ToFloat64(metrics.MessagesDelivered),
		"occupancy delivery must not count MessagesDelivered")
}

// TestPresence_OccupancyNotPublication pins B2 §8.2: a join/leave is an
// occupancy event, never a publication — the broker records no transient
// publication, and no publication reaches the client envelope.
func TestPresence_OccupancyNotPublication(t *testing.T) {
	ctx := context.Background()
	broker := &countingBroker{}
	node := NewNode(nil)
	node.SetBroker(broker)
	require.NoError(t, node.Run(ctx))

	const ch = "np.ch"
	_, _ = connectAndSubscribe(t, node, "client-a", &clientpb.Subscription{Channel: ch})
	_, _ = connectAndSubscribe(t, node, "client-b", &clientpb.Subscription{Channel: ch})

	require.True(t, broker.publishedOccupancyTo(ch, "join"),
		"join must flow as one occupancy publish")
	require.Empty(t, broker.transientChannels(),
		"occupancy is not a transient publication, so nothing may be PublishTransient'd")
}

// TestPresence_OccupancyGenOrderingAndDedupe pins B2 §5.3/§7.5: an explicit
// leave takes a fresh gen greater than the join it follows, and a receiver
// drops a replayed/late event with gen <= last_applied[ch][session]
// (counting it as ErrLateOccupancy).
func TestPresence_OccupancyGenOrderingAndDedupe(t *testing.T) {
	ctx := context.Background()

	// Emit side: the presence adapter issues strictly increasing gens per
	// channel, so the leave always outnumbers its preceding join.
	broker := &countingBroker{}
	node := NewNode(nil)
	node.SetBroker(broker)
	require.NoError(t, node.Run(ctx))

	const ch = "gen.ch"
	clientB, _ := connectAndSubscribe(t, node, "client-b", &clientpb.Subscription{Channel: ch})
	joinGen := lastGenOf(broker.occupancyEmits(), ch, "join")
	require.Greater(t, joinGen, uint64(0), "the join must carry a non-zero gen")

	require.NoError(t, clientB.Close(Disconnect{}))
	leaveGen := func() uint64 {
		// The disconnect path emits leave from the session teardown; wait for
		// it instead of sleeping.
		var gen uint64
		require.Eventually(t, func() bool {
			gen = lastGenOf(broker.occupancyEmits(), ch, "leave")
			return gen > 0
		}, time.Second, 10*time.Millisecond, "leaving must emit an occupancy leave")
		return gen
	}()
	require.Greater(t, leaveGen, joinGen, "an explicit leave must take a fresh gen")

	// Receiver side: a replayed older gen is dropped by last_applied.
	recv := NewNode(nil)
	require.NoError(t, recv.Run(ctx))
	_, transportA := connectAndSubscribe(t, recv, "client-a", &clientpb.Subscription{Channel: ch})
	transportA.messages = nil
	evt := func(gen uint64, action string) OccupancyEvent {
		return OccupancyEvent{
			Event: &clientpb.PresenceEvent{Channel: ch, Action: action,
				Info: &clientpb.PresenceInfo{SessionId: "sess-z"}},
			Gen: gen,
		}
	}

	require.NoError(t, recv.onOccupancy(ch, evt(5, "join")))
	require.Len(t, presenceEventsOf(t, transportA), 1)
	require.ErrorIs(t, recv.onOccupancy(ch, evt(3, "join")), ErrLateOccupancy,
		"an older gen must be counted and dropped")
	require.Len(t, presenceEventsOf(t, transportA), 1,
		"the replayed join must not be delivered a second time")
	require.NoError(t, recv.onOccupancy(ch, evt(6, "leave")))
	events := presenceEventsOf(t, transportA)
	require.Len(t, events, 2)
	require.Equal(t, "leave", events[1].GetAction())
}

// TestPresence_OccupancyWildcardCoverage pins B2 §5.2/§7.3: a session
// subscribed only to im.** covers the exact im.room.1 channel, receives its
// join, and the wildcard pattern itself never enters the store.
func TestPresence_OccupancyWildcardCoverage(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	require.NoError(t, node.Run(ctx))

	_, transportA := connectAndSubscribe(t, node, "client-a", &clientpb.Subscription{Channel: "im.**"})
	transportA.messages = nil

	clientB, _ := connectAndSubscribe(t, node, "client-b", &clientpb.Subscription{Channel: "im.room.1"})

	events := presenceEventsOf(t, transportA)
	require.Len(t, events, 1, "wildcard coverage must deliver the exact-channel join")
	require.Equal(t, "im.room.1", events[0].GetChannel())
	require.Equal(t, "join", events[0].GetAction())
	require.Equal(t, clientB.SessionID(), events[0].GetInfo().GetSessionId())
	require.Zero(t, publicationsOf(t, transportA), "presence must not arrive as a publication")

	present, err := node.Presence(ctx, "im.**")
	require.NoError(t, err)
	require.Empty(t, present, "wildcard patterns must not be presence store keys")
	present, err = node.Presence(ctx, "im.room.1")
	require.NoError(t, err)
	require.Contains(t, present, clientB.SessionID())
}

// failingPresenceStore embeds a working store but fails every Add so tests
// can prove store failures never unwind the subscription (B2 §7.7).
type failingPresenceStore struct {
	PresenceStore
}

func (f *failingPresenceStore) Add(context.Context, string, *PresenceInfo) error {
	return errors.New("injected store failure")
}

// TestPresence_OccupancyStoreFailureKeepsSubscription pins B2 §7.7: when the
// store Add fails, the subscription stays live and no disconnect happens —
// the join is still emitted over the live bus.
func TestPresence_OccupancyStoreFailureKeepsSubscription(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	node.SetPresenceStore(&failingPresenceStore{PresenceStore: NewMemoryPresenceStore()})
	require.NoError(t, node.Run(ctx))

	const ch = "store.fail.ch"
	client, transport := connectAndSubscribe(t, node, "client-a", &clientpb.Subscription{Channel: ch})

	require.Equal(t, 1, node.Hub().NumSubscribers(ch),
		"a store failure must not roll back the subscription")
	require.False(t, transport.isClosed(), "a store failure must not disconnect the client")
	require.NotNil(t, client)
}

// TestPresence_OccupancyReconnectDoesNotRejoin pins B2 §6: a heartbeat-style
// store refresh is not a join, so refreshing presence for an already tracked
// session never emits a second occupancy event (no gen noise).
func TestPresence_OccupancyReconnectDoesNotRejoin(t *testing.T) {
	ctx := context.Background()
	broker := &countingBroker{}
	node := NewNode(nil)
	node.SetBroker(broker)
	require.NoError(t, node.Run(ctx))

	const ch = "refresh.ch"
	client, _ := connectAndSubscribe(t, node, "client-a", &clientpb.Subscription{Channel: ch})

	// Refresh the TTL exactly like the heartbeat path (store.Add directly,
	// no presenceJoin).
	require.NoError(t, node.SetPresenceForSession(ctx, ch, client))

	require.Len(t, broker.occupancyEmits(), 1,
		"a store refresh must not emit a second join")
}
