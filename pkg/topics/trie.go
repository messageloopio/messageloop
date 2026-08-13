package topics

import (
	"strings"
	"sync"
)

type node struct {
	word     string
	subs     map[Subscriber]struct{}
	parent   *node
	children map[string]*node
}

func (n *node) orphan() {
	if n.parent == nil {
		// Root
		return
	}
	delete(n.parent.children, n.word)
	if len(n.parent.subs) == 0 && len(n.parent.children) == 0 {
		n.parent.orphan()
	}
}

type trieMatcher struct {
	root *node
	mu   sync.RWMutex
}

func NewTrieMatcher() Matcher {
	return &trieMatcher{
		root: &node{
			subs:     make(map[Subscriber]struct{}),
			children: make(map[string]*node),
		},
	}
}

// Subscribe adds the Subscriber to the topic and returns a Subscription.
func (t *trieMatcher) Subscribe(topic string, sub Subscriber) (*Subscription, error) {
	if err := validateSubscriber(sub); err != nil {
		return nil, err
	}
	if !validTopic(topic) {
		return nil, ErrBadTopic
	}
	t.mu.Lock()
	curr := t.root
	for _, word := range strings.Split(topic, delimiter) {
		child, ok := curr.children[word]
		if !ok {
			child = &node{
				word:     word,
				subs:     make(map[Subscriber]struct{}),
				parent:   curr,
				children: make(map[string]*node),
			}
			curr.children[word] = child
		}
		curr = child
	}
	curr.subs[sub] = struct{}{}
	t.mu.Unlock()
	return &Subscription{Topic: topic, Subscriber: sub}, nil
}

// Unsubscribe removes the Subscription.
func (t *trieMatcher) Unsubscribe(sub *Subscription) {
	if sub == nil {
		return
	}
	t.mu.Lock()
	curr := t.root
	for _, word := range strings.Split(sub.Topic, delimiter) {
		child, ok := curr.children[word]
		if !ok {
			// Subscription doesn't exist.
			t.mu.Unlock()
			return
		}
		curr = child
	}
	delete(curr.subs, sub.Subscriber)
	if len(curr.subs) == 0 && len(curr.children) == 0 {
		curr.orphan()
	}
	t.mu.Unlock()
}

// Lookup returns the Subscribers for the given topic.
func (t *trieMatcher) Lookup(topic string) []Subscriber {
	words := strings.Split(topic, delimiter)
	for _, word := range words {
		if word == empty {
			// Topics with explicit empty segments never match, including
			// against "**" branches that would otherwise absorb them.
			return nil
		}
	}
	t.mu.RLock()
	var (
		subMap = t.lookup(words, t.root)
		subs   = make([]Subscriber, len(subMap))
		i      = 0
	)
	t.mu.RUnlock()
	for sub := range subMap {
		subs[i] = sub
		i++
	}
	return subs
}

func (t *trieMatcher) lookup(words []string, node *node) map[Subscriber]struct{} {
	subs := make(map[Subscriber]struct{})
	// A "**" branch matches any remainder, including the empty one: its
	// subscribers are collected at every level ("a.**" matches "a", "a.b", ...).
	if n, ok := node.children[multiWildcard]; ok {
		for sub := range n.subs {
			subs[sub] = struct{}{}
		}
	}
	if len(words) == 0 {
		for sub := range node.subs {
			subs[sub] = struct{}{}
		}
		return subs
	}
	if words[0] == empty {
		// Topics with explicit empty segments never match.
		return subs
	}
	if n, ok := node.children[words[0]]; ok {
		for k, v := range t.lookup(words[1:], n) {
			subs[k] = v
		}
	}
	if n, ok := node.children[wildcard]; ok {
		for k, v := range t.lookup(words[1:], n) {
			subs[k] = v
		}
	}
	return subs
}
