package v2

import (
	"sync"
	"time"
)

// dedupEntry stores deduplication state for a client sequence number.
type dedupEntry struct {
	expiresAt time.Time
}

// DedupCache tracks processed client sequence numbers to prevent duplicate message delivery.
// Key format: "session_id:client_seq"
// TTL defaults to 5 minutes.
type DedupCache struct {
	mu      sync.Mutex
	entries map[string]dedupEntry
	ttl     time.Duration
}

// NewDedupCache creates a DedupCache with a 5-minute TTL.
func NewDedupCache() *DedupCache {
	return NewDedupCacheWithTTL(5 * time.Minute)
}

// NewDedupCacheWithTTL creates a DedupCache with a custom TTL.
func NewDedupCacheWithTTL(ttl time.Duration) *DedupCache {
	return &DedupCache{
		entries: make(map[string]dedupEntry),
		ttl:     ttl,
	}
}

// IsDuplicate checks if the given session+seq has been seen before.
// Returns true if duplicate (already processed). If not a duplicate,
// records it and returns false.
func (c *DedupCache) IsDuplicate(sessionID string, clientSeq uint64) bool {
	key := sessionID + ":" + uint64ToStr(clientSeq)
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	if entry, ok := c.entries[key]; ok {
		if now.Before(entry.expiresAt) {
			return true
		}
		// Expired — treat as new
	}

	c.entries[key] = dedupEntry{expiresAt: now.Add(c.ttl)}
	return false
}

// Evict removes expired entries. Call periodically to prevent unbounded growth.
func (c *DedupCache) Evict() {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range c.entries {
		if now.After(v.expiresAt) {
			delete(c.entries, k)
		}
	}
}

// Size returns the number of entries currently in the cache.
func (c *DedupCache) Size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// uint64ToStr converts uint64 to string without importing strconv at package level.
func uint64ToStr(n uint64) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := 20
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}
