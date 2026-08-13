package topics

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOptimizedInvertedBitmapMatcherConcurrentSubscribe(t *testing.T) {
	topics := make([]string, 64)
	for i := range topics {
		topics[i] = fmt.Sprintf("%d.%d.%d", i, i, i)
	}
	testMatcherConcurrentSubscribe(t, NewOptimizedInvertedBitmapMatcher(3), topics, true)
}

func TestOptimizedInvertedBitmapMatcher(t *testing.T) {
	assert := assert.New(t)
	var (
		ib = NewOptimizedInvertedBitmapMatcher(5)
		s0 = 0
		s1 = 1
		s2 = 2
	)

	sub0, err := ib.Subscribe("forex.*", s0)
	assert.NoError(err)
	sub1, err := ib.Subscribe("*.usd", s0)
	assert.NoError(err)
	sub2, err := ib.Subscribe("forex.eur", s0)
	assert.NoError(err)
	sub3, err := ib.Subscribe("*.eur", s1)
	assert.NoError(err)
	sub4, err := ib.Subscribe("forex.*", s1)
	assert.NoError(err)
	sub5, err := ib.Subscribe("trade", s1)
	assert.NoError(err)
	sub6, err := ib.Subscribe("*", s2)
	assert.NoError(err)

	assertEqual(assert, []Subscriber{s0, s1}, ib.Lookup("forex.eur"))
	assertEqual(assert, []Subscriber{s2}, ib.Lookup("forex"))
	assertEqual(assert, []Subscriber{}, ib.Lookup("trade.jpy"))
	assertEqual(assert, []Subscriber{s0, s1}, ib.Lookup("forex.jpy"))
	assertEqual(assert, []Subscriber{s1, s2}, ib.Lookup("trade"))

	ib.Unsubscribe(sub0)
	ib.Unsubscribe(sub1)
	ib.Unsubscribe(sub2)
	ib.Unsubscribe(sub3)
	ib.Unsubscribe(sub4)
	ib.Unsubscribe(sub5)
	ib.Unsubscribe(sub6)

	assertEqual(assert, []Subscriber{}, ib.Lookup("forex.eur"))
	assertEqual(assert, []Subscriber{}, ib.Lookup("forex"))
	assertEqual(assert, []Subscriber{}, ib.Lookup("trade.jpy"))
	assertEqual(assert, []Subscriber{}, ib.Lookup("forex.jpy"))
	assertEqual(assert, []Subscriber{}, ib.Lookup("trade"))
}

func TestOptimizedInvertedBitmapMatcherRejectsEmptySegments(t *testing.T) {
	assert := assert.New(t)
	ib := NewOptimizedInvertedBitmapMatcher(5)

	for _, topic := range []string{"a.", ".a", "a..b"} {
		_, err := ib.Subscribe(topic, 0)
		assert.ErrorIs(err, ErrBadTopic, "topic %q", topic)
	}

	assert.Empty(ib.Lookup("a."))
	assert.Empty(ib.Lookup(".a"))
	assert.Empty(ib.Lookup("a..b"))
	assert.Empty(ib.Lookup("."))
}

func TestOptimizedInvertedBitmapMatcherUnsubscribeStaleEmpty(t *testing.T) {
	assert := assert.New(t)
	m := NewOptimizedInvertedBitmapMatcher(3)

	sub0, err := m.Subscribe("a", 0)
	assert.NoError(err)
	m.Unsubscribe(sub0)

	sub1, err := m.Subscribe("b.c.d", 1)
	assert.NoError(err)
	assert.Equal(sub0.ID, sub1.ID, "position must be reclaimed")

	// Regression: subscribing to "a" padded empty constituents at depths 1
	// and 2. Those bits must not survive Unsubscribe and later mis-match
	// shorter lookups on a reclaimed position.
	assert.Empty(m.Lookup("b.c"))
	assert.Empty(m.Lookup("b"))
	assert.Empty(m.Lookup("c.d"))
	assertEqual(assert, []Subscriber{1}, m.Lookup("b.c.d"))
}

func TestOptimizedInvertedBitmapMatcherDuplicateUnsubscribe(t *testing.T) {
	assert := assert.New(t)
	m := NewOptimizedInvertedBitmapMatcher(2)

	subA, err := m.Subscribe("a.b", 10)
	assert.NoError(err)
	_, err = m.Subscribe("c.d", 20)
	assert.NoError(err)

	m.Unsubscribe(subA)
	m.Unsubscribe(subA)

	subC, err := m.Subscribe("e.f", 30)
	assert.NoError(err)
	subD, err := m.Subscribe("g.h", 40)
	assert.NoError(err)

	// Regression: unsubscribing the same Subscription twice must not enqueue
	// its position twice, which aliased two live subscriptions to one ID and
	// made them overwrite each other.
	assert.NotEqual(subC.ID, subD.ID, "reclaimed position must not be handed out twice")
	assertEqual(assert, []Subscriber{30}, m.Lookup("e.f"))
	assertEqual(assert, []Subscriber{40}, m.Lookup("g.h"))
}

func TestOptimizedInvertedBitmapMatcherPaddingSemantics(t *testing.T) {
	assert := assert.New(t)
	ib := NewOptimizedInvertedBitmapMatcher(5)

	sub0, err := ib.Subscribe("a", 0)
	assert.NoError(err)
	assertEqual(assert, []Subscriber{0}, ib.Lookup("a"))
	// Trailing padding (empty segments) keeps matching exact-length only:
	// "a" matches "a" but not longer topics like "a.b".
	assert.Empty(ib.Lookup("a.b"))

	sub1, err := ib.Subscribe("a.b", 1)
	assert.NoError(err)
	assertEqual(assert, []Subscriber{1}, ib.Lookup("a.b"))
	assertEqual(assert, []Subscriber{0}, ib.Lookup("a"))
	assert.Empty(ib.Lookup("a.b.c"))

	ib.Unsubscribe(sub0)
	ib.Unsubscribe(sub1)
	assert.Empty(ib.Lookup("a"))
	assert.Empty(ib.Lookup("a.b"))
}

func BenchmarkOptimizedInvertedBitmapMatcherSubscribe(b *testing.B) {
	var (
		ib = NewOptimizedInvertedBitmapMatcher(5)
		s0 = 0
	)
	populateMatcher(ib, 1000, 5)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ib.Subscribe("foo.*.baz.qux.quux", s0)
	}
}

func BenchmarkOptimizedInvertedBitmapMatcherUnsubscribe(b *testing.B) {
	var (
		ib = NewOptimizedInvertedBitmapMatcher(5)
		s0 = 0
	)
	populateMatcher(ib, 1000, 5)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id, _ := ib.Subscribe("foo.*.baz.qux.quux", s0)
		ib.Unsubscribe(id)
	}
}

func BenchmarkOptimizedInvertedBitmapMatcherLookup(b *testing.B) {
	var (
		ib = NewOptimizedInvertedBitmapMatcher(5)
		s0 = 0
	)
	_, _ = ib.Subscribe("foo.*.baz.qux.quux", s0)
	populateMatcher(ib, 1000, 5)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ib.Lookup("foo.bar.baz.qux.quux")
	}
}

func BenchmarkOptimizedInvertedBitmapMatcherSubscribeCold(b *testing.B) {
	var (
		ib = NewOptimizedInvertedBitmapMatcher(5)
		s0 = 0
	)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ib.Subscribe("foo.*.baz.qux.quux", s0)
	}
}

func BenchmarkOptimizedInvertedBitmapMatcherUnsubscribeCold(b *testing.B) {
	var (
		ib = NewOptimizedInvertedBitmapMatcher(5)
		s0 = 0
	)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id, _ := ib.Subscribe("foo.*.baz.qux.quux", s0)
		ib.Unsubscribe(id)
	}
}

func BenchmarkOptimizedInvertedBitmapMatcherLookupCold(b *testing.B) {
	var (
		ib = NewOptimizedInvertedBitmapMatcher(5)
		s0 = 0
	)
	_, _ = ib.Subscribe("foo.*.baz.qux.quux", s0)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ib.Lookup("foo.bar.baz.qux.quux")
	}
}

func BenchmarkMultithreaded1Thread5050OptimizedInvertedBitmap(b *testing.B) {
	numItems := 1000
	numThreads := 1
	benchmark5050(b, numItems, numThreads, func(items [][]string) Matcher {
		return NewOptimizedInvertedBitmapMatcher(uint(numItems))
	})
}

func BenchmarkMultithreaded2Thread5050OptimizedInvertedBitmap(b *testing.B) {
	numItems := 1000
	numThreads := 2
	benchmark5050(b, numItems, numThreads, func(items [][]string) Matcher {
		return NewOptimizedInvertedBitmapMatcher(uint(numItems))
	})
}

func BenchmarkMultithreaded4Thread5050OptimizedInvertedBitmap(b *testing.B) {
	numItems := 1000
	numThreads := 4
	benchmark5050(b, numItems, numThreads, func(items [][]string) Matcher {
		return NewOptimizedInvertedBitmapMatcher(uint(numItems))
	})
}

func BenchmarkMultithreaded8Thread5050OptimizedInvertedBitmap(b *testing.B) {
	numItems := 1000
	numThreads := 8
	benchmark5050(b, numItems, numThreads, func(items [][]string) Matcher {
		return NewOptimizedInvertedBitmapMatcher(uint(numItems))
	})
}

func BenchmarkMultithreaded12Thread5050OptimizedInvertedBitmap(b *testing.B) {
	numItems := 1000
	numThreads := 12
	benchmark5050(b, numItems, numThreads, func(items [][]string) Matcher {
		return NewOptimizedInvertedBitmapMatcher(uint(numItems))
	})
}

func BenchmarkMultithreaded16Thread5050OptimizedInvertedBitmap(b *testing.B) {
	numItems := 1000
	numThreads := 16
	benchmark5050(b, numItems, numThreads, func(items [][]string) Matcher {
		return NewOptimizedInvertedBitmapMatcher(uint(numItems))
	})
}

func BenchmarkMultithreaded1Thread9010OptimizedInvertedBitmap(b *testing.B) {
	numItems := 1000
	numThreads := 1
	benchmark9010(b, numItems, numThreads, func(items [][]string) Matcher {
		return NewOptimizedInvertedBitmapMatcher(uint(numItems))
	})
}

func BenchmarkMultithreaded2Thread9010OptimizedInvertedBitmap(b *testing.B) {
	numItems := 1000
	numThreads := 2
	benchmark9010(b, numItems, numThreads, func(items [][]string) Matcher {
		return NewOptimizedInvertedBitmapMatcher(uint(numItems))
	})
}

func BenchmarkMultithreaded4Thread9010OptimizedInvertedBitmap(b *testing.B) {
	numItems := 1000
	numThreads := 4
	benchmark9010(b, numItems, numThreads, func(items [][]string) Matcher {
		return NewOptimizedInvertedBitmapMatcher(uint(numItems))
	})
}

func BenchmarkMultithreaded8Thread9010OptimizedInvertedBitmap(b *testing.B) {
	numItems := 1000
	numThreads := 8
	benchmark9010(b, numItems, numThreads, func(items [][]string) Matcher {
		return NewOptimizedInvertedBitmapMatcher(uint(numItems))
	})
}

func BenchmarkMultithreaded12Thread9010OptimizedInvertedBitmap(b *testing.B) {
	numItems := 1000
	numThreads := 12
	benchmark9010(b, numItems, numThreads, func(items [][]string) Matcher {
		return NewOptimizedInvertedBitmapMatcher(uint(numItems))
	})
}

func BenchmarkMultithreaded16Thread9010OptimizedInvertedBitmap(b *testing.B) {
	numItems := 1000
	numThreads := 16
	benchmark9010(b, numItems, numThreads, func(items [][]string) Matcher {
		return NewOptimizedInvertedBitmapMatcher(uint(numItems))
	})
}
