package topics

import (
	"errors"
	"reflect"
	"strings"
)

const (
	delimiter = "."
	wildcard  = "*"
	// multiWildcard matches zero or more segments and is only valid as the
	// final segment of a pattern (MQTT-style suffix wildcard).
	multiWildcard = "**"
	empty         = ""
)

// ErrBadTopic is returned when a topic is rejected: it is empty, contains
// explicit empty segments (e.g. "a.", ".a", "a..b"), contains "**" outside of
// the final segment (e.g. "a.**.b", "a**b"), or does not fit within the
// matcher's topic space.
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
// segment. The final segment "**" is a multi-segment wildcard matching zero
// or more segments (MQTT-style suffix wildcard): "a.**" matches "a", "a.b"
// and "a.b.c", and a bare "**" matches every topic. "**" anywhere else
// (middle position like "a.**.b" or embedded like "a**b") is rejected. The
// empty topic, topics with explicit empty segments ("a.", ".a", "a..b") and
// topics with middle-position "**" are rejected by Subscribe with ErrBadTopic
// and never match in Lookup. Segment count is significant for everything but
// a trailing "**": "a" does not match "a.b".
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

// ValidateTopic reports whether topic is a valid channel or subscription
// pattern: a non-empty dot-separated list of non-empty segments with "**"
// allowed only as the final segment. It is the single validation entry point
// shared by the matchers, the hub's exact-subscription path and the broker
// publish paths.
func ValidateTopic(topic string) error {
	if !validTopic(topic) {
		return ErrBadTopic
	}
	return nil
}

// validTopic reports whether topic is a non-empty dot-separated list of
// non-empty segments with "**" allowed only as the final segment. "**"
// embedded inside a segment ("a**b") is rejected; a single "*" embedded in a
// segment ("a*b") keeps its literal-match semantics and is allowed.
func validTopic(topic string) bool {
	constituents := strings.Split(topic, delimiter)
	last := len(constituents) - 1
	for i, constituent := range constituents {
		if constituent == empty {
			return false
		}
		if strings.Contains(constituent, multiWildcard) &&
			(i != last || constituent != multiWildcard) {
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

// Match reports whether the subscription pattern matches the concrete topic.
// It is the exported form of matchCriteria — the single pattern-matching
// implementation shared by every caller (e.g. sessionCoversChannel); there is
// no second glob dialect.
func Match(pattern, topic string) bool {
	return matchCriteria(pattern, topic)
}

// matchCriteria reports whether the subscription pattern (pattern) matches
// the concrete topic (topic). pattern may contain single-segment "*"
// wildcards and a trailing "**" multi-segment wildcard; topic must be
// literal. Without "**" both sides must have the same number of segments;
// with a trailing "**" the pattern's leading segments must match the topic's
// first segments and the rest of the topic is absorbed (zero or more
// segments). A topic with explicit empty segments never matches.
func matchCriteria(pattern, topic string) bool {
	patternConstituents := strings.Split(pattern, delimiter)
	topicConstituents := strings.Split(topic, delimiter)

	for _, constituent := range topicConstituents {
		if constituent == empty {
			return false
		}
	}

	multi := false
	if n := len(patternConstituents); n > 0 && patternConstituents[n-1] == multiWildcard {
		patternConstituents = patternConstituents[:n-1]
		multi = true
	}

	if multi {
		if len(topicConstituents) < len(patternConstituents) {
			return false
		}
		topicConstituents = topicConstituents[:len(patternConstituents)]
	}

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
