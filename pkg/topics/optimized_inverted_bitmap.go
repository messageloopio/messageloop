package topics

import (
	"strings"
	"sync"

	"github.com/RoaringBitmap/roaring"
)

type constituentBitmap struct {
	bitmaps map[string]*roaring.Bitmap
}

func newConstituentBitmap() *constituentBitmap {
	bitmaps := map[string]*roaring.Bitmap{
		empty:    roaring.New(),
		wildcard: roaring.New(),
	}
	return &constituentBitmap{bitmaps: bitmaps}
}

func (c *constituentBitmap) index(constituent string, subPos uint32) {
	bitmap, ok := c.bitmaps[constituent]
	if !ok {
		bitmap = roaring.New()
		c.bitmaps[constituent] = bitmap
	}
	bitmap.Add(subPos)
}

func (c *constituentBitmap) lookup(constituent string) *roaring.Bitmap {
	if constituent == empty {
		return c.bitmaps[empty]
	}
	bitmap := c.bitmaps[wildcard]
	if bm, ok := c.bitmaps[constituent]; ok {
		bitmap = roaring.FastOr(bitmap, bm)
	}
	return bitmap
}

type optimizedInvertedBitmapMatcher struct {
	constituentBitmaps []*constituentBitmap
	maxConstituents    uint
	subscribers        map[uint32]Subscriber
	subPos             uint32
	deletedPositions   []uint32
	mu                 sync.RWMutex
}

func NewOptimizedInvertedBitmapMatcher(topicSpaceSize uint) Matcher {
	bitmaps := make([]*constituentBitmap, topicSpaceSize)
	for i := uint(0); i < topicSpaceSize; i++ {
		bitmaps[i] = newConstituentBitmap()
	}
	return &optimizedInvertedBitmapMatcher{
		constituentBitmaps: bitmaps,
		maxConstituents:    topicSpaceSize,
		subscribers:        make(map[uint32]Subscriber),
		deletedPositions:   []uint32{},
	}
}

// Subscribe adds the Subscriber to the topic and returns a Subscription.
func (b *optimizedInvertedBitmapMatcher) Subscribe(topic string, sub Subscriber) (*Subscription, error) {
	if err := validateSubscriber(sub); err != nil {
		return nil, err
	}
	if !validTopic(topic) {
		// Reject explicit empty segments (e.g. "a.", ".a", "a..b") and the
		// empty topic to stay consistent with the other matchers. Trailing
		// padding with empty segments is applied below and is unrelated to
		// this check.
		return nil, ErrBadTopic
	}
	constituents := strings.Split(topic, delimiter)
	if uint(len(constituents)) > b.maxConstituents {
		return nil, ErrBadTopic
	}

	b.mu.Lock()
	var (
		i           int
		constituent string
		pos         uint32
	)

	if len(b.deletedPositions) > 0 {
		pos = b.deletedPositions[0]
		b.deletedPositions = b.deletedPositions[1:]
	} else {
		pos = b.subPos
		b.subPos++
	}

	for i, constituent = range constituents {
		b.constituentBitmaps[i].index(constituent, pos)
	}
	for i := uint(i + 1); i < b.maxConstituents; i++ {
		b.constituentBitmaps[i].index(empty, pos)
	}

	b.subscribers[pos] = sub
	b.mu.Unlock()
	return &Subscription{ID: pos, Topic: topic, Subscriber: sub}, nil
}

// Unsubscribe removes the Subscription. It is idempotent: unsubscribing a
// Subscription that is no longer registered, or a nil Subscription, is a
// no-op.
func (b *optimizedInvertedBitmapMatcher) Unsubscribe(sub *Subscription) {
	if sub == nil {
		return
	}
	b.mu.Lock()
	existing, ok := b.subscribers[sub.ID]
	if ok && existing == sub.Subscriber {
		constituents := strings.Split(sub.Topic, delimiter)
		for i, cb := range b.constituentBitmaps {
			if i < len(constituents) {
				if bm, ok := cb.bitmaps[constituents[i]]; ok {
					bm.Remove(sub.ID)
				}
			} else if bm, ok := cb.bitmaps[empty]; ok {
				// Clear the trailing empty constituents padded at subscribe
				// time; leaving them behind mis-matches shorter topics once
				// the position is reclaimed.
				bm.Remove(sub.ID)
			}
		}
		b.deletedPositions = append(b.deletedPositions, sub.ID)
		delete(b.subscribers, sub.ID)
	}
	b.mu.Unlock()
}

// Lookup returns the Subscribers for the given topic.
func (b *optimizedInvertedBitmapMatcher) Lookup(topic string) []Subscriber {
	constituents := strings.Split(topic, delimiter)
	if uint(len(constituents)) > b.maxConstituents {
		return nil
	}
	for _, constituent := range constituents {
		if constituent == empty {
			// Topics with explicit empty segments (e.g. "a.", ".a", "a..b")
			// never match, consistent with the other matchers.
			return nil
		}
	}

	bitmaps := make([]*roaring.Bitmap, b.maxConstituents)
	var (
		i           int
		constituent string
	)
	b.mu.RLock()
	for i, constituent = range constituents {
		bitmaps[i] = b.constituentBitmaps[i].lookup(constituent)
		if bitmaps[i].IsEmpty() {
			// If we get an empty bitmap, there are no subscribers.
			b.mu.RUnlock()
			return nil
		}
	}
	for i := uint(i + 1); i < b.maxConstituents; i++ {
		bitmaps[i] = b.constituentBitmaps[i].lookup(empty)
	}
	result := roaring.FastAnd(bitmaps...)
	subscriberSet := make(map[Subscriber]struct{}, result.GetCardinality())
	for iter := result.Iterator(); iter.HasNext(); {
		subscriberSet[b.subscribers[iter.Next()]] = struct{}{}
	}
	b.mu.RUnlock()

	subscribers := make([]Subscriber, len(subscriberSet))
	i = 0
	for sub := range subscriberSet {
		subscribers[i] = sub
		i++
	}
	return subscribers
}
