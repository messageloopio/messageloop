package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/lynx-go/x/log"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/messageloopio/messageloop/pkg/topics"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
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

// Hub is the connection registry. Sessions and subscriptions are sharded to
// reduce lock contention (64 session shards, 16384 subscription shards).
type Hub struct {
	mu              sync.RWMutex
	sessions        map[string]*Session
	connShards      [numHubShards]*connShard
	subShards       [numHubShards]*subShard
	maxConnsPerUser int

	// Wildcard subscription support
	matcher  topics.Matcher
	wcSubsMu sync.Mutex
	wcSubs   map[string]*topics.Subscription // key: "sessionID:channel"
}

// NewHub initializes a Hub. maxTimeLagMilli bounds how far a subscriber's
// queue may lag before it is dropped; maxConnsPerUser limits concurrent
// connections per user (0 means unlimited).
func NewHub(maxTimeLagMilli int64, maxConnsPerUser int) *Hub {
	h := &Hub{
		sessions:        map[string]*Session{},
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

// AddSub registers a subscriber on a channel. Wildcard patterns go to the
// topic matcher; exact channels are validated and stored in a sub shard.
// It reports whether the subscriber is new to the channel.
func (h *Hub) AddSub(ch string, sub Subscriber) (bool, error) {
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

	key := sub.Session.SessionID() + ":" + ch
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
func (h *Hub) RemoveSub(ch string, c *Session) (bool, bool) {
	if isWildcard(ch) {
		return h.removeWildcardSub(ch, c)
	}
	return h.subShards[index(ch, numHubShards)].removeSub(ch, c)
}

func (h *Hub) removeWildcardSub(ch string, c *Session) (bool, bool) {
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
	clients map[string]*Session
	// registry to hold active client connections grouped by user.
	users map[string]map[string]struct{}
}

func newConnShard() *connShard {
	return &connShard{
		clients: make(map[string]*Session),
		users:   make(map[string]map[string]struct{}),
	}
}

// addWithLimit adds a connection into the registry, enforcing per-user connection limits.
// Returns DisconnectConnectionLimit if maxPerUser > 0 and the limit is reached.
func (h *connShard) addWithLimit(c *Session, maxPerUser int) error {
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

// Subscriber represents a session subscribed to a channel.
type Subscriber struct {
	Session   *Session
	Ephemeral bool
	// DeliveredOffset is the highest offset of a publication successfully
	// delivered to Session on this exact channel. Maintained by the broadcast
	// path under the subShard lock (see recordDeliveredOffsets) and read by
	// the cluster snapshot path via LookupSubscriber; it feeds
	// ClusterSessionSnapshot.ChannelOffsets for exact cross-node resume.
	// Zero when nothing was delivered yet (transient publications carry
	// offset 0 and never update it). Wildcard subscriptions never track it.
	DeliveredOffset uint64
}

// NewSubscriber creates a new Subscriber.
func NewSubscriber(session *Session, ephemeral bool) Subscriber {
	return Subscriber{
		Session:   session,
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

	uid := sub.Session.SessionID()

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
func (h *subShard) removeSub(ch string, c *Session) (bool, bool) {
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
func (h *Hub) Add(c *Session) error {
	// h.mu is taken before the connShard lock, matching RemoveSessionIfMatches
	// and PrepareSessionUser: addWithLimit checks the per-user limit and registers
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

func (h *Hub) BroadcastPublication(ch string, pub *Publication) error {
	// Merge exact and wildcard subscribers by session ID: a client subscribed
	// to the channel exactly and via a wildcard pattern must receive the
	// publication only once, with a single message ID.
	subscribers := make(map[string]*Session)
	for _, client := range h.GetSubscribers(ch) {
		subscribers[client.SessionID()] = client
	}
	for _, candidate := range h.matcher.Lookup(ch) {
		sub, ok := candidate.(Subscriber)
		if !ok || sub.Session == nil {
			continue
		}
		subscribers[sub.Session.SessionID()] = sub.Session
	}
	if len(subscribers) == 0 {
		return nil
	}

	clients := make([]*Session, 0, len(subscribers))
	for _, client := range subscribers {
		clients = append(clients, client)
	}

	ctx := context.Background()

	// Create Payload from publication data, preserving the original
	// oneof variant (Binary/Text/JSON). The message position carries the
	// broker StreamEpoch plus the channel offset.
	payload := pub.PayloadProtoV2()

	// jsonRaw carries the original payload bytes when the publication is a
	// valid JSON object: JSON-encoding subscribers get them spliced into the
	// frame verbatim instead of a json.Unmarshal→structpb→protojson round
	// trip, which loses integer precision beyond 2^53 (float64) and key
	// order. Non-object JSON (arrays, scalars) cannot be represented by
	// structpb and keeps the PayloadProtoV2 degrade-to-text behavior.
	var jsonRaw []byte
	if pub.Kind == PayloadKindJSON && isJSONObject(pub.Payload) {
		jsonRaw = bytes.TrimSpace(pub.Payload)
	}
	// jsonPlaceholderPayload stands in for the payload while the JSON frame
	// is marshaled, so protojson emits the empty-Struct splice point.
	jsonPlaceholderPayload := &sharedv2.Payload{
		ContentType: pub.ContentType,
		Data:        &sharedv2.Payload_Json{Json: &structpb.Struct{}},
	}

	msg := &clientpb.Message{
		Channel:  ch,
		Id:       publicationMessageID(ch, pub.Offset),
		Position: positionFrom(pub.Epoch, pub.Offset, true),
		Payload:  payload,
		Metadata: func() *sharedv2.Metadata {
			if len(pub.Metadata) == 0 {
				return nil
			}
			return &sharedv2.Metadata{Entries: pub.Metadata}
		}(),
	}

	out := MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Publication{Publication: &clientpb.Publication{
			Messages: []*clientpb.Message{msg},
		}}
	})

	const broadcastParallelThreshold = 8

	// Serialize the publication once per distinct wire encoding: subscribers
	// sharing a marshaler receive the same frame bytes, so the fan-out pays
	// one MarshalAppend (and one heap copy) per encoding instead of per
	// subscriber. clientEncodings[i] is the marshaler of clients[i].
	clientEncodings := make([]Marshaler, len(clients))
	encodings := make(map[Marshaler]struct{}, 2)
	for i, client := range clients {
		m := client.currentMarshaler()
		clientEncodings[i] = m
		encodings[m] = struct{}{}
	}
	control := outboundFrameClass(out)
	frames := make(map[Marshaler][]byte, len(encodings))
	marshalErrs := make(map[Marshaler]error, len(encodings))
	for m := range encodings {
		buf := getBuffer()
		var b []byte
		var err error
		spliced := false
		if jsonRaw != nil && m.Name() == (JSONMarshaler{}).Name() {
			// Swap in the empty-Struct placeholder so protojson emits the
			// splice point, then graft the raw payload bytes in. The loop is
			// sequential: the swap is restored before the next encoding
			// marshals, and sendFrame's re-marshal fallback always sees the
			// real payload.
			msg.Payload = jsonPlaceholderPayload
			b, err = m.MarshalAppend((*buf)[:0], out)
			msg.Payload = payload
			if err == nil {
				// spliceRawJSONPayload returns a fresh slice, so the pooled
				// buffer needs no copy below.
				b = spliceRawJSONPayload(b, jsonRaw)
				spliced = true
			}
		} else {
			b, err = m.MarshalAppend((*buf)[:0], out)
		}
		if err != nil {
			putBuffer(buf)
			marshalErrs[m] = err
			continue
		}
		// The frame bytes are written asynchronously by each session's writer
		// goroutine, so they must outlive the pooled buffer: copy once per
		// encoding, then share the copy across that encoding's subscribers.
		if spliced {
			frames[m] = b
		} else {
			frames[m] = append([]byte(nil), b...)
		}
		putBuffer(buf)
	}

	// delivered marks the positions of clients that received the publication,
	// so the last-delivered-offset bookkeeping below only counts successful
	// sends. One slot per client: each fan-out goroutine writes only its own
	// index, so no locking is needed.
	delivered := make([]bool, len(clients))

	// sendOne delivers the pre-marshaled frame to one subscriber and records
	// the outcome; a marshal failure for the client's encoding fails all of
	// that encoding's sends with the same error.
	sendOne := func(i int, client *Session) {
		m := clientEncodings[i]
		err, failed := marshalErrs[m]
		if !failed {
			err = client.sendFrame(ctx, frames[m], control, m, out)
		}
		if err != nil {
			log.ErrorContext(ctx, "send publication error", err)
			if client.rt.Metrics() != nil {
				client.rt.Metrics().DeliveryFailures.Inc()
			}
		} else {
			delivered[i] = true
			if client.rt.Metrics() != nil {
				client.rt.Metrics().MessagesDelivered.Inc()
			}
		}
	}

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
				sendOne(i, client)
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
			go func(i int, client *Session) {
				defer func() {
					if r := recover(); r != nil {
						log.ErrorContext(ctx, "panic in send publication", fmt.Errorf("panic: %v, channel: %s", r, ch))
					}
					<-sem
					wg.Done()
				}()
				sendOne(i, client)
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

// isJSONObject reports whether data holds a valid JSON object. It decides
// whether a JSON-kind payload may pass through to JSON frames verbatim:
// protojson renders the Payload json oneof (a structpb.Struct) as a JSON
// object, so only objects can be spliced without changing the wire shape.
func isJSONObject(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(trimmed)
}

// jsonPayloadSplicePoint is the exact protojson rendering of an empty Struct
// in the Payload json oneof field. protojson escapes quotes inside string
// values, so this byte sequence can only occur at the payload site.
const jsonPayloadSplicePoint = `"json":{}`

// spliceRawJSONPayload replaces the empty-Struct placeholder in frame with
// the raw payload bytes, returning a fresh slice. If the placeholder is
// missing (defensive), the frame is returned unchanged.
func spliceRawJSONPayload(frame, raw []byte) []byte {
	i := bytes.Index(frame, []byte(jsonPayloadSplicePoint))
	if i < 0 {
		return frame
	}
	// `"json":` is kept from the placeholder; only the `{}` is replaced.
	out := make([]byte, 0, len(frame)+len(raw)-2)
	out = append(out, frame[:i+len(jsonPayloadSplicePoint)-2]...)
	out = append(out, raw...)
	out = append(out, frame[i+len(jsonPayloadSplicePoint):]...)
	return out
}

// recordDeliveredOffsets updates the last successfully delivered offset for
// every exact subscription of ch that received the publication. The update
// runs in a single pass under one subShard write lock per publication, so the
// broadcast hot path pays one short lock acquisition regardless of fan-out
// size, never one per subscriber. Wildcard patterns are not channels and
// never receive offset tracking (their deliveries are not resumable
// per-channel); the guard keeps their records untouched.
func (h *Hub) recordDeliveredOffsets(ch string, offset uint64, clients []*Session, delivered []bool) {
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
func (h *Hub) RemoveSessionIfMatches(sessionID string, c *Session) bool {
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
func (h *Hub) GetSubscribers(ch string) []*Session {
	shard := h.subShards[index(ch, numHubShards)]
	shard.mu.RLock()
	defer shard.mu.RUnlock()

	subscribers, ok := shard.subs[ch]
	if !ok {
		return nil
	}

	result := make([]*Session, 0, len(subscribers))
	for _, sub := range subscribers {
		result = append(result, sub.Session)
	}
	return result
}

// GetMatchingSubscribers returns exact and wildcard subscribers that match the given channel.
func (h *Hub) GetMatchingSubscribers(ch string) []*Session {
	matched := make(map[string]*Session)
	for _, client := range h.GetSubscribers(ch) {
		matched[client.SessionID()] = client
	}

	for _, candidate := range h.matcher.Lookup(ch) {
		sub, ok := candidate.(Subscriber)
		if !ok || sub.Session == nil {
			continue
		}
		matched[sub.Session.SessionID()] = sub.Session
	}

	result := make([]*Session, 0, len(matched))
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
type PresenceRecipient struct {
	Client    *Session
	Ephemeral bool
}

// presenceRecipients returns the clients covered by ch — subscribed exactly
// (read from the channel's subShard) or via a matching wildcard pattern
// (matcher lookup) — deduplicated by session ID, together with each
// subscription's ephemeral flag.
func (h *Hub) PresenceRecipients(ch string) []PresenceRecipient {
	recipients := make(map[string]PresenceRecipient)
	add := func(client *Session, ephemeral bool) {
		if client == nil {
			return
		}
		sid := client.SessionID()
		if existing, ok := recipients[sid]; ok {
			// A session covered by any non-ephemeral subscription must
			// receive events. An ephemeral exact sub must not hide a
			// tracked wildcard (or the reverse).
			if existing.Ephemeral && !ephemeral {
				recipients[sid] = PresenceRecipient{Client: client, Ephemeral: false}
			}
			return
		}
		recipients[sid] = PresenceRecipient{Client: client, Ephemeral: ephemeral}
	}

	shard := h.subShards[index(ch, numHubShards)]
	shard.mu.RLock()
	if subs, ok := shard.subs[ch]; ok {
		for _, sub := range subs {
			add(sub.Session, sub.Ephemeral)
		}
	}
	shard.mu.RUnlock()

	for _, candidate := range h.matcher.Lookup(ch) {
		sub, ok := candidate.(Subscriber)
		if !ok {
			continue
		}
		add(sub.Session, sub.Ephemeral)
	}

	result := make([]PresenceRecipient, 0, len(recipients))
	for _, r := range recipients {
		result = append(result, r)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Client.SessionID() < result[j].Client.SessionID()
	})
	return result
}

// LookupSubscriber returns the current subscriber record for a client/channel pair.
func (h *Hub) LookupSubscriber(ch string, c *Session) (Subscriber, bool) {
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
func (h *Hub) LookupSession(sessionID string) *Session {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.sessions[sessionID]
}

// SessionsByUser returns a copy of all local client sessions registered under
// userID, sorted by session ID. An empty userID always returns an empty slice
// even when the per-user registry contains anonymous connections under the
// empty key: anonymous sessions are never addressable by the user-based admin
// API.
func (h *Hub) SessionsByUser(userID string) []*Session {
	if userID == "" {
		return nil
	}
	shard := h.connShards[index(userID, numHubShards)]
	shard.mu.RLock()
	defer shard.mu.RUnlock()

	sessionIDs, ok := shard.users[userID]
	if !ok {
		return nil
	}
	result := make([]*Session, 0, len(sessionIDs))
	for sessionID := range sessionIDs {
		if client, ok := shard.clients[sessionID]; ok {
			result = append(result, client)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].SessionID() < result[j].SessionID()
	})
	return result
}

// DrainAll sends a disconnect to all connected clients and waits for them to close.
func (h *Hub) DrainAll(disconnect Disconnect) {
	h.mu.RLock()
	sessions := make([]*Session, 0, len(h.sessions))
	for _, c := range h.sessions {
		sessions = append(sessions, c)
	}
	h.mu.RUnlock()

	var wg sync.WaitGroup
	for _, c := range sessions {
		wg.Add(1)
		go func(c *Session) {
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

// PrepareSessionUser atomically moves a session's connShard registration to a
// new user, enforcing maxConnsPerUser for the target user. It backs the
// cross-user local resume: the limit check and the migration run under the
// shard lock (matching addWithLimit) so a concurrent AddClient cannot claim
// the last slot in between (TOCTOU fix). The sessions map entry is untouched:
// the session pointer stays stable. A same-user call is a no-op. On failure
// nothing is mutated, so the old session stays fully Attached.
func (h *Hub) PrepareSessionUser(sessionID string, c *Session, newUser string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	oldUser := c.UserID()
	if oldUser == newUser {
		return nil
	}
	oldIdx := index(oldUser, numHubShards)
	newIdx := index(newUser, numHubShards)
	if oldIdx == newIdx {
		shard := h.connShards[oldIdx]
		shard.mu.Lock()
		defer shard.mu.Unlock()
		if h.maxConnsPerUser > 0 && len(shard.users[newUser]) >= h.maxConnsPerUser {
			return DisconnectConnectionLimit
		}
		if users, ok := shard.users[oldUser]; ok {
			delete(users, sessionID)
			if len(users) == 0 {
				delete(shard.users, oldUser)
			}
		}
		shard.clients[sessionID] = c
		if _, ok := shard.users[newUser]; !ok {
			shard.users[newUser] = make(map[string]struct{})
		}
		shard.users[newUser][sessionID] = struct{}{}
		return nil
	}

	// Different shards: check the target limit first, then move the entry.
	newShard := h.connShards[newIdx]
	newShard.mu.Lock()
	defer newShard.mu.Unlock()
	if h.maxConnsPerUser > 0 && len(newShard.users[newUser]) >= h.maxConnsPerUser {
		return DisconnectConnectionLimit
	}
	oldShard := h.connShards[oldIdx]
	oldShard.mu.Lock()
	delete(oldShard.clients, sessionID)
	if users, ok := oldShard.users[oldUser]; ok {
		delete(users, sessionID)
		if len(users) == 0 {
			delete(oldShard.users, oldUser)
		}
	}
	oldShard.mu.Unlock()
	newShard.clients[sessionID] = c
	if _, ok := newShard.users[newUser]; !ok {
		newShard.users[newUser] = make(map[string]struct{})
	}
	newShard.users[newUser][sessionID] = struct{}{}
	return nil
}
