package messageloop

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop/config"
	"github.com/messageloopio/messageloop/proxy"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
)

type Node struct {
	hub              *Hub
	broker           Broker
	presence         PresenceStore
	subLocks         map[int]*sync.Mutex
	proxy            *proxy.Router
	heartbeatManager *HeartbeatManager
	rpcTimeout       time.Duration
	surveys          map[string]*Survey
	surveyMu         sync.RWMutex
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
		rpcTimeout: proxy.DefaultRPCTimeout,
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
	return node
}

// Run starts the broker in the background, bound to ctx.
// Waits until the broker's handler is registered before returning.
func (n *Node) Run(ctx context.Context) error {
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

func (n *Node) subLock(ch string) *sync.Mutex {
	return n.subLocks[index(ch, numSubLocks)]
}

func (n *Node) Hub() *Hub {
	return n.hub
}

// AddClient adds a client session to the node's hub.
func (n *Node) AddClient(c *Client) {
	n.hub.add(c)
}

// AddSubscription adds a subscription for a client to a channel.
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
		if err := n.broker.Subscribe(ch); err != nil {
			n.hub.removeSub(ch, sub.Client)
			return err
		}
	}
	return nil
}

// RemoveSubscription removes a subscription for a client from a channel.
func (n *Node) RemoveSubscription(ch string, c *Client) error {
	mu := n.subLock(ch)
	mu.Lock()
	defer mu.Unlock()
	last, removed := n.hub.removeSub(ch, c)
	if removed && last {
		_ = n.broker.Unsubscribe(ch)
	}
	return nil
}

// Publish sends payload to ch via the broker.
func (n *Node) Publish(ch string, payload []byte, isText bool) error {
	_, err := n.broker.Publish(ch, payload, isText)
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
	subscribers := n.hub.GetSubscribers(channel)
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

	return results, nil
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
