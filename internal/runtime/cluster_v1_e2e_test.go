package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/messageloopio/messageloop/config"
	"github.com/messageloopio/messageloop/internal/runtime"
	"github.com/messageloopio/messageloop/internal/session"
	"github.com/messageloopio/messageloop/internal/stream"
	"github.com/messageloopio/messageloop/pkg/redisbroker"
	"github.com/messageloopio/messageloop/shared"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

// cluster_v1_e2e_test.go covers the four PR-10 cluster paths: admin
// disconnect by user (existing TestAdmin_DisconnectUsersAcrossNodes),
// wildcard x presence across nodes (below), Subscribe recovery on the Redis
// broker (below) and the client-initiated Survey aggregated across nodes
// (below). All tests require a live Redis (requireClusterRedis) and Skip
// otherwise.

func testBoolPtr(v bool) *bool { return &v }

// TestPresence_OccupancyWildcardAcrossNodes proves B2 cross-node wildcard
// coverage over the Redis live bus: A subscribes the wildcard pattern im.** on
// nodeA, B joins the exact channel im.room.1 on nodeB. A receives the join as
// a first-class PresenceEvent{channel=im.room.1} (never as a publication) and
// B receives no self-join. A node that is not interested in the im tree (the
// second leg of this test) never receives the event. The local
// TestPresence_OccupancyWildcardCoverage test cannot cover this: cross-node
// fan-out needs the Redis broker pipe.
func TestPresence_OccupancyWildcardAcrossNodes(t *testing.T) {
	redisCfg := requireClusterRedis(t, clusterRedisIntegrationDB)
	ctx := context.Background()

	newNode := func() *runtime.Node {
		node := runtime.NewNode(nil)
		node.SetBroker(redisbroker.New(redisCfg))
		node.SetPresenceStore(redisbroker.NewPresenceStore(redisCfg))
		nodeCtx, cancel := context.WithCancel(ctx)
		t.Cleanup(func() { cancel(); node.Shutdown() })
		require.NoError(t, node.Run(nodeCtx))
		return node
	}
	nodeA := newNode()
	nodeB := newNode()

	const pattern = "im.**"
	const exact = "im.room.1"

	connectAndSubscribe := func(t *testing.T, node *runtime.Node, clientID, channel string) (*session.Client, *integrationCapturingTransport) {
		t.Helper()
		transport := &integrationCapturingTransport{}
		client, _, err := runtime.NewClient(ctx, node, transport, shared.JSONMarshaler{})
		require.NoError(t, err)
		require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
			Id:       "connect-" + clientID,
			Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{Version: "2.0.0", ClientId: clientID}},
		}))
		require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
			Id: "subscribe-" + clientID,
			Envelope: &clientpb.InboundMessage_Subscribe{
				Subscribe: &clientpb.Subscribe{Subscriptions: []*clientpb.Subscription{{Channel: channel}}},
			},
		}))
		return client, transport
	}

	// A subscribes the wildcard pattern on nodeA: patterns are never tracked
	// for presence, so subscribing emits nothing.
	_, transportA := connectAndSubscribe(t, nodeA, "client-a", pattern)
	transportA.clearMessages()

	// B joins the exact channel on nodeB: the join is published as an
	// occupancy event on the exact channel through the shared Redis broker.
	// A's compiled interest (im.* + im) must receive the exact-channel event.
	clientB, transportB := connectAndSubscribe(t, nodeB, "client-b", exact)

	require.Eventually(t, func() bool {
		events := integrationPresenceEventsOf(transportA)
		return len(events) == 1 && events[0].GetChannel() == exact && events[0].GetAction() == "join" &&
			events[0].GetInfo().GetSessionId() == clientB.SessionID()
	}, 5*time.Second, 25*time.Millisecond, "A's wildcard subscription must receive the exact-channel join")

	// No duplicate delivery may land after the expected single event.
	require.Never(t, func() bool {
		return len(integrationPresenceEventsOf(transportA)) != 1
	}, 300*time.Millisecond, 50*time.Millisecond)
	require.Empty(t, integrationPresenceEventsOf(transportB),
		"the joiner must not receive its own join")
	require.Zero(t, integrationPublicationCount(transportA),
		"occupancy frames must never become publications on the wildcard side")

	// A node with no interest in the im tree (only chat.1) receives nothing
	// for im.room.1: occupancy follows compiled interest (B2 §8.3). Anchor the
	// negative window behind the leave A (interested) actually receives, so
	// the assertion is meaningful rather than trivially passing before the
	// event crossed the bus.
	nodeC := newNode()
	_, uninterestedTransport := connectAndSubscribe(t, nodeC, "client-c", "chat.1")
	require.NoError(t, clientB.HandleMessage(ctx, &clientpb.InboundMessage{
		Id: "unsubscribe-b",
		Envelope: &clientpb.InboundMessage_Unsubscribe{
			Unsubscribe: &clientpb.Unsubscribe{Subscriptions: []*clientpb.Subscription{{Channel: exact}}},
		},
	}))
	require.Eventually(t, func() bool {
		events := integrationPresenceEventsOf(transportA)
		return len(events) == 2 && events[1].GetAction() == "leave"
	}, 5*time.Second, 25*time.Millisecond, "A must receive B's exact-channel leave")
	require.Never(t, func() bool {
		return len(integrationPresenceEventsOf(uninterestedTransport)) > 0
	}, 300*time.Millisecond, 50*time.Millisecond,
		"a node subscribed only to chat.1 must not receive im.room.1 occupancy")
}

// TestSubscribe_RecoverRedisHistory proves the PR-03 Subscribe recovery on
// the Redis broker: messages 1..3 are published to a channel, then a fresh
// connection subscribes with recover=true, offset=1 (the first message) and
// the shared broker epoch. The SubscribeAck must carry the subsequent
// messages (2 and 3) and report recovered=true with the last offset.
func TestSubscribe_RecoverRedisHistory(t *testing.T) {
	redisCfg := requireClusterRedis(t, clusterRedisIntegrationDB)
	ctx := context.Background()

	node := runtime.NewNode(nil)
	node.SetBroker(redisbroker.New(redisCfg))
	nodeCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(func() { cancel(); node.Shutdown() })
	require.NoError(t, node.Run(nodeCtx))

	channel := "recover." + uuid.NewString()
	first, err := node.Publish(channel, &stream.Publication{Payload: []byte("m1"), Kind: stream.PayloadKindText})
	require.NoError(t, err)
	require.NotZero(t, first, "redis history must assign a real offset")
	second, err := node.Publish(channel, &stream.Publication{Payload: []byte("m2"), Kind: stream.PayloadKindText})
	require.NoError(t, err)
	require.Greater(t, second, first)
	third, err := node.Publish(channel, &stream.Publication{Payload: []byte("m3"), Kind: stream.PayloadKindText})
	require.NoError(t, err)

	epocher, ok := node.Broker().(interface{ Epoch() string })
	require.True(t, ok, "the redis broker must expose the shared epoch")
	require.NotEmpty(t, epocher.Epoch())

	transport := &integrationCapturingTransport{}
	client, _, err := runtime.NewClient(ctx, node, transport, shared.JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-recover", "user-recover", "client-recover")
	require.NoError(t, node.AddClient(client))

	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id: "subscribe-recover",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{Subscriptions: []*clientpb.Subscription{{
				Channel: channel,
				Recover: true,
				Cursor:  &sharedpb.Position{StreamEpoch: epocher.Epoch(), Offset: &first},
			}}},
		},
	}))

	// The replay streams after the bare SubscribeAck: wait until the
	// per-channel RecoverComplete lands, then read the whole stream.
	require.Eventually(t, func() bool {
		return integrationRecoverCompleteFor(t, transport, channel) != nil
	}, 5*time.Second, 25*time.Millisecond)

	var replayPayloads []string
	for _, pub := range integrationReplaysOf(t, transport) {
		for _, m := range pub.GetMessages() {
			require.True(t, m.GetReplay(), "recovered messages must carry replay=true")
			replayPayloads = append(replayPayloads, m.GetPayload().GetText())
		}
	}
	require.Equal(t, []string{"m2", "m3"}, replayPayloads,
		"recovered publications must be the messages after the requested cursor")

	rc := integrationRecoverCompleteFor(t, transport, channel)
	require.NotNil(t, rc)
	require.False(t, rc.GetTruncated())
	require.False(t, rc.GetGap())
	require.Nil(t, rc.GetError())
	pos := rc.GetPosition()
	require.NotNil(t, pos)
	require.NotNil(t, pos.Offset, "the authoritative position must carry the last delivered offset")
	require.Equal(t, third, pos.GetOffset())
	require.Equal(t, epocher.Epoch(), pos.GetStreamEpoch())
}

// TestClientSurvey_AggregatesAcrossRedisNodes proves the PR-07 client
// Survey over the cluster: one subscriber on each of two nodes, channel
// policy survey on and ACL allow_survey open. A initiates with an inbound
// SurveyRequest; B answers the outbound SurveyRequest by its server
// generated request_id (respondToSurvey — never an inbound echo loopback);
// A asynchronously receives the aggregated SurveyResult containing B's
// answer.
func TestClientSurvey_AggregatesAcrossRedisNodes(t *testing.T) {
	redisCfg := requireClusterRedis(t, clusterRedisIntegrationDB)
	ctx := context.Background()

	serverCfg := &config.Server{
		RequireAuth: true,
		Authorizer: config.AuthorizerConfig{
			Default: config.ChannelPolicySpec{Survey: testBoolPtr(true)},
			Rules:   []config.AuthorizerRule{{Pattern: "csurvey.**", AllowSurvey: []string{"*"}}},
		},
	}
	nodeA := newClusterRedisTestNodeWithConfig(t, ctx, redisCfg, "node-a", serverCfg)
	nodeB := newClusterRedisTestNodeWithConfig(t, ctx, redisCfg, "node-b", serverCfg)

	channel := "csurvey." + uuid.NewString()

	transportA := &integrationCapturingTransport{}
	clientA, _, err := runtime.NewClient(ctx, nodeA, transportA, shared.JSONMarshaler{})
	require.NoError(t, err)
	clientA.ForceTestIDs("sess-csurvey-a", "user-csurvey-a", "client-a")
	require.NoError(t, nodeA.AddClient(clientA))
	require.NoError(t, nodeA.AddSubscription(ctx, channel, session.NewSubscriber(clientA, false)))

	transportB := &integrationCapturingTransport{}
	clientB, _, err := runtime.NewClient(ctx, nodeB, transportB, shared.JSONMarshaler{})
	require.NoError(t, err)
	clientB.ForceTestIDs("sess-csurvey-b", "user-csurvey-b", "client-b")
	require.NoError(t, nodeB.AddClient(clientB))
	require.NoError(t, nodeB.AddSubscription(ctx, channel, session.NewSubscriber(clientB, false)))

	transportA.clearMessages()
	transportB.clearMessages()

	requestID := "client-survey-" + uuid.NewString()
	require.NoError(t, clientA.HandleMessage(ctx, &clientpb.InboundMessage{
		Id: "survey-initiate",
		Envelope: &clientpb.InboundMessage_SurveyRequest{
			SurveyRequest: &clientpb.SurveyRequest{
				RequestId: requestID,
				Channel:   channel,
				Payload:   &sharedpb.Payload{Data: &sharedpb.Payload_Binary{Binary: []byte("ready?")}},
				TimeoutMs: 1500,
			},
		},
	}))

	// Both subscribers receive the outbound SurveyRequest (the remote one
	// via the ClusterCommandSurvey broadcast); replies carry the local
	// server-generated request id of each node's localSurvey. The initiator
	// answers immediately while its local survey is open: Node.Survey runs
	// localSurvey to completion before the cluster broadcast, so B's request
	// only arrives later (same ordering as the Admin cluster survey test).
	reqA := waitForSurveyRequestIntegration(t, transportA)
	require.NotEmpty(t, reqA.GetRequestId())
	respondToSurvey(t, ctx, clientA, transportA, []byte("reply-a"))

	reqB := waitForSurveyRequestIntegration(t, transportB)
	require.NotEmpty(t, reqB.GetRequestId())
	respondToSurvey(t, ctx, clientB, transportB, []byte("reply-b"))

	// The aggregated SurveyResult arrives asynchronously on the initiator.
	result := waitForSurveyResultIntegration(t, transportA)
	require.Equal(t, requestID, result.GetRequestId())
	require.Equal(t, channel, result.GetChannel())

	answersBySession := make(map[string]*clientpb.SurveyAnswer, len(result.GetAnswers()))
	for _, answer := range result.GetAnswers() {
		answersBySession[answer.GetSessionId()] = answer
	}
	require.Contains(t, answersBySession, clientB.SessionID(),
		"the remote node's answer must be aggregated into the result")
	require.Equal(t, []byte("reply-b"), answerPayloadOf(answersBySession[clientB.SessionID()]))
	require.Contains(t, answersBySession, clientA.SessionID(),
		"the initiator's own answer is included when it replies in time")
	require.Equal(t, []byte("reply-a"), answerPayloadOf(answersBySession[clientA.SessionID()]))
	// user_id metadata is only attached for sessions local to the node that
	// built the result (nodeA); the remote session's answer has no entry.
	require.Equal(t, "user-csurvey-a", answersBySession[clientA.SessionID()].GetMetadata().GetEntries()["user_id"])
	require.Equal(t, "", answersBySession[clientB.SessionID()].GetMetadata().GetEntries()["user_id"])
}

// waitForSurveyRequestIntegration decodes the outbound SurveyRequest the
// transport has received so far (the last message stays the SurveyRequest
// until the async SurveyResult lands).
func waitForSurveyRequestIntegration(t *testing.T, transport *integrationCapturingTransport) *clientpb.SurveyRequest {
	t.Helper()
	var req *clientpb.SurveyRequest
	require.Eventually(t, func() bool {
		msg := transport.getLastMessage()
		if len(msg) == 0 {
			return false
		}
		out := &clientpb.OutboundMessage{}
		if err := (shared.JSONMarshaler{}).Unmarshal(msg, out); err != nil {
			return false
		}
		req = out.GetSurveyRequest()
		return req != nil
	}, 10*time.Second, 25*time.Millisecond)
	return req
}

// waitForSurveyResultIntegration decodes the outbound SurveyResult envelope.
func waitForSurveyResultIntegration(t *testing.T, transport *integrationCapturingTransport) *clientpb.SurveyResult {
	t.Helper()
	var result *clientpb.SurveyResult
	require.Eventually(t, func() bool {
		transport.mu.Lock()
		messages := append([][]byte(nil), transport.messages...)
		transport.mu.Unlock()
		for _, data := range messages {
			var out clientpb.OutboundMessage
			if err := (shared.JSONMarshaler{}).Unmarshal(data, &out); err != nil {
				continue
			}
			if result = out.GetSurveyResult(); result != nil {
				return true
			}
		}
		return false
	}, 10*time.Second, 25*time.Millisecond)
	return result
}

func answerPayloadOf(answer *clientpb.SurveyAnswer) []byte {
	if answer.GetPayload() == nil {
		return nil
	}
	return answer.GetPayload().GetBinary()
}

// integrationReplaysOf decodes every captured frame and returns the replay
// Publications (every message carries replay=true) in write order.
func integrationReplaysOf(t *testing.T, transport *integrationCapturingTransport) []*clientpb.Publication {
	t.Helper()
	var replays []*clientpb.Publication
	transport.mu.Lock()
	frames := append([][]byte(nil), transport.messages...)
	transport.mu.Unlock()
	for _, data := range frames {
		out := &clientpb.OutboundMessage{}
		require.NoError(t, (shared.JSONMarshaler{}).Unmarshal(data, out))
		pub := out.GetPublication()
		if pub == nil {
			continue
		}
		replay := true
		for _, m := range pub.GetMessages() {
			if m == nil || !m.GetReplay() {
				replay = false
			}
		}
		if replay {
			replays = append(replays, pub)
		}
	}
	return replays
}

// integrationRecoverCompleteFor returns the RecoverComplete for channel, or
// nil when no captured frame carries one yet.
func integrationRecoverCompleteFor(t *testing.T, transport *integrationCapturingTransport, channel string) *clientpb.RecoverComplete {
	t.Helper()
	transport.mu.Lock()
	frames := append([][]byte(nil), transport.messages...)
	transport.mu.Unlock()
	for _, data := range frames {
		out := &clientpb.OutboundMessage{}
		require.NoError(t, (shared.JSONMarshaler{}).Unmarshal(data, out))
		if rc := out.GetRecoverComplete(); rc != nil && rc.GetChannel() == channel {
			return rc
		}
	}
	return nil
}
