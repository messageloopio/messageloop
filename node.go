package messageloop

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/deeplooplabs/messageloop/proxy"
	"github.com/lynx-go/x/log"
)

type Node struct {
	dispatcher *eventDispatcher
	hub        *Hub
	broker     Broker
	subLocks   map[int]*sync.Mutex
	proxy      *proxy.Router
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

func NewNode() *Node {
	subLocks := make(map[int]*sync.Mutex, numSubLocks)
	for i := 0; i < numSubLocks; i++ {
		subLocks[i] = &sync.Mutex{}
	}

	node := &Node{
		subLocks: subLocks,
		hub:      newHub(0),
	}
	broker := newMemoryBroker(node)
	node.SetBroker(broker)

	return node
}

func (n *Node) Run() error {
	if err := n.Broker().RegisterBrokerEventHandler(n); err != nil {
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

func (n *Node) addClient(c *ClientSession) {
	n.hub.add(c)
}

func (n *Node) addSubscription(ctx context.Context, ch string, sub subscriber) error {
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
				n.hub.removeSub(ch, sub.client)
				return err
			}
		}
	}
	return nil
}

func (n *Node) removeSubscription(ch string, c *ClientSession) error {
	mu := n.subLock(ch)
	mu.Lock()
	defer mu.Unlock()
	_, _ = n.hub.removeSub(ch, c)
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

// createProxy creates an RPCProxy instance based on the configuration.
func (n *Node) createProxy(cfg *proxy.ProxyConfig) (proxy.RPCProxy, error) {
	if cfg.GRPC != nil {
		return proxy.NewGRPCProxy(cfg)
	}
	if cfg.HTTP != nil {
		return proxy.NewHTTPProxy(cfg)
	}
	// Auto-detect: if endpoint starts with http:// or https://, use HTTP
	// Otherwise, assume gRPC
	if len(cfg.Endpoint) > 7 && (cfg.Endpoint[:7] == "http://" || cfg.Endpoint[:8] == "https://") {
		return proxy.NewHTTPProxy(cfg)
	}
	return proxy.NewGRPCProxy(cfg)
}

// FindProxy finds a proxy for the given channel and method.
// Returns nil if no matching proxy is found.
func (n *Node) FindProxy(channel, method string) proxy.RPCProxy {
	if n.proxy == nil {
		return nil
	}
	return n.proxy.Match(channel, method)
}

// AddProxy adds a proxy to the router.
func (n *Node) AddProxy(p proxy.RPCProxy, channelPattern, methodPattern string) error {
	if n.proxy == nil {
		n.proxy = proxy.NewRouter()
	}
	return n.proxy.Add(p, channelPattern, methodPattern)
}

// ProxyRPC proxies an RPC request to the configured backend.
func (n *Node) ProxyRPC(ctx context.Context, channel, method string, req *proxy.RPCProxyRequest) (*proxy.RPCProxyResponse, error) {
	p := n.FindProxy(channel, method)
	if p == nil {
		return nil, errors.New("no proxy found for channel/method")
	}
	return p.ProxyRPC(ctx, req)
}
