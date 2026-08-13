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

// newDoubleWildcardMatchers returns one instance of every Matcher
// implementation with a topic space deep enough for trailing "**" patterns.
// The bitmap implementations are bounded by their topic space, so the test
// topics are restricted to the space entries below.
func newDoubleWildcardMatchers() []Matcher {
	return []Matcher{
		NewNaiveMatcher(),
		NewTrieMatcher(),
		NewCSTrieMatcher(),
		NewInvertedBitmapMatcher([]string{"a", "a.b", "a.b.c", "a.b.c.d", "x.y", "x.y.z"}),
		NewOptimizedInvertedBitmapMatcher(6),
	}
}

// TestMatchersDoubleWildcardSuffix pins the trailing "**" semantics across
// all five implementations (B2): "a.**" matches "a", "a.b" and "a.b.c" (zero
// or more trailing segments), a bare "**" matches everything, and the
// single-segment "*" is unaffected.
func TestMatchersDoubleWildcardSuffix(t *testing.T) {
	assert := assert.New(t)

	for _, m := range newDoubleWildcardMatchers() {
		sub0, err := m.Subscribe("a.**", 0)
		assert.NoError(err)
		sub1, err := m.Subscribe("a.b.**", 1)
		assert.NoError(err)
		sub2, err := m.Subscribe("**", 2)
		assert.NoError(err)
		sub3, err := m.Subscribe("x.y", 3)
		assert.NoError(err)
		sub4, err := m.Subscribe("a.*", 4)
		assert.NoError(err)

		assertEqual(assert, []Subscriber{0, 2}, m.Lookup("a"))
		assertEqual(assert, []Subscriber{0, 1, 2, 4}, m.Lookup("a.b"))
		assertEqual(assert, []Subscriber{0, 1, 2}, m.Lookup("a.b.c"))
		assertEqual(assert, []Subscriber{0, 1, 2}, m.Lookup("a.b.c.d"))
		assertEqual(assert, []Subscriber{2, 3}, m.Lookup("x.y"))
		assertEqual(assert, []Subscriber{2}, m.Lookup("x.y.z"))

		// Lookup stays lenient for malformed topics: they never match, even
		// with a bare "**" subscription registered.
		assert.Empty(m.Lookup("a."))
		assert.Empty(m.Lookup(".a"))
		assert.Empty(m.Lookup("a..b"))

		m.Unsubscribe(sub0)
		m.Unsubscribe(sub1)
		m.Unsubscribe(sub2)
		m.Unsubscribe(sub3)
		m.Unsubscribe(sub4)

		assert.Empty(m.Lookup("a"))
		assert.Empty(m.Lookup("a.b"))
		assert.Empty(m.Lookup("a.b.c"))
		assert.Empty(m.Lookup("x.y"))
	}
}

// TestMatchersRejectMiddleAndEmbeddedDoubleWildcard pins that "**" is only
// meaningful as the final segment: middle-position "**" ("a.**.b", "**.a",
// "a.**.**") and "**" embedded inside a segment ("a**b") are rejected with
// ErrBadTopic by every implementation.
func TestMatchersRejectMiddleAndEmbeddedDoubleWildcard(t *testing.T) {
	assert := assert.New(t)

	badTopics := []string{"a.**.b", "**.a", "a.**.**", "a**b", "a.b**"}
	for _, m := range newConsistencyMatchers() {
		for _, topic := range badTopics {
			_, err := m.Subscribe(topic, 0)
			assert.ErrorIs(err, ErrBadTopic, "Subscribe(%q)", topic)
			assert.Empty(m.Lookup(topic), "Lookup(%q)", topic)
		}
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
