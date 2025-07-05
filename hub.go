package messageloop

import (
	"hash/fnv"
	"sync"
)

const numHubShards = 64

type Hub struct {
	mu         sync.RWMutex
	sessions   map[string]*Client
	connShards [numHubShards]*connShard
	subShards  [numHubShards]*subShard
}

// newHub initializes Hub.
func newHub(maxTimeLagMilli int64) *Hub {
	h := &Hub{
		sessions: map[string]*Client{},
	}
	for i := 0; i < numHubShards; i++ {
		h.connShards[i] = newConnShard()
		h.subShards[i] = newSubShard(maxTimeLagMilli)
	}
	return h
}

func (h *Hub) addSub(ch string, sub subscriber) (bool, error) {
	return h.subShards[index(ch, numHubShards)].addSub(ch, sub)
}

// removeSub removes connection from clientHub subscriptions registry.
func (h *Hub) removeSub(ch string, c *Client) (bool, bool) {
	return h.subShards[index(ch, numHubShards)].removeSub(ch, c)
}

// index chooses bucket number in range [0, numBuckets).
func index(s string, numBuckets int) int {
	if numBuckets == 1 {
		return 0
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(s))
	return int(hash.Sum64() % uint64(numBuckets))
}

type connShard struct {
	mu sync.RWMutex
	// match client ID with actual client connection.
	clients map[string]*Client
	// registry to hold active client connections grouped by user.
	users map[string]map[string]struct{}
}

func newConnShard() *connShard {
	return &connShard{
		clients: make(map[string]*Client),
		users:   make(map[string]map[string]struct{}),
	}
}

type subShard struct {
	mu sync.RWMutex
	// registry to hold active subscriptions of clients to channels with some additional info.
	subs            map[string]map[string]subscriber
	maxTimeLagMilli int64
}

func newSubShard(maxTimeLagMilli int64) *subShard {
	return &subShard{
		subs:            make(map[string]map[string]subscriber),
		maxTimeLagMilli: maxTimeLagMilli,
	}
}

type subscriber struct {
	client    *Client
	ephemeral bool
}

// addSub adds connection into clientHub subscriptions registry.
func (h *subShard) addSub(ch string, sub subscriber) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	uid := sub.client.ID()

	_, ok := h.subs[ch]
	if !ok {
		h.subs[ch] = make(map[string]subscriber)
	}
	h.subs[ch][uid] = sub
	if !ok {
		return true, nil
	}
	return false, nil
}

// removeSub removes connection from clientHub subscriptions registry.
// Returns true if channel does not have any subscribers left in first return value.
// Returns true if found and really removed from registry in second return value.
func (h *subShard) removeSub(ch string, c *Client) (bool, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	uid := c.ID()

	// try to find subscription to delete, return early if not found.
	if _, ok := h.subs[ch]; !ok {
		return true, false
	}
	if _, ok := h.subs[ch][uid]; !ok {
		return true, false
	}

	// actually remove subscription from hub.
	delete(h.subs[ch], uid)

	// clean up subs map if it's needed.
	if len(h.subs[ch]) == 0 {
		delete(h.subs, ch)
		return true, true
	}

	return false, true
}
