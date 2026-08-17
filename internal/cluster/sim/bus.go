package sim

import (
	"context"
	"fmt"
	"sync"

	"github.com/messageloopio/messageloop"
)

// SimDroppedErrorCode marks a command dropped by Bus.DropNext: the sender
// observes the "unknown final state" outcome a lost Evict produces over a
// real transport.
const SimDroppedErrorCode = "SIM_DROPPED"

// targetNotAliveErrorCode marks a command whose target incarnation never
// registered a handler with the bus.
const targetNotAliveErrorCode = "TARGET_NODE_NOT_ALIVE"

// Bus is the simulator's in-memory command bus. Delivery is synchronous and
// deterministic: SendCommand invokes the target's handler on the caller's
// goroutine and returns its result (matching the "single-node RPC = function
// call" bullseye). Losing an Evict or withholding commands is explicit
// orchestration (DropNext / Hold+Flush), never a scheduling accident.
//
// There is no HMAC and no background read loop: B4 §4.4 exempts in-memory
// buses from signing, and Start/Shutdown are no-ops.
type Bus struct {
	mu sync.Mutex

	handlers map[nodeIncarnation]messageloop.ClusterCommandHandler
	// fallback is the handler registered through SetHandler. The production
	// wiring calls SetHandler once per process; a shared simulator bus cannot
	// infer the caller's incarnation, so World registers each node with
	// Register instead. The fallback only answers commands whose target has
	// no registered handler.
	fallback messageloop.ClusterCommandHandler

	hold     bool
	queue    []*messageloop.ClusterCommand
	dropNext int
	seq      uint64
}

// NewBus returns an empty Bus with no registered handlers.
func NewBus() *Bus {
	return &Bus{handlers: make(map[nodeIncarnation]messageloop.ClusterCommandHandler)}
}

func (b *Bus) Start(context.Context) error    { return nil }
func (b *Bus) Shutdown(context.Context) error { return nil }

// Register binds handler to one node incarnation. World calls it once per
// node with the node's ClusterCommandHandler.
func (b *Bus) Register(nodeID, incarnationID string, handler messageloop.ClusterCommandHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[nodeIncarnation{nodeID: nodeID, incarnationID: incarnationID}] = handler
}

// SetHandler sets the fallback handler (see the Bus comment). The simulator
// routes by TargetNodeID + TargetIncarnationID; prefer Register.
func (b *Bus) SetHandler(handler messageloop.ClusterCommandHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fallback = handler
}

// SendCommand delivers cmd to the target incarnation's handler. By default
// the handler runs synchronously on the caller's goroutine and its result is
// returned. Under Hold the command is queued (result pending) until Flush;
// under DropNext the command is discarded with an unknown_final_state result
// and the handler never runs. An unregistered target yields a failed result
// instead of a panic.
func (b *Bus) SendCommand(ctx context.Context, cmd *messageloop.ClusterCommand) (*messageloop.ClusterCommandResult, error) {
	b.mu.Lock()
	if b.dropNext > 0 {
		b.dropNext--
		b.mu.Unlock()
		return &messageloop.ClusterCommandResult{
			CommandID:    cmd.CommandID,
			SessionID:    cmd.SessionID,
			Status:       messageloop.ClusterCommandStatusUnknownFinalState,
			ErrorCode:    SimDroppedErrorCode,
			ErrorMessage: "command dropped by simulator",
		}, nil
	}
	if b.hold {
		b.queue = append(b.queue, copyCommand(cmd))
		b.mu.Unlock()
		return &messageloop.ClusterCommandResult{
			CommandID: cmd.CommandID,
			SessionID: cmd.SessionID,
			Status:    messageloop.ClusterCommandStatusPending,
		}, nil
	}
	handler := b.handlerForLocked(cmd.TargetNodeID, cmd.TargetIncarnationID)
	b.mu.Unlock()
	return b.deliver(ctx, handler, cmd)
}

// BroadcastCommand sends one copy of cmd to every registered incarnation,
// each with a fresh deterministic CommandID. Hold/DropNext apply to each
// copy exactly as for a unicast SendCommand.
func (b *Bus) BroadcastCommand(ctx context.Context, cmd *messageloop.ClusterCommand) ([]*messageloop.ClusterCommandResult, error) {
	b.mu.Lock()
	targets := make([]nodeIncarnation, 0, len(b.handlers))
	for target := range b.handlers {
		targets = append(targets, target)
	}
	b.mu.Unlock()

	results := make([]*messageloop.ClusterCommandResult, 0, len(targets))
	for _, target := range targets {
		copy := copyCommand(cmd)
		b.mu.Lock()
		b.seq++
		copy.CommandID = fmt.Sprintf("sim-broadcast-%d", b.seq)
		b.mu.Unlock()
		copy.TargetNodeID = target.nodeID
		copy.TargetIncarnationID = target.incarnationID
		result, err := b.SendCommand(ctx, copy)
		if err != nil {
			return results, err
		}
		results = append(results, result)
	}
	return results, nil
}

// Hold switches the bus to queueing mode: SendCommand enqueues commands
// without running handlers until Flush.
func (b *Bus) Hold() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hold = true
}

// Flush delivers every queued command in FIFO order, returns their results
// in the same order, and switches the bus back to synchronous mode.
func (b *Bus) Flush(ctx context.Context) []*messageloop.ClusterCommandResult {
	b.mu.Lock()
	queued := b.queue
	b.queue = nil
	b.hold = false
	handlers := make([]messageloop.ClusterCommandHandler, 0, len(queued))
	for _, cmd := range queued {
		handlers = append(handlers, b.handlerForLocked(cmd.TargetNodeID, cmd.TargetIncarnationID))
	}
	b.mu.Unlock()

	results := make([]*messageloop.ClusterCommandResult, 0, len(queued))
	for i, cmd := range queued {
		result, _ := b.deliver(ctx, handlers[i], cmd)
		results = append(results, result)
	}
	return results
}

// DropNext makes the next SendCommand (unicast or one broadcast copy) vanish:
// no handler runs and the sender gets an unknown_final_state result with
// SimDroppedErrorCode.
func (b *Bus) DropNext() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dropNext++
}

func (b *Bus) handlerForLocked(nodeID, incarnationID string) messageloop.ClusterCommandHandler {
	if handler, ok := b.handlers[nodeIncarnation{nodeID: nodeID, incarnationID: incarnationID}]; ok {
		return handler
	}
	return b.fallback
}

// deliver runs the handler synchronously. A nil handler means the target
// incarnation is not registered: the command fails with
// TARGET_NODE_NOT_ALIVE rather than panicking.
func (b *Bus) deliver(ctx context.Context, handler messageloop.ClusterCommandHandler, cmd *messageloop.ClusterCommand) (*messageloop.ClusterCommandResult, error) {
	if handler == nil {
		return &messageloop.ClusterCommandResult{
			CommandID:    cmd.CommandID,
			SessionID:    cmd.SessionID,
			Status:       messageloop.ClusterCommandStatusFailed,
			ErrorCode:    targetNotAliveErrorCode,
			ErrorMessage: fmt.Sprintf("target incarnation %s/%s not registered", cmd.TargetNodeID, cmd.TargetIncarnationID),
		}, nil
	}
	return handler(ctx, cmd)
}

func copyCommand(cmd *messageloop.ClusterCommand) *messageloop.ClusterCommand {
	copy := *cmd
	if cmd.Metadata != nil {
		copy.Metadata = make(map[string]string, len(cmd.Metadata))
		for key, value := range cmd.Metadata {
			copy.Metadata[key] = value
		}
	}
	copy.Payload = append([]byte(nil), cmd.Payload...)
	return &copy
}

var _ messageloop.ClusterCommandBus = (*Bus)(nil)
