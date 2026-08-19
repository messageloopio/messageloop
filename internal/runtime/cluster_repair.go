package runtime

import (
	"context"
	"math/rand"
	"sync"
	"time"

	"github.com/lynx-go/x/log"
)

const (
	// defaultClusterRepairInterval is the period of the projection republish,
	// dead-projection reaping, and user-index rebuild work.
	defaultClusterRepairInterval = 30 * time.Second
	// defaultClusterMembershipInterval is the base period of the node-lease
	// SCAN that drives membership OnLeave; each tick is jittered by ±20%.
	defaultClusterMembershipInterval = 5 * time.Second
	// clusterMembershipJitterFraction is the ± fraction applied to the
	// membership SCAN period so nodes do not synchronize their scans.
	clusterMembershipJitterFraction = 0.2
)

// ClusterRepairerConfig configures the single cluster repair loop.
type ClusterRepairerConfig struct {
	// Interval is the period of the projection/user-index repair work
	// (default 30s).
	Interval time.Duration
	// MembershipInterval is the base period of the node-lease SCAN driving
	// OnLeave (default 5s, ±20% jitter per tick).
	MembershipInterval time.Duration
	// NodeID and IncarnationID identify this node; the repairer never fires
	// OnLeave for its own incarnation. When the repairer is constructed with
	// a *Node, the node's identity takes precedence.
	NodeID        string
	IncarnationID string
	// OnLeave, when set, is invoked after a departed incarnation's session
	// fencing has been invalidated. It exists for tests and optional
	// integrations; the invalidation itself does not depend on it.
	OnLeave func(nodeID, incarnationID string)
}

// NewClusterRepairer creates the single cluster repairer (PR-KA-B4): one
// lifecycle component, one ticker, driving projection republish, dead
// projection reaping, user→sessions index rebuild, and membership OnLeave.
// node and store may be nil (projection work is skipped); a directory without
// lease enumeration disables the corresponding repair work. A repairer with
// no work to do at all is a no-op.
func NewClusterRepairer(node *Node, directory SessionDirectory, store ClusterQueryStore, cfg ClusterRepairerConfig) ClusterRepairer {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultClusterRepairInterval
	}
	if cfg.MembershipInterval <= 0 {
		cfg.MembershipInterval = defaultClusterMembershipInterval
	}
	sessionLister, _ := directory.(ClusterSessionLeaseLister)
	nodeLister, _ := directory.(ClusterNodeLeaseLister)
	noLeaseWork := directory == nil || (sessionLister == nil && nodeLister == nil)
	noProjectionWork := node == nil || store == nil
	if noLeaseWork && noProjectionWork {
		return &noopClusterRepairer{}
	}
	return &clusterRepairer{
		node:          node,
		directory:     directory,
		sessionLister: sessionLister,
		nodeLister:    nodeLister,
		store:         store,
		cfg:           cfg,
		rand:          rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

type clusterRepairer struct {
	node          *Node
	directory     SessionDirectory
	sessionLister ClusterSessionLeaseLister
	nodeLister    ClusterNodeLeaseLister
	store         ClusterQueryStore
	cfg           ClusterRepairerConfig
	rand          *rand.Rand

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
	start  bool
	stop   bool

	// membership tracks the (NodeID, IncarnationID) set seen alive on the
	// previous SCAN beat. The first beat only primes the set: a repairer that
	// starts after a peer died must not fire OnLeave for incarnations it has
	// never seen alive.
	membershipMu sync.Mutex
	alive        map[nodeIncarnation]struct{}
	primed       bool
}

type nodeIncarnation struct {
	nodeID        string
	incarnationID string
}

func (r *clusterRepairer) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.start {
		return nil
	}
	repairCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.start = true

	// The first passes run synchronously so a freshly started node immediately
	// has an up-to-date projection, user index, and membership view. Repair is
	// best-effort: a first-pass failure only warns and is retried on tick.
	if err := r.repairOnce(repairCtx); err != nil {
		log.WarnContext(repairCtx, "cluster repair failed", "error", err)
	}
	if err := r.membershipOnce(repairCtx); err != nil {
		log.WarnContext(repairCtx, "cluster membership scan failed", "error", err)
	}

	r.wg.Add(1)
	go r.loop(repairCtx)
	return nil
}

func (r *clusterRepairer) Shutdown(ctx context.Context) error {
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

// loop is the single ticker of the cluster control plane: every
// (jittered) membership beat it SCANs node leases for OnLeave, and every
// cfg.Interval it runs the full repair pass.
func (r *clusterRepairer) loop(ctx context.Context) {
	defer r.wg.Done()
	timer := time.NewTimer(r.nextMembershipDelay())
	defer timer.Stop()
	lastRepair := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-timer.C:
			if err := r.membershipOnce(ctx); err != nil {
				log.WarnContext(ctx, "cluster membership scan failed", "error", err)
			}
			if now.Sub(lastRepair) >= r.cfg.Interval {
				lastRepair = now
				if err := r.repairOnce(ctx); err != nil {
					log.WarnContext(ctx, "cluster repair failed", "error", err)
				}
			}
			timer.Reset(r.nextMembershipDelay())
		}
	}
}

// nextMembershipDelay returns the membership SCAN period with ±20% jitter.
// Only the loop goroutine calls it, so the rand source needs no locking.
func (r *clusterRepairer) nextMembershipDelay() time.Duration {
	base := r.cfg.MembershipInterval
	jitter := (r.rand.Float64()*2 - 1) * clusterMembershipJitterFraction
	delay := time.Duration(float64(base) * (1 + jitter))
	if delay <= 0 {
		return base
	}
	return delay
}

// repairOnce runs the 30-second work: republish this node's channel
// projections, reap dead node projections, and rebuild the user→sessions
// index from authoritative session leases.
func (r *clusterRepairer) repairOnce(ctx context.Context) error {
	if err := r.repairProjections(ctx); err != nil {
		return err
	}
	return r.repairUserIndex(ctx)
}

// repairProjections republishes this node's local channel projections and
// reaps owner projections whose node lease has expired (without reaping they
// linger until the projection TTL and keep phantom channels visible).
func (r *clusterRepairer) repairProjections(ctx context.Context) error {
	if r.node == nil || r.store == nil {
		return nil
	}
	channels := r.node.Hub().GetActiveChannels()
	counts := make(map[string]int64, len(channels))
	for _, channel := range channels {
		if channel.Name == "" || channel.Subscribers <= 0 {
			continue
		}
		counts[channel.Name] = int64(channel.Subscribers)
	}
	if err := r.store.ReplaceNodeChannels(ctx, counts, defaultClusterQueryProjectionTTL); err != nil {
		if r.node.metrics != nil {
			r.node.metrics.ClusterProjectionRepairFailures.Inc()
		}
		return err
	}
	if r.node.metrics != nil {
		r.node.metrics.ClusterProjectionRepairs.Inc()
	}

	projections, err := r.store.ListNodeProjections(ctx)
	if err != nil {
		log.WarnContext(ctx, "failed to list node projections for reaping", err)
		return nil
	}
	directory := r.directory
	if directory == nil {
		directory = r.node.clusterSessionDirectory()
	}
	for _, projection := range projections {
		if projection.NodeID == r.node.ClusterNodeID() && projection.IncarnationID == r.node.ClusterIncarnationID() {
			// The node's own projection is refreshed above; never reap it.
			continue
		}
		lease, err := directory.GetNodeLease(ctx, projection.NodeID, projection.IncarnationID)
		if err != nil {
			log.WarnContext(ctx, "failed to check node lease for projection reaping",
				err, "node_id", projection.NodeID, "incarnation_id", projection.IncarnationID)
			continue
		}
		if lease != nil {
			continue
		}
		if err := r.store.DeleteNodeProjection(ctx, projection.NodeID, projection.IncarnationID); err != nil {
			log.WarnContext(ctx, "failed to reap dead owner projection",
				err, "node_id", projection.NodeID, "incarnation_id", projection.IncarnationID)
			continue
		}
		log.DebugContext(ctx, "reaped dead owner projection",
			"node_id", projection.NodeID, "incarnation_id", projection.IncarnationID)
	}

	log.DebugContext(ctx, "cluster projection repair applied", "channels", len(counts))
	return nil
}

// repairUserIndex rescans every session lease and re-adds the membership of
// non-empty users, refreshing member TTLs to each lease's remaining lifetime
// so members expire together with their lease. Missing or user-changed leases
// are not enumerated here: stale set entries are filtered at expansion time
// (GetSessionLease re-check).
func (r *clusterRepairer) repairUserIndex(ctx context.Context) error {
	if r.sessionLister == nil || r.directory == nil {
		return nil
	}
	leases, err := r.sessionLister.ListSessionLeases(ctx)
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

// membershipOnce SCANs the node leases, updates the alive-incarnation set,
// and fires OnLeave for every incarnation that was alive on the previous beat
// and is gone now. The first beat only primes the set. This is control-plane
// work; no hot path (publish/subscribe/ping) ever SCANs.
func (r *clusterRepairer) membershipOnce(ctx context.Context) error {
	if r.nodeLister == nil {
		return nil
	}
	leases, err := r.nodeLister.ListNodeLeases(ctx)
	if err != nil {
		return err
	}

	now := time.Now()
	current := make(map[nodeIncarnation]struct{}, len(leases))
	for _, lease := range leases {
		if lease == nil || lease.NodeID == "" || lease.IncarnationID == "" {
			continue
		}
		if !lease.ExpiresAt.IsZero() && !lease.ExpiresAt.After(now) {
			// The lease record exists but has already expired.
			continue
		}
		current[nodeIncarnation{nodeID: lease.NodeID, incarnationID: lease.IncarnationID}] = struct{}{}
	}

	r.membershipMu.Lock()
	if !r.primed {
		r.alive = current
		r.primed = true
		r.membershipMu.Unlock()
		return nil
	}
	previous := r.alive
	r.alive = current
	r.membershipMu.Unlock()

	selfNode, selfIncarnation := r.self()
	for member := range previous {
		if _, ok := current[member]; ok {
			continue
		}
		if member.nodeID == selfNode && member.incarnationID == selfIncarnation {
			// Never OnLeave ourselves: a SCAN hiccup must not invalidate our
			// own session fencing.
			continue
		}
		r.onLeave(ctx, member.nodeID, member.incarnationID)
	}
	return nil
}

func (r *clusterRepairer) self() (string, string) {
	if r.node != nil {
		return r.node.ClusterNodeID(), r.node.ClusterIncarnationID()
	}
	return r.cfg.NodeID, r.cfg.IncarnationID
}

// onLeave invalidates a departed incarnation's session fencing instead of
// waiting out the 600s session lease TTL: every session lease naming the dead
// incarnation is deleted (which also syncs the user index), and its owner
// projection is dropped. The grace period is one membership SCAN period.
func (r *clusterRepairer) onLeave(ctx context.Context, nodeID, incarnationID string) {
	log.InfoContext(ctx, "cluster node incarnation left, invalidating its session fencing",
		"node_id", nodeID, "incarnation_id", incarnationID)

	if r.sessionLister != nil && r.directory != nil {
		leases, err := r.sessionLister.ListSessionLeases(ctx)
		if err != nil {
			log.WarnContext(ctx, "failed to list session leases for OnLeave", err,
				"node_id", nodeID, "incarnation_id", incarnationID)
		} else {
			for _, lease := range leases {
				if lease == nil || lease.SessionID == "" {
					continue
				}
				if lease.NodeID != nodeID || lease.IncarnationID != incarnationID {
					continue
				}
				if err := r.directory.DeleteSessionLease(ctx, lease.SessionID); err != nil {
					log.WarnContext(ctx, "failed to delete dead incarnation's session lease", err,
						"session_id", lease.SessionID, "node_id", nodeID, "incarnation_id", incarnationID)
				}
			}
		}
	}

	if r.store != nil {
		if err := r.store.DeleteNodeProjection(ctx, nodeID, incarnationID); err != nil {
			log.WarnContext(ctx, "failed to delete dead incarnation's projection", err,
				"node_id", nodeID, "incarnation_id", incarnationID)
		}
	}

	if r.cfg.OnLeave != nil {
		r.cfg.OnLeave(nodeID, incarnationID)
	}
}

var _ ClusterRepairer = (*clusterRepairer)(nil)
