package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ClusterOptions configures the cluster control-plane runtime.
type ClusterOptions struct {
	Enabled       bool
	NodeID        string
	Backend       string
	IncarnationID string
}

func (o ClusterOptions) Normalize() (ClusterOptions, error) {
	if !o.Enabled {
		return ClusterOptions{}, nil
	}

	nodeID := strings.TrimSpace(o.NodeID)
	if nodeID == "" {
		return ClusterOptions{}, errors.New("cluster node_id is required when cluster is enabled")
	}

	backend := strings.TrimSpace(o.Backend)
	if backend == "" {
		backend = "redis"
	}

	switch backend {
	case "redis", "memory", "noop":
	default:
		return ClusterOptions{}, fmt.Errorf("unsupported cluster backend: %s", backend)
	}

	incarnationID := strings.TrimSpace(o.IncarnationID)

	return ClusterOptions{
		Enabled: true,
		NodeID:  nodeID,
		Backend: backend,
		// An empty IncarnationID is left for the node epoch allocator in
		// NewCluster (KD-K27): production generations come from a monotonic
		// node_epoch (Redis INCR / process-local counter), never a random
		// UUID. Callers that pass an explicit ID (tests, C1 sim) keep it.
		IncarnationID: incarnationID,
	}, nil
}

// ClusterLifecycle is the lifecycle contract shared by cluster control-plane components.
type ClusterLifecycle interface {
	Start(ctx context.Context) error
	Shutdown(ctx context.Context) error
}

// SessionDirectory stores cluster-visible ownership and resumable session state.
// The user→sessions index (AddUserSession/RemoveUserSession/ListUserSessions)
// is a hint for user-targeted admin operations: it is never authoritative,
// expansions always re-check the session lease.
//
// Lease writes on production hot paths must go through
// CompareAndSwapSessionLease (same-fence refresh, or CAS(nil) for first
// registration). The unconditional-SET lease put method was removed in
// PR-KA-B4: a blind write would write a fenced owner's lease back over the
// new owner (KD-K4), so no write path may bypass the CAS.
type SessionDirectory interface {
	ClusterLifecycle
	PutNodeLease(ctx context.Context, lease *ClusterNodeLease, ttl time.Duration) error
	GetNodeLease(ctx context.Context, nodeID, incarnationID string) (*ClusterNodeLease, error)
	CompareAndSwapSessionLease(ctx context.Context, expected, desired *ClusterSessionLease, ttl time.Duration) (bool, error)
	GetSessionLease(ctx context.Context, sessionID string) (*ClusterSessionLease, error)
	DeleteSessionLease(ctx context.Context, sessionID string) error
	PutSessionSnapshot(ctx context.Context, snapshot *ClusterSessionSnapshot, ttl time.Duration) error
	GetSessionSnapshot(ctx context.Context, sessionID string) (*ClusterSessionSnapshot, error)
	DeleteSessionSnapshot(ctx context.Context, sessionID string) error
	// AddUserSession records that sessionID currently belongs to userID, with
	// the same TTL as the session lease. Empty user IDs never enter the index.
	AddUserSession(ctx context.Context, userID, sessionID string, ttl time.Duration) error
	// RemoveUserSession drops a session's membership from a user's index.
	RemoveUserSession(ctx context.Context, userID, sessionID string) error
	// ListUserSessions returns the indexed session IDs of userID. The result
	// is a hint: callers must verify each session's lease before acting.
	ListUserSessions(ctx context.Context, userID string) ([]string, error)
}

// ClusterCommandBus delivers cluster commands and command results between node incarnations.
type ClusterCommandBus interface {
	ClusterLifecycle
	SetHandler(handler ClusterCommandHandler)
	SendCommand(ctx context.Context, cmd *ClusterCommand) (*ClusterCommandResult, error)
	BroadcastCommand(ctx context.Context, cmd *ClusterCommand) ([]*ClusterCommandResult, error)
}

// ClusterNodeProjection identifies one node's owner projection key.
type ClusterNodeProjection struct {
	NodeID        string
	IncarnationID string
}

// ClusterQueryStore stores cluster-visible query projections.
type ClusterQueryStore interface {
	ClusterLifecycle
	AdjustChannelSubscriptions(ctx context.Context, channel string, delta int64, ttl time.Duration) error
	ReplaceNodeChannels(ctx context.Context, channels map[string]int64, ttl time.Duration) error
	ListChannels(ctx context.Context) ([]ClusterChannelInfo, error)
	// ListNodeProjections returns the node:incarnation pairs with stored
	// owner projections.
	ListNodeProjections(ctx context.Context) ([]ClusterNodeProjection, error)
	// DeleteNodeProjection removes the owner projection of node:incarnation.
	DeleteNodeProjection(ctx context.Context, nodeID, incarnationID string) error
}

// ClusterNodeLeaseManager renews cluster node liveness.
type ClusterNodeLeaseManager interface {
	ClusterLifecycle
}

// ClusterRepairer is the single cluster control-plane repair loop (PR-KA-B4):
// it republishes this node's channel projections, reaps dead node
// projections, rebuilds the user→sessions index, and drives membership
// OnLeave from a short-period node-lease SCAN. NewCluster starts at most one
// repairer.
type ClusterRepairer interface {
	ClusterLifecycle
}

// ClusterSessionLeaseLister enumerates every session lease stored in the
// directory; only a lease-capable backend (e.g. Redis with SCAN) implements
// it. The repairer uses it to rebuild user-index memberships and to
// invalidate a dead incarnation's session fencing on OnLeave.
type ClusterSessionLeaseLister interface {
	ListSessionLeases(ctx context.Context) ([]*ClusterSessionLease, error)
}

// ClusterNodeLeaseLister enumerates every node lease stored in the directory;
// only a lease-capable backend (e.g. Redis with SCAN) implements it. The
// repairer uses it to detect node incarnations that left the cluster.
type ClusterNodeLeaseLister interface {
	ListNodeLeases(ctx context.Context) ([]*ClusterNodeLease, error)
}

// ClusterCommandType identifies a cluster control-plane command.
type ClusterCommandType string

const (
	// ClusterCommandDisconnect closes a remote session.
	ClusterCommandDisconnect ClusterCommandType = "disconnect"
	// ClusterCommandSubscribe subscribes a remote session to a channel.
	ClusterCommandSubscribe ClusterCommandType = "subscribe"
	// ClusterCommandUnsubscribe unsubscribes a remote session from a channel.
	ClusterCommandUnsubscribe ClusterCommandType = "unsubscribe"
	// ClusterCommandPublish publishes a payload to a remote session.
	ClusterCommandPublish ClusterCommandType = "publish"
	// ClusterCommandTakeover transfers session ownership to another node incarnation.
	ClusterCommandTakeover ClusterCommandType = "takeover"
	// ClusterCommandSurvey runs a cluster-scoped survey.
	ClusterCommandSurvey ClusterCommandType = "survey"
)

// ClusterCommandStatus represents the terminal or in-flight status of a cluster command.
type ClusterCommandStatus string

const (
	// ClusterCommandStatusPending indicates the command has been accepted but not completed.
	ClusterCommandStatusPending ClusterCommandStatus = "pending"
	// ClusterCommandStatusSucceeded indicates the command completed successfully.
	ClusterCommandStatusSucceeded ClusterCommandStatus = "succeeded"
	// ClusterCommandStatusFailed indicates the command completed with an error.
	ClusterCommandStatusFailed ClusterCommandStatus = "failed"
	// ClusterCommandStatusInProgress indicates another owner is already processing the command.
	ClusterCommandStatusInProgress ClusterCommandStatus = "in_progress"
	// ClusterCommandStatusUnknownFinalState indicates the side effect may have happened but no terminal result was observed.
	ClusterCommandStatusUnknownFinalState ClusterCommandStatus = "unknown_final_state"
)

// ClusterCommand is the transport model for cluster-scoped control-plane operations.
type ClusterCommand struct {
	CommandID           string
	Type                ClusterCommandType
	TargetNodeID        string
	TargetIncarnationID string
	SessionID           string
	Channel             string
	LeaseVersion        uint64
	IssuedAt            time.Time
	// IssuedBy is the NodeID of the command sender, recorded for audit
	// purposes only: it is NOT part of the signed canonical bytes and is
	// forgeable. The security boundary is Signature, not IssuedBy.
	IssuedBy string
	Payload  []byte
	Metadata map[string]string
	// Signature is hex(HMAC-SHA256(canonical(command))) — see
	// internal/cluster/hmac. Commands without a valid signature are rejected
	// by the receiving bus before claiming or handling.
	Signature string
}

// ClusterCommandResult is the normalized result of a cluster command execution.
type ClusterCommandResult struct {
	CommandID     string
	SessionID     string
	NodeID        string
	IncarnationID string
	Status        ClusterCommandStatus
	ErrorCode     string
	ErrorMessage  string
	Metadata      map[string]string
	// IssuedAt is when the answering node emitted the result; verifiers
	// reject results outside the ±30s clock-skew window.
	IssuedAt time.Time
	// Signature is hex(HMAC-SHA256(canonical(result))) — see
	// internal/cluster/hmac. Forged results (unsigned or badly signed) are
	// discarded by the waiting sender as if no reply had arrived.
	Signature string
}
