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
	"github.com/messageloopio/messageloop/pkg/redisbroker"
	"github.com/messageloopio/messageloop/proxy"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

const clusterRedisIntegrationDB = 15

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
			Connect: &clientpb.Connect{ClientId: "client-old", Token: "token"},
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
			Connect: &clientpb.Connect{ClientId: "client-new", Token: "token", SessionId: oldSessionID},
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

	connected := &clientpb.OutboundMessage{}
	require.NoError(t, messageloop.JSONMarshaler{}.Unmarshal(newTransport.getLastMessage(), connected))
	require.NotNil(t, connected.GetConnected())
	require.True(t, connected.GetConnected().Resumed)
	require.Equal(t, oldSessionID, connected.GetConnected().SessionId)
	channels := connected.GetConnected().Subscriptions
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

	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id: surveyRequest.RequestId,
		Envelope: &clientpb.InboundMessage_SurveyRequest{
			SurveyRequest: surveyRequest,
		},
	}))
	transport.clearMessages()
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id: "reply-" + client.SessionID(),
		Envelope: &clientpb.InboundMessage_SurveyReply{
			SurveyReply: &clientpb.SurveyReply{
				RequestId: client.LastSurveyRequestID(),
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

func newClusterRedisTestNode(t *testing.T, parent context.Context, redisCfg config.RedisConfig, nodeID string) *messageloop.Node {
	t.Helper()

	// Cluster test nodes require authentication: session takeover/resume is
	// only allowed for authenticated connects (see Task 9).
	node := messageloop.NewNode(&config.Server{RequireAuth: true})
	node.SetBroker(redisbroker.New(redisCfg))
	node.SetPresenceStore(redisbroker.NewPresenceStore(redisCfg))

	cluster, err := messageloop.NewCluster(messageloop.ClusterOptions{Enabled: true, NodeID: nodeID, Backend: "redis"}, messageloop.ClusterDependencies{})
	require.NoError(t, err)

	clusterDeps := messageloop.ClusterDependencies{}
	clusterDeps.SessionDirectory = redisbroker.NewSessionDirectory(redisCfg)
	clusterDeps.CommandBus = redisbroker.NewClusterCommandBus(redisCfg, cluster.NodeID(), cluster.IncarnationID())
	clusterDeps.QueryStore = redisbroker.NewClusterQueryStore(redisCfg, cluster.NodeID(), cluster.IncarnationID())
	clusterDeps.NodeLeaseManager = messageloop.NewClusterNodeLeaseManager(clusterDeps.SessionDirectory, messageloop.ClusterNodeLeaseManagerConfig{
		NodeID:        cluster.NodeID(),
		IncarnationID: cluster.IncarnationID(),
	})
	clusterDeps.ProjectionRepairer = messageloop.NewClusterProjectionRepairer(node, clusterDeps.QueryStore, messageloop.ClusterProjectionRepairerConfig{Interval: 200 * time.Millisecond})
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
