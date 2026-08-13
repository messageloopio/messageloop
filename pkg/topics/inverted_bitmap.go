package topics

import (
	"sync"

	"github.com/RoaringBitmap/roaring"
)

type invertedBitmapMatcher struct {
	bitmaps          map[string]*roaring.Bitmap
	subPos           uint32
	subscribers      map[uint32]Subscriber
	deletedPositions []uint32
	mu               sync.RWMutex
}

func NewInvertedBitmapMatcher(topicSpace []string) Matcher {
	bitmaps := make(map[string]*roaring.Bitmap)
	for _, topic := range topicSpace {
		bitmaps[topic] = roaring.New()
	}
	return &invertedBitmapMatcher{
		bitmaps:          bitmaps,
		subscribers:      make(map[uint32]Subscriber),
		deletedPositions: []uint32{},
	}
}

func (b *invertedBitmapMatcher) Subscribe(topic string, sub Subscriber) (*Subscription, error) {
	if err := validateSubscriber(sub); err != nil {
		return nil, err
	}
	if !validTopic(topic) {
		return nil, ErrBadTopic
	}
	b.mu.Lock()
	var (
		pos       uint32
		reclaimed = false
	)
	if len(b.deletedPositions) > 0 {
		pos = b.deletedPositions[0]
		b.deletedPositions = b.deletedPositions[1:]
		reclaimed = true
	} else {
		pos = b.subPos
	}

	match := false
	for t, bitmap := range b.bitmaps {
		// The subscription topic may contain "*" wildcards, so it is the
		// pattern; the topic-space entry is the concrete topic.
		if matchCriteria(topic, t) {
			bitmap.Add(pos)
			match = true
		}
	}

	if !match {
		if reclaimed {
			b.deletedPositions = append(b.deletedPositions, pos)
		}
		b.mu.Unlock()
		return nil, ErrBadTopic
	}

	if !reclaimed {
		b.subPos++
	}

	b.subscribers[pos] = sub
	b.mu.Unlock()
	return &Subscription{ID: pos, Topic: topic, Subscriber: sub}, nil
}

// Unsubscribe removes the Subscription. It is idempotent: unsubscribing a
// Subscription that is no longer registered, or a nil Subscription, is a
// no-op.
func (b *invertedBitmapMatcher) Unsubscribe(sub *Subscription) {
	if sub == nil {
		return
	}
	b.mu.Lock()
	existing, ok := b.subscribers[sub.ID]
	if ok && existing == sub.Subscriber {
		for _, bm := range b.bitmaps {
			bm.Remove(sub.ID)
		}
		b.deletedPositions = append(b.deletedPositions, sub.ID)
		delete(b.subscribers, sub.ID)
	}
	b.mu.Unlock()
}

// Lookup returns the Subscribers for the given topic.
func (b *invertedBitmapMatcher) Lookup(topic string) []Subscriber {
	b.mu.RLock()
	bm, ok := b.bitmaps[topic]
	if !ok {
		b.mu.RUnlock()
		return nil
	}

	subscriberSet := make(map[Subscriber]struct{}, bm.GetCardinality())
	for iter := bm.Iterator(); iter.HasNext(); {
		subscriberSet[b.subscribers[iter.Next()]] = struct{}{}
	}
	b.mu.RUnlock()

	subscribers := make([]Subscriber, len(subscriberSet))
	i := 0
	for sub := range subscriberSet {
		subscribers[i] = sub
		i++
	}
	return subscribers
}
