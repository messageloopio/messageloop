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

const (
	numHubShards = 64
	// broadcastParallelLimit caps the number of concurrent per-subscriber
	// sends during a publication broadcast to avoid unbounded goroutine
	// growth on channels with thousands of subscribers.
	broadcastParallelLimit = 64
)

// publicationID builds the stable, globally unique message ID for a
// publication: channel + offset. Realtime delivery and connect-time history
// recovery use the same rule so clients can deduplicate by ID.
func publicationID(channel string, offset uint64) string {
	return fmt.Sprintf("%s-%d", channel, offset)
}

// publicationMessageID builds the message ID delivered for a publication.
// Publications with a broker-assigned offset use the stable channel-offset
// rule (realtime and recovery agree). Transient publications (offset 0, e.g.
// presence join/leave events) fall back to a per-event UUID: every transient
// event on a channel would otherwise share the same ID "channel-0" and
// clients could not distinguish them.
func publicationMessageID(channel string, offset uint64) string {
	if offset > 0 {
		return publicationID(channel, offset)
	}
	return uuid.NewString()
}

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

	// node back-reference lets broadcastPublication delegate presence frame
	// rewrites to the node's deliverPresenceEvent. Set by NewNode.
	node *Node
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
	// Exact channels never reach the matcher, so their validity is checked
	// here: channels with explicit empty segments ("a.", ".a", "a..b") and
	// the empty channel are rejected with ErrBadTopic instead of being
	// registered silently (B1).
	if err := topics.ValidateTopic(ch); err != nil {
		return false, err
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
	// DeliveredOffset is the highest offset of a publication successfully
	// delivered to Client on this exact channel. Maintained by the broadcast
	// path under the subShard lock (see recordDeliveredOffsets) and read by
	// the cluster snapshot path via LookupSubscriber; it feeds
	// ClusterSessionSnapshot.ChannelOffsets for exact cross-node resume.
	// Zero when nothing was delivered yet (transient publications carry
	// offset 0 and never update it). Wildcard subscriptions never track it.
	DeliveredOffset uint64
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

// add adds a connection into the hub, enforcing per-user connection limits.
func (h *Hub) add(c *Client) error {
	// h.mu is taken before the connShard lock, matching RemoveSessionIfMatches
	// and ReplaceSession: addWithLimit checks the per-user limit and registers
	// the connection atomically under the shard lock, and the sessions map
	// update is serialized under h.mu.
	h.mu.Lock()
	defer h.mu.Unlock()
	shard := h.connShards[index(c.UserID(), numHubShards)]
	if err := shard.addWithLimit(c, h.maxConnsPerUser); err != nil {
		return err
	}
	if c.SessionID() != "" {
		h.sessions[c.SessionID()] = c
	}
	return nil
}

// NumSubscribers returns number of current subscribers for a given channel.
func (h *Hub) NumSubscribers(ch string) int {
	return h.subShards[index(ch, numHubShards)].NumSubscribers(ch)
}

func (h *Hub) broadcastPublication(ch string, pub *Publication) error {
	// Presence frames (ml.type=presence) never become chat publications:
	// they are rewritten into first-class presence events and dropped here.
	// Phase 1 emit never produces such frames (emitPresence is local-only),
	// so this branch is exercised by injected tests.
	if pub != nil && pub.Metadata[PresenceMetaTypeKey] == PresenceMetaTypeValue {
		evt := parsePresencePublication(pub)
		if evt == nil {
			log.WarnContext(context.Background(), "dropping unparseable presence publication", "channel", ch)
			if h.node != nil && h.node.metrics != nil {
				h.node.metrics.PresenceFailures.WithLabelValues("rewrite").Inc()
			}
			return nil
		}
		if evt.Channel == "" {
			evt.Channel = ch
		}
		if h.node != nil {
			h.node.deliverPresenceEvent(evt.Channel, evt, "")
		}
		return nil
	}

	// Merge exact and wildcard subscribers by session ID: a client subscribed
	// to the channel exactly and via a wildcard pattern must receive the
	// publication only once, with a single message ID.
	subscribers := make(map[string]*Client)
	for _, client := range h.GetSubscribers(ch) {
		subscribers[client.SessionID()] = client
	}
	for _, candidate := range h.matcher.Lookup(ch) {
		sub, ok := candidate.(Subscriber)
		if !ok || sub.Client == nil {
			continue
		}
		subscribers[sub.Client.SessionID()] = sub.Client
	}
	if len(subscribers) == 0 {
		return nil
	}

	clients := make([]*Client, 0, len(subscribers))
	for _, client := range subscribers {
		clients = append(clients, client)
	}

	ctx := context.Background()

	// Create Payload from publication data, preserving the original
	// oneof variant (Binary/Text/JSON).
	payload := pub.PayloadProto()

	msg := &clientpb.Message{
		Channel: ch,
		Id:      publicationMessageID(ch, pub.Offset),
		Offset:  pub.Offset,
		Payload: payload,
		Metadata: func() *sharedpb.Metadata {
			if len(pub.Metadata) == 0 {
				return nil
			}
			return &sharedpb.Metadata{Entries: pub.Metadata}
		}(),
	}

	out := MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Publication{Publication: &clientpb.Publication{
			Messages: []*clientpb.Message{msg},
		}}
	})

	const broadcastParallelThreshold = 8

	// delivered marks the positions of clients that received the publication,
	// so the last-delivered-offset bookkeeping below only counts successful
	// sends. One slot per client: each fan-out goroutine writes only its own
	// index, so no locking is needed.
	delivered := make([]bool, len(clients))

	if len(clients) <= broadcastParallelThreshold {
		// Serial send for small fan-out — avoids goroutine overhead
		for i, client := range clients {
			// A panic in one send must not take down the broker handler;
			// the parallel branch below recovers too.
			func(i int) {
				defer func() {
					if r := recover(); r != nil {
						log.ErrorContext(ctx, "panic in send publication", fmt.Errorf("panic: %v, channel: %s", r, ch))
					}
				}()
				if err := client.Send(ctx, out); err != nil {
					log.ErrorContext(ctx, "send publication error", err)
					if client.node.metrics != nil {
						client.node.metrics.DeliveryFailures.Inc()
					}
				} else {
					delivered[i] = true
					if client.node.metrics != nil {
						client.node.metrics.MessagesDelivered.Inc()
					}
				}
			}(i)
		}
	} else {
		// Parallel send for large fan-out, bounded to broadcastParallelLimit
		// concurrent goroutines.
		var wg sync.WaitGroup
		sem := make(chan struct{}, broadcastParallelLimit)
		for i, client := range clients {
			sem <- struct{}{}
			wg.Add(1)
			go func(i int, client *Client) {
				defer func() {
					if r := recover(); r != nil {
						log.ErrorContext(ctx, "panic in send publication", fmt.Errorf("panic: %v, channel: %s", r, ch))
					}
					<-sem
					wg.Done()
				}()
				if err := client.Send(ctx, out); err != nil {
					log.ErrorContext(ctx, "send publication error", err)
					if client.node.metrics != nil {
						client.node.metrics.DeliveryFailures.Inc()
					}
				} else {
					delivered[i] = true
					if client.node.metrics != nil {
						client.node.metrics.MessagesDelivered.Inc()
					}
				}
			}(i, client)
		}
		wg.Wait()
	}

	// Record the last successfully delivered offset per exact subscription.
	// Transient publications (offset 0) never update the bookkeeping.
	if pub.Offset > 0 {
		h.recordDeliveredOffsets(ch, pub.Offset, clients, delivered)
	}

	return nil
}

// recordDeliveredOffsets updates the last successfully delivered offset for
// every exact subscription of ch that received the publication. The update
// runs in a single pass under one subShard write lock per publication, so the
// broadcast hot path pays one short lock acquisition regardless of fan-out
// size, never one per subscriber. Wildcard patterns are not channels and
// never receive offset tracking (their deliveries are not resumable
// per-channel); the guard keeps their records untouched.
func (h *Hub) recordDeliveredOffsets(ch string, offset uint64, clients []*Client, delivered []bool) {
	if isWildcard(ch) || len(delivered) == 0 {
		return
	}
	shard := h.subShards[index(ch, numHubShards)]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	subs, ok := shard.subs[ch]
	if !ok {
		return
	}
	for i, client := range clients {
		if !delivered[i] {
			continue
		}
		uid := client.SessionID()
		sub, ok := subs[uid]
		if !ok {
			// The subscription was removed between the fan-out and this
			// pass (e.g. concurrent unsubscribe/close): nothing to record.
			continue
		}
		// Max-guard: concurrent publications may deliver out of order, but
		// the recorded offset must never regress.
		if sub.DeliveredOffset < offset {
			sub.DeliveredOffset = offset
			subs[uid] = sub
		}
	}
}

// RemoveSession removes a session from the sessions map and connShards.
func (h *Hub) RemoveSession(sessionID string) {
	h.RemoveSessionIfMatches(sessionID, nil)
}

// RemoveSessionIfMatches removes a session from the sessions map and connShards
// only when the registered client is the given client (nil matches any client).
// On close this prevents a failed or stale connection from evicting a session
// that a newer client has taken over or is resuming. It returns true when the
// session was removed.
func (h *Hub) RemoveSessionIfMatches(sessionID string, c *Client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Get the client session to find the user ID before deleting
	session, ok := h.sessions[sessionID]
	if !ok {
		return false
	}
	if c != nil && session != c {
		return false
	}
	delete(h.sessions, sessionID)

	// Also remove from connShards
	userID := session.UserID()
	h.connShards[index(userID, numHubShards)].remove(sessionID)
	return true
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

// presenceRecipient couples a subscriber client with its subscription's
// ephemeral flag. GetMatchingSubscribers loses the flag, so the presence
// delivery path collects recipients here instead.
type presenceRecipient struct {
	client    *Client
	ephemeral bool
}

// presenceRecipients returns the clients covered by ch — subscribed exactly
// (read from the channel's subShard) or via a matching wildcard pattern
// (matcher lookup) — deduplicated by session ID, together with each
// subscription's ephemeral flag.
func (h *Hub) presenceRecipients(ch string) []presenceRecipient {
	recipients := make(map[string]presenceRecipient)
	add := func(client *Client, ephemeral bool) {
		if client == nil {
			return
		}
		sid := client.SessionID()
		if existing, ok := recipients[sid]; ok {
			// A session covered by any non-ephemeral subscription must
			// receive events. An ephemeral exact sub must not hide a
			// tracked wildcard (or the reverse).
			if existing.ephemeral && !ephemeral {
				recipients[sid] = presenceRecipient{client: client, ephemeral: false}
			}
			return
		}
		recipients[sid] = presenceRecipient{client: client, ephemeral: ephemeral}
	}

	shard := h.subShards[index(ch, numHubShards)]
	shard.mu.RLock()
	if subs, ok := shard.subs[ch]; ok {
		for _, sub := range subs {
			add(sub.Client, sub.Ephemeral)
		}
	}
	shard.mu.RUnlock()

	for _, candidate := range h.matcher.Lookup(ch) {
		sub, ok := candidate.(Subscriber)
		if !ok {
			continue
		}
		add(sub.Client, sub.Ephemeral)
	}

	result := make([]presenceRecipient, 0, len(recipients))
	for _, r := range recipients {
		result = append(result, r)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].client.SessionID() < result[j].client.SessionID()
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

// GetActiveChannels returns all channels with at least one subscriber, along
// with subscriber counts. Wildcard patterns are not channels (a subscription
// to "chat.*" subscribes the matcher, not a channel), so they are neither
// listed nor counted; the exact-channel counts are already unique per session
// because each shard keys subscribers by session ID.
func (h *Hub) GetActiveChannels() []ChannelInfo {
	counts := make(map[string]int)
	for i := 0; i < numHubShards; i++ {
		shard := h.subShards[i]
		shard.mu.RLock()
		for ch, subs := range shard.subs {
			if len(subs) > 0 {
				counts[ch] = len(subs)
			}
		}
		shard.mu.RUnlock()
	}

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
// and all subscription shards. Used for session resumption. It enforces the same
// per-user connection limit as addWithLimit: replacing with a client of a different
// user that already sits at the limit fails with DisconnectConnectionLimit.
func (h *Hub) ReplaceSession(sessionID string, newClient *Client) error {
	h.mu.Lock()
	oldClient, exists := h.sessions[sessionID]
	if !exists {
		h.mu.Unlock()
		return nil
	}

	// Per-user connection limit: a same-user replacement keeps the user's
	// connection count unchanged; a different user must have room. The limit
	// check and the shard registration are atomic under the shard lock (a
	// concurrent AddClient only takes the shard lock), so the last slot
	// cannot be claimed in between (TOCTOU fix). h.mu is held throughout so
	// a concurrent close() cannot evict the session mid-replacement.
	oldIdx := index(oldClient.UserID(), numHubShards)
	newIdx := index(newClient.UserID(), numHubShards)
	newShard := h.connShards[newIdx]
	newShard.mu.Lock()
	if h.maxConnsPerUser > 0 && oldClient.UserID() != newClient.UserID() &&
		len(newShard.users[newClient.UserID()]) >= h.maxConnsPerUser {
		newShard.mu.Unlock()
		h.mu.Unlock()
		return DisconnectConnectionLimit
	}
	if oldIdx != newIdx {
		h.connShards[oldIdx].remove(sessionID)
	} else {
		// Same shard: the old entry is overwritten below; drop the old
		// user's registration first so the count does not leak.
		if users, ok := newShard.users[oldClient.UserID()]; ok {
			delete(users, sessionID)
			if len(users) == 0 {
				delete(newShard.users, oldClient.UserID())
			}
		}
	}
	newShard.clients[sessionID] = newClient
	uid := newClient.UserID()
	if _, ok := newShard.users[uid]; !ok {
		newShard.users[uid] = make(map[string]struct{})
	}
	newShard.users[uid][sessionID] = struct{}{}
	newShard.mu.Unlock()

	h.sessions[sessionID] = newClient
	h.mu.Unlock()

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

	// Replace wildcard subscriptions. The matcher stores the Subscriber as an
	// interface value copy, so each subscription of this session must be
	// rebuilt: Unsubscribe the old record, Subscribe with the new client.
	h.wcSubsMu.Lock()
	for key, topicSub := range h.wcSubs {
		if !strings.HasPrefix(key, sessionID+":") {
			continue
		}
		sub, ok := topicSub.Subscriber.(Subscriber)
		if !ok {
			continue
		}
		h.matcher.Unsubscribe(topicSub)
		newSub, err := h.matcher.Subscribe(topicSub.Topic, Subscriber{Client: newClient, Ephemeral: sub.Ephemeral})
		if err != nil {
			log.ErrorContext(context.Background(), "failed to rebuild wildcard subscription during session replace",
				err, "topic", topicSub.Topic, "session", sessionID)
			delete(h.wcSubs, key)
			continue
		}
		h.wcSubs[key] = newSub
	}
	h.wcSubsMu.Unlock()
	return nil
}
