package redisbroker

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestClusterQueryStore_AdjustChannelSubscriptions verifies the Lua-based
// projection adjustment: increments and decrements from zero, field removal
// at zero, and full-hash deletion when the last field is removed.
func TestClusterQueryStore_AdjustChannelSubscriptions(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	store := NewClusterQueryStore(redisCfg, "node-q", "inc-q").(*redisClusterQueryStore)
	t.Cleanup(func() { _ = store.client.Close() })
	ctx := context.Background()
	key := store.ownerProjectionKey()

	// Increment from zero.
	require.NoError(t, store.AdjustChannelSubscriptions(ctx, "ch-a", 2, time.Minute))
	fields, err := store.client.HGetAll(ctx, key).Result()
	require.NoError(t, err)
	require.Equal(t, "2", fields["ch-a"], "delta must accumulate from zero")

	// Increment again and verify the TTL is refreshed.
	require.NoError(t, store.AdjustChannelSubscriptions(ctx, "ch-a", 1, time.Minute))
	fields, err = store.client.HGetAll(ctx, key).Result()
	require.NoError(t, err)
	require.Equal(t, "3", fields["ch-a"])

	// Decrement keeps the field while the count stays positive.
	require.NoError(t, store.AdjustChannelSubscriptions(ctx, "ch-a", -1, time.Minute))
	fields, err = store.client.HGetAll(ctx, key).Result()
	require.NoError(t, err)
	require.Equal(t, "2", fields["ch-a"])

	// Decrement to zero removes the field but keeps the hash (other fields).
	require.NoError(t, store.AdjustChannelSubscriptions(ctx, "ch-b", 1, time.Minute))
	require.NoError(t, store.AdjustChannelSubscriptions(ctx, "ch-a", -2, time.Minute))
	hExists, err := store.client.HExists(ctx, key, "ch-a").Result()
	require.NoError(t, err)
	require.Zero(t, hExists, "field reaching zero must be removed")

	// Removing the last field deletes the whole hash.
	require.NoError(t, store.AdjustChannelSubscriptions(ctx, "ch-b", -1, time.Minute))
	keyExists, err := store.client.Exists(ctx, key).Result()
	require.NoError(t, err)
	require.Zero(t, keyExists, "empty owner hash must be deleted")

	// A delta that drives a field below zero is clamped to zero (removed).
	require.NoError(t, store.AdjustChannelSubscriptions(ctx, "ch-c", -5, time.Minute))
	keyExists, err = store.client.Exists(ctx, key).Result()
	require.NoError(t, err)
	require.Zero(t, keyExists, "negative clamps must not recreate the hash")

	// Empty channel / zero delta are no-ops.
	require.NoError(t, store.AdjustChannelSubscriptions(ctx, "", 1, time.Minute))
	require.NoError(t, store.AdjustChannelSubscriptions(ctx, "ch-d", 0, time.Minute))
	keyExists, err = store.client.Exists(ctx, key).Result()
	require.NoError(t, err)
	require.Zero(t, keyExists)
}

// TestClusterQueryStore_ReplaceAndListChannels verifies ReplaceNodeChannels
// semantics: it replaces the whole projection and expires it.
func TestClusterQueryStore_ReplaceAndListChannels(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	store := NewClusterQueryStore(redisCfg, "node-q", "inc-q").(*redisClusterQueryStore)
	t.Cleanup(func() { _ = store.client.Close() })
	ctx := context.Background()

	require.NoError(t, store.ReplaceNodeChannels(ctx, map[string]int64{"news": 2, "sports": 1, "invalid": -3}, time.Minute))
	key := store.ownerProjectionKey()
	fields, err := store.client.HGetAll(ctx, key).Result()
	require.NoError(t, err)
	require.Equal(t, "2", fields["news"])
	require.Equal(t, "1", fields["sports"])
	require.NotContains(t, fields, "invalid", "non-positive counts must be skipped")

	// Replace with an empty map deletes the projection.
	require.NoError(t, store.ReplaceNodeChannels(ctx, map[string]int64{}, time.Minute))
	exists, err := store.client.Exists(ctx, key).Result()
	require.NoError(t, err)
	require.Zero(t, exists)
}
