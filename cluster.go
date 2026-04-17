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
type SessionDirectory interface {
	ClusterLifecycle
	PutNodeLease(ctx context.Context, lease *ClusterNodeLease, ttl time.Duration) error
	GetNodeLease(ctx context.Context, nodeID, incarnationID string) (*ClusterNodeLease, error)
	PutSessionLease(ctx context.Context, lease *ClusterSessionLease, ttl time.Duration) error
	CompareAndSwapSessionLease(ctx context.Context, expected, desired *ClusterSessionLease, ttl time.Duration) (bool, error)
	GetSessionLease(ctx context.Context, sessionID string) (*ClusterSessionLease, error)
	DeleteSessionLease(ctx context.Context, sessionID string) error
	PutSessionSnapshot(ctx context.Context, snapshot *ClusterSessionSnapshot, ttl time.Duration) error
	GetSessionSnapshot(ctx context.Context, sessionID string) (*ClusterSessionSnapshot, error)
	DeleteSessionSnapshot(ctx context.Context, sessionID string) error
}

// ClusterCommandBus delivers cluster commands and command results between node incarnations.
type ClusterCommandBus interface {
	ClusterLifecycle
	SetHandler(handler ClusterCommandHandler)
	SendCommand(ctx context.Context, cmd *ClusterCommand) (*ClusterCommandResult, error)
	BroadcastCommand(ctx context.Context, cmd *ClusterCommand) ([]*ClusterCommandResult, error)
}

// ClusterQueryStore stores cluster-visible query projections.
type ClusterQueryStore interface {
	ClusterLifecycle
	AdjustChannelSubscriptions(ctx context.Context, channel string, delta int64, ttl time.Duration) error
	ListChannels(ctx context.Context) ([]ClusterChannelInfo, error)
}

// ClusterNodeLeaseManager renews cluster node liveness.
type ClusterNodeLeaseManager interface {
	ClusterLifecycle
}

// ClusterProjectionRepairer periodically repairs shared query projections.
type ClusterProjectionRepairer interface {
	ClusterLifecycle
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
	Payload             []byte
	Metadata            map[string]string
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
}

// ClusterDependencies groups the control-plane adapters used by a cluster-enabled node.
type ClusterDependencies struct {
	SessionDirectory   SessionDirectory
	CommandBus         ClusterCommandBus
	QueryStore         ClusterQueryStore
	NodeLeaseManager   ClusterNodeLeaseManager
	ProjectionRepairer ClusterProjectionRepairer
}

// ClusterRuntime owns lifecycle coordination for cluster control-plane adapters.
type ClusterRuntime struct {
	options ClusterOptions
	deps    ClusterDependencies

	startOnce    sync.Once
	shutdownOnce sync.Once
	startErr     error
	shutdownErr  error
}

// NewClusterRuntime creates a lifecycle coordinator for cluster control-plane components.
func NewClusterRuntime(options ClusterOptions, deps ClusterDependencies) (*ClusterRuntime, error) {
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
	if deps.ProjectionRepairer == nil {
		deps.ProjectionRepairer = &noopClusterProjectionRepairer{}
	}

	return &ClusterRuntime{
		options: normalized,
		deps:    deps,
	}, nil
}

// Enabled reports whether the cluster control plane is active for this runtime.
func (r *ClusterRuntime) Enabled() bool {
	if r == nil {
		return false
	}
	return r.options.Enabled
}

// NodeID returns the configured cluster node identifier.
func (r *ClusterRuntime) NodeID() string {
	if r == nil {
		return ""
	}
	return r.options.NodeID
}

// IncarnationID returns the generated process incarnation identifier.
func (r *ClusterRuntime) IncarnationID() string {
	if r == nil {
		return ""
	}
	return r.options.IncarnationID
}

// Backend returns the configured cluster backend.
func (r *ClusterRuntime) Backend() string {
	if r == nil {
		return ""
	}
	return r.options.Backend
}

// Start starts all cluster control-plane components exactly once.
func (r *ClusterRuntime) Start(ctx context.Context) error {
	if r == nil || !r.options.Enabled {
		return nil
	}

	r.startOnce.Do(func() {
		for _, component := range r.components() {
			if err := component.Start(ctx); err != nil {
				r.startErr = err
				return
			}
		}
	})

	return r.startErr
}

// Shutdown stops all cluster control-plane components exactly once.
func (r *ClusterRuntime) Shutdown(ctx context.Context) error {
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

func (r *ClusterRuntime) components() []ClusterLifecycle {
	return []ClusterLifecycle{
		r.deps.SessionDirectory,
		r.deps.CommandBus,
		r.deps.QueryStore,
		r.deps.NodeLeaseManager,
		r.deps.ProjectionRepairer,
	}
}

type noopClusterComponent struct{}

func (noopClusterComponent) Start(context.Context) error    { return nil }
func (noopClusterComponent) Shutdown(context.Context) error { return nil }

type noopSessionDirectory struct{ noopClusterComponent }
type noopClusterCommandBus struct{ noopClusterComponent }
type noopClusterQueryStore struct{ noopClusterComponent }
type noopClusterNodeLeaseManager struct{ noopClusterComponent }
type noopClusterProjectionRepairer struct{ noopClusterComponent }
