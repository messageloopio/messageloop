package messageloop

import (
	"sync"
)

type Node struct {
	dispatcher *eventDispatcher
	hub        *Hub
	broker     Broker
	subLocks   map[int]*sync.Mutex
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

func (n *Node) HandleJoin(ch string, info *ClientInfo) error {
	return nil
}

func (n *Node) HandleLeave(ch string, info *ClientInfo) error {
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

func (n *Node) addSubscription(ch string, sub subscriber) error {
	mu := n.subLock(ch)
	mu.Lock()
	defer mu.Unlock()
	first, err := n.hub.addSub(ch, sub)
	if err != nil {
		return err
	}
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
