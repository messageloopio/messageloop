package messageloop

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/messageloopio/messageloop/config"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
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
func (g *gapHistoryBroker) PublishOccupancy(string, OccupancyEvent) error      { return nil }
func (g *gapHistoryBroker) SetOccupancyHandler(OccupancyHandler) error         { return nil }

func (g *gapHistoryBroker) History(ch string, sinceOffset uint64, limit int) (*HistoryPage, error) {
	return &HistoryPage{Gap: true, GapReason: g.reason}, nil
}

// trimmedHistoryBroker holds history whose head was trimmed to firstRetained,
// so History reports a head_trimmed gap only when the caller set a since
// offset below it (reading from the beginning with since==0 stays gap-free).
type trimmedHistoryBroker struct {
	pubs          []*Publication
	firstRetained uint64
}

func (b *trimmedHistoryBroker) Start(ctx context.Context, handler PublicationHandler) error {
	<-ctx.Done()
	return nil
}

func (b *trimmedHistoryBroker) Subscribe(string) error   { return nil }
func (b *trimmedHistoryBroker) Unsubscribe(string) error { return nil }
func (b *trimmedHistoryBroker) Publish(ch string, pub *Publication) (uint64, error) {
	return 0, nil
}
func (b *trimmedHistoryBroker) PublishTransient(ch string, pub *Publication) error { return nil }
func (b *trimmedHistoryBroker) PublishOccupancy(string, OccupancyEvent) error      { return nil }
func (b *trimmedHistoryBroker) SetOccupancyHandler(OccupancyHandler) error         { return nil }

func (b *trimmedHistoryBroker) History(ch string, sinceOffset uint64, limit int) (*HistoryPage, error) {
	page := &HistoryPage{FirstRetained: b.firstRetained}
	if sinceOffset > 0 && b.firstRetained > sinceOffset {
		page.Gap = true
		page.GapReason = HistoryGapHeadTrimmed
	}
	for _, p := range b.pubs {
		if p.Offset >= sinceOffset {
			if limit > 0 && len(page.Publications) >= limit {
				break
			}
			page.Publications = append(page.Publications, p)
		}
	}
	return page, nil
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

// connectOn connects a fresh client to node and drives one Connect request,
// returning every outbound frame in write order (Connected first, then the
// replay stream for recover=true channels).
func connectOn(t *testing.T, node *Node, transport *capturingTransport, connect *clientpb.Connect) []*clientpb.OutboundMessage {
	t.Helper()
	ctx := context.Background()
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id:       "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: connect},
	}))
	return outboundMessages(t, transport)
}

// subscribeAckMsgs sends one Subscribe on an existing authenticated client and
// returns the SubscribeAck (documented as the first frame) plus every outbound
// frame in write order.
func subscribeAckMsgs(t *testing.T, client *Client, transport *capturingTransport, id string, subscriptions []*clientpb.Subscription) (*clientpb.SubscribeAck, []*clientpb.OutboundMessage) {
	t.Helper()
	transport.resetMessages()
	require.NoError(t, client.HandleMessage(context.Background(), &clientpb.InboundMessage{
		Id: id,
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{Subscriptions: subscriptions},
		},
	}))
	msgs := outboundMessages(t, transport)
	require.NotEmpty(t, msgs)
	ack := msgs[0].GetSubscribeAck()
	require.NotNil(t, ack, "the first frame of a Subscribe request must be the SubscribeAck")
	return ack, msgs
}

// subscribeAck connects a fresh client and sends one Subscribe request,
// returning the SubscribeAck envelope (first frame).
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
	ack, _ := subscribeAckMsgs(t, client, transport, "sub-1", subscriptions)
	return ack
}

// mustPosOffset reads the required offset of a Position, failing the test.
func mustPosOffset(t *testing.T, p *sharedv2.Position) uint64 {
	t.Helper()
	off, set := posOffset(p)
	require.True(t, set, "position offset must be set")
	return off
}

// --- §9.1: Subscribe recovery from an offset streams replay then complete ---

func TestSubscribe_RecoverFromOffset(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	node.SetBroker(&fakeEpochHistoryBroker{epoch: "v2", pubs: recoveryPubs("sub.ch", 1, 10)})
	require.NoError(t, node.Run(ctx))

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id:       "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "client-1"}},
	}))

	ack, msgs := subscribeAckMsgs(t, client, transport, "sub-1", []*clientpb.Subscription{
		{Channel: "sub.ch", Recover: true, Cursor: cursorOf("v2", 5)},
	})

	// Ack carries no publication batch; the recover state is PENDING because
	// History runs for this channel.
	assert.Equal(t, clientpb.RecoverState_RECOVER_STATE_PENDING, ack.GetRecover())
	// The ack frame itself must not be a publication; the replay stream is
	// distinct and all replay frames carry replay=true (checked below).
	require.Nil(t, msgs[0].GetPublication())

	replays := replayPublications(msgs)
	assert.Equal(t, []uint64{6, 7, 8, 9, 10}, publicationOffsets(replays))
	for _, pub := range replays {
		msgs1 := pub.GetMessages()
		require.Len(t, msgs1, 1)
		m := msgs1[0]
		assert.True(t, m.GetReplay(), "recovered messages must carry replay=true")
		assert.Equal(t, fmt.Sprintf("sub.ch-%d", mustPosOffset(t, m.GetPosition())), m.GetId(),
			"recovered ID must follow the channel-offset rule")
		assert.Equal(t, "sub.ch", m.GetChannel())
	}
	completes := recoverCompletes(msgs)
	require.Len(t, completes, 1, "every recover=true channel must end with exactly one RecoverComplete")
	rc := completes[0]
	assert.Equal(t, "sub.ch", rc.GetChannel())
	assert.False(t, rc.GetTruncated())
	assert.False(t, rc.GetGap())
	assert.Equal(t, uint64(10), mustPosOffset(t, rc.GetPosition()), "ok position must be the last delivered offset")
	assert.Nil(t, rc.GetError())
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

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id:       "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "client-1"}},
	}))

	ack, msgs := subscribeAckMsgs(t, client, transport, "sub-1", []*clientpb.Subscription{
		{Channel: "err.ch", Recover: true, Cursor: cursorOf("v2", 1)},
	})

	// The subscription must be committed: recovery failure never rolls it back.
	assertSubscriptionChannels(t, ack.GetSubscriptions(), []string{"err.ch"})
	assert.Equal(t, 1, node.Hub().NumSubscribers("err.ch"))
	assert.Empty(t, replayPublications(msgs), "no publications may be delivered on failure")
	completes := recoverCompletes(msgs)
	require.Len(t, completes, 1, "a failed recovery must still end with its RecoverComplete")
	rc := completes[0]
	assert.Equal(t, "err.ch", rc.GetChannel())
	require.NotNil(t, rc.GetError(), "a failed recovery must be visible, not swallowed")
	assert.Equal(t, "RECOVER_FAILED", rc.GetError().GetCode())
	assert.Equal(t, "recover_error", rc.GetError().GetType())
	off, set := posOffset(rc.GetPosition())
	assert.Equal(t, uint64(1), off, "a failed recovery echoes the cursor")
	assert.True(t, set)
}

// --- §9.3: a fresh (non-resume) recover replays at most
// MaxRecoveredPublications and reports truncation via RecoverComplete ---

func TestConnect_RecoverTruncated(t *testing.T) {
	ctx := context.Background()
	metrics := NewMetrics(prometheus.NewRegistry())
	node := NewNode(nil)
	node.SetMetrics(metrics)
	const total = 2000
	node.SetBroker(&fakeHistoryBroker{pubs: recoveryPubs("trunc.ch", 1, total)})
	require.NoError(t, node.Run(ctx))

	msgs := connectOn(t, node, &capturingTransport{}, &clientpb.Connect{
		ClientId: "client-1",
		Subscriptions: []*clientpb.Subscription{
			{Channel: "trunc.ch", Recover: true, Fresh: true},
		},
	})

	connected := msgs[0].GetConnected()
	require.NotNil(t, connected, "the first Connect frame must be the bare Connected envelope")
	require.Nil(t, msgs[0].GetPublication(), "the Connected frame must not carry a publication")

	replays := replayPublications(msgs)
	require.Len(t, replays, MaxRecoveredPublications)
	offsets := publicationOffsets(replays)
	assert.Equal(t, uint64(1), offsets[0], "fresh=true must replay from the beginning")
	assert.Equal(t, uint64(MaxRecoveredPublications), offsets[len(offsets)-1])

	completes := recoverCompletes(msgs)
	require.Len(t, completes, 1)
	rc := completes[0]
	assert.Equal(t, "trunc.ch", rc.GetChannel())
	assert.True(t, rc.GetTruncated(), "reaching the cap must set RecoverComplete.truncated")
	assert.Equal(t, uint64(MaxRecoveredPublications), mustPosOffset(t, rc.GetPosition()),
		"truncated position must be the last delivered publication")
	assert.Nil(t, rc.GetError())

	assert.Equal(t, float64(1), testutil.ToFloat64(metrics.RecoveryTruncatedTotal.WithLabelValues("connect")),
		"messageloop_recovery_truncated_total must increment")
	assert.Equal(t, float64(1), testutil.ToFloat64(metrics.RecoveryTotal.WithLabelValues("connect", "truncated")))
}

// --- §9.4: recover=false never calls History and carries no completion ---

func TestSubscribe_RecoverFalse(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	broker := &countingRecoveryBroker{fakeEpochHistoryBroker: &fakeEpochHistoryBroker{epoch: "v2", pubs: recoveryPubs("plain.ch", 1, 3)}}
	node.SetBroker(broker)
	require.NoError(t, node.Run(ctx))

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id:       "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "client-1"}},
	}))

	ack, msgs := subscribeAckMsgs(t, client, transport, "sub-1", []*clientpb.Subscription{{Channel: "plain.ch"}})

	assert.Zero(t, broker.historyCalls.Load(), "recover=false must not call History")
	assert.Equal(t, clientpb.RecoverState_RECOVER_STATE_NONE, ack.GetRecover(),
		"an ack without recover=true must report NONE")
	assert.Empty(t, recoverCompletes(msgs), "a recover=false channel gets no RecoverComplete")
}

// --- §9.5: wildcard subscriptions are skipped with RECOVER_SKIPPED ---

func TestSubscribe_RecoverWildcardSkipped(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	broker := &countingRecoveryBroker{fakeEpochHistoryBroker: &fakeEpochHistoryBroker{epoch: "v2", pubs: recoveryPubs("im.room", 1, 3)}}
	node.SetBroker(broker)
	require.NoError(t, node.Run(ctx))

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id:       "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "client-1"}},
	}))

	ack, msgs := subscribeAckMsgs(t, client, transport, "sub-1", []*clientpb.Subscription{
		{Channel: "im.**", Recover: true, Cursor: cursorOf("v2", 5)},
	})

	// Wildcard subscriptions live in the matcher, not the exact-channel
	// shards, so the ack subscription list proves the subscribe succeeded.
	assertSubscriptionChannels(t, ack.GetSubscriptions(), []string{"im.**"})
	assert.Zero(t, broker.historyCalls.Load(), "wildcard channels must never call History")
	assert.Equal(t, clientpb.RecoverState_RECOVER_STATE_SKIPPED, ack.GetRecover(),
		"an all-skipped batch must report SKIPPED on the ack")
	assert.Empty(t, replayPublications(msgs))
	completes := recoverCompletes(msgs)
	require.Len(t, completes, 1, "a skipped channel must still close with RecoverComplete")
	rc := completes[0]
	require.NotNil(t, rc.GetError())
	assert.Equal(t, "RECOVER_SKIPPED", rc.GetError().GetCode())
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

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id:       "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "client-1"}},
	}))

	ack, msgs := subscribeAckMsgs(t, client, transport, "sub-1", []*clientpb.Subscription{
		{Channel: "game.tick.fps", Recover: true, Cursor: cursorOf("v2", 5)},
	})

	assert.Zero(t, broker.historyCalls.Load(), "policy-denied channels must never call History")
	assert.Equal(t, clientpb.RecoverState_RECOVER_STATE_SKIPPED, ack.GetRecover())
	completes := recoverCompletes(msgs)
	require.Len(t, completes, 1)
	rc := completes[0]
	require.NotNil(t, rc.GetError())
	assert.Equal(t, "RECOVER_SKIPPED", rc.GetError().GetCode())
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

	msgs := connectOn(t, node, &capturingTransport{}, &clientpb.Connect{
		ClientId:  "client-1",
		Token:     "t",
		SessionId: "sess-off-resume",
		Subscriptions: []*clientpb.Subscription{
			{Channel: "miss.ch", Recover: true},
		},
	})

	assert.Empty(t, replayPublications(msgs), "a missing server-recorded offset must not replay history")
	completes := recoverCompletes(msgs)
	require.Len(t, completes, 1)
	rc := completes[0]
	assert.Equal(t, "miss.ch", rc.GetChannel())
	require.NotNil(t, rc.GetError())
	assert.Equal(t, "RECOVER_SKIPPED", rc.GetError().GetCode())
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
	msgs := connectOn(t, node, &capturingTransport{}, &clientpb.Connect{
		ClientId:  "client-1",
		Token:     "t",
		SessionId: "sess-off-resume",
	})

	assert.Equal(t, []uint64{6, 7, 8, 9, 10}, publicationOffsets(replayPublications(msgs)))
	completes := recoverCompletes(msgs)
	require.Len(t, completes, 1)
	rc := completes[0]
	assert.Equal(t, "off.news", rc.GetChannel())
	assert.False(t, rc.GetTruncated())
	assert.Equal(t, uint64(10), mustPosOffset(t, rc.GetPosition()))
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

	first, msgs1 := subscribeAckMsgs(t, client, transport, "sub-1", []*clientpb.Subscription{{Channel: "resub.ch"}})
	assert.Equal(t, clientpb.RecoverState_RECOVER_STATE_NONE, first.GetRecover())
	assert.Empty(t, recoverCompletes(msgs1))
	require.True(t, client.hasSubscription("resub.ch"))
	assert.Equal(t, 1, node.Hub().NumSubscribers("resub.ch"))

	_, msgs2 := subscribeAckMsgs(t, client, transport, "sub-2", []*clientpb.Subscription{
		{Channel: "resub.ch", Recover: true, Cursor: cursorOf("v2", 5)},
	})
	assert.Equal(t, []uint64{6, 7, 8}, publicationOffsets(replayPublications(msgs2)),
		"re-subscribing with recover must catch up from the given cursor")
	completes := recoverCompletes(msgs2)
	require.Len(t, completes, 1)
	assert.Equal(t, uint64(8), mustPosOffset(t, completes[0].GetPosition()))
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

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id:       "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "client-1"}},
	}))

	_, msgs := subscribeAckMsgs(t, client, transport, "sub-1", []*clientpb.Subscription{
		{Channel: "gap.ch", Recover: true, Cursor: cursorOf("v2", 5)},
	})

	assert.Empty(t, replayPublications(msgs))
	completes := recoverCompletes(msgs)
	require.Len(t, completes, 1)
	rc := completes[0]
	assert.Equal(t, "gap.ch", rc.GetChannel())
	assert.True(t, rc.GetTruncated(), "an empty batch with a gap must be truncated, not RecoverOK")
	assert.True(t, rc.GetGap())
	assert.Equal(t, sharedv2.GapReason_GAP_REASON_EMPTY_EXPIRED, rc.GetGapReason())
	assert.Equal(t, uint64(5), mustPosOffset(t, rc.GetPosition()), "the cursor must be echoed")
	assert.Equal(t, float64(1), testutil.ToFloat64(metrics.RecoveryGapTotal.WithLabelValues("empty_expired")),
		"messageloop_recovery_gap_total{reason=empty_expired} must increment")
}

// --- C4: a middle gap maps to GAP_REASON_MIDDLE on the wire + metric ---

func TestSubscribe_RecoverMiddleGapMapsToProto(t *testing.T) {
	ctx := context.Background()
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	node := NewNode(nil)
	node.SetMetrics(metrics)
	node.SetBroker(&gapHistoryBroker{reason: HistoryGapMiddle})
	require.NoError(t, node.Run(ctx))

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id:       "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "client-1"}},
	}))

	_, msgs := subscribeAckMsgs(t, client, transport, "sub-1", []*clientpb.Subscription{
		{Channel: "middle.ch", Recover: true, Cursor: cursorOf("v2", 5)},
	})

	completes := recoverCompletes(msgs)
	require.Len(t, completes, 1)
	rc := completes[0]
	assert.Equal(t, "middle.ch", rc.GetChannel())
	assert.True(t, rc.GetGap())
	assert.Equal(t, sharedv2.GapReason_GAP_REASON_MIDDLE, rc.GetGapReason(),
		"HistoryGapMiddle must map to GAP_REASON_MIDDLE on the wire")
	assert.Equal(t, float64(1), testutil.ToFloat64(metrics.RecoveryGapTotal.WithLabelValues("middle")),
		"messageloop_recovery_gap_total{reason=middle} must increment")
}

// --- §9.10: an empty successful batch echoes the cursor, not 0 ---

func TestSubscribe_RecoverEmptyEchoesCursor(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	node.SetBroker(&fakeEpochHistoryBroker{epoch: "v2", pubs: recoveryPubs("empty.ch", 1, 3)})
	require.NoError(t, node.Run(ctx))

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id:       "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "client-1"}},
	}))

	_, msgs := subscribeAckMsgs(t, client, transport, "sub-1", []*clientpb.Subscription{
		{Channel: "empty.ch", Recover: true, Cursor: cursorOf("v2", 5)},
	})

	assert.Empty(t, replayPublications(msgs))
	completes := recoverCompletes(msgs)
	require.Len(t, completes, 1)
	rc := completes[0]
	assert.False(t, rc.GetTruncated())
	assert.False(t, rc.GetGap())
	assert.Equal(t, uint64(5), mustPosOffset(t, rc.GetPosition()), "an empty batch must echo the cursor, not 0")
	assert.Nil(t, rc.GetError())
}

// --- §6.1/§6.2: frame order — bare ack first, then replay, then complete ---

func TestSubscribe_AckThenReplayThenCompleteOrder(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	node.SetBroker(&fakeEpochHistoryBroker{epoch: "v2", pubs: recoveryPubs("ord.ch", 1, 3)})
	require.NoError(t, node.Run(ctx))

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id:       "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "client-1"}},
	}))

	_, msgs := subscribeAckMsgs(t, client, transport, "sub-1", []*clientpb.Subscription{
		{Channel: "ord.ch", Recover: true, Fresh: true},
	})

	require.Len(t, msgs, 5, "SubscribeAck + 3 replay + RecoverComplete")
	require.NotNil(t, msgs[0].GetSubscribeAck(), "frame 0 must be the SubscribeAck")
	require.Nil(t, msgs[0].GetPublication(), "the ack frame must not carry a publication")
	for i := 1; i <= 3; i++ {
		require.NotNil(t, msgs[i].GetPublication(), "frame %d must be a replay publication", i)
		require.True(t, msgs[i].GetPublication().GetMessages()[0].GetReplay())
	}
	require.NotNil(t, msgs[4].GetRecoverComplete(), "the last frame must be the RecoverComplete")
}

// --- §6.3: fresh replays from the beginning ---

func TestSubscribe_FreshReplaysFromStart(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	broker := &countingRecoveryBroker{fakeEpochHistoryBroker: &fakeEpochHistoryBroker{epoch: "v2", pubs: recoveryPubs("fresh.ch", 1, 7)}}
	node.SetBroker(broker)
	require.NoError(t, node.Run(ctx))

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id:       "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "client-1"}},
	}))

	_, msgs := subscribeAckMsgs(t, client, transport, "sub-1", []*clientpb.Subscription{
		{Channel: "fresh.ch", Recover: true, Fresh: true, Cursor: cursorOf("v2", 99)},
	})

	// fresh ignores cursor.offset entirely: replay from offset 1.
	assert.Equal(t, []uint64{1, 2, 3, 4, 5, 6, 7}, publicationOffsets(replayPublications(msgs)))
	assert.Equal(t, int32(1), broker.historyCalls.Load())
	rc := findReplayComplete(msgs, "fresh.ch")
	require.NotNil(t, rc)
	assert.Positive(t, mustPosOffset(t, rc.GetPosition()))
}

// --- §6.3: cursor.offset=0 with fresh=false is NOT a fresh replay ---

func TestSubscribe_OffsetZeroFreshFalseIsNotFromStart(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	// History head was trimmed to offset 6: a from-start read (since 0) is a
	// clean prefix, but a since=1 read exposes the head_trimmed gap.
	node.SetBroker(&trimmedHistoryBroker{pubs: recoveryPubs("trim.ch", 6, 10), firstRetained: 6})
	require.NoError(t, node.Run(ctx))

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id:       "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "client-1"}},
	}))

	// cursor.offset is explicitly 0: a resume-from-offset-1, NOT a fresh
	// replay. The two differ exactly by the gap the trimmed head exposes.
	_, msgs := subscribeAckMsgs(t, client, transport, "sub-1", []*clientpb.Subscription{
		{Channel: "trim.ch", Recover: true, Cursor: cursorOf("v2", 0)},
	})

	rc := findReplayComplete(msgs, "trim.ch")
	require.NotNil(t, rc)
	assert.True(t, rc.GetGap(), "a since=1 read past the first retained offset must expose the gap")
	assert.Equal(t, sharedv2.GapReason_GAP_REASON_HEAD_TRIMMED, rc.GetGapReason())
	assert.Equal(t, []uint64{6, 7, 8, 9, 10}, publicationOffsets(replayPublications(msgs)))
}

// --- §6.3/§4.1: no cursor + no server-recorded offset = skip, not a flood ---

func TestSubscribe_NoCursorNoDeliveredOffsetSkips(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	broker := &countingRecoveryBroker{fakeEpochHistoryBroker: &fakeEpochHistoryBroker{epoch: "v2", pubs: recoveryPubs("flood.ch", 1, 1000)}}
	node.SetBroker(broker)
	require.NoError(t, node.Run(ctx))

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id:       "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "client-1"}},
	}))

	_, msgs := subscribeAckMsgs(t, client, transport, "sub-1", []*clientpb.Subscription{
		{Channel: "flood.ch", Recover: true},
	})

	assert.Zero(t, broker.historyCalls.Load(), "no cursor and no delivered offset must never call History")
	assert.Empty(t, replayPublications(msgs), "no cursor must not flood the history")
	rc := findReplayComplete(msgs, "flood.ch")
	require.NotNil(t, rc)
	require.NotNil(t, rc.GetError())
	assert.Equal(t, "RECOVER_SKIPPED", rc.GetError().GetCode())
}

// --- §4.1/§6.8: no cursor but a server-recorded delivered offset resumes +1 ---

func TestSubscribe_NoCursorResumesFromDeliveredOffset(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	node.SetBroker(&fakeEpochHistoryBroker{epoch: "v2", pubs: recoveryPubs("del.ch", 1, 10)})
	require.NoError(t, node.Run(ctx))

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id:       "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "client-1"}},
	}))
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id: "sub-1",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{Subscriptions: []*clientpb.Subscription{{Channel: "del.ch"}}},
		},
	}))

	// Deliver three live publications: the hub records DeliveredOffset=3.
	transport.resetMessages()
	require.NoError(t, node.hub.broadcastPublication("del.ch", &Publication{Channel: "del.ch", Offset: 1, Payload: []byte("a")}))
	require.NoError(t, node.hub.broadcastPublication("del.ch", &Publication{Channel: "del.ch", Offset: 2, Payload: []byte("b")}))
	require.NoError(t, node.hub.broadcastPublication("del.ch", &Publication{Channel: "del.ch", Offset: 3, Payload: []byte("c")}))
	sub, ok := node.Hub().LookupSubscriber("del.ch", client)
	require.True(t, ok)
	require.Equal(t, uint64(3), sub.DeliveredOffset)

	// Re-subscribe with recover=true but no cursor: resume from delivered+1.
	_, msgs := subscribeAckMsgs(t, client, transport, "sub-2", []*clientpb.Subscription{
		{Channel: "del.ch", Recover: true},
	})
	assert.Equal(t, []uint64{4, 5, 6, 7, 8, 9, 10}, publicationOffsets(replayPublications(msgs)),
		"no cursor must continue from the server-recorded delivered offset+1, not from the start")
	rc := findReplayComplete(msgs, "del.ch")
	require.NotNil(t, rc)
	assert.Equal(t, uint64(10), mustPosOffset(t, rc.GetPosition()))
}

// --- §6.6: every outbound frame honors MaxMessageSize; no Connected exemption ---

func TestSession_OutboundFrameHonorsMaxMessageSize(t *testing.T) {
	ctx := context.Background()
	node := NewNode(&config.Server{Limits: config.Limits{MaxMessageSize: 512}})
	require.NoError(t, node.Run(ctx))

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id:       "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "client-1"}},
	}))

	big := make([]byte, 2048)
	pub := &clientpb.Publication{Messages: []*clientpb.Message{
		{
			Id:       "big-1",
			Channel:  "size.ch",
			Position: positionFrom("", 1, true),
			Payload: &sharedv2.Payload{
				Data: &sharedv2.Payload_Binary{Binary: big},
			},
		},
	}}
	require.ErrorIs(t, client.Send(ctx, MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Publication{Publication: pub}
	})), ErrOutboundTooLarge, "an oversized publication frame must fail the max-size cap")

	// The Connected envelope gets no exemption: an oversized Connected fails
	// the same way (B3 removes the pre-stream batch carrier).
	require.ErrorIs(t, client.Send(ctx, MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Connected{
			Connected: &clientpb.Connected{SessionId: "x", StreamEpoch: string(big)},
		}
	})), ErrOutboundTooLarge, "Connected must honor MaxMessageSize like every other frame")
}
