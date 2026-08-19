package runtime

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
// incarnation path in cluster.go, internal/cluster/epoch.go and
// cmd/server/main.go must not call
// uuid.NewString. The broker StreamEpoch UUID (pkg/redisbroker) is a
// different clock and intentionally untouched.
func TestNoUUIDIncarnationInProductionSource(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"cluster.go", "../cluster/epoch.go", "../../cmd/server/main.go"} {
		source, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.NotContains(t, string(source), "uuid.NewString",
			"%s must not allocate incarnations via uuid.NewString", path)
	}
}
