package messageloop

import (
	"context"
	"sync"
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
	handler     PublicationHandler
	historySize int
	epoch       string

	mu      sync.RWMutex
	history map[string]*channelHistory
	ready   chan struct{}
	once    sync.Once
}

// Start stores the handler and blocks until ctx is cancelled.
// The memory broker requires no background goroutines.
func (b *memoryBroker) Start(ctx context.Context, handler PublicationHandler) error {
	b.handler = handler
	b.once.Do(func() { close(b.ready) })
	<-ctx.Done()
	return nil
}

// Ready returns a channel that is closed once the handler has been registered.
func (b *memoryBroker) Ready() <-chan struct{} {
	return b.ready
}

func (b *memoryBroker) Subscribe(_ string) error   { return nil }
func (b *memoryBroker) Unsubscribe(_ string) error { return nil }

func (b *memoryBroker) Publish(ch string, payload []byte, isText bool) (uint64, error) {
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
	pub := &Publication{
		Channel: ch,
		Offset:  offset,
		Epoch:   b.epoch,
		Payload: payload,
		IsText:  isText,
		Time:    time.Now().UnixMilli(),
	}
	slot := (h.head + h.count) % b.historySize
	if h.count == b.historySize {
		// Buffer full: overwrite oldest entry and advance head.
		h.entries[h.head] = pub
		h.head = (h.head + 1) % b.historySize
	} else {
		h.entries[slot] = pub
		h.count++
	}
	h.mu.Unlock()

	if b.handler != nil {
		return offset, b.handler(ch, pub)
	}
	return offset, nil
}

func (b *memoryBroker) History(ch string, sinceOffset uint64, limit int) ([]*Publication, error) {
	b.mu.RLock()
	h, ok := b.history[ch]
	b.mu.RUnlock()
	if !ok {
		return nil, nil
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
		if limit > 0 && len(result) >= limit {
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
