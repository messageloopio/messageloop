package topics

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// newConsistencyMatchers returns one instance of every Matcher implementation.
// The inverted bitmap matcher is seeded with a small topic space, the
// optimized bitmap matcher with a small topic space size.
func newConsistencyMatchers() []Matcher {
	return []Matcher{
		NewNaiveMatcher(),
		NewTrieMatcher(),
		NewCSTrieMatcher(),
		NewInvertedBitmapMatcher([]string{"a.b", "c.d", "e.f"}),
		NewOptimizedInvertedBitmapMatcher(3),
	}
}

// TestMatchersRejectEmptySegmentTopics pins the shared topic semantics: all
// five implementations reject explicit empty segments and the empty topic,
// and never return subscribers for them (naive is the baseline).
func TestMatchersRejectEmptySegmentTopics(t *testing.T) {
	assert := assert.New(t)

	badTopics := []string{"", "a.", ".a", "a..b"}
	for _, m := range newConsistencyMatchers() {
		for _, topic := range badTopics {
			_, err := m.Subscribe(topic, 0)
			assert.ErrorIs(err, ErrBadTopic, "Subscribe(%q)", topic)
			assert.Empty(m.Lookup(topic), "Lookup(%q)", topic)
		}

		// Valid topics still work after the rejection above.
		sub, err := m.Subscribe("a.b", 1)
		assert.NoError(err)
		assert.NotEmpty(m.Lookup("a.b"))
		m.Unsubscribe(sub)
		assert.Empty(m.Lookup("a.b"))
	}
}

// TestMatchersUnsubscribeNil ensures Unsubscribe never panics on a nil
// Subscription; it is a documented no-op.
func TestMatchersUnsubscribeNil(t *testing.T) {
	for _, m := range newConsistencyMatchers() {
		sub, err := m.Subscribe("a.b", 1)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		m.Unsubscribe(nil)
		assert.NotEmpty(t, m.Lookup("a.b"))
		m.Unsubscribe(sub)
	}
}

// TestMatchersRejectNonComparableSubscriber pins the Subscriber contract:
// implementations store subscribers in maps, so non-comparable values are
// rejected with ErrBadSubscriber instead of panicking later.
func TestMatchersRejectNonComparableSubscriber(t *testing.T) {
	assert := assert.New(t)
	for _, m := range newConsistencyMatchers() {
		_, err := m.Subscribe("a.b", make([]int, 1))
		assert.ErrorIs(err, ErrBadSubscriber)
		assert.Empty(m.Lookup("a.b"), "rejected subscriber must not be registered")
	}
}

func TestNaiveMatcherConcurrentSubscribe(t *testing.T) {
	topics := make([]string, 64)
	for i := range topics {
		topics[i] = fmt.Sprintf("%d.%d.%d", i, i, i)
	}
	testMatcherConcurrentSubscribe(t, NewNaiveMatcher(), topics, false)
}

func TestTrieMatcherConcurrentSubscribe(t *testing.T) {
	topics := make([]string, 64)
	for i := range topics {
		topics[i] = fmt.Sprintf("%d.%d.%d", i, i, i)
	}
	testMatcherConcurrentSubscribe(t, NewTrieMatcher(), topics, false)
}

func TestCSTrieMatcherConcurrentSubscribe(t *testing.T) {
	topics := make([]string, 64)
	for i := range topics {
		topics[i] = fmt.Sprintf("%d.%d.%d", i, i, i)
	}
	testMatcherConcurrentSubscribe(t, NewCSTrieMatcher(), topics, false)
}
