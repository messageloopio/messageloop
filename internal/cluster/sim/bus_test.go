package sim

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/messageloopio/messageloop/internal/cluster"
)

func takeoverCommand(targetNode, targetIncarnation string) *cluster.ClusterCommand {
	return &cluster.ClusterCommand{
		CommandID:           "cmd-1",
		Type:                cluster.ClusterCommandTakeover,
		TargetNodeID:        targetNode,
		TargetIncarnationID: targetIncarnation,
		SessionID:           "sess-1",
	}
}

// TestBus_SynchronousDelivery: by default SendCommand runs the target's
// handler on the caller's goroutine and returns its result.
func TestBus_SynchronousDelivery(t *testing.T) {
	bus := NewBus()
	var calls atomic.Int32
	bus.Register("node-a", "inc-a", func(_ context.Context, cmd *cluster.ClusterCommand) (*cluster.ClusterCommandResult, error) {
		calls.Add(1)
		return &cluster.ClusterCommandResult{
			CommandID: cmd.CommandID,
			SessionID: cmd.SessionID,
			Status:    cluster.ClusterCommandStatusSucceeded,
		}, nil
	})

	result, err := bus.SendCommand(context.Background(), takeoverCommand("node-a", "inc-a"))
	require.NoError(t, err)
	require.Equal(t, cluster.ClusterCommandStatusSucceeded, result.Status)
	require.Equal(t, int32(1), calls.Load())
}

// TestBus_DropNext: a dropped command never runs its handler and the sender
// observes unknown_final_state with the recognizable SIM_DROPPED code.
func TestBus_DropNext(t *testing.T) {
	bus := NewBus()
	var calls atomic.Int32
	bus.Register("node-a", "inc-a", func(context.Context, *cluster.ClusterCommand) (*cluster.ClusterCommandResult, error) {
		calls.Add(1)
		return &cluster.ClusterCommandResult{Status: cluster.ClusterCommandStatusSucceeded}, nil
	})

	bus.DropNext()
	result, err := bus.SendCommand(context.Background(), takeoverCommand("node-a", "inc-a"))
	require.NoError(t, err)
	require.Equal(t, cluster.ClusterCommandStatusUnknownFinalState, result.Status)
	require.Equal(t, SimDroppedErrorCode, result.ErrorCode)
	require.Equal(t, int32(0), calls.Load(), "dropped command must not run the handler")

	// Only one command is dropped; the bus recovers on the next send.
	result, err = bus.SendCommand(context.Background(), takeoverCommand("node-a", "inc-a"))
	require.NoError(t, err)
	require.Equal(t, cluster.ClusterCommandStatusSucceeded, result.Status)
	require.Equal(t, int32(1), calls.Load())
}

// TestBus_HoldFlush: held commands queue without running handlers; Flush
// delivers them FIFO and returns their results.
func TestBus_HoldFlush(t *testing.T) {
	bus := NewBus()
	var calls atomic.Int32
	var handled []string
	bus.Register("node-a", "inc-a", func(_ context.Context, cmd *cluster.ClusterCommand) (*cluster.ClusterCommandResult, error) {
		calls.Add(1)
		handled = append(handled, cmd.CommandID)
		return &cluster.ClusterCommandResult{CommandID: cmd.CommandID, Status: cluster.ClusterCommandStatusSucceeded}, nil
	})

	bus.Hold()
	first := takeoverCommand("node-a", "inc-a")
	second := takeoverCommand("node-a", "inc-a")
	second.CommandID = "cmd-2"
	result, err := bus.SendCommand(context.Background(), first)
	require.NoError(t, err)
	require.Equal(t, cluster.ClusterCommandStatusPending, result.Status)
	_, err = bus.SendCommand(context.Background(), second)
	require.NoError(t, err)
	require.Equal(t, int32(0), calls.Load(), "held commands must not run handlers")

	results := bus.Flush(context.Background())
	require.Len(t, results, 2)
	require.Equal(t, "cmd-1", results[0].CommandID)
	require.Equal(t, "cmd-2", results[1].CommandID)
	require.Equal(t, []string{"cmd-1", "cmd-2"}, handled, "Flush delivers FIFO")

	// Flush switches the bus back to synchronous delivery.
	_, err = bus.SendCommand(context.Background(), takeoverCommand("node-a", "inc-a"))
	require.NoError(t, err)
	require.Equal(t, int32(3), calls.Load())
}

// TestBus_UnregisteredTarget: a command aimed at an incarnation that never
// registered fails cleanly instead of panicking.
func TestBus_UnregisteredTarget(t *testing.T) {
	bus := NewBus()
	result, err := bus.SendCommand(context.Background(), takeoverCommand("node-z", "inc-z"))
	require.NoError(t, err)
	require.Equal(t, cluster.ClusterCommandStatusFailed, result.Status)
	require.Equal(t, "TARGET_NODE_NOT_ALIVE", result.ErrorCode)
}

// TestBus_BroadcastFansOut: every registered incarnation receives its own
// copy with a fresh CommandID.
func TestBus_BroadcastFansOut(t *testing.T) {
	bus := NewBus()
	received := make(map[string][]string) // incarnation -> command IDs
	register := func(nodeID, incarnationID string) {
		bus.Register(nodeID, incarnationID, func(_ context.Context, cmd *cluster.ClusterCommand) (*cluster.ClusterCommandResult, error) {
			received[incarnationID] = append(received[incarnationID], cmd.CommandID)
			return &cluster.ClusterCommandResult{
				CommandID:     cmd.CommandID,
				NodeID:        nodeID,
				IncarnationID: incarnationID,
				Status:        cluster.ClusterCommandStatusSucceeded,
			}, nil
		})
	}
	register("node-a", "inc-a")
	register("node-b", "inc-b")

	results, err := bus.BroadcastCommand(context.Background(), &cluster.ClusterCommand{Type: cluster.ClusterCommandSurvey})
	require.NoError(t, err)
	require.Len(t, results, 2)
	require.Len(t, received["inc-a"], 1)
	require.Len(t, received["inc-b"], 1)
	require.NotEqual(t, received["inc-a"][0], received["inc-b"][0], "each copy gets a fresh CommandID")
}
