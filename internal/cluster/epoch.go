package cluster

import (
	"context"
	"errors"
	"strconv"
	"sync"
)

// NodeEpochAllocator issues the next node_epoch for nodeID (KD-K27). The
// node epoch is the process generation of a node: allocated once at startup,
// strictly monotonic, and rendered as the decimal IncarnationID string via
// FormatNodeEpoch. A larger epoch is a newer generation of the same node.
//
// Redis: INCR ml2:cluster:node_epoch:{nodeID} (see pkg/redisbroker). Memory:
// per-nodeID process-local counter. Allocation is a startup-only operation,
// never a hot-path one.
type NodeEpochAllocator interface {
	NextNodeEpoch(ctx context.Context, nodeID string) (uint64, error)
}

// FormatNodeEpoch renders epoch as the IncarnationID string (decimal, no
// leading zeros except 0). Epoch 0 is not a valid issued value.
func FormatNodeEpoch(epoch uint64) string {
	return strconv.FormatUint(epoch, 10)
}

// ParseNodeEpoch reports whether incarnationID is a decimal epoch issued by
// a NodeEpochAllocator. "inc-a" → (_, false); "12" → (12, true). Only the
// canonical FormatNodeEpoch form of a non-zero epoch parses: "0", "01" and
// "+1" are not issued epochs.
func ParseNodeEpoch(incarnationID string) (uint64, bool) {
	if incarnationID == "" {
		return 0, false
	}
	epoch, err := strconv.ParseUint(incarnationID, 10, 64)
	if err != nil || epoch == 0 {
		return 0, false
	}
	if FormatNodeEpoch(epoch) != incarnationID {
		return 0, false
	}
	return epoch, true
}

// NodeEpochNewer reports whether a is a strictly newer process generation of
// the same node than b. Both IDs must ParseNodeEpoch OK; otherwise false.
// The comparison is numeric: "10" is newer than "2".
func NodeEpochNewer(a, b string) bool {
	epochA, ok := ParseNodeEpoch(a)
	if !ok {
		return false
	}
	epochB, ok := ParseNodeEpoch(b)
	if !ok {
		return false
	}
	return epochA > epochB
}

// MemoryNodeEpochAllocator is the process-local NodeEpochAllocator for the
// memory / noop cluster backends: a per-nodeID monotonic uint64 counter,
// first issue is 1. Different node IDs do not share a sequence. It is only
// meaningful within a single process; it provides no cross-process fencing.
type MemoryNodeEpochAllocator struct {
	mu     sync.Mutex
	epochs map[string]uint64
}

// NewMemoryNodeEpochAllocator creates an empty process-local allocator.
func NewMemoryNodeEpochAllocator() *MemoryNodeEpochAllocator {
	return &MemoryNodeEpochAllocator{epochs: make(map[string]uint64)}
}

// NextNodeEpoch increments and returns the per-nodeID counter, starting at 1.
func (a *MemoryNodeEpochAllocator) NextNodeEpoch(_ context.Context, nodeID string) (uint64, error) {
	if nodeID == "" {
		return 0, errors.New("node_epoch: node_id is required")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.epochs[nodeID]++
	return a.epochs[nodeID], nil
}

// sharedMemoryNodeEpochAllocator backs NewCluster when the backend is
// memory/noop and the caller left IncarnationID empty: two independent
// NewCluster calls for the same nodeID must still observe an increasing
// sequence within this process.
var sharedMemoryNodeEpochAllocator = NewMemoryNodeEpochAllocator()

// AllocateNodeIncarnation fills an empty IncarnationID from the node epoch
// allocator. A directory that implements NodeEpochAllocator (the Redis
// session directory) wins; the redis backend without such a directory is a
// startup error (falling back to a random or process-local ID would silently
// break fencing); memory/noop backends use the process-local counter.
func AllocateNodeIncarnation(options ClusterOptions, directory SessionDirectory) (string, error) {
	if allocator, ok := directory.(NodeEpochAllocator); ok {
		epoch, err := allocator.NextNodeEpoch(context.Background(), options.NodeID)
		if err != nil {
			return "", err
		}
		return FormatNodeEpoch(epoch), nil
	}
	if options.Backend == "redis" {
		return "", errors.New("cluster node_epoch: redis backend requires a session directory that implements NodeEpochAllocator; refusing to allocate an incarnation")
	}
	epoch, err := sharedMemoryNodeEpochAllocator.NextNodeEpoch(context.Background(), options.NodeID)
	if err != nil {
		return "", err
	}
	return FormatNodeEpoch(epoch), nil
}
