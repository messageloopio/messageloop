package topics

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	numSubs = 1000
	numMsgs = 100000
)

var (
	subs = make([]string, numSubs)
	msgs = make([]string, numMsgs)
)

func init() {
	for i := 0; i < numSubs; i++ {
		if i%10 == 0 {
			subs[i] = fmt.Sprintf("*.%d.%d", rand.Intn(10), rand.Intn(10))
		} else if i%25 == 0 {
			subs[i] = fmt.Sprintf("%d.*.%d", rand.Intn(10), rand.Intn(10))
		} else if i%45 == 0 {
			subs[i] = fmt.Sprintf("%d.%d.*", rand.Intn(10), rand.Intn(10))
		} else {
			subs[i] = fmt.Sprintf("%d.%d.%d", rand.Intn(10), rand.Intn(10), rand.Intn(10))
		}
	}
	for i := 0; i < numMsgs; i++ {
		topic := subs[i%numSubs]
		msgs[i] = strings.ReplaceAll(topic, "*", strconv.Itoa(rand.Intn(10)))
	}
}

func TestThroughput(t *testing.T) {
	naive := NewNaiveMatcher()
	subscribeSubs(t, naive)
	testThroughputLookups(t, naive, "naive")

	// Reference results from the naive matcher for a sample of the random
	// topic collection; every matcher must agree with them.
	expected := make(map[string][]Subscriber, numMsgs/10)
	for i := 0; i < numMsgs; i += 10 {
		expected[msgs[i]] = naive.Lookup(msgs[i])
	}

	matchers := []struct {
		name    string
		matcher Matcher
	}{
		{"inverted bitmap", NewInvertedBitmapMatcher(msgs)},
		{"optimized inverted bitmap", NewOptimizedInvertedBitmapMatcher(3)},
		{"trie", NewTrieMatcher()},
		{"cs-trie", NewCSTrieMatcher()},
	}
	for _, tc := range matchers {
		subscribeSubs(t, tc.matcher)
		assertLookupConsistency(t, tc.name, tc.matcher, expected)
		testThroughputLookups(t, tc.matcher, tc.name)
	}
}

func subscribeSubs(t *testing.T, m Matcher) {
	t.Helper()
	for i, sub := range subs {
		if _, err := m.Subscribe(sub, i); err != nil {
			t.Fatalf("subscribe %q: %v", sub, err)
		}
	}
}

func testThroughputLookups(t *testing.T, m Matcher, name string) {
	t.Helper()
	before := time.Now()
	for _, msg := range msgs {
		m.Lookup(msg)
	}
	dur := time.Since(before)
	throughput := numMsgs / dur.Seconds()
	fmt.Printf("%s: %f msg/sec\n", name, throughput)
}

func assertLookupConsistency(t *testing.T, name string, m Matcher, expected map[string][]Subscriber) {
	t.Helper()
	for _, msg := range msgs {
		want, ok := expected[msg]
		if !ok {
			continue
		}
		if got := m.Lookup(msg); !equalSubscriberSets(want, got) {
			t.Fatalf("%s: Lookup(%q) = %v, want %v (naive)", name, msg, got, want)
		}
	}
}

func equalSubscriberSets(a, b []Subscriber) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	set := make(map[Subscriber]struct{}, len(a))
	for _, s := range a {
		set[s] = struct{}{}
	}
	for _, s := range b {
		if _, ok := set[s]; !ok {
			return false
		}
	}
	return true
}

func BenchmarkPopulateNaive(b *testing.B) {
	benchmarkPopulate(b, NewNaiveMatcher())
}

func BenchmarkPopulateInvertedBitmap(b *testing.B) {
	benchmarkPopulate(b, NewInvertedBitmapMatcher(msgs))
}

func BenchmarkPopulateOptimizedInvertedBitmap(b *testing.B) {
	benchmarkPopulate(b, NewOptimizedInvertedBitmapMatcher(3))
}

func BenchmarkPopulateTrie(b *testing.B) {
	benchmarkPopulate(b, NewTrieMatcher())
}

func BenchmarkPopulateCSTrie(b *testing.B) {
	benchmarkPopulate(b, NewCSTrieMatcher())
}

func benchmarkPopulate(b *testing.B, m Matcher) {
	b.ReportAllocs()
	b.ResetTimer()
	for j := 0; j < b.N; j++ {
		for i, sub := range subs {
			if _, err := m.Subscribe(sub, i); err != nil {
				b.Fatal(err)
			}
		}
	}
}
