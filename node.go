package messageloop

import (
	"context"
	"fmt"
	"sync"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/binding/format/protobuf/v2/pb"
	"github.com/google/uuid"
	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop/config"
	clientpb "github.com/messageloopio/messageloop/genproto/v1"
	"github.com/messageloopio/messageloop/proxy"
)

type Node struct {
	dispatcher       *eventDispatcher
	hub              *Hub
	broker           Broker
	subLocks         map[int]*sync.Mutex
	proxy            *proxy.Router
	heartbeatManager *HeartbeatManager
	rpcTimeout       time.Duration
	surveys          map[string]*Survey
	surveyMu         sync.RWMutex
}

func (n *Node) HandlePublication(ch string, pub *Publication) error {
	return n.handlePublication(ch, pub)
}

func (n *Node) handlePublication(ch string, pub *Publication) error {

	numSubscribers := n.hub.NumSubscribers(ch)
	if numSubscribers == 0 {
		return nil
	}
	return n.hub.broadcastPublication(ch, pub)
}

func (n *Node) HandleJoin(ch string, info *ClientDesc) error {
	return nil
}

func (n *Node) HandleLeave(ch string, info *ClientDesc) error {
	return nil
}

const (
	numSubLocks = 16384
)

func NewNode(cfg *config.Server) *Node {
	subLocks := make(map[int]*sync.Mutex, numSubLocks)
	for i := 0; i < numSubLocks; i++ {
		subLocks[i] = &sync.Mutex{}
	}

	node := &Node{
		subLocks:   subLocks,
		hub:        newHub(0),
		rpcTimeout: proxy.DefaultRPCTimeout, // Default 30s
		surveys:    make(map[string]*Survey),
	}

	// Initialize RPC timeout if config is provided
	if cfg != nil && cfg.RPCTimeout != "" {
		rpcTimeout, err := time.ParseDuration(cfg.RPCTimeout)
		if err != nil {
			rpcTimeout = proxy.DefaultRPCTimeout
		}
		node.rpcTimeout = rpcTimeout
	}

	// Initialize heartbeat manager if config is provided
	if cfg != nil && cfg.Heartbeat.IdleTimeout != "" {
		idleTimeout, err := time.ParseDuration(cfg.Heartbeat.IdleTimeout)
		if err != nil {
			idleTimeout = 300 * time.Second
		}

		node.heartbeatManager = NewHeartbeatManager(HeartbeatConfig{
			IdleTimeout: idleTimeout,
		})
	}

	broker := newMemoryBroker(node)
	node.SetBroker(broker)

	return node
}

func (n *Node) Run() error {
	if err := n.Broker().RegisterEventHandler(n); err != nil {
		return err
	}
	return nil
}

var _ BrokerEventHandler = new(Node)

func (n *Node) SetBroker(broker Broker) {
	n.broker = broker
}

func (n *Node) subLock(ch string) *sync.Mutex {
	return n.subLocks[index(ch, numSubLocks)]
}

func (n *Node) Hub() *Hub {
	return n.hub
}

// AddClient adds a client session to the node's hub.
func (n *Node) AddClient(c *ClientSession) {
	n.addClient(c)
}

func (n *Node) addClient(c *ClientSession) {
	n.hub.add(c)
}

// AddSubscription adds a subscription for a client to a channel.
// This is an exported method for use by the server-side API.
func (n *Node) AddSubscription(ctx context.Context, ch string, sub Subscriber) error {
	mu := n.subLock(ch)
	mu.Lock()
	defer mu.Unlock()
	log.InfoContext(ctx, "add subscriber", "channel", ch, "sub", sub)
	first, err := n.hub.addSub(ch, sub)
	if err != nil {
		return err
	}
	log.InfoContext(ctx, "added subscriber", "channel", ch, "sub", sub)
	if first {
		if n.broker != nil {
			if err := n.broker.Subscribe(ch); err != nil {
				n.hub.removeSub(ch, sub.Client)
				return err
			}
		}
	}
	return nil
}

// RemoveSubscription removes a subscription for a client from a channel.
// This is an exported method for use by the server-side API.
func (n *Node) RemoveSubscription(ch string, c *ClientSession) error {
	mu := n.subLock(ch)
	mu.Lock()
	defer mu.Unlock()
	last, removed := n.hub.removeSub(ch, c)
	if removed && last && n.broker != nil {
		_ = n.broker.Unsubscribe(ch)
	}
	return nil
}

type eventDispatcher struct {
}

func (n *Node) Publish(channel string, data []byte, opts ...PublishOption) error {
	pubOpts := PublishOptions{}
	for _, opt := range opts {
		opt(&pubOpts)
	}
	_, _, err := n.Broker().Publish(channel, data, pubOpts)
	return err
}

func (n *Node) Broker() Broker {
	return n.broker
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

// createProxy creates a Proxy instance based on the configuration.
func (n *Node) createProxy(cfg *proxy.ProxyConfig) (proxy.Proxy, error) {
	if cfg.GRPC != nil {
		return proxy.NewGRPCProxy(cfg)
	}
	if cfg.HTTP != nil {
		return proxy.NewHTTPProxy(cfg)
	}
	// Auto-detect: if endpoint starts with http:// or https://, use HTTP
	// Otherwise, assume gRPC
	if (len(cfg.Endpoint) >= 7 && cfg.Endpoint[:7] == "http://") ||
		(len(cfg.Endpoint) >= 8 && cfg.Endpoint[:8] == "https://") {
		return proxy.NewHTTPProxy(cfg)
	}
	return proxy.NewGRPCProxy(cfg)
}

// FindProxy finds a proxy for the given channel and method.
// Returns nil if no matching proxy is found.
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
// Returns 0 if heartbeat manager is not configured.
func (n *Node) GetHeartbeatIdleTimeout() time.Duration {
	if n.heartbeatManager != nil {
		return n.heartbeatManager.Config().IdleTimeout
	}
	return 0
}

// Survey sends a request to all subscribers of a channel and collects responses.
// Returns collected results or error if the channel has no subscribers.
func (n *Node) Survey(ctx context.Context, channel string, payload []byte, timeout time.Duration) ([]*SurveyResult, error) {
	// Get subscribers for the channel
	subscribers := n.hub.GetSubscribers(channel)
	if len(subscribers) == 0 {
		return []*SurveyResult{}, nil
	}

	// Create survey instance
	surveyID := uuid.NewString()
	survey := NewSurvey(surveyID, channel, payload, timeout)

	// Register survey for response collection
	n.registerSurvey(survey)
	defer n.unregisterSurvey(surveyID)

	// Send survey request to all subscribers concurrently
	var wg sync.WaitGroup
	for _, sub := range subscribers {
		wg.Add(1)
		go func(session *ClientSession) {
			defer wg.Done()
			n.sendSurveyRequest(ctx, session, survey)
		}(sub)
	}

	// Wait for all sends to complete
	wg.Wait()

	// Wait for responses or timeout
	results := survey.Wait(ctx)
	survey.Close()

	return results, nil
}

// sendSurveyRequest sends a survey request to a single client session.
func (n *Node) sendSurveyRequest(ctx context.Context, session *ClientSession, survey *Survey) {
	// Create the CloudEvent for the survey request
	event := &cloudevents.CloudEvent{
		Id:          survey.ID(),
		Source:      survey.Channel(),
		SpecVersion: "1.0",
		Type:        "com.messageloop.survey",
		Data: &cloudevents.CloudEvent_BinaryData{
			BinaryData: survey.Payload(),
		},
	}

	// Create outbound message with survey request
	msg := MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_SurveyRequest{
			SurveyRequest: &clientpb.SurveyRequest{
				RequestId: survey.ID(),
				Payload:   event,
			},
		}
	})

	// Send to client (fire and forget, responses come through handleMessage)
	if err := session.Send(ctx, msg); err != nil {
		log.WarnContext(ctx, "failed to send survey request", "session", session.SessionID(), "error", err)
		survey.AddResponse(session.SessionID(), nil, err)
	}
}

// registerSurvey registers a survey in the registry.
func (n *Node) registerSurvey(survey *Survey) {
	n.surveyMu.Lock()
	defer n.surveyMu.Unlock()
	n.surveys[survey.ID()] = survey
}

// unregisterSurvey removes a survey from the registry.
func (n *Node) unregisterSurvey(surveyID string) {
	n.surveyMu.Lock()
	defer n.surveyMu.Unlock()
	delete(n.surveys, surveyID)
}

// getSurvey retrieves a survey by ID.
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
