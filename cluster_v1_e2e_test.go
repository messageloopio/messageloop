package messageloop_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/config"
	"github.com/messageloopio/messageloop/pkg/redisbroker"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	"github.com/stretchr/testify/require"
)

// cluster_v1_e2e_test.go covers the four PR-10 cluster paths: admin
// disconnect by user (existing TestAdmin_DisconnectUsersAcrossNodes),
// wildcard x presence across nodes (below), Subscribe recovery on the Redis
// broker (below) and the client-initiated Survey aggregated across nodes
// (below). All tests require a live Redis (requireClusterRedis) and Skip
// otherwise.

func testBoolPtr(v bool) *bool { return &v }

// TestPresence_ClusterEmitWildcardAcrossNodes closes the PR-04b wildcard
// gap: A subscribes the wildcard pattern im.** on nodeA, B joins the exact
// channel im.room.1 on nodeB, cluster_emit=true. A receives the join as a
// first-class PresenceEvent{channel=im.room.1} (never as a publication) and
// B receives no self-join. The local TestPresence_WildcardSubscriberReceives
// exact-join test cannot cover this: cross-node fan-out needs the Redis
// broker pipe.
func TestPresence_ClusterEmitWildcardAcrossNodes(t *testing.T) {
	redisCfg := requireClusterRedis(t, clusterRedisIntegrationDB)
	ctx := context.Background()

	newNode := func() *messageloop.Node {
		node := messageloop.NewNode(&config.Server{Presence: config.Presence{ClusterEmit: true}})
		node.SetBroker(redisbroker.New(redisCfg))
		nodeCtx, cancel := context.WithCancel(ctx)
		t.Cleanup(func() { cancel(); node.Shutdown() })
		require.NoError(t, node.Run(nodeCtx))
		return node
	}
	nodeA := newNode()
	nodeB := newNode()

	const pattern = "im.**"
	const exact = "im.room.1"

	connectAndSubscribe := func(t *testing.T, node *messageloop.Node, clientID, channel string) (*messageloop.Client, *integrationCapturingTransport) {
		t.Helper()
		transport := &integrationCapturingTransport{}
		client, _, err := messageloop.NewClient(ctx, node, transport, messageloop.JSONMarshaler{})
		require.NoError(t, err)
		require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
			Id:       "connect-" + clientID,
			Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: clientID}},
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

	// B joins the exact channel on nodeB. With cluster_emit=true the join is
	// published through the shared Redis broker; A's wildcard subscription
	// must receive the exact-channel event.
	clientB, transportB := connectAndSubscribe(t, nodeB, "client-b", exact)

	require.Eventually(t, func() bool {
		events := integrationPresenceEventsOf(transportA)
		return len(events) == 1 && events[0].GetChannel() == exact && events[0].GetAction() == "join" &&
			events[0].GetInfo().GetSessionId() == clientB.SessionID()
	}, 5*time.Second, 25*time.Millisecond, "A's wildcard subscription must receive the exact-channel join")

	// Give any (wrong) duplicate delivery a chance to land, then pin counts.
	time.Sleep(300 * time.Millisecond)
	events := integrationPresenceEventsOf(transportA)
	require.Len(t, events, 1, "A must receive exactly one join")
	require.Zero(t, integrationPublicationCount(transportA),
		"presence frames must never become publications on the wildcard side")
	require.Empty(t, integrationPresenceEventsOf(transportB),
		"the joiner must not receive its own join")
}

// TestSubscribe_RecoverRedisHistory proves the PR-03 Subscribe recovery on
// the Redis broker: messages 1..3 are published to a channel, then a fresh
// connection subscribes with recover=true, offset=1 (the first message) and
// the shared broker epoch. The SubscribeAck must carry the subsequent
// messages (2 and 3) and report recovered=true with the last offset.
func TestSubscribe_RecoverRedisHistory(t *testing.T) {
	redisCfg := requireClusterRedis(t, clusterRedisIntegrationDB)
	ctx := context.Background()

	node := messageloop.NewNode(nil)
	node.SetBroker(redisbroker.New(redisCfg))
	nodeCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(func() { cancel(); node.Shutdown() })
	require.NoError(t, node.Run(nodeCtx))

	channel := "recover." + uuid.NewString()
	first, err := node.Publish(channel, &messageloop.Publication{Payload: []byte("m1"), Kind: messageloop.PayloadKindText})
	require.NoError(t, err)
	require.NotZero(t, first, "redis history must assign a real offset")
	second, err := node.Publish(channel, &messageloop.Publication{Payload: []byte("m2"), Kind: messageloop.PayloadKindText})
	require.NoError(t, err)
	require.Greater(t, second, first)
	third, err := node.Publish(channel, &messageloop.Publication{Payload: []byte("m3"), Kind: messageloop.PayloadKindText})
	require.NoError(t, err)

	epocher, ok := node.Broker().(interface{ Epoch() string })
	require.True(t, ok, "the redis broker must expose the shared epoch")
	require.NotEmpty(t, epocher.Epoch())

	transport := &integrationCapturingTransport{}
	client, _, err := messageloop.NewClient(ctx, node, transport, messageloop.JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-recover", "user-recover", "client-recover")
	require.NoError(t, node.AddClient(client))

	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id: "subscribe-recover",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{Subscriptions: []*clientpb.Subscription{{
				Channel: channel,
				Recover: true,
				Offset:  first,
				Epoch:   epocher.Epoch(),
			}}},
		},
	}))

	var ack *clientpb.SubscribeAck
	require.Eventually(t, func() bool {
		msg := transport.getLastMessage()
		if len(msg) == 0 {
			return false
		}
		out := &clientpb.OutboundMessage{}
		if err := (messageloop.JSONMarshaler{}).Unmarshal(msg, out); err != nil {
			return false
		}
		ack = out.GetSubscribeAck()
		return ack != nil
	}, 5*time.Second, 25*time.Millisecond)

	var payloads []string
	for _, pub := range ack.GetPublications() {
		for _, m := range pub.GetMessages() {
			payloads = append(payloads, m.GetPayload().GetText())
		}
	}
	require.Equal(t, []string{"m2", "m3"}, payloads,
		"recovered publications must be the messages after the requested offset")

	require.Len(t, ack.GetRecoverResults(), 1)
	res := ack.GetRecoverResults()[0]
	require.True(t, res.GetRecovered())
	require.False(t, res.GetTruncated())
	require.Equal(t, third, res.GetOffset(), "offset must be the last recovered publication")
	require.Equal(t, epocher.Epoch(), res.GetEpoch())
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
		Channels:    config.ChannelConfig{Default: config.ChannelPolicySpec{Survey: testBoolPtr(true)}},
		ACL:         config.ACLConfig{Rules: []config.ACLRule{{ChannelPattern: "csurvey.**", AllowSurvey: []string{"*"}}}},
	}
	nodeA := newClusterRedisTestNodeWithConfig(t, ctx, redisCfg, "node-a", serverCfg)
	nodeB := newClusterRedisTestNodeWithConfig(t, ctx, redisCfg, "node-b", serverCfg)

	channel := "csurvey." + uuid.NewString()

	transportA := &integrationCapturingTransport{}
	clientA, _, err := messageloop.NewClient(ctx, nodeA, transportA, messageloop.JSONMarshaler{})
	require.NoError(t, err)
	clientA.ForceTestIDs("sess-csurvey-a", "user-csurvey-a", "client-a")
	require.NoError(t, nodeA.AddClient(clientA))
	require.NoError(t, nodeA.AddSubscription(ctx, channel, messageloop.NewSubscriber(clientA, false)))

	transportB := &integrationCapturingTransport{}
	clientB, _, err := messageloop.NewClient(ctx, nodeB, transportB, messageloop.JSONMarshaler{})
	require.NoError(t, err)
	clientB.ForceTestIDs("sess-csurvey-b", "user-csurvey-b", "client-b")
	require.NoError(t, nodeB.AddClient(clientB))
	require.NoError(t, nodeB.AddSubscription(ctx, channel, messageloop.NewSubscriber(clientB, false)))

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
		if err := (messageloop.JSONMarshaler{}).Unmarshal(msg, out); err != nil {
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
			if err := (messageloop.JSONMarshaler{}).Unmarshal(data, &out); err != nil {
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
