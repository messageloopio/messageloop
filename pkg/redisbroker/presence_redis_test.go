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

// TestRedisPresenceStore_NextOccupancyGenIncr pins B2 §4: the Redis presence
// adapter issues strictly increasing per-channel generations via INCR.
func TestRedisPresenceStore_NextOccupancyGenIncr(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	store := NewPresenceStore(redisCfg).(*redisPresenceStore)
	t.Cleanup(func() { _ = store.client.Close() })
	ctx := context.Background()

	g1, err := store.NextOccupancyGen(ctx, "ch-a")
	require.NoError(t, err)
	require.Greater(t, g1, uint64(0), "the first gen is 1-based, never 0")
	g2, err := store.NextOccupancyGen(ctx, "ch-a")
	require.NoError(t, err)
	require.Greater(t, g2, g1, "gens are strictly increasing per channel")
	gOther, err := store.NextOccupancyGen(ctx, "ch-b")
	require.NoError(t, err)
	require.Equal(t, uint64(1), gOther, "gens are per-channel, not global")
}

// TestRedisPresenceStore_SyntheticLeaveHookOnPrune pins B2 §5.3: when Get
// prunes a ghost member whose TTL key evaporated, the synthetic-leave hook
// fires for that (channel, client). No fixed sleeps: the member TTL is
// shortened and Then the hook is observed via Eventually.
func TestRedisPresenceStore_SyntheticLeaveHookOnPrune(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	store := NewPresenceStore(redisCfg).(*redisPresenceStore)
	t.Cleanup(func() { _ = store.client.Close() })
	ctx := context.Background()
	ch := "ghost.prune.ch"

	var mu sync.Mutex
	var pruned []string
	store.SetSyntheticLeaveHook(func(_ context.Context, gotCh, clientID string) {
		mu.Lock()
		defer mu.Unlock()
		if gotCh == ch {
			pruned = append(pruned, clientID)
		}
	})

	require.NoError(t, store.Add(ctx, ch, &messageloop.PresenceInfo{ClientID: "ghost1", UserID: "u1"}))
	// Fast-forward the member TTL so the next Get fast-forwards the expiry.
	require.NoError(t, store.client.Expire(ctx, store.memberKey(ch, "ghost1"), 500*time.Millisecond).Err())

	require.Eventually(t, func() bool {
		if _, err := store.Get(ctx, ch); err != nil {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		return len(pruned) == 1
	}, 5*time.Second, 25*time.Millisecond, "the evicted ghost member must synthesize a leave")
	mu.Lock()
	require.Equal(t, []string{"ghost1"}, pruned)
	mu.Unlock()
}
