package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/messageloopio/messageloop/internal/cluster"
)

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
	normalized, err := options.Normalize()
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

	// An empty IncarnationID is issued by the node epoch allocator (KD-K27):
	// INCR on Redis, the process-local counter on memory/noop. A redis
	// backend whose directory cannot allocate is a startup error — never a
	// silent random or non-monotonic ID. This must happen before the
	// repairer derivation below, which copies the incarnation into its
	// config.
	if normalized.Enabled && normalized.IncarnationID == "" {
		incarnationID, err := cluster.AllocateNodeIncarnation(normalized, deps.SessionDirectory)
		if err != nil {
			return nil, err
		}
		normalized.IncarnationID = incarnationID
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

// IncarnationID returns the process incarnation identifier: the decimal
// node_epoch allocated at startup, or the caller-provided ID in tests.
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
	// ClusterNodeLeaseManager and ClusterRepairer are structurally identical
	// marker interfaces (both are a bare ClusterLifecycle), so an interface
	// case cannot tell them apart; match the concrete types the constructors
	// produce instead.
	case *clusterNodeLeaseManager, *noopClusterNodeLeaseManager:
		return "node_lease_manager"
	case *clusterRepairer, *noopClusterRepairer:
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
