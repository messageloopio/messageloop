package messageloop

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop/pkg/topics"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
)

const numHubShards = 64

type Hub struct {
	mu              sync.RWMutex
	sessions        map[string]*Client
	connShards      [numHubShards]*connShard
	subShards       [numHubShards]*subShard
	maxConnsPerUser int

	// Wildcard subscription support
	matcher  topics.Matcher
	wcSubsMu sync.Mutex
	wcSubs   map[string]*topics.Subscription // key: "sessionID:channel"
}

// newHub initializes Hub.
func newHub(maxTimeLagMilli int64, maxConnsPerUser int) *Hub {
	h := &Hub{
		sessions:        map[string]*Client{},
		maxConnsPerUser: maxConnsPerUser,
		matcher:         topics.NewCSTrieMatcher(),
		wcSubs:          make(map[string]*topics.Subscription),
	}
	for i := 0; i < numHubShards; i++ {
		h.connShards[i] = newConnShard()
		h.subShards[i] = newSubShard(maxTimeLagMilli)
	}
	return h
}

// isWildcard returns true if the channel pattern contains a wildcard character.
func isWildcard(ch string) bool {
	return strings.Contains(ch, "*")
}

func (h *Hub) addSub(ch string, sub Subscriber) (bool, error) {
	if isWildcard(ch) {
		return h.addWildcardSub(ch, sub)
	}
	return h.subShards[index(ch, numHubShards)].addSub(ch, sub)
}

func (h *Hub) addWildcardSub(ch string, sub Subscriber) (bool, error) {
	h.wcSubsMu.Lock()
	defer h.wcSubsMu.Unlock()

	key := sub.Client.SessionID() + ":" + ch
	if _, exists := h.wcSubs[key]; exists {
		return false, nil
	}
	topicSub, err := h.matcher.Subscribe(ch, sub)
	if err != nil {
		return false, err
	}
	h.wcSubs[key] = topicSub
	return true, nil
}

// removeSub removes connection from clientHub subscriptions registry.
func (h *Hub) removeSub(ch string, c *Client) (bool, bool) {
	if isWildcard(ch) {
		return h.removeWildcardSub(ch, c)
	}
	return h.subShards[index(ch, numHubShards)].removeSub(ch, c)
}

func (h *Hub) removeWildcardSub(ch string, c *Client) (bool, bool) {
	h.wcSubsMu.Lock()
	defer h.wcSubsMu.Unlock()

	key := c.SessionID() + ":" + ch
	topicSub, exists := h.wcSubs[key]
	if !exists {
		return true, false
	}
	h.matcher.Unsubscribe(topicSub)
	delete(h.wcSubs, key)
	return true, true
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

// addWithLimit adds a connection into the registry, enforcing per-user connection limits.
// Returns DisconnectConnectionLimit if maxPerUser > 0 and the limit is reached.
func (h *connShard) addWithLimit(c *Client, maxPerUser int) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	uid := c.SessionID()
	user := c.UserID()

	if maxPerUser > 0 {
		if sessions, ok := h.users[user]; ok && len(sessions) >= maxPerUser {
			return DisconnectConnectionLimit
		}
	}

	h.clients[uid] = c

	if _, ok := h.users[user]; !ok {
		h.users[user] = make(map[string]struct{})
	}
	h.users[user][uid] = struct{}{}
	return nil
}

// remove removes a connection from the registry by session ID.
func (h *connShard) remove(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	client, ok := h.clients[sessionID]
	if !ok {
		return
	}
	delete(h.clients, sessionID)

	user := client.UserID()
	if users, ok := h.users[user]; ok {
		delete(users, sessionID)
		if len(users) == 0 {
			delete(h.users, user)
		}
	}
}

type subShard struct {
	mu sync.RWMutex
	// registry to hold active subscriptions of clients to channels with some additional info.
	subs            map[string]map[string]Subscriber
	maxTimeLagMilli int64
}

func newSubShard(maxTimeLagMilli int64) *subShard {
	return &subShard{
		subs:            make(map[string]map[string]Subscriber),
		maxTimeLagMilli: maxTimeLagMilli,
	}
}

// Subscriber represents a client that can subscribe to channels.
type Subscriber struct {
	Client    *Client
	Ephemeral bool
}

// NewSubscriber creates a new Subscriber.
func NewSubscriber(client *Client, ephemeral bool) Subscriber {
	return Subscriber{
		Client:    client,
		Ephemeral: ephemeral,
	}
}

// NumSubscribers returns number of current subscribers for a given channel.
func (h *subShard) NumSubscribers(ch string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	clients, ok := h.subs[ch]
	if !ok {
		return 0
	}
	return len(clients)
}

// addSub adds connection into clientHub subscriptions registry.
func (h *subShard) addSub(ch string, sub Subscriber) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	uid := sub.Client.SessionID()

	_, ok := h.subs[ch]
	if !ok {
		h.subs[ch] = make(map[string]Subscriber)
	}
	h.subs[ch][uid] = sub
	if !ok {
		return true, nil
	}
	return false, nil
}

//func pubToProto(pub *Publication) *clientpb.Publication {
//	if pub == nil {
//		return nil
//	}
//	return &clientpb.Publication{
//		Offset: pub.Offset,
//		Data:   pub.Data,
//		Info:   infoToProto(pub.Info),
//		Tags:   pub.Tags,
//	}
//}

// removeSub removes connection from clientHub subscriptions registry.
// Returns true if channel does not have any subscribers left in first return value.
// Returns true if found and really removed from registry in second return value.
func (h *subShard) removeSub(ch string, c *Client) (bool, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	uid := c.SessionID()

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

func (h *subShard) broadcastPublication(channel string, pub *Publication) error {
	h.mu.RLock()
	subscribers, ok := h.subs[channel]
	if !ok {
		h.mu.RUnlock()
		return nil
	}

	// Copy subscribers under lock to avoid data race during iteration.
	subs := make([]Subscriber, 0, len(subscribers))
	for _, sub := range subscribers {
		subs = append(subs, sub)
	}
	h.mu.RUnlock()

	ctx := context.Background()

	// Create Payload from publication data
	var payload *sharedpb.Payload
	if len(pub.Payload) > 0 {
		if pub.IsText {
			payload = &sharedpb.Payload{
				Data: &sharedpb.Payload_Text{
					Text: string(pub.Payload),
				},
			}
		} else {
			payload = &sharedpb.Payload{
				Data: &sharedpb.Payload_Binary{
					Binary: pub.Payload,
				},
			}
		}
	}

	msg := &clientpb.Message{
		Channel: channel,
		Id:      uuid.NewString(),
		Offset:  pub.Offset,
		Payload: payload,
	}

	out := MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Publication{Publication: &clientpb.Publication{
			Messages: []*clientpb.Message{msg},
		}}
	})

	const broadcastParallelThreshold = 8

	if len(subs) <= broadcastParallelThreshold {
		// Serial send for small fan-out — avoids goroutine overhead
		for _, sub := range subs {
			if err := sub.Client.Send(ctx, out); err != nil {
				log.ErrorContext(ctx, "send publication error", err)
				if sub.Client.node.metrics != nil {
					sub.Client.node.metrics.DeliveryFailures.Inc()
				}
			} else if sub.Client.node.metrics != nil {
				sub.Client.node.metrics.MessagesDelivered.Inc()
			}
		}
	} else {
		// Parallel send for large fan-out
		var wg sync.WaitGroup
		for _, sub := range subs {
			wg.Add(1)
			go func(sub Subscriber) {
				defer func() {
					if r := recover(); r != nil {
						log.ErrorContext(ctx, "panic in send publication", fmt.Errorf("panic: %v, channel: %s", r, channel))
					}
					wg.Done()
				}()
				if err := sub.Client.Send(ctx, out); err != nil {
					log.ErrorContext(ctx, "send publication error", err)
					if sub.Client.node.metrics != nil {
						sub.Client.node.metrics.DeliveryFailures.Inc()
					}
				} else if sub.Client.node.metrics != nil {
					sub.Client.node.metrics.MessagesDelivered.Inc()
				}
			}(sub)
		}
		wg.Wait()
	}

	return nil
}

// add adds a connection into the hub, enforcing per-user connection limits.
func (h *Hub) add(c *Client) error {
	shard := h.connShards[index(c.UserID(), numHubShards)]
	if err := shard.addWithLimit(c, h.maxConnsPerUser); err != nil {
		return err
	}
	h.mu.Lock()
	if c.SessionID() != "" {
		h.sessions[c.SessionID()] = c
	}
	h.mu.Unlock()
	return nil
}

// NumSubscribers returns number of current subscribers for a given channel.
func (h *Hub) NumSubscribers(ch string) int {
	return h.subShards[index(ch, numHubShards)].NumSubscribers(ch)
}

func (h *Hub) broadcastPublication(ch string, pub *Publication) error {
	// Broadcast to exact subscribers via shards.
	if err := h.subShards[index(ch, numHubShards)].broadcastPublication(ch, pub); err != nil {
		return err
	}

	// Broadcast to wildcard subscribers via matcher.
	wcMatches := h.matcher.Lookup(ch)
	if len(wcMatches) == 0 {
		return nil
	}

	ctx := context.Background()

	// Build the outbound message for wildcard subscribers.
	var payload *sharedpb.Payload
	if len(pub.Payload) > 0 {
		if pub.IsText {
			payload = &sharedpb.Payload{Data: &sharedpb.Payload_Text{Text: string(pub.Payload)}}
		} else {
			payload = &sharedpb.Payload{Data: &sharedpb.Payload_Binary{Binary: pub.Payload}}
		}
	}
	msg := &clientpb.Message{
		Channel: ch,
		Id:      uuid.NewString(),
		Offset:  pub.Offset,
		Payload: payload,
	}
	out := MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Publication{Publication: &clientpb.Publication{
			Messages: []*clientpb.Message{msg},
		}}
	})

	var wg sync.WaitGroup
	for _, match := range wcMatches {
		sub, ok := match.(Subscriber)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(sub Subscriber) {
			defer func() {
				if r := recover(); r != nil {
					log.ErrorContext(ctx, "panic in wildcard send publication", fmt.Errorf("panic: %v, channel: %s", r, ch))
				}
				wg.Done()
			}()
			if err := sub.Client.Send(ctx, out); err != nil {
				log.ErrorContext(ctx, "wildcard send publication error", err)
				if sub.Client.node.metrics != nil {
					sub.Client.node.metrics.DeliveryFailures.Inc()
				}
			} else if sub.Client.node.metrics != nil {
				sub.Client.node.metrics.MessagesDelivered.Inc()
			}
		}(sub)
	}
	wg.Wait()
	return nil
}

// RemoveSession removes a session from the sessions map and connShards.
func (h *Hub) RemoveSession(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Get the client session to find the user ID before deleting
	session, ok := h.sessions[sessionID]
	if !ok {
		return
	}
	delete(h.sessions, sessionID)

	// Also remove from connShards
	userID := session.UserID()
	h.connShards[index(userID, numHubShards)].remove(sessionID)
}

// GetSubscribers returns a copy of all subscribers for a given channel.
func (h *Hub) GetSubscribers(ch string) []*Client {
	shard := h.subShards[index(ch, numHubShards)]
	shard.mu.RLock()
	defer shard.mu.RUnlock()

	subscribers, ok := shard.subs[ch]
	if !ok {
		return nil
	}

	result := make([]*Client, 0, len(subscribers))
	for _, sub := range subscribers {
		result = append(result, sub.Client)
	}
	return result
}

// GetMatchingSubscribers returns exact and wildcard subscribers that match the given channel.
func (h *Hub) GetMatchingSubscribers(ch string) []*Client {
	matched := make(map[string]*Client)
	for _, client := range h.GetSubscribers(ch) {
		matched[client.SessionID()] = client
	}

	for _, candidate := range h.matcher.Lookup(ch) {
		sub, ok := candidate.(Subscriber)
		if !ok || sub.Client == nil {
			continue
		}
		matched[sub.Client.SessionID()] = sub.Client
	}

	result := make([]*Client, 0, len(matched))
	for _, client := range matched {
		result = append(result, client)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].SessionID() < result[j].SessionID()
	})
	return result
}

// LookupSubscriber returns the current subscriber record for a client/channel pair.
func (h *Hub) LookupSubscriber(ch string, c *Client) (Subscriber, bool) {
	if isWildcard(ch) {
		h.wcSubsMu.Lock()
		defer h.wcSubsMu.Unlock()
		topicSub, ok := h.wcSubs[c.SessionID()+":"+ch]
		if !ok {
			return Subscriber{}, false
		}
		sub, ok := topicSub.Subscriber.(Subscriber)
		return sub, ok
	}

	shard := h.subShards[index(ch, numHubShards)]
	shard.mu.RLock()
	defer shard.mu.RUnlock()

	subscribers, ok := shard.subs[ch]
	if !ok {
		return Subscriber{}, false
	}
	sub, ok := subscribers[c.SessionID()]
	return sub, ok
}

// LookupSession returns a client session by session ID.
// Returns nil if session not found.
func (h *Hub) LookupSession(sessionID string) *Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.sessions[sessionID]
}

// DrainAll sends a disconnect to all connected clients and waits for them to close.
func (h *Hub) DrainAll(disconnect Disconnect) {
	h.mu.RLock()
	sessions := make([]*Client, 0, len(h.sessions))
	for _, c := range h.sessions {
		sessions = append(sessions, c)
	}
	h.mu.RUnlock()

	var wg sync.WaitGroup
	for _, c := range sessions {
		wg.Add(1)
		go func(c *Client) {
			defer wg.Done()
			_ = c.Close(disconnect)
		}(c)
	}
	wg.Wait()
}

// ChannelInfo holds channel name and subscriber count for admin queries.
type ChannelInfo struct {
	Name        string
	Subscribers int
}

// GetActiveChannels returns all channels with at least one subscriber, along with subscriber counts.
func (h *Hub) GetActiveChannels() []ChannelInfo {
	counts := make(map[string]int)
	for i := 0; i < numHubShards; i++ {
		shard := h.subShards[i]
		shard.mu.RLock()
		for ch, subs := range shard.subs {
			if len(subs) > 0 {
				counts[ch] += len(subs)
			}
		}
		shard.mu.RUnlock()
	}

	h.wcSubsMu.Lock()
	for _, sub := range h.wcSubs {
		if sub == nil || sub.Topic == "" {
			continue
		}
		counts[sub.Topic]++
	}
	h.wcSubsMu.Unlock()

	result := make([]ChannelInfo, 0, len(counts))
	for ch, count := range counts {
		if count <= 0 {
			continue
		}
		result = append(result, ChannelInfo{Name: ch, Subscribers: count})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}

// ReplaceSession atomically replaces a session's client reference in the sessions map
// and all subscription shards. Used for session resumption.
func (h *Hub) ReplaceSession(sessionID string, newClient *Client) {
	h.mu.Lock()
	oldClient, exists := h.sessions[sessionID]
	if !exists {
		h.mu.Unlock()
		return
	}
	h.sessions[sessionID] = newClient
	h.mu.Unlock()

	// Replace in connShards: remove old, add new
	oldUserID := oldClient.UserID()
	h.connShards[index(oldUserID, numHubShards)].remove(sessionID)
	newShard := h.connShards[index(newClient.UserID(), numHubShards)]
	newShard.mu.Lock()
	newShard.clients[sessionID] = newClient
	uid := newClient.UserID()
	if _, ok := newShard.users[uid]; !ok {
		newShard.users[uid] = make(map[string]struct{})
	}
	newShard.users[uid][sessionID] = struct{}{}
	newShard.mu.Unlock()

	// Replace subscriber references in all subShards
	for i := 0; i < numHubShards; i++ {
		shard := h.subShards[i]
		shard.mu.Lock()
		for _, subs := range shard.subs {
			if sub, ok := subs[sessionID]; ok {
				sub.Client = newClient
				subs[sessionID] = sub
			}
		}
		shard.mu.Unlock()
	}
}
