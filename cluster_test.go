package messageloop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type trackingClusterComponent struct {
	startCount    int
	shutdownCount int
}

func (c *trackingClusterComponent) Start(context.Context) error {
	c.startCount++
	return nil
}

func (c *trackingClusterComponent) Shutdown(context.Context) error {
	c.shutdownCount++
	return nil
}

func (c *trackingClusterComponent) PutNodeLease(context.Context, *ClusterNodeLease, time.Duration) error {
	return nil
}

func (c *trackingClusterComponent) GetNodeLease(context.Context, string, string) (*ClusterNodeLease, error) {
	return nil, nil
}

func (c *trackingClusterComponent) PutSessionLease(context.Context, *ClusterSessionLease, time.Duration) error {
	return nil
}

func (c *trackingClusterComponent) CompareAndSwapSessionLease(context.Context, *ClusterSessionLease, *ClusterSessionLease, time.Duration) (bool, error) {
	return true, nil
}

func (c *trackingClusterComponent) GetSessionLease(context.Context, string) (*ClusterSessionLease, error) {
	return nil, nil
}

func (c *trackingClusterComponent) DeleteSessionLease(context.Context, string) error {
	return nil
}

func (c *trackingClusterComponent) PutSessionSnapshot(context.Context, *ClusterSessionSnapshot, time.Duration) error {
	return nil
}

func (c *trackingClusterComponent) GetSessionSnapshot(context.Context, string) (*ClusterSessionSnapshot, error) {
	return nil, nil
}

func (c *trackingClusterComponent) DeleteSessionSnapshot(context.Context, string) error {
	return nil
}

func (c *trackingClusterComponent) AddUserSession(context.Context, string, string, time.Duration) error {
	return nil
}

func (c *trackingClusterComponent) RemoveUserSession(context.Context, string, string) error {
	return nil
}

func (c *trackingClusterComponent) ListUserSessions(context.Context, string) ([]string, error) {
	return nil, nil
}

func (c *trackingClusterComponent) SetHandler(ClusterCommandHandler) {}

func (c *trackingClusterComponent) SendCommand(context.Context, *ClusterCommand) (*ClusterCommandResult, error) {
	return nil, nil
}

func (c *trackingClusterComponent) BroadcastCommand(context.Context, *ClusterCommand) ([]*ClusterCommandResult, error) {
	return nil, nil
}

func (c *trackingClusterComponent) AdjustChannelSubscriptions(context.Context, string, int64, time.Duration) error {
	return nil
}

func (c *trackingClusterComponent) ReplaceNodeChannels(context.Context, map[string]int64, time.Duration) error {
	return nil
}

func (c *trackingClusterComponent) ListChannels(context.Context) ([]ClusterChannelInfo, error) {
	return nil, nil
}

func (c *trackingClusterComponent) ListNodeProjections(context.Context) ([]ClusterNodeProjection, error) {
	return nil, nil
}

func (c *trackingClusterComponent) DeleteNodeProjection(context.Context, string, string) error {
	return nil
}

// failingClusterComponent fails its Start call exactly once, on the call
// whose startCount equals failOn.
type failingClusterComponent struct {
	trackingClusterComponent
	failOn int
}

func (c *failingClusterComponent) Start(context.Context) error {
	c.startCount++
	if c.failOn > 0 && c.startCount == c.failOn {
		return errors.New("start failed")
	}
	return nil
}

func TestNewCluster_Disabled(t *testing.T) {
	t.Parallel()

	runtime, err := NewCluster(ClusterOptions{}, ClusterDependencies{})
	assert.NoError(t, err)
	assert.NotNil(t, runtime)
	assert.False(t, runtime.Enabled())
	assert.Empty(t, runtime.NodeID())
	assert.Empty(t, runtime.IncarnationID())
	assert.Empty(t, runtime.Backend())
}

func TestNewCluster_EnabledRequiresNodeID(t *testing.T) {
	t.Parallel()

	runtime, err := NewCluster(ClusterOptions{Enabled: true, Backend: "redis"}, ClusterDependencies{})
	assert.Nil(t, runtime)
	assert.EqualError(t, err, "cluster node_id is required when cluster is enabled")
}

func TestNewCluster_EnabledGeneratesIncarnationID(t *testing.T) {
	t.Parallel()

	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", Backend: "redis"}, ClusterDependencies{})
	assert.NoError(t, err)
	assert.True(t, runtime.Enabled())
	assert.Equal(t, "node-a", runtime.NodeID())
	assert.Equal(t, "redis", runtime.Backend())
	assert.NotEmpty(t, runtime.IncarnationID())
}

func TestCluster_StartAndShutdownOnlyOnce(t *testing.T) {
	t.Parallel()

	sessionDirectory := &trackingClusterComponent{}
	commandBus := &trackingClusterComponent{}
	queryStore := &trackingClusterComponent{}
	nodeLeaseManager := &trackingClusterComponent{}
	projectionRepairer := &trackingClusterComponent{}

	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a"}, ClusterDependencies{
		SessionDirectory:   sessionDirectory,
		CommandBus:         commandBus,
		QueryStore:         queryStore,
		NodeLeaseManager:   nodeLeaseManager,
		ProjectionRepairer: projectionRepairer,
	})
	assert.NoError(t, err)

	ctx := context.Background()
	assert.NoError(t, runtime.Start(ctx))
	assert.NoError(t, runtime.Start(ctx))
	assert.NoError(t, runtime.Shutdown(ctx))
	assert.NoError(t, runtime.Shutdown(ctx))

	assert.Equal(t, 1, sessionDirectory.startCount)
	assert.Equal(t, 1, commandBus.startCount)
	assert.Equal(t, 1, queryStore.startCount)
	assert.Equal(t, 1, nodeLeaseManager.startCount)
	assert.Equal(t, 1, projectionRepairer.startCount)

	assert.Equal(t, 1, sessionDirectory.shutdownCount)
	assert.Equal(t, 1, commandBus.shutdownCount)
	assert.Equal(t, 1, queryStore.shutdownCount)
	assert.Equal(t, 1, nodeLeaseManager.shutdownCount)
	assert.Equal(t, 1, projectionRepairer.shutdownCount)
}

func TestCluster_StartPartialFailureRollsBackAndAllowsRetry(t *testing.T) {
	sessionDirectory := &trackingClusterComponent{}
	commandBus := &trackingClusterComponent{}
	queryStore := &failingClusterComponent{failOn: 1}
	nodeLeaseManager := &trackingClusterComponent{}
	projectionRepairer := &trackingClusterComponent{}

	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a"}, ClusterDependencies{
		SessionDirectory:   sessionDirectory,
		CommandBus:         commandBus,
		QueryStore:         queryStore,
		NodeLeaseManager:   nodeLeaseManager,
		ProjectionRepairer: projectionRepairer,
	})
	require.NoError(t, err)

	ctx := context.Background()
	err = runtime.Start(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "start failed")

	// The failing component and its successors must not have been started.
	assert.Equal(t, 1, sessionDirectory.startCount)
	assert.Equal(t, 1, commandBus.startCount)
	assert.Equal(t, 1, queryStore.startCount)
	assert.Equal(t, 0, nodeLeaseManager.startCount)
	assert.Equal(t, 0, projectionRepairer.startCount)

	// Already-started components are shut down in reverse order.
	assert.Equal(t, 1, sessionDirectory.shutdownCount)
	assert.Equal(t, 1, commandBus.shutdownCount)
	assert.Equal(t, 0, queryStore.shutdownCount)
	assert.Equal(t, 0, nodeLeaseManager.shutdownCount)
	assert.Equal(t, 0, projectionRepairer.shutdownCount)

	// A failed start can be retried on the same instance.
	require.NoError(t, runtime.Start(ctx))
	assert.Equal(t, 2, sessionDirectory.startCount)
	assert.Equal(t, 2, commandBus.startCount)
	assert.Equal(t, 2, queryStore.startCount)
	assert.Equal(t, 1, nodeLeaseManager.startCount)
	assert.Equal(t, 1, projectionRepairer.startCount)

	require.NoError(t, runtime.Shutdown(ctx))
	assert.Equal(t, 2, sessionDirectory.shutdownCount)
	assert.Equal(t, 2, commandBus.shutdownCount)
	assert.Equal(t, 1, queryStore.shutdownCount)
	assert.Equal(t, 1, nodeLeaseManager.shutdownCount)
	assert.Equal(t, 1, projectionRepairer.shutdownCount)
}
