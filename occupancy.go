package messageloop

import (
	"context"
	"errors"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
)

// OccupancyEvent is the LiveBus payload for join/leave. Gen is node-to-node
// ordering (KD-K8). It is NOT added to the v1 client PresenceEvent (A0 froze
// v1; v2 already has gen for a later runtime switch).
type OccupancyEvent struct {
	Event *clientpb.PresenceEvent // channel, action, info
	Gen   uint64
}

// ErrLateOccupancy is returned (and counted) when a receiver drops an event
// because gen <= last_applied[ch][session].
var ErrLateOccupancy = errors.New("late occupancy event")

// OccupancyGenSource is implemented by presence adapters that issue the
// per-channel monotonic occupancy generation (B2 §4): the memory store counts
// in-process per channel, the Redis store INCRs a cluster-wide key. A node
// whose presence adapter is not gen-capable falls back to an in-process
// per-channel counter so same-node monotonicity always holds (B2 §4).
type OccupancyGenSource interface {
	// NextOccupancyGen returns the next strictly-increasing generation for ch.
	// Gen 0 (returned on error) is invalid: callers drop the emit.
	NextOccupancyGen(ctx context.Context, ch string) (uint64, error)
}

// SyntheticLeaveReporter is implemented by PresenceStore implementations that
// prune ghost members whose TTL evaporated (Redis). It lets the node
// synthesize a leave for the vanished session with a fresh generation (B2
// §5.3). The memory store has no TTL and never reports.
type SyntheticLeaveReporter interface {
	// SetSyntheticLeaveHook registers the callback invoked for every ghost
	// member pruned by an existing Get/refresh path. Must be called before the
	// store is used concurrently.
	SetSyntheticLeaveHook(hook func(ctx context.Context, ch, clientID string))
}