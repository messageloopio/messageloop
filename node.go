package messageloop

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
	"github.com/messageloopio/messageloop/config"
	"github.com/messageloopio/messageloop/proxy"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	"github.com/prometheus/client_golang/prometheus"
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
	acl              *ACLEngine
	requireAuth      bool
	healthCheck      func(context.Context) error
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
		hub:         newHub(0, limits.MaxConnectionsPerUser),
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
	// linger forever when the operator forgets to configure it.
	idleTimeout := DefaultHeartbeatIdleTimeout
	if cfg != nil && cfg.Heartbeat.IdleTimeout != "" {
		parsed, err := time.ParseDuration(cfg.Heartbeat.IdleTimeout)
		if err != nil {
			parsed = DefaultHeartbeatIdleTimeout
		}
		idleTimeout = parsed
	}
	node.heartbeatManager = NewHeartbeatManager(HeartbeatConfig{
		IdleTimeout: idleTimeout,
	})

	node.broker = NewMemoryBroker(MemoryBrokerOptions{})

	if cfg != nil && len(cfg.ACL.Rules) > 0 {
		rules := make([]ACLRule, len(cfg.ACL.Rules))
		for i, r := range cfg.ACL.Rules {
			rules[i] = ACLRule{
				ChannelPattern: r.ChannelPattern,
				AllowSubscribe: r.AllowSubscribe,
				AllowPublish:   r.AllowPublish,
				DenyAll:        r.DenyAll,
			}
		}
		node.acl = NewACLEngine(rules)
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

	// Broker failures are funneled through an error channel instead of a
	// panic: a broker that fails to start (e.g. Redis unreachable) must
	// surface as a Run error so the caller (lynx) can react, rather than
	// crashing the process after Run has returned (P1-A6).
	startErr := make(chan error, 1)
	go func() {
		if err := n.broker.Start(ctx, func(ch string, pub *Publication) error {
			return n.hub.broadcastPublication(ch, pub)
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
func (n *Node) SetPresenceForSession(ctx context.Context, ch string, c *Client) error {
	if c == nil {
		return nil
	}
	return n.presence.Add(ctx, ch, &PresenceInfo{
		ClientID:    c.SessionID(),
		UserID:      c.UserID(),
		ConnectedAt: c.connectedAt.UnixMilli(),
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

// AddClient adds a client session to the node's hub.
// Returns an error (DisconnectConnectionLimit) if the per-user connection limit is exceeded.
func (n *Node) AddClient(c *Client) error {
	if err := n.hub.add(c); err != nil {
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

	if _, exists := n.hub.LookupSubscriber(ch, sub.Client); exists {
		return nil
	}

	var first bool

	// Each step captures the state needed to commit and undo itself.
	err := runSubSaga([]subSagaStep{
		{
			name: "hub.addSub",
			commit: func() (err error) {
				first, err = n.hub.addSub(ch, sub)
				return err
			},
			rollback: func() {
				_, _ = n.hub.removeSub(ch, sub.Client)
			},
		},
		{
			name: "track.client",
			commit: func() error {
				sub.Client.mu.Lock()
				defer sub.Client.mu.Unlock()
				// Reject subscriptions on a closing/closed client: close()
				// snapshots subscribedChannels under this same lock, so a
				// subscribe admitted here after the snapshot would never be
				// cleaned up and would leak in the hub (P1-A3).
				if sub.Client.status == statusClosed {
					return fmt.Errorf("client is closed")
				}
				sub.Client.subscribedChannels[ch] = struct{}{}
				return nil
			},
			rollback: func() {
				sub.Client.mu.Lock()
				delete(sub.Client.subscribedChannels, ch)
				sub.Client.mu.Unlock()
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
				return n.syncClusterSessionState(ctx, sub.Client)
			},
			rollback: func() {
				rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = n.syncClusterSessionState(rctx, sub.Client)
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
				last, removed = n.hub.removeSub(ch, c)
				if !removed {
					return fmt.Errorf("subscription not found for channel %s", ch)
				}
				return nil
			},
			rollback: func() {
				_, _ = n.hub.addSub(ch, subscriber)
			},
		},
		{
			name: "untrack.client",
			commit: func() error {
				c.mu.Lock()
				delete(c.subscribedChannels, ch)
				c.mu.Unlock()
				return nil
			},
			rollback: func() {
				c.mu.Lock()
				c.subscribedChannels[ch] = struct{}{}
				c.mu.Unlock()
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

func (n *Node) sendSurveyRequest(ctx context.Context, session *Client, survey *Survey) {
	var payload *sharedpb.Payload
	if len(survey.Payload()) > 0 {
		payload = &sharedpb.Payload{
			Data: &sharedpb.Payload_Binary{
				Binary: survey.Payload(),
			},
		}
	}

	msg := MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_SurveyRequest{
			SurveyRequest: &clientpb.SurveyRequest{
				RequestId: survey.ID(),
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
func (n *Node) PublishPresenceJoin(channel, clientID, userID string) {
	evt := newPresenceEvent("join", channel, clientID, userID)
	data, err := marshalPresenceEvent(evt)
	if err != nil {
		return
	}
	err = n.PublishTransient(presenceChannel(channel), &Publication{Payload: data, Kind: PayloadKindText})
	if err != nil {
		log.WarnContext(context.Background(), "failed to publish presence join event",
			err, "channel", channel, "client_id", clientID)
		if n.metrics != nil {
			n.metrics.PresencePublishFailures.Inc()
		}
	}
}

// PublishPresenceLeave publishes a presence leave event to the channel's presence sub-channel.
// Presence events are transient: they are delivered in real time but never
// written to broker history, so they do not leak into the recovery stream.
func (n *Node) PublishPresenceLeave(channel, clientID, userID string) {
	evt := newPresenceEvent("leave", channel, clientID, userID)
	data, err := marshalPresenceEvent(evt)
	if err != nil {
		return
	}
	err = n.PublishTransient(presenceChannel(channel), &Publication{Payload: data, Kind: PayloadKindText})
	if err != nil {
		log.WarnContext(context.Background(), "failed to publish presence leave event",
			err, "channel", channel, "client_id", clientID)
		if n.metrics != nil {
			n.metrics.PresencePublishFailures.Inc()
		}
	}
}
