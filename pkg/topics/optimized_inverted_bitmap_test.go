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
	testMatcherConcurrentSubscribe(t, NewOptimizedInvertedBitmapMatcher(3), topics)
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
