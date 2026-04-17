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
	subLocks         map[int]*sync.Mutex
	proxy            *proxy.Router
	heartbeatManager *HeartbeatManager
	rpcTimeout       time.Duration
	limits           config.Limits
	metrics          *Metrics
	surveys          map[string]*Survey
	surveyMu         sync.RWMutex
	acl              *ACLEngine
}

const (
	numSubLocks = 16384
)

func NewNode(cfg *config.Server) *Node {
	subLocks := make(map[int]*sync.Mutex, numSubLocks)
	for i := 0; i < numSubLocks; i++ {
		subLocks[i] = &sync.Mutex{}
	}

	var limits config.Limits
	if cfg != nil {
		limits = cfg.Limits
	}

	node := &Node{
		subLocks:   subLocks,
		hub:        newHub(0, limits.MaxConnectionsPerUser),
		rpcTimeout: proxy.DefaultRPCTimeout,
		limits:     limits,
		surveys:    make(map[string]*Survey),
		presence:   NewMemoryPresenceStore(),
	}

	if cfg != nil && cfg.RPCTimeout != "" {
		rpcTimeout, err := time.ParseDuration(cfg.RPCTimeout)
		if err != nil {
			rpcTimeout = proxy.DefaultRPCTimeout
		}
		node.rpcTimeout = rpcTimeout
	}

	if cfg != nil && cfg.Heartbeat.IdleTimeout != "" {
		idleTimeout, err := time.ParseDuration(cfg.Heartbeat.IdleTimeout)
		if err != nil {
			idleTimeout = 300 * time.Second
		}
		node.heartbeatManager = NewHeartbeatManager(HeartbeatConfig{
			IdleTimeout: idleTimeout,
		})
	}

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

	go func() {
		if err := n.broker.Start(ctx, func(ch string, pub *Publication) error {
			return n.hub.broadcastPublication(ch, pub)
		}); err != nil {
			log.ErrorContext(ctx, "broker stopped with error", err)
		}
	}()
	type readyBroker interface{ Ready() <-chan struct{} }
	if r, ok := n.broker.(readyBroker); ok {
		select {
		case <-r.Ready():
		case <-ctx.Done():
		}
	}
	return nil
}

// Shutdown gracefully drains all client connections.
func (n *Node) Shutdown() {
	n.hub.DrainAll(DisconnectForceNoReconnect)
	if n.cluster != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := n.cluster.Shutdown(ctx); err != nil {
			log.WarnContext(ctx, "cluster shutdown error", "error", err)
		}
	}
}

// SetCluster sets the cluster control-plane coordinator for this node.
func (n *Node) SetCluster(runtime *Cluster) {
	n.cluster = runtime
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
	return n.subLocks[index(ch, numSubLocks)]
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
		n.metrics.ConnectionsTotal.Inc()
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
	log.InfoContext(ctx, "add subscriber", "channel", ch, "sub", sub)
	first, err := n.hub.addSub(ch, sub)
	if err != nil {
		return err
	}
	log.InfoContext(ctx, "added subscriber", "channel", ch, "sub", sub)
	sub.Client.mu.Lock()
	sub.Client.subscribedChannels[ch] = struct{}{}
	sub.Client.mu.Unlock()
	if first {
		if err := n.broker.Subscribe(ch); err != nil {
			n.hub.removeSub(ch, sub.Client)
			sub.Client.mu.Lock()
			delete(sub.Client.subscribedChannels, ch)
			sub.Client.mu.Unlock()
			return err
		}
	}
	if err := n.syncClusterSessionState(ctx, sub.Client); err != nil {
		n.hub.removeSub(ch, sub.Client)
		if first {
			_ = n.broker.Unsubscribe(ch)
		}
		sub.Client.mu.Lock()
		delete(sub.Client.subscribedChannels, ch)
		sub.Client.mu.Unlock()
		return fmt.Errorf("sync cluster session subscription: %w", err)
	}
	if err := n.adjustClusterChannelSubscriptions(ctx, ch, 1); err != nil {
		n.hub.removeSub(ch, sub.Client)
		if first {
			_ = n.broker.Unsubscribe(ch)
		}
		sub.Client.mu.Lock()
		delete(sub.Client.subscribedChannels, ch)
		sub.Client.mu.Unlock()
		_ = n.syncClusterSessionState(ctx, sub.Client)
		return fmt.Errorf("sync cluster channel projection: %w", err)
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
	last, removed := n.hub.removeSub(ch, c)
	if !removed {
		return nil
	}
	c.mu.Lock()
	delete(c.subscribedChannels, ch)
	c.mu.Unlock()
	if removed {
		if last {
			if err := n.broker.Unsubscribe(ch); err != nil {
				n.hub.addSub(ch, subscriber)
				c.mu.Lock()
				c.subscribedChannels[ch] = struct{}{}
				c.mu.Unlock()
				return err
			}
		}
		ctx := context.Background()
		if err := n.syncClusterSessionState(ctx, c); err != nil {
			n.hub.addSub(ch, subscriber)
			if last {
				_ = n.broker.Subscribe(ch)
			}
			c.mu.Lock()
			c.subscribedChannels[ch] = struct{}{}
			c.mu.Unlock()
			return fmt.Errorf("sync cluster session unsubscription: %w", err)
		}
		if err := n.adjustClusterChannelSubscriptions(ctx, ch, -1); err != nil {
			n.hub.addSub(ch, subscriber)
			if last {
				_ = n.broker.Subscribe(ch)
			}
			c.mu.Lock()
			c.subscribedChannels[ch] = struct{}{}
			c.mu.Unlock()
			_ = n.syncClusterSessionState(ctx, c)
			return fmt.Errorf("sync cluster channel projection: %w", err)
		}
		if n.metrics != nil {
			n.metrics.SubscriptionsTotal.Dec()
		}
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
func (n *Node) Publish(ch string, payload []byte, isText bool) error {
	if n.metrics != nil {
		timer := prometheus.NewTimer(n.metrics.PublishDuration)
		defer timer.ObserveDuration()
	}
	_, err := n.broker.Publish(ch, payload, isText)
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

	n.registerSurvey(survey)
	defer n.unregisterSurvey(surveyID)

	var wg sync.WaitGroup
	for _, sub := range subscribers {
		wg.Add(1)
		go func(session *Client) {
			defer wg.Done()
			n.sendSurveyRequest(ctx, session, survey)
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

	if err := session.Send(ctx, msg); err != nil {
		log.WarnContext(ctx, "failed to send survey request", "session", session.SessionID(), "error", err)
		survey.AddResponse(session.SessionID(), nil, err)
	}
}

func (n *Node) registerSurvey(survey *Survey) {
	n.surveyMu.Lock()
	defer n.surveyMu.Unlock()
	n.surveys[survey.ID()] = survey
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
func (n *Node) AddSurveyResponse(ctx context.Context, sessionID string, requestID string, payload []byte, err error) {
	survey := n.getSurvey(requestID)
	if survey == nil {
		log.WarnContext(ctx, "survey not found for response", "request_id", requestID, "session", sessionID)
		return
	}
	survey.AddResponse(sessionID, payload, err)
}

// presenceChannel returns the internal channel name for presence events.
func presenceChannel(ch string) string {
	return ch + "/__presence"
}

// PublishPresenceJoin publishes a presence join event to the channel's presence sub-channel.
func (n *Node) PublishPresenceJoin(channel, clientID, userID string) {
	evt := newPresenceEvent("join", channel, clientID, userID)
	data, err := marshalPresenceEvent(evt)
	if err != nil {
		return
	}
	_ = n.Publish(presenceChannel(channel), data, true)
}

// PublishPresenceLeave publishes a presence leave event to the channel's presence sub-channel.
func (n *Node) PublishPresenceLeave(channel, clientID, userID string) {
	evt := newPresenceEvent("leave", channel, clientID, userID)
	data, err := marshalPresenceEvent(evt)
	if err != nil {
		return
	}
	_ = n.Publish(presenceChannel(channel), data, true)
}
