package messageloop

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
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
		ready:       make(chan struct{}),
		epoch:       uuid.NewString(),
	}
}

// channelHistory is a fixed-capacity ring buffer for one channel.
type channelHistory struct {
	mu      sync.Mutex
	entries []*Publication // ring buffer; len == broker.historySize
	head    int            // index of oldest valid entry
	count   int            // number of valid entries
	nextOff uint64         // next offset to assign (1-based)
}

type memoryBroker struct {
	handler     atomic.Pointer[PublicationHandler]
	historySize int
	epoch       string

	mu      sync.RWMutex
	history map[string]*channelHistory
	subs    map[string]int // subscriber count per channel
	ready   chan struct{}
	once    sync.Once
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

// Subscribe increments the subscriber count for ch. The channel's history is
// retained while at least one subscriber is registered.
func (b *memoryBroker) Subscribe(ch string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[ch]++
	return nil
}

// Unsubscribe decrements the subscriber count for ch. When the last
// subscriber leaves and the channel has no retained history entries, the
// channel's history entry is reclaimed so the history map does not grow
// without bound. History is intentionally retained while the last subscriber
// is away so that reconnect with recovery still works; the ring buffer
// capacity bounds the retained entries per channel.
//
// Publish takes b.mu only to resolve the channelHistory reference and releases
// it before taking h.mu; Unsubscribe holds b.mu while deleting the map entry,
// so a Publish that already resolved the reference keeps writing to a live
// object guarded by h.mu — safe.
func (b *memoryBroker) Unsubscribe(ch string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs[ch] > 0 {
		b.subs[ch]--
	}
	if b.subs[ch] == 0 {
		delete(b.subs, ch)
		if h, ok := b.history[ch]; ok {
			h.mu.Lock()
			empty := h.count == 0
			h.mu.Unlock()
			if empty {
				delete(b.history, ch)
			}
		}
	}
	return nil
}

func (b *memoryBroker) Publish(ch string, pub *Publication) (uint64, error) {
	b.mu.Lock()
	h, ok := b.history[ch]
	if !ok {
		h = &channelHistory{entries: make([]*Publication, b.historySize)}
		b.history[ch] = h
	}
	b.mu.Unlock()

	h.mu.Lock()
	h.nextOff++
	offset := h.nextOff
	stored := *pub
	stored.Channel = ch
	stored.Offset = offset
	stored.Epoch = b.epoch
	stored.Time = time.Now().UnixMilli()
	pub.Offset = offset
	slot := (h.head + h.count) % b.historySize
	if h.count == b.historySize {
		// Buffer full: overwrite oldest entry and advance head.
		h.entries[h.head] = &stored
		h.head = (h.head + 1) % b.historySize
	} else {
		h.entries[slot] = &stored
		h.count++
	}
	h.mu.Unlock()

	if h := b.handler.Load(); h != nil {
		return offset, (*h)(ch, &stored)
	}
	return offset, nil
}

// PublishTransient delivers payload to subscribers in real time without
// writing history. The offset is always 0 because transient publications
// have no history entry.
func (b *memoryBroker) PublishTransient(ch string, pub *Publication) error {
	stored := *pub
	stored.Channel = ch
	stored.Offset = 0
	stored.Epoch = b.epoch
	stored.Time = time.Now().UnixMilli()
	if h := b.handler.Load(); h != nil {
		return (*h)(ch, &stored)
	}
	return nil
}

func (b *memoryBroker) History(ch string, sinceOffset uint64, limit int) ([]*Publication, error) {
	b.mu.RLock()
	h, ok := b.history[ch]
	b.mu.RUnlock()
	if !ok {
		return nil, nil
	}

	if limit <= 0 {
		limit = DefaultHistoryLimit
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	var result []*Publication
	for i := 0; i < h.count; i++ {
		pub := h.entries[(h.head+i)%b.historySize]
		if pub == nil || pub.Offset < sinceOffset {
			continue
		}
		result = append(result, pub)
		if len(result) >= limit {
			break
		}
	}
	return result, nil
}

var _ Broker = (*memoryBroker)(nil)

// Epoch returns the broker's epoch identifier.
func (b *memoryBroker) Epoch() string {
	return b.epoch
}
