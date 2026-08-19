package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/messageloopio/messageloop/config"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

// newHeartbeatNode builds a node with the given heartbeat config plus a fresh
// metrics registry, so heartbeat tests can observe 3511 disconnects.
func newHeartbeatNode(t *testing.T, hb config.Heartbeat) *Node {
	t.Helper()
	node := NewNode(&config.Server{Heartbeat: hb})
	node.SetMetrics(NewMetrics(prometheus.NewRegistry()))
	return node
}

// newHeartbeatClient wires a test client with a capturing transport and
// ensures the client is closed (heartbeat goroutine stopped) at test end.
func newHeartbeatClient(t *testing.T, node *Node) (*Client, *capturingTransport) {
	t.Helper()
	transport := &capturingTransport{}
	client, _, err := NewClient(context.Background(), node, transport, JSONMarshaler{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close(Disconnect{}) })
	return client, transport
}

// disableHeartbeatJitter pins the node's ping scheduling to the exact
// interval (0.8~1.2 jitter off) for timing-sensitive tests. Must be called
// after the node exists and before any client is created on it.
func disableHeartbeatJitter(t *testing.T, node *Node) {
	t.Helper()
	node.heartbeatManager.SetJitterForTest(func(d time.Duration) time.Duration { return d })
}

// waitForOutboundPing blocks until the transport has captured at least one
// server-initiated Ping frame and returns its capture time.
func waitForOutboundPing(t *testing.T, transport *capturingTransport) time.Time {
	t.Helper()
	var pingSeen time.Time
	require.Eventually(t, func() bool {
		for _, data := range transport.snapshotMessages() {
			out := &clientpb.OutboundMessage{}
			if err := ProtoJSONMarshaler.Unmarshal(data, out); err == nil {
				if _, ok := out.Envelope.(*clientpb.OutboundMessage_Ping); ok {
					pingSeen = time.Now()
					return true
				}
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond)
	return pingSeen
}

// outboundPingCount returns how many Ping frames the transport captured.
func outboundPingCount(transport *capturingTransport) int {
	count := 0
	for _, data := range transport.snapshotMessages() {
		out := &clientpb.OutboundMessage{}
		if err := ProtoJSONMarshaler.Unmarshal(data, out); err == nil {
			if _, ok := out.Envelope.(*clientpb.OutboundMessage_Ping); ok {
				count++
			}
		}
	}
	return count
}

// countingPresenceStore counts every presence Add so tests can observe how
// often handlePong's throttled refresh touches the presence store.
type countingPresenceStore struct {
	PresenceStore
	mu   sync.Mutex
	adds int
}

func (c *countingPresenceStore) Add(ctx context.Context, ch string, info *PresenceInfo) error {
	c.mu.Lock()
	c.adds++
	c.mu.Unlock()
	return c.PresenceStore.Add(ctx, ch, info)
}

func (c *countingPresenceStore) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.adds
}

// TestHeartbeat_IdleTimeoutDisconnects: idle=5s, client sends nothing; the
// idle ticker disconnects with 3511 after ~5s and counts the metric.
func TestHeartbeat_IdleTimeoutDisconnects(t *testing.T) {
	node := newHeartbeatNode(t, config.Heartbeat{IdleTimeout: "5s"})
	_, transport := newHeartbeatClient(t, node)

	start := time.Now()
	require.Eventually(t, transport.isClosed, 10*time.Second, 100*time.Millisecond)
	elapsed := time.Since(start)

	assert.GreaterOrEqual(t, elapsed, 4*time.Second, "idle disconnect must not fire early")
	assert.Less(t, elapsed, 8*time.Second, "idle disconnect must fire at the idle timeout")
	assert.Equal(t, uint32(3511), transport.getCloseReason().Code)
	assert.Equal(t, float64(1), testutil.ToFloat64(node.metrics.HeartbeatIdleDisconnects))
}

// TestHeartbeat_PingTimeoutFiresBeforeIdle: ping_interval=2s, ping_timeout=1s,
// idle=5s. The client swallows outbound pings, so the first ping's deadline
// disconnects ~1s after the ping — well before the idle=5s check.
func TestHeartbeat_PingTimeoutFiresBeforeIdle(t *testing.T) {
	node := newHeartbeatNode(t, config.Heartbeat{IdleTimeout: "5s", PingInterval: "2s", PingTimeout: "1s"})
	disableHeartbeatJitter(t, node)
	_, transport := newHeartbeatClient(t, node)

	start := time.Now()
	require.Eventually(t, transport.isClosed, 8*time.Second, 100*time.Millisecond)
	elapsed := time.Since(start)

	// First ping at 2s, deadline at 3s: disconnect must land after the ping
	// timeout (>= 2.5s) and strictly before the idle check at 5s.
	assert.GreaterOrEqual(t, elapsed, 2500*time.Millisecond)
	assert.Less(t, elapsed, 4500*time.Millisecond)
	assert.Equal(t, uint32(3511), transport.getCloseReason().Code)
	assert.Equal(t, float64(1), testutil.ToFloat64(node.metrics.HeartbeatIdleDisconnects))
}

// TestHeartbeat_PingDeadlineNotWaitNextTick: with the same config, the
// disconnect must come ~1s after the ping (the armed deadline), not at the
// next 2s tick (which would be >= 2s after the ping).
func TestHeartbeat_PingDeadlineNotWaitNextTick(t *testing.T) {
	node := newHeartbeatNode(t, config.Heartbeat{IdleTimeout: "10s", PingInterval: "2s", PingTimeout: "1s"})
	disableHeartbeatJitter(t, node)
	_, transport := newHeartbeatClient(t, node)

	pingAt := waitForOutboundPing(t, transport)
	require.Eventually(t, transport.isClosed, 5*time.Second, 20*time.Millisecond)
	elapsed := time.Since(pingAt)

	assert.GreaterOrEqual(t, elapsed, 900*time.Millisecond, "disconnect must wait at least the ping timeout")
	assert.Less(t, elapsed, 1900*time.Millisecond,
		"disconnect must fire on the ping deadline, not wait for the next 2s tick")
}

// TestHeartbeat_DefaultNoServerPing: the default config never sends an
// outbound Ping and does not disconnect (TestNewNode_HeartbeatDefaultIdleTimeout
// keeps covering the idle default).
func TestHeartbeat_DefaultNoServerPing(t *testing.T) {
	node := newHeartbeatNode(t, config.Heartbeat{})
	cfg := node.GetHeartbeatConfig()
	assert.Equal(t, DefaultHeartbeatIdleTimeout, cfg.IdleTimeout)
	assert.Zero(t, cfg.PingInterval)
	assert.Zero(t, cfg.PingTimeout)

	_, transport := newHeartbeatClient(t, node)
	time.Sleep(1200 * time.Millisecond)

	assert.False(t, transport.isClosed(), "default heartbeat must not disconnect")
	assert.Zero(t, outboundPingCount(transport), "default config must never send a server ping")
}

// TestHeartbeat_IdleAndPingDisabledKeepsConnection: idle=0s + ping_interval=0s
// starts no heartbeat goroutine at all and keeps a silent connection open.
func TestHeartbeat_IdleAndPingDisabledKeepsConnection(t *testing.T) {
	node := newHeartbeatNode(t, config.Heartbeat{IdleTimeout: "0s", PingInterval: "0s"})
	assert.Zero(t, node.GetHeartbeatIdleTimeout())
	cfg := node.GetHeartbeatConfig()
	assert.Zero(t, cfg.IdleTimeout)
	assert.Zero(t, cfg.PingInterval)

	_, transport := newHeartbeatClient(t, node)
	time.Sleep(2 * time.Second)

	assert.False(t, transport.isClosed(), "disabled heartbeat must never disconnect")
	assert.Zero(t, outboundPingCount(transport))
}

// TestHeartbeat_PongRefreshesPresenceAndLease: with ping_interval>0 a client
// that only answers with Pongs must trigger the same throttled presence /
// cluster refresh as handlePing (first Pong refreshes, second within the 10s
// window does not).
func TestHeartbeat_PongRefreshesPresenceAndLease(t *testing.T) {
	ctx := context.Background()
	directory := &countingSessionDirectory{fakeSessionDirectory: &fakeSessionDirectory{}}
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       &fakeClusterCommandBus{},
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := newHeartbeatNode(t, config.Heartbeat{IdleTimeout: "0s", PingInterval: "1s", PingTimeout: "1s"})
	node.SetCluster(runtime)
	presence := &countingPresenceStore{PresenceStore: node.presence}
	node.presence = presence

	client, _ := newHeartbeatClient(t, node)
	client.ForceTestIDs("sess-pong", "user-pong", "client-pong")
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id: "sub-1",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{
				Subscriptions: []*clientpb.Subscription{{Channel: "hb.pong"}},
			},
		},
	}))
	// Baseline after connect + subscribe: each cluster sync writes a lease
	// and a snapshot, and subscribe registered presence.
	baseline := directory.count()
	presenceBaseline := presence.count()
	require.Greater(t, presenceBaseline, 0, "subscribe must register presence")

	pong := func(id string) {
		require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
			Id:       id,
			Envelope: &clientpb.InboundMessage_Pong{Pong: &clientpb.Pong{}},
		}))
	}

	// First Pong: must refresh presence AND cluster state exactly once.
	pong("pong-1")
	require.Eventually(t, func() bool {
		return directory.count() >= baseline+2 && presence.count() > presenceBaseline
	}, 2*time.Second, 20*time.Millisecond)
	assert.Equal(t, baseline+2, directory.count(), "one Pong must trigger exactly one cluster refresh")
	afterFirst := directory.count()
	presenceAfterFirst := presence.count()

	// A second Pong inside the throttle window must not refresh again.
	pong("pong-2")
	time.Sleep(400 * time.Millisecond)
	assert.Equal(t, afterFirst, directory.count(), "second Pong within the interval must not refresh")
	assert.Equal(t, presenceAfterFirst, presence.count())
}

// TestHeartbeat_AnyInboundCancelsPingDeadline: a business frame (not a Pong)
// after a server ping cancels the pending deadline, so the connection is not
// disconnected.
func TestHeartbeat_AnyInboundCancelsPingDeadline(t *testing.T) {
	ctx := context.Background()
	node := newHeartbeatNode(t, config.Heartbeat{IdleTimeout: "5s", PingInterval: "2s", PingTimeout: "1s"})
	disableHeartbeatJitter(t, node)
	client, transport := newHeartbeatClient(t, node)
	client.ForceTestIDs("sess-biz", "user-biz", "client-biz")

	// Wait for the first outbound ping; its deadline would fire 1s later.
	waitForOutboundPing(t, transport)

	publish := func() {
		require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
			Id: "publish-1",
			Envelope: &clientpb.InboundMessage_Publish{
				Publish: &clientpb.Publish{
					Channel: "hb.biz",
					Payload: &sharedpb.Payload{
						Data: &sharedpb.Payload_Json{Json: &structpb.Struct{Fields: map[string]*structpb.Value{}}},
					},
				},
			},
		}))
	}

	// Send business traffic continuously past what would have been the
	// deadline (~1s after the ping): every inbound frame must disarm it.
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		publish()
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)

	assert.False(t, transport.isClosed(),
		"inbound business traffic must cancel the ping deadline; connection must stay open")
	assert.Zero(t, testutil.ToFloat64(node.metrics.HeartbeatIdleDisconnects))
}

// TestHeartbeat_SessionLeaseTTLFormula pins the lease formula: default config
// -> 600s, short heartbeat -> short lease, disabled heartbeat -> 600s.
func TestHeartbeat_SessionLeaseTTLFormula(t *testing.T) {
	node := NewNode(nil)
	assert.Equal(t, 600*time.Second, node.sessionLeaseTTL(), "default config must keep the 600s lease")

	short := NewNode(&config.Server{Heartbeat: config.Heartbeat{IdleTimeout: "15s", PingInterval: "5s", PingTimeout: "3s"}})
	assert.Equal(t, 35*time.Second, short.sessionLeaseTTL(), "max(30s, 2*15s, 3*5s, 15s+10s+10s)=35s")

	disabled := NewNode(&config.Server{Heartbeat: config.Heartbeat{IdleTimeout: "0s", PingInterval: "0s"}})
	assert.Equal(t, 600*time.Second, disabled.sessionLeaseTTL(), "disabled heartbeat must not shorten the lease")

	pingOnly := NewNode(&config.Server{Heartbeat: config.Heartbeat{IdleTimeout: "0s", PingInterval: "5s", PingTimeout: "3s"}})
	assert.Equal(t, 30*time.Second, pingOnly.sessionLeaseTTL(), "ping-only must still floor at 30s")
}

// TestHeartbeat_ValidateRejectsSubSecond: non-zero idle / ping_interval /
// ping_timeout below 1s must fail Validate; explicit ping_timeout=0s with an
// enabled ping_interval must fail too.
func TestHeartbeat_ValidateRejectsSubSecond(t *testing.T) {
	base := config.Config{
		Transport: config.Transport{
			WebSocket: config.WebSocketTransport{Addr: ":9080", Path: "/ws"},
			GRPC:      config.GRPCTransport{Addr: ":9090"},
		},
	}

	cases := []struct {
		name    string
		hb      config.Heartbeat
		wantErr string
	}{
		{"idle below 1s", config.Heartbeat{IdleTimeout: "500ms"}, "idle_timeout must be at least 1s"},
		{"ping_interval below 1s", config.Heartbeat{PingInterval: "200ms"}, "ping_interval must be at least 1s"},
		{"ping_timeout below 1s", config.Heartbeat{PingTimeout: "500ms"}, "ping_timeout must be at least 1s"},
		{"ping_timeout zero with ping enabled", config.Heartbeat{PingInterval: "2s", PingTimeout: "0s"}, "0s is not allowed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			cfg.Server.Heartbeat = tc.hb
			err := cfg.Validate()
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.wantErr)
		})
	}

	t.Run("second-scale valid", func(t *testing.T) {
		cfg := base
		cfg.Server.Heartbeat = config.Heartbeat{IdleTimeout: "10s", PingInterval: "2s", PingTimeout: "1s"}
		require.NoError(t, cfg.Validate())
	})

	t.Run("disabled valid", func(t *testing.T) {
		cfg := base
		cfg.Server.Heartbeat = config.Heartbeat{IdleTimeout: "0s", PingInterval: "0s", PingTimeout: "0s"}
		require.NoError(t, cfg.Validate())
	})
}

// TestHeartbeat_NewNodePingDefaults: empty ping_timeout falls back to
// ping_interval; empty ping_interval keeps pings off.
func TestHeartbeat_NewNodePingDefaults(t *testing.T) {
	node := NewNode(&config.Server{Heartbeat: config.Heartbeat{IdleTimeout: "0s", PingInterval: "2s"}})
	cfg := node.GetHeartbeatConfig()
	assert.Equal(t, 2*time.Second, cfg.PingInterval)
	assert.Equal(t, 2*time.Second, cfg.PingTimeout, "empty ping_timeout must fall back to ping_interval")

	off := NewNode(&config.Server{Heartbeat: config.Heartbeat{IdleTimeout: "5s"}})
	assert.Zero(t, off.GetHeartbeatConfig().PingInterval)
	assert.Zero(t, off.GetHeartbeatConfig().PingTimeout)
}
