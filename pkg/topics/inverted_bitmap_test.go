package topics

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInvertedBitmapMatcherConcurrentSubscribe(t *testing.T) {
	topics := make([]string, 64)
	for i := range topics {
		topics[i] = fmt.Sprintf("%d.%d.%d", i, i, i)
	}
	testMatcherConcurrentSubscribe(t, NewInvertedBitmapMatcher(topics), topics, true)
}

func TestInvertedBitmapMatcher(t *testing.T) {
	assert := assert.New(t)
	var (
		topics = []string{
			"forex",
			"forex.gbp",
			"forex.eur",
			"forex.usd",
			"forex.jpy",
			"trade",
			"trade.usd",
			"trade.jpy",
			"foo.bar.baz.qux.quux",
		}
		ib = NewInvertedBitmapMatcher(topics)
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

func TestInvertedBitmapMatcherDuplicateUnsubscribe(t *testing.T) {
	assert := assert.New(t)
	m := NewInvertedBitmapMatcher([]string{"a.b", "c.d", "e.f"})

	subA, err := m.Subscribe("a.b", 10)
	assert.NoError(err)
	_, err = m.Subscribe("c.d", 20)
	assert.NoError(err)

	m.Unsubscribe(subA)
	m.Unsubscribe(subA)

	subC, err := m.Subscribe("e.f", 30)
	assert.NoError(err)
	subD, err := m.Subscribe("a.b", 40)
	assert.NoError(err)

	// Regression: unsubscribing the same Subscription twice must not enqueue
	// its position twice, which aliased two live subscriptions to one ID and
	// made them overwrite each other.
	assert.NotEqual(subC.ID, subD.ID, "reclaimed position must not be handed out twice")
	assertEqual(assert, []Subscriber{30}, m.Lookup("e.f"))
	assertEqual(assert, []Subscriber{40}, m.Lookup("a.b"))
}

func BenchmarkInvertedBitmapMatcherSubscribe(b *testing.B) {
	var (
		topics = []string{
			"forex",
			"forex.gbp",
			"forex.eur",
			"forex.usd",
			"trade",
			"trade.usd",
			"trade.jpy",
			"foo.bar.baz.qux.quux",
		}
		ib = NewInvertedBitmapMatcher(topics)
		s0 = 0
	)
	populateMatcher(ib, 1000, 5)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ib.Subscribe("foo.*.baz.qux.quux", s0)
	}
}

func BenchmarkInvertedBitmapMatcherUnsubscribe(b *testing.B) {
	var (
		topics = []string{
			"forex",
			"forex.gbp",
			"forex.eur",
			"forex.usd",
			"trade",
			"trade.usd",
			"trade.jpy",
			"foo.bar.baz.qux.quux",
		}
		ib = NewInvertedBitmapMatcher(topics)
		s0 = 0
	)
	populateMatcher(ib, 1000, 5)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id, _ := ib.Subscribe("foo.*.baz.qux.quux", s0)
		ib.Unsubscribe(id)
	}
}

func BenchmarkInvertedBitmapMatcherLookup(b *testing.B) {
	var (
		topics = []string{
			"forex",
			"forex.gbp",
			"forex.eur",
			"forex.usd",
			"trade",
			"trade.usd",
			"trade.jpy",
			"foo.bar.baz.qux.quux",
		}
		ib = NewInvertedBitmapMatcher(topics)
		s0 = 0
	)
	_, _ = ib.Subscribe("foo.*.baz.qux.quux", s0)
	populateMatcher(ib, 1000, 5)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ib.Lookup("foo.bar.baz.qux.quux")
	}
}

func BenchmarkInvertedBitmapMatcherSubscribeCold(b *testing.B) {
	var (
		topics = []string{
			"forex",
			"forex.gbp",
			"forex.eur",
			"forex.usd",
			"trade",
			"trade.usd",
			"trade.jpy",
			"foo.bar.baz.qux.quux",
		}
		ib = NewInvertedBitmapMatcher(topics)
		s0 = 0
	)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ib.Subscribe("foo.*.baz.qux.quux", s0)
	}
}

func BenchmarkInvertedBitmapMatcherUnsubscribeCold(b *testing.B) {
	var (
		topics = []string{
			"forex",
			"forex.gbp",
			"forex.eur",
			"forex.usd",
			"trade",
			"trade.usd",
			"trade.jpy",
			"foo.bar.baz.qux.quux",
		}
		ib = NewInvertedBitmapMatcher(topics)
		s0 = 0
	)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id, _ := ib.Subscribe("foo.*.baz.qux.quux", s0)
		ib.Unsubscribe(id)
	}
}

func BenchmarkInvertedBitmapMatcherLookupCold(b *testing.B) {
	var (
		topics = []string{
			"forex",
			"forex.gbp",
			"forex.eur",
			"forex.usd",
			"trade",
			"trade.usd",
			"trade.jpy",
			"foo.bar.baz.qux.quux",
		}
		ib = NewInvertedBitmapMatcher(topics)
		s0 = 0
	)
	_, _ = ib.Subscribe("foo.*.baz.qux.quux", s0)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ib.Lookup("foo.bar.baz.qux.quux")
	}
}

func BenchmarkMultithreaded1Thread5050InvertedBitmap(b *testing.B) {
	numItems := 1000
	numThreads := 1
	benchmark5050(b, numItems, numThreads, func(items [][]string) Matcher {
		topics := []string{}
		for _, s := range items {
			topics = append(topics, s...)
		}
		return NewInvertedBitmapMatcher(topics)
	})
}

func BenchmarkMultithreaded2Thread5050InvertedBitmap(b *testing.B) {
	numItems := 1000
	numThreads := 2
	benchmark5050(b, numItems, numThreads, func(items [][]string) Matcher {
		topics := []string{}
		for _, s := range items {
			topics = append(topics, s...)
		}
		return NewInvertedBitmapMatcher(topics)
	})
}

func BenchmarkMultithreaded4Thread5050InvertedBitmap(b *testing.B) {
	numItems := 1000
	numThreads := 4
	benchmark5050(b, numItems, numThreads, func(items [][]string) Matcher {
		topics := []string{}
		for _, s := range items {
			topics = append(topics, s...)
		}
		return NewInvertedBitmapMatcher(topics)
	})
}

func BenchmarkMultithreaded8Thread5050InvertedBitmap(b *testing.B) {
	numItems := 1000
	numThreads := 8
	benchmark5050(b, numItems, numThreads, func(items [][]string) Matcher {
		topics := []string{}
		for _, s := range items {
			topics = append(topics, s...)
		}
		return NewInvertedBitmapMatcher(topics)
	})
}

func BenchmarkMultithreaded12Thread5050InvertedBitmap(b *testing.B) {
	numItems := 1000
	numThreads := 12
	benchmark5050(b, numItems, numThreads, func(items [][]string) Matcher {
		topics := []string{}
		for _, s := range items {
			topics = append(topics, s...)
		}
		return NewInvertedBitmapMatcher(topics)
	})
}

func BenchmarkMultithreaded16Thread5050InvertedBitmap(b *testing.B) {
	numItems := 1000
	numThreads := 16
	benchmark5050(b, numItems, numThreads, func(items [][]string) Matcher {
		topics := []string{}
		for _, s := range items {
			topics = append(topics, s...)
		}
		return NewInvertedBitmapMatcher(topics)
	})
}

func BenchmarkMultithreaded1Thread9010InvertedBitmap(b *testing.B) {
	numItems := 1000
	numThreads := 1
	benchmark9010(b, numItems, numThreads, func(items [][]string) Matcher {
		topics := []string{}
		for _, s := range items {
			topics = append(topics, s...)
		}
		return NewInvertedBitmapMatcher(topics)
	})
}

func BenchmarkMultithreaded2Thread9010InvertedBitmap(b *testing.B) {
	numItems := 1000
	numThreads := 2
	benchmark9010(b, numItems, numThreads, func(items [][]string) Matcher {
		topics := []string{}
		for _, s := range items {
			topics = append(topics, s...)
		}
		return NewInvertedBitmapMatcher(topics)
	})
}

func BenchmarkMultithreaded4Thread9010InvertedBitmap(b *testing.B) {
	numItems := 1000
	numThreads := 4
	benchmark9010(b, numItems, numThreads, func(items [][]string) Matcher {
		topics := []string{}
		for _, s := range items {
			topics = append(topics, s...)
		}
		return NewInvertedBitmapMatcher(topics)
	})
}

func BenchmarkMultithreaded8Thread9010InvertedBitmap(b *testing.B) {
	numItems := 1000
	numThreads := 8
	benchmark9010(b, numItems, numThreads, func(items [][]string) Matcher {
		topics := []string{}
		for _, s := range items {
			topics = append(topics, s...)
		}
		return NewInvertedBitmapMatcher(topics)
	})
}

func BenchmarkMultithreaded12Thread9010InvertedBitmap(b *testing.B) {
	numItems := 1000
	numThreads := 12
	benchmark9010(b, numItems, numThreads, func(items [][]string) Matcher {
		topics := []string{}
		for _, s := range items {
			topics = append(topics, s...)
		}
		return NewInvertedBitmapMatcher(topics)
	})
}

func BenchmarkMultithreaded16Thread9010InvertedBitmap(b *testing.B) {
	numItems := 1000
	numThreads := 16
	benchmark9010(b, numItems, numThreads, func(items [][]string) Matcher {
		topics := []string{}
		for _, s := range items {
			topics = append(topics, s...)
		}
		return NewInvertedBitmapMatcher(topics)
	})
}
