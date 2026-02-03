package messageloop

import (
	"context"
	"hash/fnv"
	"sync"

	cloudevents "github.com/cloudevents/sdk-go/binding/format/protobuf/v2/pb"
	clientpb "github.com/fleetlit/messageloop/genproto/v1"
	"github.com/google/uuid"
	"github.com/lynx-go/x/log"
)

const numHubShards = 64

type Hub struct {
	mu         sync.RWMutex
	sessions   map[string]*ClientSession
	connShards [numHubShards]*connShard
	subShards  [numHubShards]*subShard
}

// newHub initializes Hub.
func newHub(maxTimeLagMilli int64) *Hub {
	h := &Hub{
		sessions: map[string]*ClientSession{},
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
func (h *Hub) removeSub(ch string, c *ClientSession) (bool, bool) {
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
	clients map[string]*ClientSession
	// registry to hold active client connections grouped by user.
	users map[string]map[string]struct{}
}

func newConnShard() *connShard {
	return &connShard{
		clients: make(map[string]*ClientSession),
		users:   make(map[string]map[string]struct{}),
	}
}

// Add connection into clientHub connections registry.
func (h *connShard) add(c *ClientSession) {
	h.mu.Lock()
	defer h.mu.Unlock()

	uid := c.SessionID()
	user := c.UserID()

	h.clients[uid] = c

	if _, ok := h.users[user]; !ok {
		h.users[user] = make(map[string]struct{})
	}
	h.users[user][uid] = struct{}{}
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
	client    *ClientSession
	ephemeral bool
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
func (h *subShard) addSub(ch string, sub subscriber) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	uid := sub.client.SessionID()

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
func (h *subShard) removeSub(ch string, c *ClientSession) (bool, bool) {
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
	subscribers, ok := h.subs[channel]
	if !ok {
		return nil
	}

	ctx := context.TODO()
	msg := &clientpb.Message{
		Channel: channel,
		Id:      uuid.NewString(),
		Offset:  pub.Offset,
	}

	// Create CloudEvent from publication payload
	if len(pub.Payload) > 0 {
		msg.Payload = &cloudevents.CloudEvent{
			Id:          msg.Id,
			Source:      channel,
			SpecVersion: "1.0",
			Type:        channel + ".message",
			Attributes: map[string]*cloudevents.CloudEventAttributeValue{
				"datacontenttype": {
					Attr: &cloudevents.CloudEventAttributeValue_CeString{
						CeString: "application/octet-stream",
					},
				},
			},
			Data: &cloudevents.CloudEvent_BinaryData{
				BinaryData: pub.Payload,
			},
		}
	}

	out := MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Publication{Publication: &clientpb.Publication{
			Envelopes: []*clientpb.Message{msg},
		}}
	})

	for _, sub := range subscribers {
		if err := sub.client.Send(ctx, out); err != nil {
			log.ErrorContext(ctx, "send publication error", err)
			continue
		}
	}

	return nil
}

// Add connection into clientHub connections registry.
func (h *Hub) add(c *ClientSession) {
	h.mu.Lock()
	if c.SessionID() != "" {
		h.sessions[c.SessionID()] = c
	}
	h.mu.Unlock()
	h.connShards[index(c.UserID(), numHubShards)].add(c)
}

// NumSubscribers returns number of current subscribers for a given channel.
func (h *Hub) NumSubscribers(ch string) int {
	return h.subShards[index(ch, numHubShards)].NumSubscribers(ch)
}

func (h *Hub) broadcastPublication(ch string, pub *Publication) error {
	return h.subShards[index(ch, numHubShards)].broadcastPublication(ch, pub)
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
