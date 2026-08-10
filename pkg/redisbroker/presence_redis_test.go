package redisbroker

import (
	"context"
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
