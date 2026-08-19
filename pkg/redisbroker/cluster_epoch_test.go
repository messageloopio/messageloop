package redisbroker

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/messageloopio/messageloop/internal/cluster"
)

func TestRedisSessionDirectory_NextNodeEpoch(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	ctx := context.Background()

	directory := NewSessionDirectory(redisCfg)
	t.Cleanup(func() { _ = directory.Shutdown(ctx) })

	allocator, ok := directory.(cluster.NodeEpochAllocator)
	require.True(t, ok, "redis session directory must implement NodeEpochAllocator")

	first, err := allocator.NextNodeEpoch(ctx, "node-epoch-a")
	require.NoError(t, err)
	second, err := allocator.NextNodeEpoch(ctx, "node-epoch-a")
	require.NoError(t, err)
	assert.Equal(t, first+1, second, "INCR issues strictly +1 epochs for one nodeID")
	assert.Equal(t, "1", cluster.FormatNodeEpoch(first))
	assert.Equal(t, "2", cluster.FormatNodeEpoch(second))

	other, err := allocator.NextNodeEpoch(ctx, "node-epoch-b")
	require.NoError(t, err)
	assert.Equal(t, uint64(1), other, "a different nodeID starts its own INCR sequence at 1")

	_, err = allocator.NextNodeEpoch(ctx, "")
	assert.Error(t, err)
}

// TestRedisSessionDirectory_NodeEpochKeyEscapesNodeLeaseScan pins the key
// shape: ml2:cluster:node_epoch:{nodeID} must NOT match the
// ml2:cluster:node:* SCAN used by ListNodeLeases, or the membership repair
// loop would try to parse the counter as a node lease.
func TestRedisSessionDirectory_NodeEpochKeyEscapesNodeLeaseScan(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	ctx := context.Background()

	directory := NewSessionDirectory(redisCfg)
	t.Cleanup(func() { _ = directory.Shutdown(ctx) })

	allocator, ok := directory.(cluster.NodeEpochAllocator)
	require.True(t, ok)
	epoch, err := allocator.NextNodeEpoch(ctx, "node-epoch-scan")
	require.NoError(t, err)

	lease := &cluster.ClusterNodeLease{
		NodeID:        "node-epoch-scan",
		IncarnationID: cluster.FormatNodeEpoch(epoch),
		StartedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(time.Minute),
	}
	require.NoError(t, directory.PutNodeLease(ctx, lease, time.Minute))

	lister, ok := directory.(cluster.ClusterNodeLeaseLister)
	require.True(t, ok)
	leases, err := lister.ListNodeLeases(ctx)
	require.NoError(t, err)
	require.Len(t, leases, 1, "the node_epoch counter key must not be scanned as a node lease")
	assert.Equal(t, "node-epoch-scan", leases[0].NodeID)
}
