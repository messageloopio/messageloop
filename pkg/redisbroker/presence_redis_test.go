package redisbroker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/messageloopio/messageloop"
	"github.com/stretchr/testify/require"
)

// Task 13d: presence index TTL must match the member TTL, and Remove must
// clean up the index when the last member leaves.
func TestRedisPresenceStore_IndexTTLAndCleanup(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	store := NewPresenceStore(redisCfg).(*redisPresenceStore)
	t.Cleanup(func() { _ = store.client.Close() })

	ch := "presence-metrics"
	require.NoError(t, store.Add(context.Background(), ch, &messageloop.PresenceInfo{ClientID: "c1", UserID: "u1"}))
	require.NoError(t, store.Add(context.Background(), ch, &messageloop.PresenceInfo{ClientID: "c2", UserID: "u2"}))

	indexKey := store.indexKey(ch)
	ttl, err := store.client.TTL(context.Background(), indexKey).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, 30*time.Second, "index TTL must be near the member TTL (60s), not double")
	require.LessOrEqual(t, ttl, 60*time.Second)

	// Removing one member keeps the index (another member remains).
	require.NoError(t, store.Remove(context.Background(), ch, "c1"))
	exists, err := store.client.Exists(context.Background(), indexKey).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), exists, "index must survive while members remain")

	// Removing the last member deletes the index entirely.
	require.NoError(t, store.Remove(context.Background(), ch, "c2"))
	exists, err = store.client.Exists(context.Background(), indexKey).Result()
	require.NoError(t, err)
	require.Zero(t, exists, "empty index must be removed")
}

// TestRedisPresenceStore_RemoveIsAtomic verifies P2-11: Remove's
// SCARD->DEL-index decision runs inside the Lua script, so the index can
// never be deleted while the member key still exists ("online but invisible"
// ghost window closed by a concurrent Add).
func TestRedisPresenceStore_RemoveIsAtomic(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	store := NewPresenceStore(redisCfg).(*redisPresenceStore)
	t.Cleanup(func() { _ = store.client.Close() })
	ctx := context.Background()
	ch := "presence-atomic"

	// Hammer Add/Remove concurrently: with the old SCard-then-Del sequence an
	// Add landing between SREM and DEL produced a member key with no index
	// entry. The script makes that state unreachable: every Remove either
	// deletes member+index together or leaves both intact.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = store.Add(ctx, ch, &messageloop.PresenceInfo{ClientID: "c-race", UserID: "u-race"})
				_ = store.Remove(ctx, ch, "c-race")
			}
		}()
	}
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()

	// No ghost state may survive the hammer: if the member key exists, the
	// index must contain it.
	exists, err := store.client.Exists(ctx, store.memberKey(ch, "c-race")).Result()
	require.NoError(t, err)
	members, err := store.client.SMembers(ctx, store.indexKey(ch)).Result()
	require.NoError(t, err)
	if exists == 1 {
		require.Contains(t, members, "c-race",
			"member key must never outlive its index entry (ghost window)")
	}

	// Final Add makes the member online; Get must see it.
	require.NoError(t, store.Add(ctx, ch, &messageloop.PresenceInfo{ClientID: "c-race", UserID: "u-race"}))
	present, err := store.Get(ctx, ch)
	require.NoError(t, err)
	require.Contains(t, present, "c-race", "online member must be visible after Add")
}
