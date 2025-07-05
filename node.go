package messageloop

import "sync"

type Node struct {
	dispatcher *eventDispatcher
	hub        *Hub
	broker     Broker
	subLocks   map[int]*sync.Mutex
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

	return node
}

func (n *Node) SetBroker(broker Broker) {
	n.broker = broker
}

func (n *Node) subLock(ch string) *sync.Mutex {
	return n.subLocks[index(ch, numSubLocks)]
}

func (n *Node) Hub() *Hub {
	return n.hub
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

func (n *Node) removeSubscription(ch string, c *Client) error {
	mu := n.subLock(ch)
	mu.Lock()
	defer mu.Unlock()
	_, _ = n.hub.removeSub(ch, c)
	return nil
}

type eventDispatcher struct {
}
