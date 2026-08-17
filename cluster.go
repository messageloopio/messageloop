package messageloop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ClusterOptions configures the cluster control-plane runtime.
type ClusterOptions struct {
	Enabled       bool
	NodeID        string
	Backend       string
	IncarnationID string
}

func (o ClusterOptions) normalize() (ClusterOptions, error) {
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
	if incarnationID == "" {
		incarnationID = uuid.NewString()
	}

	return ClusterOptions{
		Enabled:       true,
		NodeID:        nodeID,
		Backend:       backend,
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

// ClusterDependencies groups the control-plane adapters used by a cluster-enabled node.
type ClusterDependencies struct {
	SessionDirectory SessionDirectory
	CommandBus       ClusterCommandBus
	QueryStore       ClusterQueryStore
	NodeLeaseManager ClusterNodeLeaseManager
	// Repairer is the single control-plane repair loop (projection republish
	// + dead-projection reaping + user-index rebuild + membership OnLeave).
	// When nil, NewCluster derives one from the session directory; a
	// directory without lease enumeration yields a no-op repairer.
	Repairer ClusterRepairer
}

// Cluster owns lifecycle coordination for cluster control-plane adapters.
type Cluster struct {
	options ClusterOptions
	deps    ClusterDependencies

	mu           sync.Mutex
	started      bool
	startErr     error
	shutdownOnce sync.Once
	shutdownErr  error
}

const clusterStartRollbackTimeout = 5 * time.Second

// NewCluster creates a lifecycle coordinator for cluster control-plane components.
func NewCluster(options ClusterOptions, deps ClusterDependencies) (*Cluster, error) {
	normalized, err := options.normalize()
	if err != nil {
		return nil, err
	}

	if deps.SessionDirectory == nil {
		deps.SessionDirectory = &noopSessionDirectory{}
	}
	if deps.CommandBus == nil {
		deps.CommandBus = &noopClusterCommandBus{}
	}
	if deps.QueryStore == nil {
		deps.QueryStore = &noopClusterQueryStore{}
	}
	if deps.NodeLeaseManager == nil {
		deps.NodeLeaseManager = &noopClusterNodeLeaseManager{}
	}
	if deps.Repairer == nil {
		deps.Repairer = NewClusterRepairer(nil, deps.SessionDirectory, nil, ClusterRepairerConfig{
			NodeID:        normalized.NodeID,
			IncarnationID: normalized.IncarnationID,
		})
	}

	return &Cluster{
		options: normalized,
		deps:    deps,
	}, nil
}

// Enabled reports whether the cluster control plane is active for this runtime.
func (r *Cluster) Enabled() bool {
	if r == nil {
		return false
	}
	return r.options.Enabled
}

// NodeID returns the configured cluster node identifier.
func (r *Cluster) NodeID() string {
	if r == nil {
		return ""
	}
	return r.options.NodeID
}

// IncarnationID returns the generated process incarnation identifier.
func (r *Cluster) IncarnationID() string {
	if r == nil {
		return ""
	}
	return r.options.IncarnationID
}

// Backend returns the configured cluster backend.
func (r *Cluster) Backend() string {
	if r == nil {
		return ""
	}
	return r.options.Backend
}

// Start starts all cluster control-plane components exactly once.
// If a component fails to start, already-started components are shut down in
// reverse order and the aggregated error is returned; a failed start leaves
// the instance retryable (later Start calls restart every component).
func (r *Cluster) Start(ctx context.Context) error {
	if r == nil || !r.options.Enabled {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return r.startErr
	}

	components := r.components()
	var startErrs []error
	for index, component := range components {
		if err := component.Start(ctx); err != nil {
			startErrs = append(startErrs, fmt.Errorf("start %s: %w", clusterComponentName(component), err))
			rollbackCtx, cancel := context.WithTimeout(context.Background(), clusterStartRollbackTimeout)
			for rollback := index - 1; rollback >= 0; rollback-- {
				if err := components[rollback].Shutdown(rollbackCtx); err != nil {
					startErrs = append(startErrs, fmt.Errorf("rollback shutdown %s: %w", clusterComponentName(components[rollback]), err))
				}
			}
			cancel()
			r.started = false
			r.startErr = errors.Join(startErrs...)
			return r.startErr
		}
	}

	r.started = true
	r.startErr = nil
	return nil
}

// Shutdown stops all cluster control-plane components exactly once.
func (r *Cluster) Shutdown(ctx context.Context) error {
	if r == nil || !r.options.Enabled {
		return nil
	}

	r.shutdownOnce.Do(func() {
		components := r.components()
		for index := len(components) - 1; index >= 0; index-- {
			if err := components[index].Shutdown(ctx); err != nil && r.shutdownErr == nil {
				r.shutdownErr = err
			}
		}
	})

	return r.shutdownErr
}

func (r *Cluster) components() []ClusterLifecycle {
	return []ClusterLifecycle{
		r.deps.SessionDirectory,
		r.deps.CommandBus,
		r.deps.QueryStore,
		r.deps.NodeLeaseManager,
		r.deps.Repairer,
	}
}

// clusterComponentName returns a stable label for a control-plane component
// used in lifecycle error messages.
func clusterComponentName(component ClusterLifecycle) string {
	switch component.(type) {
	case SessionDirectory:
		return "session_directory"
	case ClusterCommandBus:
		return "command_bus"
	case ClusterQueryStore:
		return "query_store"
	case ClusterNodeLeaseManager:
		return "node_lease_manager"
	case ClusterRepairer:
		return "repairer"
	default:
		return fmt.Sprintf("%T", component)
	}
}

type noopClusterComponent struct{}

func (noopClusterComponent) Start(context.Context) error    { return nil }
func (noopClusterComponent) Shutdown(context.Context) error { return nil }

type noopSessionDirectory struct{ noopClusterComponent }
type noopClusterCommandBus struct{ noopClusterComponent }
type noopClusterQueryStore struct{ noopClusterComponent }
type noopClusterNodeLeaseManager struct{ noopClusterComponent }
type noopClusterRepairer struct{ noopClusterComponent }
