package messageloop

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatNodeEpoch(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "0", FormatNodeEpoch(0))
	assert.Equal(t, "1", FormatNodeEpoch(1))
	assert.Equal(t, "12", FormatNodeEpoch(12))
}

func TestParseNodeEpoch(t *testing.T) {
	t.Parallel()

	epoch, ok := ParseNodeEpoch("12")
	assert.True(t, ok)
	assert.Equal(t, uint64(12), epoch)

	// Explicit test / C1 sim IDs are not allocator-issued epochs.
	_, ok = ParseNodeEpoch("inc-a")
	assert.False(t, ok)

	for _, invalid := range []string{"", "0", "01", "+1", "1.0", "abc", "node-a-1"} {
		_, ok := ParseNodeEpoch(invalid)
		assert.False(t, ok, "ParseNodeEpoch(%q) must fail", invalid)
	}
}

func TestNodeEpochNewer(t *testing.T) {
	t.Parallel()

	assert.True(t, NodeEpochNewer("2", "1"))
	// Numeric, not lexicographic, ordering.
	assert.True(t, NodeEpochNewer("10", "2"))
	assert.False(t, NodeEpochNewer("1", "2"))
	assert.False(t, NodeEpochNewer("2", "2"))
	// Non-epoch IDs (explicit test incarnations) never compare newer.
	assert.False(t, NodeEpochNewer("inc-a", "1"))
	assert.False(t, NodeEpochNewer("2", "inc-a"))
	assert.False(t, NodeEpochNewer("inc-a", "inc-b"))
}

func TestMemoryNodeEpochAllocator(t *testing.T) {
	t.Parallel()

	allocator := NewMemoryNodeEpochAllocator()
	ctx := context.Background()

	first, err := allocator.NextNodeEpoch(ctx, "node-mem-a")
	require.NoError(t, err)
	assert.Equal(t, uint64(1), first, "first issue for a nodeID is 1")

	second, err := allocator.NextNodeEpoch(ctx, "node-mem-a")
	require.NoError(t, err)
	assert.Equal(t, first+1, second, "same nodeID increments strictly by 1")

	other, err := allocator.NextNodeEpoch(ctx, "node-mem-b")
	require.NoError(t, err)
	assert.Equal(t, uint64(1), other, "a different nodeID starts its own sequence at 1")

	_, err = allocator.NextNodeEpoch(ctx, "")
	assert.Error(t, err)
}

func TestNewCluster_ExplicitIncarnationIDPreserved(t *testing.T) {
	t.Parallel()

	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{})
	require.NoError(t, err)
	assert.Equal(t, "inc-a", runtime.IncarnationID())
}

func TestNewCluster_EmptyIncarnationIDMemoryBackend(t *testing.T) {
	t.Parallel()

	first, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-epoch-mem", Backend: "memory"}, ClusterDependencies{})
	require.NoError(t, err)
	second, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-epoch-mem", Backend: "memory"}, ClusterDependencies{})
	require.NoError(t, err)

	epochFirst, ok := ParseNodeEpoch(first.IncarnationID())
	require.True(t, ok, "memory-allocated incarnation %q must be a decimal node epoch", first.IncarnationID())
	epochSecond, ok := ParseNodeEpoch(second.IncarnationID())
	require.True(t, ok, "memory-allocated incarnation %q must be a decimal node epoch", second.IncarnationID())
	assert.Greater(t, epochSecond, epochFirst, "two independent NewCluster calls for one nodeID issue increasing epochs")
}

func TestNewCluster_EmptyIncarnationIDRedisWithoutAllocator(t *testing.T) {
	t.Parallel()

	// A redis-backend cluster whose session directory cannot allocate node
	// epochs must fail instead of falling back to a random ID.
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", Backend: "redis"}, ClusterDependencies{
		SessionDirectory: &noopSessionDirectory{},
	})
	require.Error(t, err)
	assert.Nil(t, runtime)
	assert.True(t,
		strings.Contains(err.Error(), "node_epoch") || strings.Contains(err.Error(), "incarnation"),
		"error must mention node_epoch or incarnation, got: %v", err)
}

// TestNoUUIDIncarnationInProductionSource pins KD-K27: the production
// incarnation path in cluster.go and cmd/server/main.go must not call
// uuid.NewString. The broker StreamEpoch UUID (pkg/redisbroker) is a
// different clock and intentionally untouched.
func TestNoUUIDIncarnationInProductionSource(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"cluster.go", "cmd/server/main.go"} {
		source, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.NotContains(t, string(source), "uuid.NewString",
			"%s must not allocate incarnations via uuid.NewString", path)
	}
}
