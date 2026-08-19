package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lynx-go/x/log"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	"github.com/messageloopio/messageloop/config"
	"github.com/messageloopio/messageloop/internal/occupancy"
	"github.com/messageloopio/messageloop/internal/session"
	"github.com/messageloopio/messageloop/proxy"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

type Node struct {
	hub              *Hub
	broker           Broker
	presence         PresenceStore
	cluster          *Cluster
	subLocks         [numSubLocks]sync.Mutex
	proxy            *proxy.Router
	heartbeatManager *HeartbeatManager
	rpcTimeout       time.Duration
	limits           config.Limits
	metrics          *Metrics
	surveys          map[string]*Survey
	surveyMu         sync.RWMutex
	// authorizer is the single authorization evaluator; it is never nil
	// (PR-KA-A4). It replaced the ACL engine and the channel policy engine.
	authorizer *Authorizer
	// adminCaps carries the configured admin capability bits: nil
	// server.grpc_admin.capabilities → DefaultAdminCapabilities; an explicit
	// empty list → zero.
	adminCaps   Capability
	requireAuth bool
	healthCheck func(context.Context) error
	// occupancy tracking (B2): lastApplied records the highest applied gen
	// per (channel, session) so late/replayed occupancy events are dropped;
	// occGens is the in-process per-channel gen fallback for presence
	// adapters without a gen source.
	occMu       sync.Mutex
	lastApplied map[string]map[string]uint64 // ch -> session -> last applied gen
	occGens     map[string]uint64            // ch -> fallback gen counter
}

const (
	numSubLocks = 16384
	// clusterStepTimeout bounds each cluster-side step (session/channel
	// state sync) inside the subscription saga. Short by design: the state
	// sync is an optimization for cross-node resume/query, and a slow
	// cluster must not block client unsubscribe for 10s per channel.
	clusterStepTimeout = 2 * time.Second
)

// surveySendTimeout bounds each local survey request send; a subscriber
// whose transport blocks writes is recorded as a failed response instead of
// hanging the whole survey. Variable for testability.
var surveySendTimeout = 10 * time.Second

// maxActiveSurveys caps the survey registry size as a defense against
// unbounded growth (e.g. many slow surveys). Under normal operation the
// per-send and per-wait timeouts keep the registry small. Variable for
// testability.
var maxActiveSurveys = 1000

func NewNode(cfg *config.Server) *Node {
	var limits config.Limits
	if cfg != nil {
		limits = cfg.Limits
	}

	node := &Node{
		hub:         session.NewHub(0, limits.MaxConnectionsPerUser),
		rpcTimeout:  proxy.DefaultRPCTimeout,
		limits:      limits,
		surveys:     make(map[string]*Survey),
		presence:    NewMemoryPresenceStore(),
		requireAuth: cfg != nil && cfg.RequireAuth,
	}

	if cfg != nil && cfg.RPCTimeout != "" {
		rpcTimeout, err := time.ParseDuration(cfg.RPCTimeout)
		if err != nil {
			rpcTimeout = proxy.DefaultRPCTimeout
		}
		node.rpcTimeout = rpcTimeout
	}

	// Idle timeout detection is always on: an unconfigured heartbeat falls
	// back to DefaultHeartbeatIdleTimeout (300s) so idle connections cannot
	// linger forever when the operator forgets to configure it. Server pings
	// stay off unless ping_interval is configured (default "0s").
	idleTimeout := DefaultHeartbeatIdleTimeout
	pingInterval := time.Duration(0)
	pingTimeout := time.Duration(0)
	if cfg != nil {
		if cfg.Heartbeat.IdleTimeout != "" {
			parsed, err := time.ParseDuration(cfg.Heartbeat.IdleTimeout)
			if err != nil {
				parsed = DefaultHeartbeatIdleTimeout
			}
			idleTimeout = parsed
		}
		if cfg.Heartbeat.PingInterval != "" {
			if parsed, err := time.ParseDuration(cfg.Heartbeat.PingInterval); err == nil {
				pingInterval = parsed
			}
		}
		if cfg.Heartbeat.PingTimeout != "" {
			if parsed, err := time.ParseDuration(cfg.Heartbeat.PingTimeout); err == nil {
				pingTimeout = parsed
			}
		}
	}
	// An empty ping_timeout falls back to ping_interval so that enabling
	// server pings works without spelling out the timeout (config.Validate
	// documents this; NewNode fills the default).
	if pingInterval > 0 && pingTimeout <= 0 {
		pingTimeout = pingInterval
	}
	node.heartbeatManager = NewHeartbeatManager(HeartbeatConfig{
		IdleTimeout:  idleTimeout,
		PingInterval: pingInterval,
		PingTimeout:  pingTimeout,
	})

	node.broker = NewMemoryBroker(MemoryBrokerOptions{})

	// The single authorization table. An empty server.authorizer (or a nil
	// cfg) compiles to the pre-policy defaults (history on, presence on,
	// survey off, subscribe/publish open), so an unconfigured server behaves
	// exactly as before. Invalid rule patterns are rejected by
	// config.Validate; an Authorizer build failure must not take the server
	// down, so fall back to the empty table.
	var authzCfg config.AuthorizerConfig
	if cfg != nil {
		authzCfg = cfg.Authorizer
	}
	authorizer, err := NewAuthorizer(authzCfg)
	if err != nil {
		log.WarnContext(context.Background(), "authorizer build failed, falling back to defaults", "error", err)
		authorizer, _ = NewAuthorizer(config.AuthorizerConfig{})
	}
	node.authorizer = authorizer

	// Admin capability bits: omitted capabilities → every closed bit except
	// pattern.global; an explicit empty list → zero bits (locked admin data
	// plane). Unknown names are rejected by config.Validate and skipped here.
	node.adminCaps = DefaultAdminCapabilities
	if cfg != nil && cfg.GRPCAdmin.Capabilities != nil {
		node.adminCaps = 0
		for _, name := range cfg.GRPCAdmin.Capabilities {
			if cap, ok := ClosedCapabilityNames[name]; ok {
				node.adminCaps |= cap
			}
		}
	}

	return node
}

// Run starts the broker in the background, bound to ctx.
// Waits until the broker's handler is registered before returning.
func (n *Node) Run(ctx context.Context) error {
	if n.cluster != nil {
		if err := n.cluster.Start(ctx); err != nil {
			return fmt.Errorf("start cluster: %w", err)
		}
	}

	// The presence store's TTL-evaporation prune point (Redis only) feeds the
	// occupancy emit path: every ghost member pruned by an existing Get
	// synthesizes a leave with a fresh generation (B2 §5.3).
	if reporter, ok := n.presence.(SyntheticLeaveReporter); ok {
		reporter.SetSyntheticLeaveHook(func(ctx context.Context, ch, clientID string) {
			n.onSyntheticLeave(ctx, ch, clientID)
		})
	}

	// Broker failures are funneled through an error channel instead of a
	// panic: a broker that fails to start (e.g. Redis unreachable) must
	// surface as a Run error so the caller (lynx) can react, rather than
	// crashing the process after Run has returned (P1-A6).
	startErr := make(chan error, 1)
	go func() {
		if err := n.broker.SetOccupancyHandler(n.onOccupancy); err != nil {
			log.ErrorContext(ctx, "failed to register occupancy handler", err)
			startErr <- err
			return
		}
		// C6: catch-up gap notifications fan out to local subscribers via the
		// same second-pipe pattern as occupancy (the broker knows no sessions).
		n.broker.SetGapHandler(n.onGap)
		if err := n.broker.Start(ctx, func(ch string, pub *Publication) error {
			return n.hub.BroadcastPublication(ch, pub)
		}); err != nil {
			log.ErrorContext(ctx, "broker stopped with error", err)
			startErr <- err
		}
	}()
	type readyBroker interface{ Ready() <-chan struct{} }
	if r, ok := n.broker.(readyBroker); ok {
		select {
		case <-r.Ready():
		case err := <-startErr:
			return err
		case <-ctx.Done():
		}
	}
	return nil
}

// Shutdown gracefully drains all client connections and cleans up resources.
func (n *Node) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultShutdownTimeout)
	defer cancel()

	// Signal the disconnect code to indicate server-initiated shutdown.
	done := make(chan struct{})
	go func() {
		n.hub.DrainAll(DisconnectForceNoReconnect)
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		log.WarnContext(ctx, "shutdown: timed out draining client connections")
	}

	if n.cluster != nil {
		if err := n.cluster.Shutdown(ctx); err != nil {
			log.WarnContext(ctx, "cluster shutdown error", "error", err)
		}
	}
}

// SetCluster sets the cluster control-plane coordinator for this node.
func (n *Node) SetCluster(runtime *Cluster) {
	n.cluster = runtime
}

// SetHealthCheck registers a connectivity probe (e.g. a Redis ping) invoked
// by the health endpoint when cluster mode is enabled. Passing nil disables
// the probe.
func (n *Node) SetHealthCheck(fn func(context.Context) error) {
	n.healthCheck = fn
}

// Cluster returns the configured cluster control-plane coordinator.
func (n *Node) Cluster() *Cluster {
	return n.cluster
}

func (n *Node) SetBroker(broker Broker) {
	n.broker = broker
}

func (n *Node) Broker() Broker {
	return n.broker
}

func (n *Node) SetPresenceStore(ps PresenceStore) {
	n.presence = ps
}

// Presence returns all clients currently present in ch.
func (n *Node) Presence(ctx context.Context, ch string) (map[string]*PresenceInfo, error) {
	return n.presence.Get(ctx, ch)
}

// SetPresenceForSession records presence for one subscribed session.
// ClientID is the session ID in v1.0; SessionID carries the same value
// formally, and ConnectClientID is the Connect.client_id.
func (n *Node) SetPresenceForSession(ctx context.Context, ch string, c *Client) error {
	if c == nil {
		return nil
	}
	return n.presence.Add(ctx, ch, &PresenceInfo{
		ClientID:        c.SessionID(),
		SessionID:       c.SessionID(),
		ConnectClientID: c.ClientID(),
		UserID:          c.UserID(),
		ConnectedAt:     c.ConnectedAt().UnixMilli(),
	})
}

// ClearPresenceForSession removes presence for one subscribed session.
func (n *Node) ClearPresenceForSession(ctx context.Context, ch string, c *Client) error {
	if c == nil {
		return nil
	}
	return n.presence.Remove(ctx, ch, c.SessionID())
}

func (n *Node) subLock(ch string) *sync.Mutex {
	return &n.subLocks[index(ch, numSubLocks)]
}

func (n *Node) Hub() *Hub {
	return n.hub
}

// SetMetrics sets the Prometheus metrics collector for the node.
func (n *Node) SetMetrics(m *Metrics) {
	n.metrics = m
}

// ChannelPolicy returns the effective channel policy for ch, resolved by the
// Authorizer's Effects (server.authorizer table overlay, PR-KA-A4 §5.5).
func (n *Node) ChannelPolicy(ch string) ChannelPolicy {
	return n.authorizer.Effects(ch)
}

// userPrincipal builds the principal for an authenticated client user.
func (n *Node) userPrincipal(userID string) Principal {
	return Principal{Kind: PrincipalUser, UserID: userID}
}

// adminPrincipal builds the principal for the server-side admin API
// (UserID "admin", matching allow lists like today's adminPrincipal).
func (n *Node) adminPrincipal() Principal {
	return Principal{Kind: PrincipalAdmin, UserID: adminPrincipal, Caps: n.adminCaps}
}

// AdminDecide evaluates one action for the admin principal (used by the
// gRPC admin API: Recover / Presence / Survey gates).
func (n *Node) AdminDecide(action Action, channel string) Decision {
	return n.authorizer.Decide(n.adminPrincipal(), action, channel)
}

// AdminCapabilities returns the configured admin capability bits.
func (n *Node) AdminCapabilities() Capability {
	return n.adminCaps
}

// ReplaceRules swaps the authorizer rule table, then revokes every local
// subscription whose pattern is no longer allowed (PR-KA-A4 §8.4). Revoked
// patterns are removed whole — never split into exact channels.
func (n *Node) ReplaceRules(cfg config.AuthorizerConfig) error {
	if err := n.authorizer.ReplaceRules(cfg); err != nil {
		return err
	}
	sessions := n.hub.Sessions()
	for _, client := range sessions {
		channels := client.SubscribedChannels()
		userID := client.UserID()
		p := Principal{Kind: PrincipalUser, UserID: userID}
		for _, ch := range n.authorizer.PatternsToRevoke(p, channels) {
			if err := n.RemoveSubscription(ch, client); err != nil {
				log.WarnContext(context.Background(), "failed to revoke subscription after rule replacement",
					"channel", ch, "session", client.SessionID(), "error", err)
			}
		}
	}
	return nil
}

// AddClient adds a client session to the node's hub.
// Returns an error (DisconnectConnectionLimit) if the per-user connection limit is exceeded.
func (n *Node) AddClient(c *Client) error {
	if err := n.hub.Add(c); err != nil {
		return err
	}
	if err := n.syncClusterSessionState(context.Background(), c); err != nil {
		n.hub.RemoveSession(c.SessionID())
		return fmt.Errorf("sync cluster session: %w", err)
	}
	if n.metrics != nil {
		n.metrics.ConnectionsTotal.WithLabelValues(c.TransportLabel()).Inc()
	}
	return nil
}

// AddSubscription adds a subscription for a client to a channel.
func (n *Node) AddSubscription(ctx context.Context, ch string, sub Subscriber) error {
	mu := n.subLock(ch)
	mu.Lock()
	defer mu.Unlock()

	if _, exists := n.hub.LookupSubscriber(ch, sub.Session); exists {
		return nil
	}

	var first bool

	// Each step captures the state needed to commit and undo itself.
	err := runSubSaga([]subSagaStep{
		{
			name: "hub.addSub",
			commit: func() (err error) {
				first, err = n.hub.AddSub(ch, sub)
				return err
			},
			rollback: func() {
				_, _ = n.hub.RemoveSub(ch, sub.Session)
			},
		},
		{
			name: "track.client",
			commit: func() error {
				// Reject subscriptions on a closing/closed client: close()
				// snapshots subscribedChannels under this same lock, so a
				// subscribe admitted here after the snapshot would never be
				// cleaned up and would leak in the hub (P1-A3).
				if sub.Session.TrackChannel(ch) {
					return fmt.Errorf("client is closed")
				}
				return nil
			},
			rollback: func() {
				sub.Session.UntrackChannel(ch)
			},
		},
		{
			name: "broker.Subscribe",
			commit: func() error {
				if !first {
					return nil
				}
				if err := n.broker.Subscribe(ch); err != nil {
					return err
				}
				if n.metrics != nil {
					n.metrics.ActiveChannels.Inc()
				}
				return nil
			},
			rollback: func() {
				if !first {
					return
				}
				_ = n.broker.Unsubscribe(ch)
				if n.metrics != nil {
					n.metrics.ActiveChannels.Dec()
				}
			},
		},
		{
			name: "cluster.session",
			commit: func() error {
				return n.syncClusterSessionState(ctx, sub.Session)
			},
			rollback: func() {
				rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = n.syncClusterSessionState(rctx, sub.Session)
			},
		},
		{
			name: "cluster.channel",
			commit: func() error {
				return n.adjustClusterChannelSubscriptions(ctx, ch, 1)
			},
			rollback: func() {
				rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = n.adjustClusterChannelSubscriptions(rctx, ch, -1)
			},
		},
	})
	if err != nil {
		return err
	}

	if n.metrics != nil {
		n.metrics.SubscriptionsTotal.Inc()
	}
	return nil
}

// RemoveSubscription removes a subscription for a client from a channel.
func (n *Node) RemoveSubscription(ch string, c *Client) error {
	mu := n.subLock(ch)
	mu.Lock()
	defer mu.Unlock()

	subscriber, exists := n.hub.LookupSubscriber(ch, c)
	if !exists {
		return nil
	}
	var last, removed bool

	err := runSubSaga([]subSagaStep{
		{
			name: "hub.removeSub",
			commit: func() (err error) {
				last, removed = n.hub.RemoveSub(ch, c)
				if !removed {
					return fmt.Errorf("subscription not found for channel %s", ch)
				}
				return nil
			},
			rollback: func() {
				_, _ = n.hub.AddSub(ch, subscriber)
			},
		},
		{
			name: "untrack.client",
			commit: func() error {
				c.UntrackChannel(ch)
				return nil
			},
			rollback: func() {
				c.ForceTrackChannel(ch)
			},
		},
		{
			name: "broker.Unsubscribe",
			commit: func() error {
				if !last {
					return nil
				}
				if err := n.broker.Unsubscribe(ch); err != nil {
					return err
				}
				if n.metrics != nil {
					n.metrics.ActiveChannels.Dec()
				}
				return nil
			},
			rollback: func() {
				if !last {
					return
				}
				_ = n.broker.Subscribe(ch)
				if n.metrics != nil {
					n.metrics.ActiveChannels.Inc()
				}
			},
		},
		{
			name: "cluster.session",
			commit: func() error {
				rctx, cancel := context.WithTimeout(context.Background(), clusterStepTimeout)
				defer cancel()
				return n.syncClusterSessionState(rctx, c)
			},
			rollback: func() {
				rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = n.syncClusterSessionState(rctx, c)
			},
		},
		{
			name: "cluster.channel",
			commit: func() error {
				rctx, cancel := context.WithTimeout(context.Background(), clusterStepTimeout)
				defer cancel()
				return n.adjustClusterChannelSubscriptions(rctx, ch, -1)
			},
			rollback: func() {
				rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = n.adjustClusterChannelSubscriptions(rctx, ch, 1)
			},
		},
	})
	if err != nil {
		return err
	}

	if n.metrics != nil {
		n.metrics.SubscriptionsTotal.Dec()
	}
	return nil
}

// Channels returns active channels from either the local hub or the shared query store.
func (n *Node) Channels(ctx context.Context) ([]ChannelInfo, error) {
	if !n.ClusterEnabled() {
		return n.hub.GetActiveChannels(), nil
	}

	channels, err := n.clusterQueryStore().ListChannels(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]ChannelInfo, 0, len(channels))
	for _, ch := range channels {
		result = append(result, ChannelInfo{
			Name:        ch.Name,
			Subscribers: int(ch.Subscribers),
		})
	}
	return result, nil
}

// Publish sends payload to ch via the broker.
// Returns the offset assigned to the publication by the broker (0 if history is disabled).
func (n *Node) Publish(ch string, pub *Publication) (uint64, error) {
	if n.metrics != nil {
		timer := prometheus.NewTimer(n.metrics.PublishDuration)
		defer timer.ObserveDuration()
	}
	pol := n.ChannelPolicy(ch)
	if pol.TransientOnly || !pol.History {
		// History is disabled for this channel: callers must use
		// PublishTransient (the client path does this transparently).
		return 0, ErrHistoryDisabled
	}
	// Per-policy history sizing: a caller-specified value wins, otherwise
	// the policy cap (0 on both sides means the broker global default).
	if pub.HistorySize == 0 && pol.HistorySize > 0 {
		pub.HistorySize = pol.HistorySize
	}
	if pub.HistoryTTL == 0 && pol.HistoryTTL > 0 {
		pub.HistoryTTL = pol.HistoryTTL
	}
	offset, err := n.broker.Publish(ch, pub)
	if err == nil && n.metrics != nil {
		n.metrics.MessagesPublished.Inc()
	}
	return offset, err
}

// PublishTransient delivers payload to ch in real time without writing
// broker history. Used for events (e.g. presence join/leave) that must not
// appear in the recovery message stream.
func (n *Node) PublishTransient(ch string, pub *Publication) error {
	if n.metrics != nil {
		timer := prometheus.NewTimer(n.metrics.PublishDuration)
		defer timer.ObserveDuration()
	}
	err := n.broker.PublishTransient(ch, pub)
	if err == nil && n.metrics != nil {
		n.metrics.MessagesPublished.Inc()
	}
	return err
}

// SetupProxy configures the proxy router with the given proxy configurations.
func (n *Node) SetupProxy(cfgs []*proxy.ProxyConfig) error {
	n.proxy = proxy.NewRouter()
	for _, cfg := range cfgs {
		p, err := n.createProxy(cfg)
		if err != nil {
			return fmt.Errorf("failed to create proxy %s: %w", cfg.Name, err)
		}
		if err := n.proxy.AddFromConfig(p, cfg); err != nil {
			return fmt.Errorf("failed to add routes for proxy %s: %w", cfg.Name, err)
		}
	}
	return nil
}

func (n *Node) createProxy(cfg *proxy.ProxyConfig) (proxy.Proxy, error) {
	if cfg.GRPC != nil {
		return proxy.NewGRPCProxy(cfg)
	}
	if cfg.HTTP != nil {
		return proxy.NewHTTPProxy(cfg)
	}
	if (len(cfg.Endpoint) >= 7 && cfg.Endpoint[:7] == "http://") ||
		(len(cfg.Endpoint) >= 8 && cfg.Endpoint[:8] == "https://") {
		return proxy.NewHTTPProxy(cfg)
	}
	return proxy.NewGRPCProxy(cfg)
}

// FindProxy finds a proxy for the given channel and method.
func (n *Node) FindProxy(channel, method string) proxy.Proxy {
	if n.proxy == nil {
		return nil
	}
	return n.proxy.Match(channel, method)
}

// AddProxy adds a proxy to the router.
func (n *Node) AddProxy(p proxy.Proxy, channelPattern, methodPattern string) error {
	if n.proxy == nil {
		n.proxy = proxy.NewRouter()
	}
	return n.proxy.Add(p, channelPattern, methodPattern)
}

// ProxyRPC proxies an RPC request to the configured backend.
func (n *Node) ProxyRPC(ctx context.Context, channel, method string, req *proxy.RPCProxyRequest) (*proxy.RPCProxyResponse, error) {
	p := n.FindProxy(channel, method)
	if p == nil {
		return nil, proxy.ErrNoProxyFound
	}
	return p.RPC(ctx, req)
}

// GetRPCTimeout returns the configured RPC timeout.
func (n *Node) GetRPCTimeout() time.Duration {
	if n.rpcTimeout > 0 {
		return n.rpcTimeout
	}
	return proxy.DefaultRPCTimeout
}

// GetHeartbeatIdleTimeout returns the configured heartbeat idle timeout.
func (n *Node) GetHeartbeatIdleTimeout() time.Duration {
	if n.heartbeatManager != nil {
		return n.heartbeatManager.Config().IdleTimeout
	}
	return 0
}

// GetHeartbeatConfig returns the parsed heartbeat configuration.
func (n *Node) GetHeartbeatConfig() HeartbeatConfig {
	if n.heartbeatManager != nil {
		return n.heartbeatManager.Config()
	}
	return HeartbeatConfig{}
}

// sessionLeaseTTL returns the cluster session lease TTL for the configured
// heartbeat. With second-scale idle/ping the lease shrinks below the 600s
// default (it must be strictly longer than idle + the 10s throttled refresh
// window plus headroom), while a disabled heartbeat keeps the 600s default
// so a live-but-silent session is never taken over early.
func (n *Node) sessionLeaseTTL() time.Duration {
	idle, ping := n.heartbeatIdleAndPing()
	if idle == 0 && ping == 0 {
		return defaultClusterSessionLeaseTTL
	}
	ttl := 30 * time.Second
	if t := 2 * idle; t > ttl {
		ttl = t
	}
	if t := 3 * ping; t > ttl {
		ttl = t
	}
	if t := idle + pingClusterRefreshInterval + 10*time.Second; t > ttl {
		ttl = t
	}
	return ttl
}

// heartbeatIdleAndPing returns the configured idle timeout and ping interval.
func (n *Node) heartbeatIdleAndPing() (time.Duration, time.Duration) {
	if n.heartbeatManager == nil {
		return 0, 0
	}
	cfg := n.heartbeatManager.Config()
	return cfg.IdleTimeout, cfg.PingInterval
}

// MaxMessageSize returns the max inbound message size in bytes.
// A configured value of 0 means "use the default", so DefaultMaxMessageSize
// (64KB) is applied; both WebSocket and gRPC transports read through this
// method to keep the limit uniform.
func (n *Node) MaxMessageSize() int {
	if n.limits.MaxMessageSize > 0 {
		return n.limits.MaxMessageSize
	}
	return DefaultMaxMessageSize
}

// Survey sends a request to all subscribers of a channel and collects responses.
func (n *Node) Survey(ctx context.Context, channel string, payload []byte, timeout time.Duration) ([]*SurveyResult, error) {
	if !n.ClusterEnabled() {
		return n.localSurvey(ctx, channel, payload, timeout)
	}

	localResults, err := n.localSurvey(ctx, channel, payload, timeout)
	if err != nil {
		return nil, err
	}
	annotateSurveyResults(localResults, n.ClusterNodeID(), n.ClusterIncarnationID())

	metadata := map[string]string{}
	if timeout > 0 {
		metadata[clusterCommandMetaSurveyTimeoutMS] = strconv.FormatInt(timeout.Milliseconds(), 10)
	}
	metadata["exclude_self"] = "true"

	results, err := n.clusterCommandBus().BroadcastCommand(ctx, &ClusterCommand{
		Type:     ClusterCommandSurvey,
		Channel:  channel,
		Payload:  payload,
		Metadata: metadata,
	})
	if err != nil {
		return nil, err
	}

	aggregated := append(make([]*SurveyResult, 0, len(localResults)), localResults...)
	for _, result := range results {
		aggregated = append(aggregated, expandClusterSurveyResults(result)...)
	}
	sortSurveyResults(aggregated)
	return aggregated, nil
}

func (n *Node) localSurvey(ctx context.Context, channel string, payload []byte, timeout time.Duration) ([]*SurveyResult, error) {
	subscribers := n.hub.GetMatchingSubscribers(channel)
	if len(subscribers) == 0 {
		return []*SurveyResult{}, nil
	}

	surveyID := uuid.NewString()
	survey := NewSurvey(surveyID, channel, payload, timeout)

	// Record the subscriber sessions the survey was sent to. Responses from
	// any other session are forged and must be rejected by AddSurveyResponse.
	for _, sub := range subscribers {
		survey.AddExpectedSession(sub.SessionID())
	}

	if !n.registerSurvey(ctx, survey) {
		return nil, fmt.Errorf("survey registry full (limit %d)", maxActiveSurveys)
	}
	defer n.unregisterSurvey(surveyID)

	// Bound each subscriber's request send: a client whose transport blocks
	// writes must not hang the whole survey — the send fails at
	// surveySendTimeout and is recorded as an error response.
	sendCtx, sendCancel := context.WithTimeout(ctx, surveySendTimeout)
	defer sendCancel()
	var wg sync.WaitGroup
	for _, sub := range subscribers {
		wg.Add(1)
		go func(session *Client) {
			defer wg.Done()
			n.sendSurveyRequest(sendCtx, session, survey)
		}(sub)
	}
	wg.Wait()

	results := survey.Wait(ctx)
	survey.Close()
	sortSurveyResults(results)

	return results, nil
}

func sortSurveyResults(results []*SurveyResult) {
	sort.Slice(results, func(i, j int) bool {
		left := results[i]
		right := results[j]
		if left.NodeID != right.NodeID {
			return left.NodeID < right.NodeID
		}
		if left.IncarnationID != right.IncarnationID {
			return left.IncarnationID < right.IncarnationID
		}
		return left.SessionID < right.SessionID
	})
}

func annotateSurveyResults(results []*SurveyResult, nodeID, incarnationID string) {
	for _, result := range results {
		if result == nil {
			continue
		}
		result.NodeID = nodeID
		result.IncarnationID = incarnationID
	}
}

func expandClusterSurveyResults(result *ClusterCommandResult) []*SurveyResult {
	if result == nil {
		return nil
	}
	if result.Status != ClusterCommandStatusSucceeded {
		return []*SurveyResult{{
			NodeID:        result.NodeID,
			IncarnationID: result.IncarnationID,
			Error:         fmt.Errorf("%s: %s", result.ErrorCode, result.ErrorMessage),
		}}
	}
	records, err := decodeClusterSurveyResults(result.Metadata[clusterCommandMetaSurveyResults])
	if err != nil {
		return []*SurveyResult{{
			NodeID:        result.NodeID,
			IncarnationID: result.IncarnationID,
			Error:         fmt.Errorf("decode cluster survey results: %w", err),
		}}
	}
	expanded := make([]*SurveyResult, 0, len(records))
	for _, record := range records {
		entry := &SurveyResult{
			SessionID:     record.SessionID,
			NodeID:        result.NodeID,
			IncarnationID: result.IncarnationID,
			Payload:       append([]byte(nil), record.Payload...),
		}
		if record.ErrorMessage != "" {
			entry.Error = fmt.Errorf("%s", record.ErrorMessage)
		}
		expanded = append(expanded, entry)
	}
	return expanded
}

type clusterSurveyResultRecord struct {
	SessionID    string `json:"session_id"`
	Payload      []byte `json:"payload,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

func encodeClusterSurveyResults(results []*SurveyResult) (string, error) {
	records := make([]clusterSurveyResultRecord, 0, len(results))
	for _, result := range results {
		if result == nil {
			continue
		}
		record := clusterSurveyResultRecord{
			SessionID: result.SessionID,
			Payload:   append([]byte(nil), result.Payload...),
		}
		if result.Error != nil {
			record.ErrorMessage = result.Error.Error()
		}
		records = append(records, record)
	}
	encoded, err := json.Marshal(records)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func decodeClusterSurveyResults(encoded string) ([]clusterSurveyResultRecord, error) {
	if encoded == "" {
		return nil, nil
	}
	var records []clusterSurveyResultRecord
	if err := json.Unmarshal([]byte(encoded), &records); err != nil {
		return nil, err
	}
	return records, nil
}

// countMatchingSubscribers returns the cluster-wide number of subscribers
// matching ch. Without a cluster this is the local matching subscriber count.
// With a cluster each node is asked for its local count only (count_only)
// and never runs localSurvey. The count is a soft gate: the documented
// TOCTOU between counting and sending is accepted, and a node that fails to
// answer is skipped (its subscribers are not counted).
func (n *Node) countMatchingSubscribers(ctx context.Context, ch string) (int, error) {
	local := len(n.hub.GetMatchingSubscribers(ch))
	if !n.ClusterEnabled() {
		return local, nil
	}

	results, err := n.clusterCommandBus().BroadcastCommand(ctx, &ClusterCommand{
		Type:    ClusterCommandSurvey,
		Channel: ch,
		Metadata: map[string]string{
			clusterCommandMetaSurveyCountOnly: "true",
			"exclude_self":                    "true",
		},
	})
	if err != nil {
		return 0, err
	}

	total := local
	for _, result := range results {
		if result == nil || result.Status != ClusterCommandStatusSucceeded {
			log.WarnContext(ctx, "survey subscriber count skipped for failed node",
				"node", resultNodeID(result))
			continue
		}
		if count, err := strconv.Atoi(result.Metadata[clusterCommandMetaSurveyCount]); err == nil && count > 0 {
			total += count
		}
	}
	return total, nil
}

func resultNodeID(result *ClusterCommandResult) string {
	if result == nil {
		return ""
	}
	return result.NodeID
}

// CountMatchingSubscribers exposes the cluster-wide matching subscriber count
// for the admin survey population gate (PR-KA-A4 §7 survey.bypass_gate).
func (n *Node) CountMatchingSubscribers(ctx context.Context, ch string) (int, error) {
	return n.countMatchingSubscribers(ctx, ch)
}

// buildClientSurveyResult assembles the client-facing SurveyResult from the
// aggregated server-side results. Each answer carries its session id, the
// payload (binary variant) and, for locally known sessions, a user_id
// metadata entry (the proto has no user_id field). A single answer whose
// payload exceeds MaxSurveyAnswerBytes becomes a SURVEY_ANSWER_TOO_LARGE
// error with an empty payload; when the whole encoded result exceeds
// MaxSurveyResultBytes, subsequent answers are stripped of their payload and
// turned into errors (dropped entirely if even that stays over the cap).
func (n *Node) buildClientSurveyResult(requestID, channel string, results []*SurveyResult) *clientpb.SurveyResult {
	out := &clientpb.SurveyResult{
		RequestId: requestID,
		Channel:   channel,
	}
	for _, result := range results {
		answer := n.surveyAnswerFor(result)
		out.Answers = append(out.Answers, answer)
		if encodedSurveyResultSize(out) > MaxSurveyResultBytes {
			answer.Payload = nil
			if answer.Error == nil {
				answer.Error = surveyTooLargeError("survey result exceeds size limit")
			}
			if encodedSurveyResultSize(out) > MaxSurveyResultBytes {
				out.Answers = out.Answers[:len(out.Answers)-1]
			}
		}
	}
	return out
}

func (n *Node) surveyAnswerFor(result *SurveyResult) *clientpb.SurveyAnswer {
	answer := &clientpb.SurveyAnswer{SessionId: result.SessionID}
	if result.Error != nil {
		answer.Error = &sharedv2.Error{
			Code:    "SURVEY_FAILED",
			Type:    "survey_error",
			Message: result.Error.Error(),
		}
		return answer
	}
	if len(result.Payload) > MaxSurveyAnswerBytes {
		answer.Error = surveyTooLargeError("answer exceeds size limit")
		return answer
	}
	if len(result.Payload) > 0 {
		answer.Payload = &sharedv2.Payload{
			Data: &sharedv2.Payload_Binary{
				Binary: append([]byte(nil), result.Payload...),
			},
		}
	}
	if sess := n.hub.LookupSession(result.SessionID); sess != nil && sess.UserID() != "" {
		answer.Metadata = &sharedv2.Metadata{Entries: map[string]string{"user_id": sess.UserID()}}
	}
	return answer
}

func surveyTooLargeError(message string) *sharedv2.Error {
	return &sharedv2.Error{
		Code:    "SURVEY_ANSWER_TOO_LARGE",
		Type:    "survey_error",
		Message: message,
	}
}

// encodedSurveyResultSize returns the canonical binary encoding size of the
// outbound SurveyResult, used for the whole-result cap.
func encodedSurveyResultSize(result *clientpb.SurveyResult) int {
	encoded, err := proto.Marshal(result)
	if err != nil {
		return 1 << 30
	}
	return len(encoded)
}

func (n *Node) sendSurveyRequest(ctx context.Context, session *Client, survey *Survey) {
	var payload *sharedv2.Payload
	if len(survey.Payload()) > 0 {
		payload = &sharedv2.Payload{
			Data: &sharedv2.Payload_Binary{
				Binary: survey.Payload(),
			},
		}
	}

	msg := MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_SurveyRequest{
			SurveyRequest: &clientpb.SurveyRequest{
				RequestId: survey.ID(),
				Channel:   survey.Channel(),
				Payload:   payload,
			},
		}
	})

	// Bound the actual write: transports may block (slow consumer), so give
	// up when the send context expires and record the failure as an error
	// response instead of hanging the survey.
	sendDone := make(chan error, 1)
	go func() {
		sendDone <- session.Send(ctx, msg)
	}()
	select {
	case err := <-sendDone:
		if err != nil {
			log.WarnContext(ctx, "failed to send survey request", "session", session.SessionID(), "error", err)
			survey.AddResponse(session.SessionID(), nil, err)
		}
	case <-ctx.Done():
		log.WarnContext(ctx, "survey request send timed out", "session", session.SessionID(), "error", ctx.Err())
		survey.AddResponse(session.SessionID(), nil, ctx.Err())
	}
}

func (n *Node) registerSurvey(ctx context.Context, survey *Survey) bool {
	n.surveyMu.Lock()
	defer n.surveyMu.Unlock()
	if len(n.surveys) >= maxActiveSurveys {
		log.WarnContext(ctx, "survey registry full, rejecting survey registration",
			"survey_id", survey.ID(), "limit", maxActiveSurveys)
		return false
	}
	n.surveys[survey.ID()] = survey
	return true
}

func (n *Node) unregisterSurvey(surveyID string) {
	n.surveyMu.Lock()
	defer n.surveyMu.Unlock()
	delete(n.surveys, surveyID)
}

func (n *Node) getSurvey(surveyID string) *Survey {
	n.surveyMu.RLock()
	defer n.surveyMu.RUnlock()
	return n.surveys[surveyID]
}

// AddSurveyResponse adds a client response to the appropriate survey.
// Only sessions that were sent the survey request (i.e. subscribers of the
// survey channel) may respond; responses from any other session are forged
// and dropped.
func (n *Node) AddSurveyResponse(ctx context.Context, sessionID string, requestID string, payload []byte, err error) {
	survey := n.getSurvey(requestID)
	if survey == nil {
		log.WarnContext(ctx, "survey not found for response", "request_id", requestID, "session", sessionID)
		return
	}
	if !survey.IsExpectedSession(sessionID) {
		log.WarnContext(ctx, "dropping survey response from non-subscriber", "request_id", requestID, "session", sessionID)
		return
	}
	survey.AddResponse(sessionID, payload, err)
}

// presenceChannel returns the internal channel name for presence events.
func presenceChannel(ch string) string {
	return ch + "/__presence"
}

// PublishPresenceJoin publishes a presence join event to the channel's presence sub-channel.
// Presence events are transient: they are delivered in real time but never
// written to broker history, so they do not leak into the recovery stream.
// Kept for the legacy companion path (legacy_presence_channel=true) and for
// direct callers; first-class occupancy flows over the live bus instead.
func (n *Node) PublishPresenceJoin(channel, clientID, userID string) {
	evt := occupancy.NewPresenceEvent("join", channel, clientID, userID)
	data, err := occupancy.MarshalPresenceEvent(evt)
	if err != nil {
		return
	}
	err = n.PublishTransient(presenceChannel(channel), &Publication{Payload: data, Kind: PayloadKindText})
	if err != nil {
		log.WarnContext(context.Background(), "failed to publish presence join event",
			err, "channel", channel, "client_id", clientID)
		if n.metrics != nil {
			n.metrics.PresencePublishFailures.Inc()
			n.metrics.PresenceFailures.WithLabelValues("companion").Inc()
		}
	}
}

// PublishPresenceLeave publishes a presence leave event to the channel's presence sub-channel.
// Presence events are transient: they are delivered in real time but never
// written to broker history, so they do not leak into the recovery stream.
func (n *Node) PublishPresenceLeave(channel, clientID, userID string) {
	evt := occupancy.NewPresenceEvent("leave", channel, clientID, userID)
	data, err := occupancy.MarshalPresenceEvent(evt)
	if err != nil {
		return
	}
	err = n.PublishTransient(presenceChannel(channel), &Publication{Payload: data, Kind: PayloadKindText})
	if err != nil {
		log.WarnContext(context.Background(), "failed to publish presence leave event",
			err, "channel", channel, "client_id", clientID)
		if n.metrics != nil {
			n.metrics.PresencePublishFailures.Inc()
			n.metrics.PresenceFailures.WithLabelValues("companion").Inc()
		}
	}
}

// shouldTrackPresence is the single gate shared by every presence writer
// (subscribe, connect, unsubscribe, close, refresh, restore, cluster
// commands): ephemeral subscriptions, wildcard patterns and channels whose
// policy disables presence never touch the store, never join/leave and never
// take a snapshot.
func (n *Node) shouldTrackPresence(ch string, ephemeral bool) bool {
	return !ephemeral && !isWildcard(ch) && n.ChannelPolicy(ch).Presence
}

// presenceJoin records a session's presence in ch and emits the join event
// over the LiveBus as an occupancy event, excluding the joining session
// itself (no self-join). The store write is best-effort: a failure warns and
// counts op=store but never rolls back the subscription. Legacy companion
// publication runs only when the channel policy opts in and
// shouldTrackPresence already excluded wildcards.
func (n *Node) presenceJoin(ctx context.Context, ch string, c *Client) {
	if c == nil {
		return
	}
	ephemeral := false
	if stored, ok := n.hub.LookupSubscriber(ch, c); ok {
		ephemeral = stored.Ephemeral
	}
	if !n.shouldTrackPresence(ch, ephemeral) {
		return
	}
	if err := n.SetPresenceForSession(ctx, ch, c); err != nil {
		log.WarnContext(ctx, "failed to set presence for session", err, "channel", ch, "session", c.SessionID())
		if n.metrics != nil {
			n.metrics.PresenceFailures.WithLabelValues("store").Inc()
		}
	}
	n.publishOccupancy(ch, OccupancyEvent{
		Event: &clientpb.PresenceEvent{
			Channel: ch,
			Action:  PresenceActionJoin,
			Info: &clientpb.PresenceInfo{
				SessionId:   c.SessionID(),
				UserId:      c.UserID(),
				ClientId:    c.ClientID(),
				ConnectedAt: c.ConnectedAt().UnixMilli(),
			},
		},
	})
	if n.ChannelPolicy(ch).LegacyPresenceChannel {
		go n.PublishPresenceJoin(ch, c.SessionID(), c.UserID())
	}
}

// presenceLeave removes a session's presence from ch and emits the leave
// event over the LiveBus as an occupancy event, excluding the leaving session
// itself. Only called for subscriptions that were tracked
// (shouldTrackPresence), so wildcard and ephemeral subscriptions never leak a
// leave.
func (n *Node) presenceLeave(ctx context.Context, ch, sessionID, userID string, ephemeral bool) {
	if !n.shouldTrackPresence(ch, ephemeral) {
		return
	}
	if err := n.presence.Remove(ctx, ch, sessionID); err != nil {
		log.WarnContext(ctx, "failed to remove presence", err, "channel", ch, "session", sessionID)
		if n.metrics != nil {
			n.metrics.PresenceFailures.WithLabelValues("store").Inc()
		}
	}
	info := &clientpb.PresenceInfo{
		SessionId: sessionID,
		UserId:    userID,
	}
	if sess := n.hub.LookupSession(sessionID); sess != nil {
		info.ClientId = sess.ClientID()
		info.ConnectedAt = sess.ConnectedAt().UnixMilli()
	}
	n.publishOccupancy(ch, OccupancyEvent{
		Event: &clientpb.PresenceEvent{
			Channel: ch,
			Action:  PresenceActionLeave,
			Info:    info,
		},
	})
	if n.ChannelPolicy(ch).LegacyPresenceChannel {
		go n.PublishPresenceLeave(ch, sessionID, userID)
	}
}

// publishOccupancy attaches the next monotonic generation and fans the event
// on the LiveBus for the exact channel. A gen issuance failure (or a
// PublishOccupancy failure) warns and counts but never rolls back the
// subscription: the next snapshot covers any missed event (KD-K14).
func (n *Node) publishOccupancy(ch string, evt OccupancyEvent) {
	if evt.Event == nil {
		return
	}
	gen := n.nextOccupancyGen(ch)
	if gen == 0 {
		return
	}
	evt.Gen = gen
	if err := n.broker.PublishOccupancy(ch, evt); err != nil {
		log.WarnContext(context.Background(), "failed to emit occupancy", err, "channel", ch)
		if n.metrics != nil {
			n.metrics.PresenceFailures.WithLabelValues("emit").Inc()
		}
	}
}

// nextOccupancyGen issues the per-channel occupancy generation: the wired
// presence adapter owns cross-node monotonicity (memory = in-process counter,
// Redis = INCR), with an in-process per-channel fallback so a node without a
// gen-capable adapter still keeps same-node ordering. Returns 0 on failure,
// in which case callers drop the emit.
func (n *Node) nextOccupancyGen(ch string) uint64 {
	if src, ok := n.presence.(OccupancyGenSource); ok {
		gen, err := src.NextOccupancyGen(context.Background(), ch)
		if err != nil {
			log.WarnContext(context.Background(), "occupancy gen failed", err, "channel", ch)
			if n.metrics != nil {
				n.metrics.PresenceFailures.WithLabelValues("gen").Inc()
			}
			return 0
		}
		return gen
	}
	n.occMu.Lock()
	defer n.occMu.Unlock()
	if n.occGens == nil {
		n.occGens = make(map[string]uint64)
	}
	n.occGens[ch]++
	return n.occGens[ch]
}

// onOccupancy is the LiveBus occupancy receiver (registered as the broker's
// occupancy handler). It dedupes by generation per (channel, session) and
// fans the event out to every locally covered subscriber except the event
// subject itself (self-join/leave). It never touches the publication path.
func (n *Node) onOccupancy(ch string, evt OccupancyEvent) error {
	if evt.Gen == 0 || evt.Event == nil {
		log.WarnContext(context.Background(), "dropping invalid occupancy event", "channel", ch, "gen", evt.Gen)
		return nil
	}
	sid := evt.Event.Info.GetSessionId()
	if sid == "" {
		log.WarnContext(context.Background(), "dropping occupancy event without session", "channel", ch, "gen", evt.Gen)
		return nil
	}
	evt.Event.Channel = ch

	n.occMu.Lock()
	if n.lastApplied == nil {
		n.lastApplied = make(map[string]map[string]uint64)
	}
	bySession := n.lastApplied[ch]
	if bySession == nil {
		bySession = make(map[string]uint64)
		n.lastApplied[ch] = bySession
	}
	if last, ok := bySession[sid]; ok && evt.Gen <= last {
		n.occMu.Unlock()
		if n.metrics != nil {
			n.metrics.OccupancyGenDiscards.Inc()
		}
		return ErrLateOccupancy
	}
	bySession[sid] = evt.Gen
	n.occMu.Unlock()

	n.deliverPresenceEvent(ch, evt.Event, evt.Gen, sid)
	return nil
}

// onSyntheticLeave builds and emits a leave for a ghost member whose TTL
// evaporated in the Redis presence store (B2 §5.3). The store only knows the
// membership key, so the info is minimal; a fresh generation guarantees the
// leave is newer than any previously applied event for that session.
func (n *Node) onSyntheticLeave(ctx context.Context, ch, sessionID string) {
	if sessionID == "" {
		return
	}
	n.publishOccupancy(ch, OccupancyEvent{
		Event: &clientpb.PresenceEvent{
			Channel: ch,
			Action:  PresenceActionLeave,
			Info:    &clientpb.PresenceInfo{SessionId: sessionID},
		},
	})
}

// deliverPresenceEvent fans a presence event out to every session covered by
// ch: exact subscribers read from the channel's subShard (preserving the
// ephemeral flag) plus wildcard subscribers from the matcher. Recipients are
// deduplicated by session ID; ephemeral subscriptions and the excluded
// session never receive the event. The emitted v2 PresenceEvent carries the
// occupancy generation (B2/§4.4) so clients can order join/leave per channel.
// Delivery counts no MessagesDelivered.
func (n *Node) deliverPresenceEvent(ch string, evt *clientpb.PresenceEvent, gen uint64, excludeSession string) {
	if evt == nil {
		return
	}
	recipients := n.hub.PresenceRecipients(ch)
	if len(recipients) == 0 {
		return
	}

	// The occupancy generation travels with the live event: consumers on the
	// same channel deduplicate join/leave by it (OccupancyGen, not offset).
	evt.Gen = gen

	out := MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_PresenceEvent{PresenceEvent: evt}
	})
	ctx := context.Background()
	send := func(r PresenceRecipient) {
		if err := r.Client.Send(ctx, out); err != nil {
			log.WarnContext(ctx, "failed to send presence event", err, "channel", ch, "session", r.Client.SessionID())
			if n.metrics != nil {
				n.metrics.PresenceFailures.WithLabelValues("deliver").Inc()
				n.metrics.DeliveryFailures.Inc()
			}
		}
	}

	// Same fan-out rhythm as broadcastPublication (serial under a small
	// threshold, bounded goroutines above), but a dedicated loop: presence
	// events are never assembled into publications.
	const presenceParallelThreshold = 8
	if len(recipients) <= presenceParallelThreshold {
		for _, r := range recipients {
			if r.Ephemeral || r.Client.SessionID() == excludeSession {
				continue
			}
			send(r)
		}
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, broadcastParallelLimit)
	for _, r := range recipients {
		if r.Ephemeral || r.Client.SessionID() == excludeSession {
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(r PresenceRecipient) {
			defer func() {
				<-sem
				wg.Done()
			}()
			send(r)
		}(r)
	}
	wg.Wait()
}

// gapNoticeReasonV2 maps a catch-up gap reason to the client-wire GapReason
// (C6). It lives here, not in recover.go's gapReasonV2: the Replayer
// recovery path is untouched by C6.
func gapNoticeReasonV2(reason HistoryGapReason) sharedv2.GapReason {
	switch reason {
	case HistoryGapMiddle:
		return sharedv2.GapReason_GAP_REASON_MIDDLE
	case HistoryGapReplayTruncated:
		return sharedv2.GapReason_GAP_REASON_REPLAY_TRUNCATED
	}
	return sharedv2.GapReason_GAP_REASON_UNSPECIFIED
}

// gapNoticeReasonLabel is the live_gap_notice_total reason label.
func gapNoticeReasonLabel(reason HistoryGapReason) string {
	switch reason {
	case HistoryGapMiddle:
		return "middle"
	case HistoryGapReplayTruncated:
		return "replay_truncated"
	}
	return "unknown"
}

// onGap is the catch-up gap receiver (registered as the broker's gap
// handler, C6). It fans one GapNotice envelope out to every local session
// covered by the gap's channel — exact subscribers plus matching wildcard
// subscribers — so clients learn that reconnect catch-up could not replay
// the full missed range. The notice carries the channel, the gap reason, and
// the last known safe position (broker epoch + last-good offset; offset
// unset when unknown). With no local subscribers nothing is sent and no
// notice metric is counted (the broker's internal gap counter still ran).
// Delivery counts no MessagesDelivered.
func (n *Node) onGap(gap CatchUpGap) {
	recipients := n.hub.GetMatchingSubscribers(gap.Channel)
	if len(recipients) == 0 {
		return
	}

	notice := &clientpb.GapNotice{
		Channel:   gap.Channel,
		Position:  positionFrom(n.streamEpoch(), gap.LastGoodOffset, gap.LastGoodOffset > 0),
		GapReason: gapNoticeReasonV2(gap.Reason),
	}
	out := MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_GapNotice{GapNotice: notice}
	})

	if n.metrics != nil {
		n.metrics.LiveGapNoticeTotal.WithLabelValues(gapNoticeReasonLabel(gap.Reason)).Inc()
	}

	ctx := context.Background()
	for _, c := range recipients {
		if err := c.Send(ctx, out); err != nil {
			log.WarnContext(ctx, "failed to send gap notice", err, "channel", gap.Channel, "session", c.SessionID())
			if n.metrics != nil {
				n.metrics.DeliveryFailures.Inc()
			}
		}
	}
}

// presenceSnapshot builds the current snapshot for ch under the channel
// policy cap (MaxPresenceSnapshotClients unless presence_snapshot_limit
// overrides it). occupancy counts every client, truncated reports that the
// clients list was capped. A store failure yields an empty snapshot plus a
// Warn and op=store — callers keep the subscription alive regardless.
func (n *Node) presenceSnapshot(ctx context.Context, ch string) *clientpb.PresenceSnapshot {
	limit := MaxPresenceSnapshotClients
	if pol := n.ChannelPolicy(ch); pol.PresenceSnapshotLimit > 0 {
		limit = pol.PresenceSnapshotLimit
	}
	clients, err := n.presence.Get(ctx, ch)
	if err != nil {
		log.WarnContext(ctx, "failed to read presence snapshot", err, "channel", ch)
		if n.metrics != nil {
			n.metrics.PresenceFailures.WithLabelValues("store").Inc()
		}
		return &clientpb.PresenceSnapshot{Channel: ch}
	}
	infos := make([]*clientpb.PresenceInfo, 0, len(clients))
	for _, info := range clients {
		infos = append(infos, &clientpb.PresenceInfo{
			SessionId:   firstNonEmpty(info.SessionID, info.ClientID),
			UserId:      info.UserID,
			ClientId:    info.ConnectClientID,
			ConnectedAt: info.ConnectedAt,
		})
	}
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].SessionId < infos[j].SessionId
	})
	snapshot := &clientpb.PresenceSnapshot{Channel: ch, Occupancy: int32(len(infos))}
	if len(infos) > limit {
		snapshot.Clients = infos[:limit]
		snapshot.Truncated = true
	} else {
		snapshot.Clients = infos
	}
	return snapshot
}

// firstNonEmpty returns the first non-empty value, used to fall back from
// the formal session_id to the legacy client_id key in old Redis records.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
