package messageloop

import (
	"context"
	"sync"
	"time"

	"github.com/lynx-go/x/log"
)

const defaultClusterUserIndexRepairInterval = 30 * time.Second

// ClusterUserIndexRepairerConfig configures the periodic user-index repair loop.
type ClusterUserIndexRepairerConfig struct {
	Interval time.Duration
}

// NewClusterUserIndexRepairer creates a periodic repairer that rebuilds the
// user→sessions index from authoritative session leases. The directory must
// be able to enumerate leases (ClusterSessionLeaseLister); a directory
// without enumeration support yields a no-op repairer. The repair runs
// standalone — it is deliberately not part of the channel projection repair
// loop — and never runs when the cluster is disabled (it is only started as
// a cluster component).
func NewClusterUserIndexRepairer(directory SessionDirectory, cfg ClusterUserIndexRepairerConfig) ClusterUserIndexRepairer {
	lister, ok := directory.(ClusterSessionLeaseLister)
	if !ok {
		return &noopClusterUserIndexRepairer{}
	}
	if cfg.Interval <= 0 {
		cfg.Interval = defaultClusterUserIndexRepairInterval
	}
	return &clusterUserIndexRepairer{
		directory: directory,
		lister:    lister,
		cfg:       cfg,
	}
}

type clusterUserIndexRepairer struct {
	directory SessionDirectory
	lister    ClusterSessionLeaseLister
	cfg       ClusterUserIndexRepairerConfig

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
	start  bool
	stop   bool
}

func (r *clusterUserIndexRepairer) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.start {
		return nil
	}
	repairCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.start = true

	if err := r.repairOnce(repairCtx); err != nil {
		// The index is a hint: a first-pass failure must not take the
		// cluster down. Later ticks retry the same way.
		log.WarnContext(repairCtx, "cluster user index repair failed", "error", err)
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		ticker := time.NewTicker(r.cfg.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-repairCtx.Done():
				return
			case <-ticker.C:
				if err := r.repairOnce(repairCtx); err != nil {
					log.WarnContext(repairCtx, "cluster user index repair failed", "error", err)
				}
			}
		}
	}()

	return nil
}

func (r *clusterUserIndexRepairer) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	if r.stop {
		r.mu.Unlock()
		return nil
	}
	r.stop = true
	if r.cancel != nil {
		r.cancel()
	}
	r.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.wg.Wait()
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}

// repairOnce rescans every session lease and re-adds the membership of
// non-empty users, refreshing member TTLs to each lease's remaining lifetime
// so members expire together with their lease. Missing or user-changed
// leases are not enumerated here: stale set entries are filtered at
// expansion time (GetSessionLease re-check).
func (r *clusterUserIndexRepairer) repairOnce(ctx context.Context) error {
	leases, err := r.lister.ListSessionLeases(ctx)
	if err != nil {
		return err
	}
	for _, lease := range leases {
		if lease == nil || lease.SessionID == "" || lease.UserID == "" {
			continue
		}
		ttl := time.Until(lease.ExpiresAt)
		if ttl <= 0 {
			continue
		}
		if err := r.directory.AddUserSession(ctx, lease.UserID, lease.SessionID, ttl); err != nil {
			return err
		}
	}
	return nil
}
