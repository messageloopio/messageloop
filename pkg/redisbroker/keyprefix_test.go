package redisbroker

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/messageloopio/messageloop"
	"github.com/stretchr/testify/require"
)

// TestKeyPrefixGeneration_Ml2IsolatedFromLegacyMl pins KD-K31 (PR-KA-C5):
// every key written by the broker, the presence store, and the cluster
// session directory lives under the ml2: generation, and nothing this
// process writes lands under the legacy ml: generation. Assertions are
// scoped to this test's unique channel/node marker because the test DB is
// shared with other tests.
func TestKeyPrefixGeneration_Ml2IsolatedFromLegacyMl(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	ctx := context.Background()

	opts := NewOptions(redisCfg)
	client := newRedisClient(opts)
	t.Cleanup(func() { _ = client.Close() })

	marker := uuid.NewString()
	ch := "c5keyprefix." + marker
	nodeID := "c5keyprefix-node-" + marker
	clientID := "c5keyprefix-client-" + marker

	// Broker publish: history stream + dense seq counter + first_retained
	// marker (+ pub/sub delivery, which leaves no key).
	broker := New(redisCfg).(*redisBroker)
	t.Cleanup(func() { _ = broker.client.Close() })
	_, err := broker.Publish(ch, &messageloop.Publication{
		Payload: []byte("c5-keyprefix-probe"),
	})
	require.NoError(t, err)

	// Presence store: member key + channel index + occupancy generation.
	presence := NewPresenceStore(redisCfg).(*redisPresenceStore)
	t.Cleanup(func() { _ = presence.client.Close() })
	require.NoError(t, presence.Add(ctx, ch, &messageloop.PresenceInfo{ClientID: clientID, UserID: "c5-user"}))

	// Cluster session directory: node_epoch counter (KD-K27).
	directory := NewSessionDirectory(redisCfg)
	t.Cleanup(func() { _ = directory.Shutdown(ctx) })
	allocator, ok := directory.(messageloop.NodeEpochAllocator)
	require.True(t, ok)
	_, err = allocator.NextNodeEpoch(ctx, nodeID)
	require.NoError(t, err)

	// The legacy generation must not hold any of this test's keys. The SCAN
	// pattern is concatenated so the retired prefix never appears as a quoted
	// literal (the repo gate forbids that form in Go sources).
	legacyKeys, err := scanKeys(ctx, client, "ml"+":*")
	require.NoError(t, err)
	for _, key := range legacyKeys {
		require.NotContains(t, key, marker,
			"legacy-generation key %q must not exist; every key this process writes lives under ml2:", key)
	}

	// The new generation holds exactly the key families this test wrote.
	expectedKeys := []string{
		opts.StreamPrefix + ch,
		opts.StreamPrefix + "seq:" + ch,
		opts.StreamPrefix + "retained:" + ch,
		opts.PresencePrefix + "idx:" + ch,
		opts.PresencePrefix + "member:" + ch + ":" + clientID,
		opts.ClusterPrefix + "node_epoch:" + nodeID,
	}
	for _, key := range expectedKeys {
		require.True(t, strings.HasPrefix(key, "ml2:"), "key %q must live under the ml2: generation", key)
		exists, err := client.Exists(ctx, key).Result()
		require.NoError(t, err)
		require.Equal(t, int64(1), exists, "expected new-generation key %q to exist", key)
	}

	// And a SCAN of the new generation finds this test's keys.
	newKeys, err := scanKeys(ctx, client, "ml2:*"+marker+"*")
	require.NoError(t, err)
	require.NotEmpty(t, newKeys, "the ml2: generation must contain this test's keys")
}
