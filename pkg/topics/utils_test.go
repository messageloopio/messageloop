package topics

import (
	"math/rand"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func assertEqual(assert *assert.Assertions, expected, actual []Subscriber) {
	assert.Len(actual, len(expected))
	for _, sub := range expected {
		assert.Contains(actual, sub)
	}
}

func testMatcherConcurrentSubscribe(t *testing.T, m Matcher, topics []string) {
	assert := assert.New(t)

	subs := make([]*Subscription, len(topics))
	var wg sync.WaitGroup
	for i := range topics {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sub, err := m.Subscribe(topics[i], Subscriber(i))
			assert.NoError(err)
			subs[i] = sub
		}(i)
	}
	wg.Wait()

	assertSubscriptionIDsUnique(assert, subs)
	for i, topic := range topics {
		assert.Contains(m.Lookup(topic), Subscriber(i))
	}

	for i := range subs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m.Unsubscribe(subs[i])
		}(i)
	}
	wg.Wait()

	for _, topic := range topics {
		assert.Empty(m.Lookup(topic))
	}

	// Re-subscribe concurrently; reclaimed positions must not be handed out twice.
	subs = make([]*Subscription, len(topics))
	for i := range topics {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sub, err := m.Subscribe(topics[i], Subscriber(i))
			assert.NoError(err)
			subs[i] = sub
		}(i)
	}
	wg.Wait()

	assertSubscriptionIDsUnique(assert, subs)
	for i, topic := range topics {
		assert.Contains(m.Lookup(topic), Subscriber(i))
	}
}

func assertSubscriptionIDsUnique(assert *assert.Assertions, subs []*Subscription) {
	ids := make(map[uint32]int, len(subs))
	for i, sub := range subs {
		if !assert.NotNil(sub) {
			continue
		}
		if first, dup := ids[sub.ID]; dup {
			assert.Failf("duplicate subscription ID", "ID %d shared by subscriptions %d and %d", sub.ID, first, i)
		}
		ids[sub.ID] = i
	}
}

func populateMatcher(m Matcher, num, topicSize int) {
	for i := 0; i < num; i++ {
		prefix := ""
		topic := ""
		for j := 0; j < topicSize; j++ {
			topic += prefix + strconv.Itoa(rand.Int())
			prefix = "."
		}
		_, _ = m.Subscribe(topic, Subscriber(topic))
	}
}

func benchmark5050(b *testing.B, numItems, numThreads int, factory func([][]string) Matcher) {
	itemsToInsert := make([][]string, 0, numThreads)
	for i := 0; i < numThreads; i++ {
		items := make([]string, 0, numItems)
		for j := 0; j < numItems; j++ {
			topic := strconv.Itoa(j%10) + "." + strconv.Itoa(j%50) + "." + strconv.Itoa(j)
			items = append(items, topic)
		}
		itemsToInsert = append(itemsToInsert, items)
	}

	var wg sync.WaitGroup
	sub := Subscriber("abc")
	m := factory(itemsToInsert)
	populateMatcher(m, 1000, 5)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		wg.Add(numThreads)
		for j := 0; j < numThreads; j++ {
			go func(j int) {
				if j%2 != 0 {
					for _, key := range itemsToInsert[j] {
						_, _ = m.Subscribe(key, sub)
					}
				} else {
					for _, key := range itemsToInsert[j] {
						m.Lookup(key)
					}
				}
				wg.Done()
			}(j)
		}
		wg.Wait()
	}
}

func benchmark9010(b *testing.B, numItems, numThreads int, factory func([][]string) Matcher) {
	itemsToInsert := make([][]string, 0, numThreads)
	for i := 0; i < numThreads; i++ {
		items := make([]string, 0, numItems)
		for j := 0; j < numItems; j++ {
			topic := strconv.Itoa(j%10) + "." + strconv.Itoa(j%50) + "." + strconv.Itoa(j)
			items = append(items, topic)
		}
		itemsToInsert = append(itemsToInsert, items)
	}

	var wg sync.WaitGroup
	sub := Subscriber("abc")
	m := factory(itemsToInsert)
	populateMatcher(m, 1000, 5)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		wg.Add(numThreads)
		for j := 0; j < numThreads; j++ {
			go func(j int) {
				if j%10 == 0 {
					for _, key := range itemsToInsert[j] {
						_, _ = m.Subscribe(key, sub)
					}
				} else {
					for _, key := range itemsToInsert[j] {
						m.Lookup(key)
					}
				}
				wg.Done()
			}(j)
		}
		wg.Wait()
	}
}
