package v2_test

import (
	"testing"
	"time"

	v2 "github.com/messageloopio/messageloop/v2"
)

func TestDedupCache_FirstSeen_NotDuplicate(t *testing.T) {
	c := v2.NewDedupCache()
	if c.IsDuplicate("session-1", 1) {
		t.Error("first occurrence should not be duplicate")
	}
}

func TestDedupCache_SecondSeen_IsDuplicate(t *testing.T) {
	c := v2.NewDedupCache()
	c.IsDuplicate("session-1", 1) // record first time
	if !c.IsDuplicate("session-1", 1) {
		t.Error("second occurrence should be duplicate")
	}
}

func TestDedupCache_DifferentSessions_Independent(t *testing.T) {
	c := v2.NewDedupCache()
	c.IsDuplicate("session-1", 42)
	// Same seq for different session must NOT be flagged as duplicate
	if c.IsDuplicate("session-2", 42) {
		t.Error("same seq on different session should not be duplicate")
	}
}

func TestDedupCache_ExpiredEntry_NotDuplicate(t *testing.T) {
	c := v2.NewDedupCacheWithTTL(10 * time.Millisecond)
	c.IsDuplicate("session-1", 1)
	time.Sleep(20 * time.Millisecond)
	// Expired - should be treated as new
	if c.IsDuplicate("session-1", 1) {
		t.Error("expired entry should not be flagged as duplicate")
	}
}

func TestDedupCache_Evict_RemovesExpired(t *testing.T) {
	c := v2.NewDedupCacheWithTTL(10 * time.Millisecond)
	c.IsDuplicate("session-1", 1)
	c.IsDuplicate("session-1", 2)
	if c.Size() != 2 {
		t.Fatalf("expected 2 entries, got %d", c.Size())
	}
	time.Sleep(20 * time.Millisecond)
	c.Evict()
	if c.Size() != 0 {
		t.Errorf("expected 0 entries after eviction, got %d", c.Size())
	}
}
