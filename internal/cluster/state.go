package cluster

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrClusterCommandUnsupported indicates the current cluster command bus cannot execute distributed commands.
	ErrClusterCommandUnsupported = errors.New("cluster command bus is not configured")
	// ErrSessionFenced means Directory no longer recognizes this node's fencing
	// for the session. Callers that hold a local attachment must Fence it
	// (DisconnectStale) and must not Unbind the new owner's lease.
	ErrSessionFenced = errors.New("session fenced by another owner")
)

// ClusterNodeLease represents the liveness record for a node incarnation.
type ClusterNodeLease struct {
	NodeID        string    `json:"node_id"`
	IncarnationID string    `json:"incarnation_id"`
	StartedAt     time.Time `json:"started_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// ClusterSessionLease represents ownership of a resumable client session.
type ClusterSessionLease struct {
	SessionID      string    `json:"session_id"`
	NodeID         string    `json:"node_id"`
	IncarnationID  string    `json:"incarnation_id"`
	UserID         string    `json:"user_id,omitempty"`
	ClientID       string    `json:"client_id,omitempty"`
	LeaseVersion   uint64    `json:"lease_version"`
	Authenticated  bool      `json:"authenticated"`
	ConnectedAt    int64     `json:"connected_at,omitempty"`
	LastActivityAt int64     `json:"last_activity_at,omitempty"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// ClusterSubscriptionSnapshot stores the resumable state for one subscription key.
type ClusterSubscriptionSnapshot struct {
	Channel   string `json:"channel"`
	Ephemeral bool   `json:"ephemeral,omitempty"`
}

// ClusterSessionSnapshot stores resumable state for a client session.
type ClusterSessionSnapshot struct {
	SessionID     string                        `json:"session_id"`
	UserID        string                        `json:"user_id,omitempty"`
	ClientID      string                        `json:"client_id,omitempty"`
	Authenticated bool                          `json:"authenticated"`
	Protocol      string                        `json:"protocol,omitempty"`
	ConnectedAt   int64                         `json:"connected_at,omitempty"`
	Subscriptions []ClusterSubscriptionSnapshot `json:"subscriptions,omitempty"`
	// ChannelOffsets records the last offset successfully delivered to this
	// session on each channel, as tracked by the hub broadcast path
	// (Subscriber.DeliveredOffset in the subShard). It enables exact
	// cross-node resume: the resuming node recovers from
	// ChannelOffsets[ch]+1 instead of trusting the client-reported offset
	// (which may be missing or forged). Only channels with at least one
	// delivered history entry appear; channels with no delivered history
	// (or transient-only publications) are absent. Populated by
	// clusterSessionSnapshot at snapshot time.
	ChannelOffsets map[string]uint64 `json:"channel_offsets,omitempty"`
	// BrokerEpoch is the cluster-wide broker epoch at snapshot time; it lets
	// the resuming node detect that the broker's history was invalidated
	// (epoch change forces full recovery).
	BrokerEpoch string            `json:"broker_epoch,omitempty"`
	AuthContext map[string]string `json:"auth_context,omitempty"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// ClusterChannelInfo describes one shared subscription key projection.
type ClusterChannelInfo struct {
	Name        string `json:"name"`
	Subscribers int64  `json:"subscribers"`
}

// ClusterCommandHandler handles one incoming cluster command locally.
type ClusterCommandHandler func(ctx context.Context, cmd *ClusterCommand) (*ClusterCommandResult, error)

// SessionStateCompareAndSwapper is an optional SessionDirectory extension
// (PR-KA-D10 §1.2): the session lease CAS and the session snapshot write
// commit in one atomic step, closing the blind-write window between a won
// lease CAS and the snapshot PUT that used to follow it. The compare
// predicate is exactly the CompareAndSwapSessionLease four-field one
// (SessionID, NodeID, IncarnationID, LeaseVersion; expected == nil requires
// the lease record to be absent). ok=false writes nothing — neither lease
// nor snapshot. Wiring is by type assertion (the NodeEpochAllocator
// precedent): directories without the extension fall back to the two-step
// path in compareAndSwapSessionState.
type SessionStateCompareAndSwapper interface {
	CompareAndSwapSessionState(ctx context.Context, expected, desired *ClusterSessionLease, snapshot *ClusterSessionSnapshot, leaseTTL, snapshotTTL time.Duration) (bool, error)
}
