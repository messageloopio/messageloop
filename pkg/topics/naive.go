package topics

import (
	"sync"
)

// naiveMatcher is an implementation of Matcher which is backed by a hashmap.
type naiveMatcher struct {
	subs map[string]map[Subscriber]struct{}
	mu   sync.RWMutex
}

func NewNaiveMatcher() Matcher {
	return &naiveMatcher{subs: make(map[string]map[Subscriber]struct{})}
}

// Subscribe adds the Subscriber to the topic and returns a Subscription.
func (n *naiveMatcher) Subscribe(topic string, sub Subscriber) (*Subscription, error) {
	if err := validateSubscriber(sub); err != nil {
		return nil, err
	}
	if !validTopic(topic) {
		return nil, ErrBadTopic
	}
	n.mu.Lock()
	if _, ok := n.subs[topic]; !ok {
		n.subs[topic] = make(map[Subscriber]struct{})
	}
	n.subs[topic][sub] = struct{}{}
	n.mu.Unlock()
	return &Subscription{Topic: topic, Subscriber: sub}, nil
}

// Unsubscribe removes the Subscription.
func (n *naiveMatcher) Unsubscribe(sub *Subscription) {
	if sub == nil {
		return
	}
	n.mu.Lock()
	if subscribers, ok := n.subs[sub.Topic]; ok {
		delete(subscribers, sub.Subscriber)
		if len(subscribers) == 0 {
			// Drop the topic entry so empty topics do not accumulate.
			delete(n.subs, sub.Topic)
		}
	}
	n.mu.Unlock()
}

// Lookup returns the Subscribers for the given topic.
func (n *naiveMatcher) Lookup(topic string) []Subscriber {
	n.mu.RLock()
	subscriberSet := make(map[Subscriber]struct{})
	for existingTopic, subscribers := range n.subs {
		if matchCriteria(existingTopic, topic) {
			for sub, x := range subscribers {
				subscriberSet[sub] = x
			}
		}
	}
	n.mu.RUnlock()

	var (
		subscriberList = make([]Subscriber, len(subscriberSet))
		i              = 0
	)
	for sub := range subscriberSet {
		subscriberList[i] = sub
		i++
	}

	return subscriberList
}
