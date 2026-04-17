package messageloop

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeSessionDirectory struct {
	lease     *ClusterSessionLease
	snapshot  *ClusterSessionSnapshot
	nodeLease *ClusterNodeLease
}

func (f *fakeSessionDirectory) Start(context.Context) error    { return nil }
func (f *fakeSessionDirectory) Shutdown(context.Context) error { return nil }
func (f *fakeSessionDirectory) PutNodeLease(context.Context, *ClusterNodeLease, time.Duration) error {
	return nil
}
func (f *fakeSessionDirectory) GetNodeLease(context.Context, string, string) (*ClusterNodeLease, error) {
	return f.nodeLease, nil
}
func (f *fakeSessionDirectory) PutSessionLease(context.Context, *ClusterSessionLease, time.Duration) error {
	return nil
}
func (f *fakeSessionDirectory) CompareAndSwapSessionLease(context.Context, *ClusterSessionLease, *ClusterSessionLease, time.Duration) (bool, error) {
	return true, nil
}
func (f *fakeSessionDirectory) GetSessionLease(context.Context, string) (*ClusterSessionLease, error) {
	return f.lease, nil
}
func (f *fakeSessionDirectory) DeleteSessionLease(context.Context, string) error { return nil }
func (f *fakeSessionDirectory) PutSessionSnapshot(context.Context, *ClusterSessionSnapshot, time.Duration) error {
	return nil
}
func (f *fakeSessionDirectory) GetSessionSnapshot(context.Context, string) (*ClusterSessionSnapshot, error) {
	return f.snapshot, nil
}
func (f *fakeSessionDirectory) DeleteSessionSnapshot(context.Context, string) error { return nil }

type fakeClusterCommandBus struct {
	result           *ClusterCommandResult
	broadcastResults []*ClusterCommandResult
	broadcastErr     error
	commands         []*ClusterCommand
	handler          ClusterCommandHandler
}

func (f *fakeClusterCommandBus) Start(context.Context) error              { return nil }
func (f *fakeClusterCommandBus) Shutdown(context.Context) error           { return nil }
func (f *fakeClusterCommandBus) SetHandler(handler ClusterCommandHandler) { f.handler = handler }
func (f *fakeClusterCommandBus) SendCommand(_ context.Context, cmd *ClusterCommand) (*ClusterCommandResult, error) {
	f.commands = append(f.commands, cmd)
	if f.result != nil {
		return f.result, nil
	}
	return &ClusterCommandResult{CommandID: cmd.CommandID, SessionID: cmd.SessionID, Status: ClusterCommandStatusSucceeded}, nil
}
func (f *fakeClusterCommandBus) BroadcastCommand(context.Context, *ClusterCommand) ([]*ClusterCommandResult, error) {
	return f.broadcastResults, f.broadcastErr
}

type fakeQueryStore struct{}

func (fakeQueryStore) Start(context.Context) error    { return nil }
func (fakeQueryStore) Shutdown(context.Context) error { return nil }
func (fakeQueryStore) AdjustChannelSubscriptions(context.Context, string, int64, time.Duration) error {
	return nil
}
func (fakeQueryStore) ReplaceNodeChannels(context.Context, map[string]int64, time.Duration) error {
	return nil
}
func (fakeQueryStore) ListChannels(context.Context) ([]ClusterChannelInfo, error) { return nil, nil }

type noopTransport struct{}

func (noopTransport) Write([]byte) error        { return nil }
func (noopTransport) WriteMany(...[]byte) error { return nil }
func (noopTransport) Close(Disconnect) error    { return nil }
func (noopTransport) RemoteAddr() string        { return "127.0.0.1:0" }

func TestNode_DisconnectSession_RemoteRoutesThroughCommandBus(t *testing.T) {
	t.Parallel()

	directory := &fakeSessionDirectory{lease: &ClusterSessionLease{
		SessionID:     "sess-remote",
		NodeID:        "node-b",
		IncarnationID: "inc-b",
		LeaseVersion:  3,
	}}
	bus := &fakeClusterCommandBus{}
	runtime, err := NewClusterRuntime(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       bus,
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := NewNode(nil)
	node.SetClusterRuntime(runtime)

	ok, err := node.DisconnectSession(context.Background(), "sess-remote", Disconnect{Code: 3001, Reason: "remote"})
	require.NoError(t, err)
	assert.True(t, ok)
	require.Len(t, bus.commands, 1)
	assert.Equal(t, ClusterCommandDisconnect, bus.commands[0].Type)
	assert.Equal(t, "node-b", bus.commands[0].TargetNodeID)
	assert.Equal(t, "inc-b", bus.commands[0].TargetIncarnationID)
	assert.Equal(t, "3001", bus.commands[0].Metadata[clusterCommandMetaDisconnectCode])
	assert.Equal(t, "remote", bus.commands[0].Metadata[clusterCommandMetaDisconnectReason])
}

func TestNode_ResumeRemoteSession_UsesSnapshotAndTakeover(t *testing.T) {
	t.Parallel()

	directory := &fakeSessionDirectory{
		lease: &ClusterSessionLease{
			SessionID:     "sess-remote",
			NodeID:        "node-b",
			IncarnationID: "inc-b",
			LeaseVersion:  7,
		},
		snapshot: &ClusterSessionSnapshot{
			SessionID:     "sess-remote",
			UserID:        "user-1",
			ClientID:      "client-1",
			Subscriptions: []ClusterSubscriptionSnapshot{{Channel: "news"}, {Channel: "alerts"}},
		},
	}
	bus := &fakeClusterCommandBus{result: &ClusterCommandResult{Status: ClusterCommandStatusSucceeded}}
	runtime, err := NewClusterRuntime(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       bus,
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := NewNode(nil)
	node.SetClusterRuntime(runtime)
	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)

	snapshot, resumed, err := node.resumeRemoteSession(context.Background(), client, "sess-remote")
	require.NoError(t, err)
	assert.True(t, resumed)
	require.NotNil(t, snapshot)
	assert.Equal(t, "sess-remote", client.SessionID())
	assert.Equal(t, "user-1", client.UserID())
	assert.Equal(t, "client-1", client.ClientID())
	assert.True(t, client.hasSubscription("news"))
	assert.True(t, client.hasSubscription("alerts"))
	assert.Equal(t, uint64(8), client.clusterLeaseVersion)
	require.Len(t, bus.commands, 1)
	assert.Equal(t, ClusterCommandTakeover, bus.commands[0].Type)
	assert.Equal(t, uint64(7), bus.commands[0].LeaseVersion)
	assert.Equal(t, "node-a", bus.commands[0].Metadata[clusterCommandMetaNewNodeID])
	assert.Equal(t, "inc-a", bus.commands[0].Metadata[clusterCommandMetaNewIncarnationID])
}

func TestNode_Survey_ClusterReturnsPartialFailures(t *testing.T) {
	t.Parallel()

	bus := &fakeClusterCommandBus{broadcastResults: []*ClusterCommandResult{{
		CommandID:     "survey-cmd-1",
		NodeID:        "node-b",
		IncarnationID: "inc-b",
		Status:        ClusterCommandStatusFailed,
		ErrorCode:     "CLUSTER_COMMAND_SEND_FAILED",
		ErrorMessage:  "remote node timed out",
	}}}
	runtime, err := NewClusterRuntime(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: &fakeSessionDirectory{},
		CommandBus:       bus,
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := NewNode(nil)
	node.SetClusterRuntime(runtime)

	results, err := node.Survey(context.Background(), "chat.room", []byte("ping"), 200*time.Millisecond)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "node-b", results[0].NodeID)
	assert.Equal(t, "inc-b", results[0].IncarnationID)
	require.Error(t, results[0].Error)
	assert.Contains(t, results[0].Error.Error(), "CLUSTER_COMMAND_SEND_FAILED")
}
