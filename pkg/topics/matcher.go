package topics

import (
	"errors"
	"reflect"
	"strings"
)

const (
	delimiter = "."
	wildcard  = "*"
	empty     = ""
)

// ErrBadTopic is returned when a topic is rejected: it is empty, contains
// explicit empty segments (e.g. "a.", ".a", "a..b"), or does not fit within
// the matcher's topic space.
var ErrBadTopic = errors.New("topic does not fit within topic space")

// ErrBadSubscriber is returned by Subscribe when sub cannot be used as a map
// key. Matcher implementations store subscribers in maps, so non-comparable
// values are rejected up front instead of panicking mid-operation.
var ErrBadSubscriber = errors.New("subscriber is not comparable")

// Subscriber is a value associated with a subscription.
//
// Subscribers must be comparable: implementations store them in maps and use
// them as keys. Non-comparable values are rejected by Subscribe with
// ErrBadSubscriber. A nil Subscriber is allowed.
type Subscriber interface{}

// Subscription represents a topic subscription.
//
// ID is only meaningful for the bitmap implementations; the naive, trie and
// cs-trie implementations leave it at its zero value. A Subscription is only
// valid for the Matcher that created it.
type Subscription struct {
	ID         uint32
	Topic      string
	Subscriber Subscriber
}

// Matcher contains topic subscriptions and performs matches on them.
//
// Topics are dot-separated lists of non-empty segments (e.g. "forex.eur",
// "*", "a.b.c"). The single segment "*" is a wildcard matching exactly one
// segment. The empty topic and topics with explicit empty segments ("a.",
// ".a", "a..b") are rejected by Subscribe with ErrBadTopic and never match in
// Lookup. Segment count is significant: "a" does not match "a.b".
//
// Duplicate subscription semantics differ between implementations:
//   - The naive, trie and cs-trie matchers are idempotent per (topic,
//     Subscriber): subscribing the same subscriber to the same topic twice is
//     a no-op, and one Unsubscribe removes it.
//   - The inverted and optimized inverted bitmap matchers are
//     multi-subscription: every Subscribe call allocates a fresh position, so
//     the same subscriber may subscribe to the same topic multiple times and
//     each subscription must be removed with its own Unsubscribe.
//
// Unsubscribe is idempotent: calling it twice with the same Subscription, or
// with a nil Subscription, is a no-op.
type Matcher interface {
	// Subscribe adds the Subscriber to the topic and returns a Subscription.
	Subscribe(topic string, sub Subscriber) (*Subscription, error)

	// Unsubscribe removes the Subscription. It is idempotent and safe to call
	// with a nil Subscription.
	Unsubscribe(sub *Subscription)

	// Lookup returns the Subscribers for the given topic.
	Lookup(topic string) []Subscriber
}

// validTopic reports whether topic is a non-empty dot-separated list of
// non-empty segments.
func validTopic(topic string) bool {
	for _, constituent := range strings.Split(topic, delimiter) {
		if constituent == empty {
			return false
		}
	}
	return true
}

// validateSubscriber reports whether sub can be stored as a map key.
func validateSubscriber(sub Subscriber) error {
	if sub != nil && !reflect.TypeOf(sub).Comparable() {
		return ErrBadSubscriber
	}
	return nil
}

// matchCriteria reports whether the subscription pattern (pattern) matches
// the concrete topic (topic). pattern may contain single-segment "*"
// wildcards; topic must be literal. Both sides must have the same number of
// segments.
func matchCriteria(pattern, topic string) bool {
	patternConstituents := strings.Split(pattern, delimiter)
	topicConstituents := strings.Split(topic, delimiter)

	if len(patternConstituents) != len(topicConstituents) {
		return false
	}

	for i, constituent := range topicConstituents {
		if constituent != patternConstituents[i] && patternConstituents[i] != wildcard {
			return false
		}
	}

	return true
}
