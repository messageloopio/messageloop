package messageloop_test

import (
	"context"
	"errors"
	"os"

	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/config"
	"github.com/messageloopio/messageloop/pkg/grpcstream"
	"github.com/messageloopio/messageloop/pkg/redisbroker"
	"github.com/messageloopio/messageloop/proxy"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	serverv2 "github.com/messageloopio/messageloop/shared/genproto/server/v2"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

const clusterRedisIntegrationDB = 15

// testClusterHMACKey is the 32-byte HMAC key shared by the buses of the
// cluster integration tests.
var testClusterHMACKey = []byte("integration-test-hmac-key-0123456789")

// integrationAuthProxy authenticates any token as a fixed user.
type integrationAuthProxy struct {
	userID string
}

func (m *integrationAuthProxy) RPC(context.Context, *proxy.RPCProxyRequest) (*proxy.RPCProxyResponse, error) {
	return nil, nil
}

func (m *integrationAuthProxy) Authenticate(context.Context, *proxy.AuthenticateProxyRequest) (*proxy.AuthenticateProxyResponse, error) {
	return &proxy.AuthenticateProxyResponse{UserInfo: &proxy.UserInfo{ID: m.userID}}, nil
}

func (m *integrationAuthProxy) SubscribeAcl(context.Context, *proxy.SubscribeAclProxyRequest) (*proxy.SubscribeAclProxyResponse, error) {
	return &proxy.SubscribeAclProxyResponse{}, nil
}

func (m *integrationAuthProxy) PublishAcl(context.Context, *proxy.PublishAclProxyRequest) (*proxy.PublishAclProxyResponse, error) {
	return &proxy.PublishAclProxyResponse{}, nil
}

func (m *integrationAuthProxy) OnConnected(context.Context, *proxy.OnConnectedProxyRequest) (*proxy.OnConnectedProxyResponse, error) {
	return &proxy.OnConnectedProxyResponse{}, nil
}

func (m *integrationAuthProxy) OnSubscribed(context.Context, *proxy.OnSubscribedProxyRequest) (*proxy.OnSubscribedProxyResponse, error) {
	return &proxy.OnSubscribedProxyResponse{}, nil
}

func (m *integrationAuthProxy) OnUnsubscribed(context.Context, *proxy.OnUnsubscribedProxyRequest) (*proxy.OnUnsubscribedProxyResponse, error) {
	return &proxy.OnUnsubscribedProxyResponse{}, nil
}

func (m *integrationAuthProxy) OnDisconnected(context.Context, *proxy.OnDisconnectedProxyRequest) (*proxy.OnDisconnectedProxyResponse, error) {
	return &proxy.OnDisconnectedProxyResponse{}, nil
}

func (m *integrationAuthProxy) Name() string { return "integration-auth-stub" }
func (m *integrationAuthProxy) Close() error { return nil }

type integrationCapturingTransport struct {
	mu          sync.Mutex
	messages    [][]byte
	closeCount  atomic.Int32
	closed      atomic.Bool
	closeReason messageloop.Disconnect
}

func (c *integrationCapturingTransport) Write(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return errors.New("transport closed")
	}
	c.messages = append(c.messages, append([]byte(nil), data...))
	return nil
}

func (c *integrationCapturingTransport) WriteMany(data ...[]byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return errors.New("transport closed")
	}
	for _, item := range data {
		c.messages = append(c.messages, append([]byte(nil), item...))
	}
	return nil
}

func (c *integrationCapturingTransport) Close(disconnect messageloop.Disconnect) error {
	c.closed.Store(true)
	c.closeCount.Add(1)
	c.closeReason = disconnect
	return nil
}

func (c *integrationCapturingTransport) RemoteAddr() string {
	return "127.0.0.1:12345"
}

func (c *integrationCapturingTransport) isClosed() bool {
	return c.closed.Load()
}

func (c *integrationCapturingTransport) getCloseReason() messageloop.Disconnect {
	return c.closeReason
}

func (c *integrationCapturingTransport) getLastMessage() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.messages) == 0 {
		return nil
	}
	return c.messages[len(c.messages)-1]
}

func (c *integrationCapturingTransport) clearMessages() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = nil
}

// messagesSnapshot returns a deep copy of every captured frame.
func (c *integrationCapturingTransport) messagesSnapshot() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, 0, len(c.messages))
	for _, data := range c.messages {
		out = append(out, append([]byte(nil), data...))
	}
	return out
}

func TestClusterRedis_RemoteSessionAdminAndQueries(t *testing.T) {
	redisCfg := requireClusterRedis(t, clusterRedisIntegrationDB)
	ctx := context.Background()

	nodeA := newClusterRedisTestNode(t, ctx, redisCfg, "node-a")
	nodeB := newClusterRedisTestNode(t, ctx, redisCfg, "node-b")

	transport := &integrationCapturingTransport{}
	client, _, err := messageloop.NewClient(ctx, nodeA, transport, messageloop.JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-admin", "user-admin", "client-admin")
	require.NoError(t, nodeA.AddClient(client))

	channel := "cluster-admin-" + uuid.NewString()
	ok, err := nodeB.SubscribeSession(ctx, client.SessionID(), channel)
	require.NoError(t, err)
	require.True(t, ok)

	require.Eventually(t, func() bool {
		presence, err := nodeB.Presence(ctx, channel)
		if err != nil {
			return false
		}
		_, ok := presence[client.SessionID()]
		return ok
	}, 5*time.Second, 50*time.Millisecond)

	require.Eventually(t, func() bool {
		channels, err := nodeB.Channels(ctx)
		if err != nil {
			return false
		}
		for _, ch := range channels {
			if ch.Name == channel && ch.Subscribers == 1 {
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond)

	ok, err = nodeB.UnsubscribeSession(ctx, client.SessionID(), channel)
	require.NoError(t, err)
	require.True(t, ok)

	require.Eventually(t, func() bool {
		presence, err := nodeB.Presence(ctx, channel)
		if err != nil {
			return false
		}
		_, ok := presence[client.SessionID()]
		return !ok
	}, 5*time.Second, 50*time.Millisecond)

	require.Eventually(t, func() bool {
		channels, err := nodeB.Channels(ctx)
		if err != nil {
			return false
		}
		for _, ch := range channels {
			if ch.Name == channel {
				return false
			}
		}
		return true
	}, 5*time.Second, 50*time.Millisecond)

	ok, err = nodeB.DisconnectSession(ctx, client.SessionID(), messageloop.Disconnect{Code: 3009, Reason: "cluster-admin-test"})
	require.NoError(t, err)
	require.True(t, ok)

	require.Eventually(t, func() bool {
		return nodeA.Hub().LookupSession(client.SessionID()) == nil && transport.isClosed()
	}, 5*time.Second, 50*time.Millisecond)
	require.Equal(t, uint32(3009), transport.getCloseReason().Code)
}

func TestClusterRedis_RemoteResumeTakeover(t *testing.T) {
	redisCfg := requireClusterRedis(t, clusterRedisIntegrationDB)
	ctx := context.Background()

	nodeA := newClusterRedisTestNode(t, ctx, redisCfg, "node-a")
	nodeB := newClusterRedisTestNode(t, ctx, redisCfg, "node-b")
	authA := &integrationAuthProxy{userID: "user-old"}
	authB := &integrationAuthProxy{userID: "user-old"}
	require.NoError(t, nodeA.AddProxy(authA, "", messageloop.SystemMethodAuthenticate))
	require.NoError(t, nodeB.AddProxy(authB, "", messageloop.SystemMethodAuthenticate))

	oldTransport := &integrationCapturingTransport{}
	oldClient, _, err := messageloop.NewClient(ctx, nodeA, oldTransport, messageloop.JSONMarshaler{})
	require.NoError(t, err)

	connectMsg := &clientpb.InboundMessage{
		Id: "connect-old",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{Version: "2.0.0", ClientId: "client-old", Token: "token"},
		},
	}
	require.NoError(t, oldClient.HandleMessage(ctx, connectMsg))
	oldSessionID := oldClient.SessionID()

	channel := "cluster-resume-" + uuid.NewString()
	subscribeMsg := &clientpb.InboundMessage{
		Id: "subscribe-old",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{Subscriptions: []*clientpb.Subscription{{Channel: channel}}},
		},
	}
	require.NoError(t, oldClient.HandleMessage(ctx, subscribeMsg))

	newTransport := &integrationCapturingTransport{}
	newClient, _, err := messageloop.NewClient(ctx, nodeB, newTransport, messageloop.JSONMarshaler{})
	require.NoError(t, err)

	resumeMsg := &clientpb.InboundMessage{
		Id: "connect-new",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{Version: "2.0.0", ClientId: "client-new", Token: "token", SessionId: oldSessionID},
		},
	}
	require.NoError(t, newClient.HandleMessage(ctx, resumeMsg))

	require.Eventually(t, func() bool {
		return nodeA.Hub().LookupSession(oldSessionID) == nil
	}, 5*time.Second, 50*time.Millisecond)
	require.True(t, oldTransport.isClosed())
	require.Equal(t, oldSessionID, newClient.SessionID())

	require.Eventually(t, func() bool {
		presence, err := nodeB.Presence(ctx, channel)
		if err != nil {
			return false
		}
		_, ok := presence[oldSessionID]
		return ok
	}, 5*time.Second, 50*time.Millisecond)

	require.Eventually(t, func() bool {
		channels, err := nodeB.Channels(ctx)
		if err != nil {
			return false
		}
		for _, ch := range channels {
			if ch.Name == channel && ch.Subscribers == 1 {
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond)

	// The resumed session sends a bare Connected first; presence snapshots and
	// the per-channel recovery stream (RecoverComplete) land in later frames,
	// so the Connected envelope is located by scanning rather than assuming it
	// is the last frame.
	var connected *clientpb.Connected
	for _, data := range newTransport.messagesSnapshot() {
		var msg clientpb.OutboundMessage
		require.NoError(t, messageloop.JSONMarshaler{}.Unmarshal(data, &msg))
		if got := msg.GetConnected(); got != nil {
			connected = got
			break
		}
	}
	require.NotNil(t, connected, "the resume must send a Connected envelope")
	require.True(t, connected.Resumed)
	require.Equal(t, oldSessionID, connected.SessionId)
	channels := connected.Subscriptions
	require.Len(t, channels, 1)
	require.Equal(t, channel, channels[0].Channel)
}

func TestClusterRedis_ProjectionRepairRestoresChannels(t *testing.T) {
	redisCfg := requireClusterRedis(t, clusterRedisIntegrationDB)
	ctx := context.Background()

	node := newClusterRedisTestNode(t, ctx, redisCfg, "node-a")
	transport := &integrationCapturingTransport{}
	client, _, err := messageloop.NewClient(ctx, node, transport, messageloop.JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-repair", "user-repair", "client-repair")
	require.NoError(t, node.AddClient(client))

	channel := "cluster-repair-" + uuid.NewString()
	require.NoError(t, node.AddSubscription(ctx, channel, messageloop.NewSubscriber(client, false)))

	require.Eventually(t, func() bool {
		channels, err := node.Channels(ctx)
		if err != nil {
			return false
		}
		for _, ch := range channels {
			if ch.Name == channel && ch.Subscribers == 1 {
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond)

	redisClient := redis.NewClient(&redis.Options{Addr: redisCfg.Addr, Password: redisCfg.Password, DB: redisCfg.DB})
	t.Cleanup(func() { _ = redisClient.Close() })
	projectionKey := redisbroker.NewOptions(redisCfg).ClusterChannelPrefix + "owner:" + node.ClusterNodeID() + ":" + node.ClusterIncarnationID()
	require.NoError(t, redisClient.Del(ctx, projectionKey).Err())

	require.Eventually(t, func() bool {
		exists, err := redisClient.Exists(ctx, projectionKey).Result()
		if err != nil || exists == 0 {
			return false
		}
		channels, err := node.Channels(ctx)
		if err != nil {
			return false
		}
		for _, ch := range channels {
			if ch.Name == channel && ch.Subscribers == 1 {
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond)
}

func TestClusterRedis_SurveyAggregatesAcrossNodes(t *testing.T) {
	redisCfg := requireClusterRedis(t, clusterRedisIntegrationDB)
	ctx := context.Background()

	nodeA := newClusterRedisTestNode(t, ctx, redisCfg, "node-a")
	nodeB := newClusterRedisTestNode(t, ctx, redisCfg, "node-b")

	transportA := &integrationCapturingTransport{}
	clientA, _, err := messageloop.NewClient(ctx, nodeA, transportA, messageloop.JSONMarshaler{})
	require.NoError(t, err)
	clientA.ForceTestIDs("sess-survey-a", "user-survey-a", "client-survey-a")
	require.NoError(t, nodeA.AddClient(clientA))

	transportB := &integrationCapturingTransport{}
	clientB, _, err := messageloop.NewClient(ctx, nodeB, transportB, messageloop.JSONMarshaler{})
	require.NoError(t, err)
	clientB.ForceTestIDs("sess-survey-b", "user-survey-b", "client-survey-b")
	require.NoError(t, nodeB.AddClient(clientB))

	channel := "cluster-survey-" + uuid.NewString()
	require.NoError(t, nodeA.AddSubscription(ctx, channel, messageloop.NewSubscriber(clientA, false)))
	require.NoError(t, nodeB.AddSubscription(ctx, channel, messageloop.NewSubscriber(clientB, false)))
	transportA.clearMessages()
	transportB.clearMessages()

	var (
		surveyResults []*messageloop.SurveyResult
		surveyErr     error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		surveyResults, surveyErr = nodeA.Survey(ctx, channel, []byte("cluster survey"), 2*time.Second)
	}()

	respondToSurvey(t, ctx, clientA, transportA, []byte("reply-a"))
	respondToSurvey(t, ctx, clientB, transportB, []byte("reply-b"))

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cluster survey results")
	}

	require.NoError(t, surveyErr)
	require.Len(t, surveyResults, 2)
	resultsBySession := make(map[string]*messageloop.SurveyResult, len(surveyResults))
	for _, result := range surveyResults {
		resultsBySession[result.SessionID] = result
	}
	require.Contains(t, resultsBySession, "sess-survey-a")
	require.Equal(t, "node-a", resultsBySession["sess-survey-a"].NodeID)
	require.Equal(t, []byte("reply-a"), resultsBySession["sess-survey-a"].Payload)
	require.Contains(t, resultsBySession, "sess-survey-b")
	require.Equal(t, "node-b", resultsBySession["sess-survey-b"].NodeID)
	require.Equal(t, []byte("reply-b"), resultsBySession["sess-survey-b"].Payload)
}

func respondToSurvey(t *testing.T, ctx context.Context, client *messageloop.Client, transport *integrationCapturingTransport, payload []byte) {
	t.Helper()

	var surveyRequest *clientpb.SurveyRequest
	require.Eventually(t, func() bool {
		message := transport.getLastMessage()
		if len(message) == 0 {
			return false
		}
		outbound := &clientpb.OutboundMessage{}
		if err := (messageloop.JSONMarshaler{}).Unmarshal(message, outbound); err != nil {
			return false
		}
		surveyRequest = outbound.GetSurveyRequest()
		return surveyRequest != nil
	}, 5*time.Second, 25*time.Millisecond)

	// Reply to the survey using the outbound SurveyRequest.request_id; the
	// inbound SurveyRequest echo is gone since PR-07.
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id: "reply-" + client.SessionID(),
		Envelope: &clientpb.InboundMessage_SurveyReply{
			SurveyReply: &clientpb.SurveyReply{
				RequestId: surveyRequest.RequestId,
				Payload: &sharedpb.Payload{
					Data: &sharedpb.Payload_Binary{Binary: payload},
				},
			},
		},
	}))
}

func requireClusterRedis(t *testing.T, db int) config.RedisConfig {
	t.Helper()

	redisAddr := os.Getenv("MESSAGELOOP_TEST_REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "127.0.0.1:6379"
	}
	redisPassword := os.Getenv("MESSAGELOOP_TEST_REDIS_PASSWORD")
	if redisPassword == "" {
		redisPassword = os.Getenv("REDIS_PASSWORD")
	}

	redisCfg := config.RedisConfig{
		Addr:         redisAddr,
		Password:     redisPassword,
		DB:           db,
		DialTimeout:  "2s",
		ReadTimeout:  "2s",
		WriteTimeout: "2s",
	}
	client := redis.NewClient(&redis.Options{Addr: redisCfg.Addr, Password: redisCfg.Password, DB: redisCfg.DB})
	t.Cleanup(func() { _ = client.Close() })

	pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		t.Skipf("redis unavailable for cluster integration tests: %v", err)
	}
	require.NoError(t, client.FlushDB(pingCtx).Err())
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_ = client.FlushDB(cleanupCtx).Err()
	})
	return redisCfg
}

// newClusterRedisTestNodeWithConfig builds a Redis-backed cluster node with
// a caller-supplied server config (policy/ACL overrides for client survey).
func newClusterRedisTestNodeWithConfig(t *testing.T, parent context.Context, redisCfg config.RedisConfig, nodeID string, cfg *config.Server) *messageloop.Node {
	t.Helper()

	node := messageloop.NewNode(cfg)
	node.SetBroker(redisbroker.New(redisCfg))
	node.SetPresenceStore(redisbroker.NewPresenceStore(redisCfg))

	// The redis session directory allocates the node epoch (KD-K27); the
	// first NewCluster only exists to resolve the incarnation used to wire
	// the bus / lease manager below.
	clusterDeps := messageloop.ClusterDependencies{}
	clusterDeps.SessionDirectory = redisbroker.NewSessionDirectory(redisCfg)

	cluster, err := messageloop.NewCluster(messageloop.ClusterOptions{Enabled: true, NodeID: nodeID, Backend: "redis"}, messageloop.ClusterDependencies{
		SessionDirectory: clusterDeps.SessionDirectory,
	})
	require.NoError(t, err)

	clusterDeps.CommandBus = redisbroker.NewClusterCommandBus(redisCfg, cluster.NodeID(), cluster.IncarnationID(), testClusterHMACKey)
	clusterDeps.QueryStore = redisbroker.NewClusterQueryStore(redisCfg, cluster.NodeID(), cluster.IncarnationID())
	clusterDeps.NodeLeaseManager = messageloop.NewClusterNodeLeaseManager(clusterDeps.SessionDirectory, messageloop.ClusterNodeLeaseManagerConfig{
		NodeID:        cluster.NodeID(),
		IncarnationID: cluster.IncarnationID(),
	})
	clusterDeps.Repairer = messageloop.NewClusterRepairer(node, clusterDeps.SessionDirectory, clusterDeps.QueryStore, messageloop.ClusterRepairerConfig{Interval: 200 * time.Millisecond, MembershipInterval: 200 * time.Millisecond})
	clusterDeps.CommandBus.SetHandler(node.ClusterCommandHandler())

	cluster, err = messageloop.NewCluster(messageloop.ClusterOptions{
		Enabled:       true,
		NodeID:        cluster.NodeID(),
		Backend:       cluster.Backend(),
		IncarnationID: cluster.IncarnationID(),
	}, clusterDeps)
	require.NoError(t, err)
	node.SetCluster(cluster)

	ctx, cancel := context.WithCancel(parent)
	t.Cleanup(func() {
		cancel()
		node.Shutdown()
	})
	require.NoError(t, node.Run(ctx))
	return node
}

func newClusterRedisTestNode(t *testing.T, parent context.Context, redisCfg config.RedisConfig, nodeID string) *messageloop.Node {
	t.Helper()

	// Cluster test nodes require authentication: session takeover/resume is
	// only allowed for authenticated connects (see Task 9).
	return newClusterRedisTestNodeWithConfig(t, parent, redisCfg, nodeID, &config.Server{RequireAuth: true})
}

// Task 11b: Node.Run must not return before the Redis broker signals ready.
func TestClusterRedis_NodeRun_WaitsForBrokerReady(t *testing.T) {
	redisCfg := requireClusterRedis(t, clusterRedisIntegrationDB)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	node := messageloop.NewNode(nil)
	node.SetBroker(redisbroker.New(redisCfg))

	done := make(chan error, 1)
	go func() { done <- node.Run(ctx) }()

	ready, ok := node.Broker().(interface{ Ready() <-chan struct{} })
	require.True(t, ok, "redis broker must implement Ready")

	// Invariant: Run must never return before Ready closes.
	select {
	case <-ready.Ready():
	case err := <-done:
		select {
		case <-ready.Ready():
		default:
			t.Fatalf("Node.Run returned before broker ready: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("broker never became ready")
	}

	// Once ready, Run returns promptly.
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Node.Run did not return after broker ready")
	}
}

// integrationPresenceEventsOf decodes every presence_event envelope captured
// by the transport (publications and acks are ignored).
func integrationPresenceEventsOf(transport *integrationCapturingTransport) []*clientpb.PresenceEvent {
	transport.mu.Lock()
	messages := append([][]byte(nil), transport.messages...)
	transport.mu.Unlock()

	var events []*clientpb.PresenceEvent
	for _, data := range messages {
		var out clientpb.OutboundMessage
		if err := (messageloop.JSONMarshaler{}).Unmarshal(data, &out); err != nil {
			continue
		}
		if evt := out.GetPresenceEvent(); evt != nil {
			events = append(events, evt)
		}
	}
	return events
}

// integrationPublicationCount counts publication envelopes on the transport.
func integrationPublicationCount(transport *integrationCapturingTransport) int {
	transport.mu.Lock()
	messages := append([][]byte(nil), transport.messages...)
	transport.mu.Unlock()

	count := 0
	for _, data := range messages {
		var out clientpb.OutboundMessage
		if err := (messageloop.JSONMarshaler{}).Unmarshal(data, &out); err != nil {
			continue
		}
		if out.GetPublication() != nil {
			count++
		}
	}
	return count
}

// TestPresence_OccupancyAcrossRedisExactlyOne proves cross-node occupancy
// (B2) over the shared Redis broker with no double delivery: A (node 1) and
// C (node 2) are subscribed to the same exact channel, B joins on node 1. A
// and C each receive exactly one join for B; B receives no self-join. The
// cluster control plane is deliberately OFF — occupancy emit only needs the
// Redis pub/sub pipe.
func TestPresence_OccupancyAcrossRedisExactlyOne(t *testing.T) {
	redisCfg := requireClusterRedis(t, clusterRedisIntegrationDB)
	ctx := context.Background()

	newNode := func() *messageloop.Node {
		node := messageloop.NewNode(nil)
		node.SetBroker(redisbroker.New(redisCfg))
		node.SetPresenceStore(redisbroker.NewPresenceStore(redisCfg))
		nodeCtx, cancel := context.WithCancel(ctx)
		t.Cleanup(func() { cancel(); node.Shutdown() })
		require.NoError(t, node.Run(nodeCtx))
		return node
	}
	node1 := newNode()
	node2 := newNode()

	const ch = "emit.redis.ch"
	connectAndSubscribeIntegration := func(t *testing.T, node *messageloop.Node, clientID string) (*messageloop.Client, *integrationCapturingTransport) {
		t.Helper()
		transport := &integrationCapturingTransport{}
		client, _, err := messageloop.NewClient(ctx, node, transport, messageloop.JSONMarshaler{})
		require.NoError(t, err)
		require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
			Id:       "connect-" + clientID,
			Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{Version: "2.0.0", ClientId: clientID}},
		}))
		require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
			Id: "subscribe-" + clientID,
			Envelope: &clientpb.InboundMessage_Subscribe{
				Subscribe: &clientpb.Subscribe{Subscriptions: []*clientpb.Subscription{{Channel: ch}}},
			},
		}))
		return client, transport
	}

	_, transportA := connectAndSubscribeIntegration(t, node1, "client-a")
	_, transportC := connectAndSubscribeIntegration(t, node2, "client-c")

	// C's join is delivered to A asynchronously over Redis: wait for it, then
	// reset both transports so only B's join counts from here on.
	require.Eventually(t, func() bool {
		return len(integrationPresenceEventsOf(transportA)) == 1
	}, 5*time.Second, 25*time.Millisecond, "A must receive C's join")
	transportA.clearMessages()
	transportC.clearMessages()

	clientB, transportB := connectAndSubscribeIntegration(t, node1, "client-b")

	require.Eventually(t, func() bool {
		events := integrationPresenceEventsOf(transportA)
		return len(events) == 1 && events[0].GetInfo().GetSessionId() == clientB.SessionID()
	}, 5*time.Second, 25*time.Millisecond, "A must receive exactly one join for B")
	require.Eventually(t, func() bool {
		events := integrationPresenceEventsOf(transportC)
		return len(events) == 1 && events[0].GetInfo().GetSessionId() == clientB.SessionID()
	}, 5*time.Second, 25*time.Millisecond, "C must receive exactly one join for B")

	// No second delivery may land afterwards (a stacked/duplicated path
	// would append a second join to either transport).
	require.Never(t, func() bool {
		return len(integrationPresenceEventsOf(transportA)) != 1 || len(integrationPresenceEventsOf(transportC)) != 1
	}, 300*time.Millisecond, 50*time.Millisecond,
		"a stacked local+bus path would deliver a second join to A or C")
	require.Empty(t, integrationPresenceEventsOf(transportB),
		"the joiner must not receive its own join")
	require.Zero(t, integrationPublicationCount(transportA),
		"occupancy frames must never become publications")
}

// TestAdmin_DisconnectUsersAcrossNodes verifies PR-06 cross-node: user U has
// one session on nodeA and one on nodeB; an admin Disconnect with
// users=[U] resolves both through the Redis user index and disconnects both.
func TestAdmin_DisconnectUsersAcrossNodes(t *testing.T) {
	redisCfg := requireClusterRedis(t, clusterRedisIntegrationDB)
	ctx := context.Background()

	nodeA := newClusterRedisTestNode(t, ctx, redisCfg, "node-a")
	nodeB := newClusterRedisTestNode(t, ctx, redisCfg, "node-b")

	const userID = "cross-node-user"

	transportA := &integrationCapturingTransport{}
	clientA, _, err := messageloop.NewClient(ctx, nodeA, transportA, messageloop.JSONMarshaler{})
	require.NoError(t, err)
	clientA.ForceTestIDs("sess-user-a", userID, "client-a")
	require.NoError(t, nodeA.AddClient(clientA))

	transportB := &integrationCapturingTransport{}
	clientB, _, err := messageloop.NewClient(ctx, nodeB, transportB, messageloop.JSONMarshaler{})
	require.NoError(t, err)
	clientB.ForceTestIDs("sess-user-b", userID, "client-b")
	require.NoError(t, nodeB.AddClient(clientB))

	// Both sessions must be visible in the Redis user index (written by the
	// AddClient lease sync).
	directory := redisbroker.NewSessionDirectory(redisCfg)
	defer func() { _ = directory.Shutdown(ctx) }()
	require.Eventually(t, func() bool {
		ids, err := directory.ListUserSessions(ctx, userID)
		if err != nil {
			return false
		}
		return len(ids) == 2
	}, 5*time.Second, 50*time.Millisecond)

	// Admin Disconnect with users=[U] from nodeA: the remote session is
	// resolved via the index + lease, then routed through the command bus.
	handler := grpcstream.NewAPIServiceHandler(nodeA)
	resp, err := handler.Disconnect(ctx, &serverv2.DisconnectRequest{
		Users:  []string{userID},
		Code:   3009,
		Reason: "cross-node user disconnect",
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Len(t, resp.GetResults(), 2, "results must be keyed per session")
	require.True(t, resp.GetResults()[clientA.SessionID()], "nodeA session must be disconnected")
	require.True(t, resp.GetResults()[clientB.SessionID()], "nodeB session must be disconnected")
	require.Eventually(t, func() bool {
		return transportA.isClosed() && transportB.isClosed()
	}, 5*time.Second, 50*time.Millisecond)

	// The user index is cleaned up by the lease delete on close.
	require.Eventually(t, func() bool {
		ids, err := directory.ListUserSessions(ctx, userID)
		if err != nil {
			return false
		}
		return len(ids) == 0
	}, 5*time.Second, 50*time.Millisecond)
}

// TestClusterRedis_ResumeUserChangeMigratesIndex verifies PR-06 §9.7: after
// a remote resume where the re-authenticated user differs from the lease
// owner, the Redis user index migrates the membership (SREM old user, SADD
// new user) so expansion by the old user no longer hits the session.
func TestClusterRedis_ResumeUserChangeMigratesIndex(t *testing.T) {
	redisCfg := requireClusterRedis(t, clusterRedisIntegrationDB)
	ctx := context.Background()

	nodeA := newClusterRedisTestNode(t, ctx, redisCfg, "node-a")
	nodeB := newClusterRedisTestNode(t, ctx, redisCfg, "node-b")
	authOld := &integrationAuthProxy{userID: "user-old"}
	authNew := &integrationAuthProxy{userID: "user-new"}
	require.NoError(t, nodeA.AddProxy(authOld, "", messageloop.SystemMethodAuthenticate))
	require.NoError(t, nodeB.AddProxy(authNew, "", messageloop.SystemMethodAuthenticate))

	oldTransport := &integrationCapturingTransport{}
	oldClient, _, err := messageloop.NewClient(ctx, nodeA, oldTransport, messageloop.JSONMarshaler{})
	require.NoError(t, err)
	connectMsg := &clientpb.InboundMessage{
		Id: "connect-old",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{Version: "2.0.0", ClientId: "client-old", Token: "token"},
		},
	}
	require.NoError(t, oldClient.HandleMessage(ctx, connectMsg))
	oldSessionID := oldClient.SessionID()
	require.NotEmpty(t, oldSessionID)
	require.Equal(t, "user-old", oldClient.UserID())

	directory := redisbroker.NewSessionDirectory(redisCfg)
	defer func() { _ = directory.Shutdown(ctx) }()
	require.Eventually(t, func() bool {
		ids, err := directory.ListUserSessions(ctx, "user-old")
		return err == nil && len(ids) == 1
	}, 5*time.Second, 50*time.Millisecond)

	// Resume on nodeB with a different authenticated user: authUser wins over
	// the inherited lease user, and the next lease write must migrate the
	// index membership.
	newTransport := &integrationCapturingTransport{}
	newClient, _, err := messageloop.NewClient(ctx, nodeB, newTransport, messageloop.JSONMarshaler{})
	require.NoError(t, err)
	resumeMsg := &clientpb.InboundMessage{
		Id: "connect-new",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{Version: "2.0.0", ClientId: "client-new", Token: "token", SessionId: oldSessionID},
		},
	}
	require.NoError(t, newClient.HandleMessage(ctx, resumeMsg))
	require.Equal(t, "user-new", newClient.UserID())

	require.Eventually(t, func() bool {
		ids, err := directory.ListUserSessions(ctx, "user-old")
		if err != nil || len(ids) != 0 {
			return false
		}
		ids, err = directory.ListUserSessions(ctx, "user-new")
		return err == nil && len(ids) == 1
	}, 5*time.Second, 50*time.Millisecond)
}
