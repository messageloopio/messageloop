package messageloop

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop/pkg/topics"
)

const defaultMemoryHistorySize = 256

// MemoryBrokerOptions configures the memory broker.
type MemoryBrokerOptions struct {
	// HistorySize is the ring buffer capacity per channel.
	// 0 uses the default of 256. Disable history by using a Broker
	// that ignores Publish's history parameter (not yet exposed here).
	HistorySize int
}

// NewMemoryBroker returns an in-process Broker with ring buffer history.
func NewMemoryBroker(opts MemoryBrokerOptions) Broker {
	size := opts.HistorySize
	if size == 0 {
		size = defaultMemoryHistorySize
	}
	return &memoryBroker{
		historySize: size,
		history:     make(map[string]*channelHistory),
		subs:        make(map[string]int),
		wcCounts:    make(map[string]int),
		wcHandles:   make(map[string]*topics.Subscription),
		matcher:     topics.NewCSTrieMatcher(),
		ready:       make(chan struct{}),
		epoch:       uuid.NewString(),
	}
}

// channelHistory is a fixed-capacity ring buffer for one channel.
type channelHistory struct {
	mu      sync.Mutex
	entries []*Publication // ring buffer; len == size
	size    int            // ring capacity, fixed at first publish for this channel
	head    int            // index of oldest valid entry
	count   int            // number of valid entries
	nextOff uint64         // next offset to assign (1-based)
	ttlWarn sync.Once      // one history_ttl ignore warning per channel
}

type memoryBroker struct {
	handler     atomic.Pointer[PublicationHandler]
	occHandler  atomic.Pointer[OccupancyHandler]
	historySize int
	epoch       string

	mu      sync.RWMutex
	history map[string]*channelHistory
	subs    map[string]int // exact channel subscriber count
	// Wildcard interest mirrors the Redis broker: patterns are reference
	// counted and matched against concrete channels via the topic matcher.
	wcCounts  map[string]int                 // wildcard pattern refcount
	wcHandles map[string]*topics.Subscription // pattern -> matcher handle
	matcher   topics.Matcher                 // wildcard pattern matching
	ready     chan struct{}
	once      sync.Once
}

// Start stores the handler and blocks until ctx is cancelled.
// The memory broker requires no background goroutines.
func (b *memoryBroker) Start(ctx context.Context, handler PublicationHandler) error {
	b.handler.Store(&handler)
	b.once.Do(func() { close(b.ready) })
	<-ctx.Done()
	return nil
}

// Ready returns a channel that is closed once the handler has been registered.
func (b *memoryBroker) Ready() <-chan struct{} {
	return b.ready
}

// Subscribe registers the node's interest in ch. Wildcard patterns are
// matched against concrete channels via the topic matcher (same semantics as
// the Redis broker's interested()); exact channels and patterns are both
// reference counted. The channel's history is retained while at least one
// subscriber is registered. Keys that CompileInterest rejects (unroutable
// patterns like "*.room", bare "*"/"**", malformed topics) are refused with
// ErrPatternNotRoutable / ErrBadTopic before any state changes (A3).
func (b *memoryBroker) Subscribe(ch string) error {
	if _, err := CompileInterest(ch); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if isWildcard(ch) {
		b.wcCounts[ch]++
		if b.wcCounts[ch] == 1 {
			sub, err := b.matcher.Subscribe(ch, ch)
			if err != nil {
				delete(b.wcCounts, ch)
				return err
			}
			b.wcHandles[ch] = sub
		}
		return nil
	}
	b.subs[ch]++
	return nil
}

// Unsubscribe decrements the subscriber count for ch. When the last
// subscriber leaves and the channel has no retained history entries, the
// channel's history entry is reclaimed so the history map does not grow
// without bound. History is intentionally retained while the last subscriber
// is away so that reconnect with recovery still works; a channel that has
// ever published (nextOff > 0) is never reclaimed by an empty ring, because
// its offsets must stay detectable as gaps for recovering clients. The ring
// buffer capacity bounds the retained entries per channel.
//
// Publish takes b.mu only to resolve the channelHistory reference and releases
// it before taking h.mu; Unsubscribe holds b.mu while deleting the map entry,
// so a Publish that already resolved the reference keeps writing to a live
// object guarded by h.mu — safe.
func (b *memoryBroker) Unsubscribe(ch string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if isWildcard(ch) {
		if b.wcCounts[ch] > 0 {
			b.wcCounts[ch]--
			if b.wcCounts[ch] == 0 {
				delete(b.wcCounts, ch)
				if sub, ok := b.wcHandles[ch]; ok {
					b.matcher.Unsubscribe(sub)
					delete(b.wcHandles, ch)
				}
			}
		}
		return nil
	}
	if b.subs[ch] > 0 {
		b.subs[ch]--
	}
	if b.subs[ch] == 0 {
		delete(b.subs, ch)
		if h, ok := b.history[ch]; ok {
			h.mu.Lock()
			reclaim := h.count == 0 && h.nextOff == 0
			h.mu.Unlock()
			if reclaim {
				delete(b.history, ch)
			}
		}
	}
	return nil
}

// interested reports whether this node wants messages for the given concrete
// channel: an exact subscription or any wildcard pattern that matches it
// (aligned with the Redis broker's interested()).
func (b *memoryBroker) interested(ch string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.subs[ch] > 0 {
		return true
	}
	return len(b.matcher.Lookup(ch)) > 0
}

// Publish writes the publication to the channel ring and, only when this
// node is interested (exact or wildcard match), delivers it to the handler.
// The handler's error or panic never negates the publish: the offset is
// already assigned and the history entry written, so the failure is logged
// and Publish still returns (offset, nil).
func (b *memoryBroker) Publish(ch string, pub *Publication) (uint64, error) {
	// Channels with explicit empty segments ("a.", ".a", "a..b") and the
	// empty channel are rejected up front so malformed channels never produce
	// history entries or handler invocations (B1).
	if err := topics.ValidateTopic(ch); err != nil {
		return 0, err
	}
	b.mu.Lock()
	h, ok := b.history[ch]
	if !ok {
		// The ring capacity is decided on the channel's first publish: a
		// per-publication HistorySize wins, otherwise the broker global.
		// Existing rings are never resized by later HistorySize values;
		// they are reclaimed (and re-created with the new size) only when
		// the last subscriber leaves and the ring is empty.
		cap := b.historySize
		if pub.HistorySize > 0 {
			cap = pub.HistorySize
		}
		h = &channelHistory{entries: make([]*Publication, cap), size: cap}
		b.history[ch] = h
	}
	b.mu.Unlock()

	if pub.HistoryTTL != 0 {
		// The memory broker has no history TTL; the warning is emitted once
		// per channel to avoid log spam on high-frequency channels.
		h.ttlWarn.Do(func() {
			log.WarnContext(context.Background(), "memory broker: history_ttl is not supported, ignoring",
				"channel", ch, "history_ttl", pub.HistoryTTL)
		})
	}

	h.mu.Lock()
	h.nextOff++
	offset := h.nextOff
	stored := *pub
	stored.Channel = ch
	stored.Offset = offset
	stored.Epoch = b.epoch
	stored.Time = time.Now().UnixMilli()
	pub.Offset = offset
	slot := (h.head + h.count) % h.size
	if h.count == h.size {
		// Buffer full: overwrite oldest entry and advance head.
		h.entries[h.head] = &stored
		h.head = (h.head + 1) % h.size
	} else {
		h.entries[slot] = &stored
		h.count++
	}
	h.mu.Unlock()

	if handler := b.handler.Load(); handler != nil && b.interested(ch) {
		ctx := context.Background()
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.ErrorContext(ctx, "memory broker: publish handler panicked; publish stands",
						fmt.Errorf("panic: %v", r), "channel", ch, "offset", offset)
				}
			}()
			if err := (*handler)(ch, &stored); err != nil {
				log.ErrorContext(ctx, "memory broker: publish handler failed; publish stands",
					err, "channel", ch, "offset", offset)
			}
		}()
	}
	return offset, nil
}

// SetOccupancyHandler registers the live occupancy handler. Occupancy events
// never reach the publication handler.
func (b *memoryBroker) SetOccupancyHandler(handler OccupancyHandler) error {
	b.occHandler.Store(&handler)
	return nil
}

// SetGapHandler is a no-op: the memory broker has no pub/sub reconnect and
// therefore no catch-up, so catch-up gaps cannot occur (C6).
func (b *memoryBroker) SetGapHandler(handler GapHandler) {}

// PublishOccupancy invokes the occupancy handler only when this node is
// interested in ch (exact or wildcard match), mirroring PublishTransient's
// interest gate. It never writes history. Synchronous by design (B2 §5.2);
// the handler's error or panic is logged and never fails the Join/Leave.
func (b *memoryBroker) PublishOccupancy(ch string, evt OccupancyEvent) error {
	if err := topics.ValidateTopic(ch); err != nil {
		return err
	}
	handler := b.occHandler.Load()
	if handler == nil || !b.interested(ch) {
		return nil
	}
	ctx := context.Background()
	func() {
		defer func() {
			if r := recover(); r != nil {
				log.ErrorContext(ctx, "memory broker: occupancy handler panicked; event dropped",
					fmt.Errorf("panic: %v", r), "channel", ch)
			}
		}()
		if err := (*handler)(ch, evt); err != nil {
			log.ErrorContext(ctx, "memory broker: occupancy handler failed; event dropped",
				err, "channel", ch)
		}
	}()
	return nil
}

// PublishTransient delivers payload to subscribers in real time without
// writing history. The offset is always 0 because transient publications
// have no history entry. Like Publish, the handler is only invoked when this
// node is interested, and a handler error/panic never propagates.
func (b *memoryBroker) PublishTransient(ch string, pub *Publication) error {
	if err := topics.ValidateTopic(ch); err != nil {
		return err
	}
	stored := *pub
	stored.Channel = ch
	stored.Offset = 0
	stored.Epoch = b.epoch
	stored.Time = time.Now().UnixMilli()
	if handler := b.handler.Load(); handler != nil && b.interested(ch) {
		ctx := context.Background()
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.ErrorContext(ctx, "memory broker: transient handler panicked; publish stands",
						fmt.Errorf("panic: %v", r), "channel", ch)
				}
			}()
			if err := (*handler)(ch, &stored); err != nil {
				log.ErrorContext(ctx, "memory broker: transient handler failed; publish stands",
					err, "channel", ch)
			}
		}()
	}
	return nil
}

// History returns a page of publications stored for ch with offset >=
// sinceOffset, plus gap metadata (§5 of the A2 spec): sinceOffset 0 reads
// from the head (no gap); a positive sinceOffset with no retained entries is
// HistoryGapEmptyExpired; retained entries starting after sinceOffset are
// HistoryGapHeadTrimmed. FirstRetained is the oldest retained offset, or 0
// when nothing is retained.
func (b *memoryBroker) History(ch string, sinceOffset uint64, limit int) (*HistoryPage, error) {
	b.mu.RLock()
	h, ok := b.history[ch]
	b.mu.RUnlock()

	page := &HistoryPage{}
	if !ok {
		// Nothing ever published (or the empty ring was reclaimed): an empty
		// page is a clean read only from the beginning; any positive offset
		// cannot be proven covered.
		if sinceOffset > 0 {
			page.Gap = true
			page.GapReason = HistoryGapEmptyExpired
		}
		return page, nil
	}

	if limit <= 0 {
		limit = DefaultHistoryLimit
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.count == 0 {
		if sinceOffset > 0 {
			page.Gap = true
			page.GapReason = HistoryGapEmptyExpired
		}
		return page, nil
	}

	firstRetained := h.entries[h.head].Offset
	page.FirstRetained = firstRetained
	if sinceOffset > 0 && firstRetained > sinceOffset {
		page.Gap = true
		page.GapReason = HistoryGapHeadTrimmed
	}

	var result []*Publication
	for i := 0; i < h.count; i++ {
		pub := h.entries[(h.head+i)%h.size]
		if pub == nil || pub.Offset < sinceOffset {
			continue
		}
		result = append(result, pub)
		if len(result) >= limit {
			break
		}
	}
	page.Publications = result
	page.Truncated = limit > 0 && len(result) == limit
	return page, nil
}

var _ Broker = (*memoryBroker)(nil)

// Epoch returns the broker's epoch identifier.
func (b *memoryBroker) Epoch() string {
	return b.epoch
}
