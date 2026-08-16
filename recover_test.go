package messageloop

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/messageloopio/messageloop/config"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingRecoveryBroker wraps fakeEpochHistoryBroker with a History call
// counter and an optional error, so tests can prove whether recovery ran.
type countingRecoveryBroker struct {
	*fakeEpochHistoryBroker
	historyCalls atomic.Int32
	historyErr   error
}

func (c *countingRecoveryBroker) History(ch string, sinceOffset uint64, limit int) (*HistoryPage, error) {
	c.historyCalls.Add(1)
	if c.historyErr != nil {
		return nil, c.historyErr
	}
	return c.fakeEpochHistoryBroker.History(ch, sinceOffset, limit)
}

// gapHistoryBroker returns an empty history page with the given gap reason,
// proving recoverSubscription never claims RecoverOK on an unprovable empty
// batch (A2 §10.9).
type gapHistoryBroker struct {
	reason HistoryGapReason
}

func (g *gapHistoryBroker) Start(ctx context.Context, handler PublicationHandler) error {
	<-ctx.Done()
	return nil
}

func (g *gapHistoryBroker) Subscribe(string) error   { return nil }
func (g *gapHistoryBroker) Unsubscribe(string) error { return nil }
func (g *gapHistoryBroker) Publish(ch string, pub *Publication) (uint64, error) {
	return 0, nil
}
func (g *gapHistoryBroker) PublishTransient(ch string, pub *Publication) error { return nil }

func (g *gapHistoryBroker) History(ch string, sinceOffset uint64, limit int) (*HistoryPage, error) {
	return &HistoryPage{Gap: true, GapReason: g.reason}, nil
}

// recoveryPubs builds history publications for channel ch with offsets
// first..last.
func recoveryPubs(ch string, first, last uint64) []*Publication {
	pubs := make([]*Publication, 0, last-first+1)
	for i := first; i <= last; i++ {
		pubs = append(pubs, &Publication{Channel: ch, Offset: i, Payload: []byte("m")})
	}
	return pubs
}

// subscribeOn sends one Subscribe on an existing authenticated client and
// returns the SubscribeAck envelope.
func subscribeOn(t *testing.T, client *Client, transport *capturingTransport, id string, subscriptions []*clientpb.Subscription) *clientpb.SubscribeAck {
	t.Helper()
	transport.messages = nil
	require.NoError(t, client.HandleMessage(context.Background(), &clientpb.InboundMessage{
		Id: id,
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{Subscriptions: subscriptions},
		},
	}))
	var out clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getLastMessage(), &out))
	ack := out.GetSubscribeAck()
	require.NotNil(t, ack, "expected a SubscribeAck envelope")
	return ack
}

// subscribeAck connects a fresh client and sends one Subscribe request,
// returning the SubscribeAck envelope.
func subscribeAck(t *testing.T, node *Node, subscriptions []*clientpb.Subscription) *clientpb.SubscribeAck {
	t.Helper()
	ctx := context.Background()
	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id:       "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "client-1"}},
	}))
	return subscribeOn(t, client, transport, "sub-1", subscriptions)
}

// publicationOffsets flattens the messages of a publication batch into
// offsets.
func publicationOffsets(pubs []*clientpb.Publication) []uint64 {
	offsets := make([]uint64, 0, len(pubs))
	for _, pub := range pubs {
		for _, m := range pub.GetMessages() {
			offsets = append(offsets, m.GetOffset())
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
	assert.Equal(t, channels, got)
}

// --- §9.1: Subscribe recovery from an offset continues history ---

func TestSubscribe_RecoverFromOffset(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	node.SetBroker(&fakeEpochHistoryBroker{epoch: "v2", pubs: recoveryPubs("sub.ch", 1, 10)})
	require.NoError(t, node.Run(ctx))

	ack := subscribeAck(t, node, []*clientpb.Subscription{
		{Channel: "sub.ch", Recover: true, Offset: 5, Epoch: "v2"},
	})

	assert.Equal(t, []uint64{6, 7, 8, 9, 10}, publicationOffsets(ack.GetPublications()))
	for _, pub := range ack.GetPublications() {
		msgs := pub.GetMessages()
		require.Len(t, msgs, 1)
		m := msgs[0]
		assert.Equal(t, fmt.Sprintf("sub.ch-%d", m.GetOffset()), m.GetId(),
			"recovered ID must follow the channel-offset rule")
	}
	require.Len(t, ack.GetRecoverResults(), 1)
	res := ack.GetRecoverResults()[0]
	assert.Equal(t, "sub.ch", res.GetChannel())
	assert.True(t, res.GetRecovered())
	assert.False(t, res.GetTruncated())
	assert.Equal(t, uint64(10), res.GetOffset())
	assert.Nil(t, res.GetError())
	assert.Equal(t, "v2", res.GetEpoch())
}

// --- §9.2: a History error must surface, and the subscription must stay ---

func TestSubscribe_RecoverHistoryError(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	broker := &countingRecoveryBroker{
		fakeEpochHistoryBroker: &fakeEpochHistoryBroker{epoch: "v2", pubs: recoveryPubs("err.ch", 1, 5)},
		historyErr:             errors.New("history unavailable"),
	}
	node.SetBroker(broker)
	require.NoError(t, node.Run(ctx))

	ack := subscribeAck(t, node, []*clientpb.Subscription{
		{Channel: "err.ch", Recover: true, Offset: 1, Epoch: "v2"},
	})

	// The subscription must be committed: recovery failure never rolls it back.
	assertSubscriptionChannels(t, ack.GetSubscriptions(), []string{"err.ch"})
	assert.Equal(t, 1, node.Hub().NumSubscribers("err.ch"))
	assert.Empty(t, ack.GetPublications(), "no publications may be delivered on failure")
	require.Len(t, ack.GetRecoverResults(), 1)
	res := ack.GetRecoverResults()[0]
	assert.False(t, res.GetRecovered())
	assert.False(t, res.GetTruncated())
	require.NotNil(t, res.GetError(), "a failed recovery must be visible, not swallowed")
	assert.Equal(t, "RECOVER_FAILED", res.GetError().GetCode())
	assert.Equal(t, "recover_error", res.GetError().GetType())
}

// --- §9.3: a fresh (non-resume) recover with offset 0 replays at most
// MaxRecoveredPublications and reports truncation ---

func TestConnect_RecoverTruncated(t *testing.T) {
	ctx := context.Background()
	metrics := NewMetrics(prometheus.NewRegistry())
	node := NewNode(nil)
	node.SetMetrics(metrics)
	const total = 2000
	node.SetBroker(&fakeHistoryBroker{pubs: recoveryPubs("trunc.ch", 1, total)})
	require.NoError(t, node.Run(ctx))

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId: "client-1",
				Subscriptions: []*clientpb.Subscription{
					{Channel: "trunc.ch", Recover: true, Offset: 0},
				},
			},
		},
	}))

	var out clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getLastMessage(), &out))
	connected := out.GetConnected()
	require.NotNil(t, connected)
	require.Len(t, connected.GetPublications(), MaxRecoveredPublications)
	offsets := publicationOffsets(connected.GetPublications())
	assert.Equal(t, uint64(1), offsets[0], "offset 0 + non-resume must replay from the beginning (KD-2)")
	assert.Equal(t, uint64(MaxRecoveredPublications), offsets[len(offsets)-1])
	require.True(t, connected.GetTruncated(), "reaching the cap must set Connected.truncated")
	require.Len(t, connected.GetRecoverResults(), 1)
	res := connected.GetRecoverResults()[0]
	assert.True(t, res.GetRecovered())
	assert.True(t, res.GetTruncated())
	assert.Equal(t, uint64(MaxRecoveredPublications), res.GetOffset(),
		"truncated offset must be the last delivered publication")
	assert.Equal(t, float64(1), testutil.ToFloat64(metrics.RecoveryTruncatedTotal.WithLabelValues("connect")),
		"messageloop_recovery_truncated_total must increment")
	assert.Equal(t, float64(1), testutil.ToFloat64(metrics.RecoveryTotal.WithLabelValues("connect", "truncated")))
}

// --- §9.4: recover=false never calls History and carries no error ---

func TestSubscribe_RecoverFalse(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	broker := &countingRecoveryBroker{fakeEpochHistoryBroker: &fakeEpochHistoryBroker{epoch: "v2", pubs: recoveryPubs("plain.ch", 1, 3)}}
	node.SetBroker(broker)
	require.NoError(t, node.Run(ctx))

	ack := subscribeAck(t, node, []*clientpb.Subscription{{Channel: "plain.ch"}})

	assert.Zero(t, broker.historyCalls.Load(), "recover=false must not call History")
	require.Len(t, ack.GetRecoverResults(), 1)
	res := ack.GetRecoverResults()[0]
	assert.False(t, res.GetRecovered())
	assert.Nil(t, res.GetError(), "a skip without a recover request must carry no error")
	assert.Equal(t, uint64(0), res.GetOffset())
}

// --- §9.5: wildcard subscriptions are skipped with RECOVER_SKIPPED ---

func TestSubscribe_RecoverWildcardSkipped(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	broker := &countingRecoveryBroker{fakeEpochHistoryBroker: &fakeEpochHistoryBroker{epoch: "v2", pubs: recoveryPubs("im.room", 1, 3)}}
	node.SetBroker(broker)
	require.NoError(t, node.Run(ctx))

	ack := subscribeAck(t, node, []*clientpb.Subscription{
		{Channel: "im.**", Recover: true, Offset: 5, Epoch: "v2"},
	})

	// Wildcard subscriptions live in the matcher, not the exact-channel
	// shards, so the ack subscription list proves the subscribe succeeded.
	assertSubscriptionChannels(t, ack.GetSubscriptions(), []string{"im.**"})
	assert.Zero(t, broker.historyCalls.Load(), "wildcard channels must never call History")
	require.Len(t, ack.GetRecoverResults(), 1)
	res := ack.GetRecoverResults()[0]
	assert.False(t, res.GetRecovered())
	require.NotNil(t, res.GetError())
	assert.Equal(t, "RECOVER_SKIPPED", res.GetError().GetCode())
}

// --- §9.6: transient_only policy denies recovery with RECOVER_SKIPPED ---

func TestSubscribe_RecoverPolicySkipped(t *testing.T) {
	ctx := context.Background()
	node := NewNode(&config.Server{
		Authorizer: config.AuthorizerConfig{
			Rules: []config.AuthorizerRule{
				{Pattern: "game.tick.**", ChannelPolicySpec: config.ChannelPolicySpec{TransientOnly: policyBoolPtr(true)}},
			},
		},
	})
	broker := &countingRecoveryBroker{fakeEpochHistoryBroker: &fakeEpochHistoryBroker{epoch: "v2", pubs: recoveryPubs("game.tick.fps", 1, 3)}}
	node.SetBroker(broker)
	require.NoError(t, node.Run(ctx))

	ack := subscribeAck(t, node, []*clientpb.Subscription{
		{Channel: "game.tick.fps", Recover: true, Offset: 5, Epoch: "v2"},
	})

	assert.Zero(t, broker.historyCalls.Load(), "policy-denied channels must never call History")
	require.Len(t, ack.GetRecoverResults(), 1)
	res := ack.GetRecoverResults()[0]
	assert.False(t, res.GetRecovered(), "a policy skip is not a recovered empty batch")
	require.NotNil(t, res.GetError())
	assert.Equal(t, "RECOVER_SKIPPED", res.GetError().GetCode())
}

// --- §9.7: a resume without ChannelOffsets[ch] must skip, never replay ---

func TestConnect_ResumeMissingOffsetSkipped(t *testing.T) {
	snapshot := &ClusterSessionSnapshot{
		SessionID:     "sess-off-resume",
		UserID:        "user-1",
		ClientID:      "client-1",
		Subscriptions: []ClusterSubscriptionSnapshot{{Channel: "miss.ch"}},
		BrokerEpoch:   "v2",
	}
	node := remoteResumeTestNode(t, snapshot, recoveryPubs("miss.ch", 1, 10), "v2")

	connected := connectResume(t, node, &capturingTransport{}, []*clientpb.Subscription{
		{Channel: "miss.ch", Recover: true, Offset: 0, Epoch: "v2"},
	})

	assert.Empty(t, connected.GetPublications(), "a missing server-recorded offset must not replay history")
	require.Len(t, connected.GetRecoverResults(), 1)
	res := connected.GetRecoverResults()[0]
	assert.Equal(t, "miss.ch", res.GetChannel())
	assert.False(t, res.GetRecovered())
	require.NotNil(t, res.GetError())
	assert.Equal(t, "RECOVER_SKIPPED", res.GetError().GetCode())
	assert.Equal(t, uint64(0), res.GetOffset())
}

// --- §9.8: snapshot-only channels still resume from ChannelOffsets[ch]+1 ---

func TestConnect_ResumeSnapshotChannelNotInConnect(t *testing.T) {
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
	node := remoteResumeTestNode(t, snapshot, recoveryPubs("off.news", 1, 10), "v2")

	// The Connect request does not list off.news: the snapshot union must
	// still recover it from offset 6.
	connected := connectResume(t, node, &capturingTransport{}, nil)

	assert.Equal(t, []uint64{6, 7, 8, 9, 10}, publicationOffsets(connected.GetPublications()))
	require.Len(t, connected.GetRecoverResults(), 1)
	res := connected.GetRecoverResults()[0]
	assert.Equal(t, "off.news", res.GetChannel())
	assert.True(t, res.GetRecovered())
	assert.Equal(t, uint64(10), res.GetOffset())
}

// connectResume drives a remote resume Connect against a node built by
// remoteResumeTestNode and returns the Connected envelope.
func connectResume(t *testing.T, node *Node, transport *capturingTransport, subs []*clientpb.Subscription) *clientpb.Connected {
	t.Helper()
	ctx := context.Background()
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId:      "client-1",
				Token:         "t",
				SessionId:     "sess-off-resume",
				Subscriptions: subs,
			},
		},
	}))
	var out clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getLastMessage(), &out))
	connected := out.GetConnected()
	require.NotNil(t, connected)
	return connected
}

// --- §9.9: a re-subscribe with recover=true is a legal catch-up ---

func TestSubscribe_ResubscribeCatchUp(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	node.SetBroker(&fakeEpochHistoryBroker{epoch: "v2", pubs: recoveryPubs("resub.ch", 1, 8)})
	require.NoError(t, node.Run(ctx))

	// Same client, two Subscribe requests: the second is a catch-up on an
	// already-subscribed channel, not a fresh session.
	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id:       "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "client-1"}},
	}))

	first := subscribeOn(t, client, transport, "sub-1", []*clientpb.Subscription{{Channel: "resub.ch"}})
	require.Len(t, first.GetRecoverResults(), 1)
	assert.Nil(t, first.GetRecoverResults()[0].GetError())
	require.True(t, client.hasSubscription("resub.ch"))
	assert.Equal(t, 1, node.Hub().NumSubscribers("resub.ch"))

	second := subscribeOn(t, client, transport, "sub-2", []*clientpb.Subscription{
		{Channel: "resub.ch", Recover: true, Offset: 5, Epoch: "v2"},
	})
	assert.Equal(t, []uint64{6, 7, 8}, publicationOffsets(second.GetPublications()),
		"re-subscribing with recover must catch up from the given offset")
	require.Len(t, second.GetRecoverResults(), 1)
	assert.True(t, second.GetRecoverResults()[0].GetRecovered())
	assert.Equal(t, uint64(8), second.GetRecoverResults()[0].GetOffset())
	assert.Equal(t, 1, node.Hub().NumSubscribers("resub.ch"),
		"re-subscribe is catch-up on the same session, not a second subscriber")
}

// --- §10.9: an empty batch with a gap must never claim RecoverOK ---

func TestSubscribe_RecoverEmptyBatchGapIsTruncated(t *testing.T) {
	ctx := context.Background()
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	node := NewNode(nil)
	node.SetMetrics(metrics)
	node.SetBroker(&gapHistoryBroker{reason: HistoryGapEmptyExpired})
	require.NoError(t, node.Run(ctx))

	ack := subscribeAck(t, node, []*clientpb.Subscription{
		{Channel: "gap.ch", Recover: true, Offset: 5, Epoch: "v2"},
	})

	assert.Empty(t, ack.GetPublications())
	require.Len(t, ack.GetRecoverResults(), 1)
	res := ack.GetRecoverResults()[0]
	assert.True(t, res.GetRecovered())
	assert.True(t, res.GetTruncated(), "an empty batch with a gap must be truncated, not RecoverOK")
	assert.Equal(t, uint64(5), res.GetOffset(), "the cursor must be echoed")
	assert.Equal(t, float64(1), testutil.ToFloat64(metrics.RecoveryGapTotal.WithLabelValues("empty_expired")),
		"messageloop_recovery_gap_total{reason=empty_expired} must increment")
}

// --- §9.10: an empty successful batch echoes the cursor, not 0 ---

func TestSubscribe_RecoverEmptyEchoesCursor(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	node.SetBroker(&fakeEpochHistoryBroker{epoch: "v2", pubs: recoveryPubs("empty.ch", 1, 3)})
	require.NoError(t, node.Run(ctx))

	ack := subscribeAck(t, node, []*clientpb.Subscription{
		{Channel: "empty.ch", Recover: true, Offset: 5, Epoch: "v2"},
	})

	assert.Empty(t, ack.GetPublications())
	require.Len(t, ack.GetRecoverResults(), 1)
	res := ack.GetRecoverResults()[0]
	assert.True(t, res.GetRecovered())
	assert.False(t, res.GetTruncated())
	assert.Equal(t, uint64(5), res.GetOffset(), "an empty batch must echo the cursor, not 0")
}
