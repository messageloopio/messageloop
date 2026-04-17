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
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

const clusterRedisIntegrationDB = 15

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

	oldTransport := &integrationCapturingTransport{}
	oldClient, _, err := messageloop.NewClient(ctx, nodeA, oldTransport, messageloop.JSONMarshaler{})
	require.NoError(t, err)

	connectMsg := &clientpb.InboundMessage{
		Id: "connect-old",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{ClientId: "client-old"},
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
			Connect: &clientpb.Connect{ClientId: "client-new", SessionId: oldSessionID},
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

	node := messageloop.NewNode(nil)
	node.SetBroker(redisbroker.New(redisCfg))
	node.SetPresenceStore(redisbroker.NewPresenceStore(redisCfg))

	clusterRuntime, err := messageloop.NewClusterRuntime(messageloop.ClusterOptions{Enabled: true, NodeID: nodeID, Backend: "redis"}, messageloop.ClusterDependencies{})
	require.NoError(t, err)

	clusterDeps := messageloop.ClusterDependencies{}
	clusterDeps.SessionDirectory = redisbroker.NewSessionDirectory(redisCfg)
	clusterDeps.CommandBus = redisbroker.NewClusterCommandBus(redisCfg, clusterRuntime.NodeID(), clusterRuntime.IncarnationID())
	clusterDeps.QueryStore = redisbroker.NewClusterQueryStore(redisCfg)
	clusterDeps.NodeLeaseManager = messageloop.NewClusterNodeLeaseManager(clusterDeps.SessionDirectory, messageloop.ClusterNodeLeaseManagerConfig{
		NodeID:        clusterRuntime.NodeID(),
		IncarnationID: clusterRuntime.IncarnationID(),
	})
	clusterDeps.CommandBus.SetHandler(node.ClusterCommandHandler())

	clusterRuntime, err = messageloop.NewClusterRuntime(messageloop.ClusterOptions{
		Enabled:       true,
		NodeID:        clusterRuntime.NodeID(),
		Backend:       clusterRuntime.Backend(),
		IncarnationID: clusterRuntime.IncarnationID(),
	}, clusterDeps)
	require.NoError(t, err)
	node.SetClusterRuntime(clusterRuntime)

	ctx, cancel := context.WithCancel(parent)
	t.Cleanup(func() {
		cancel()
		node.Shutdown()
	})
	require.NoError(t, node.Run(ctx))
	return node
}
