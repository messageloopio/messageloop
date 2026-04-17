package messageloop

import (
	"context"
	"sync"
	"time"

	"github.com/lynx-go/x/log"
)

const defaultClusterProjectionRepairInterval = 30 * time.Second

// ClusterProjectionRepairerConfig configures the shared projection repair loop.
type ClusterProjectionRepairerConfig struct {
	Interval time.Duration
}

// NewClusterProjectionRepairer creates a periodic repairer that republishes this node's local channel projections.
func NewClusterProjectionRepairer(node *Node, store ClusterQueryStore, cfg ClusterProjectionRepairerConfig) ClusterProjectionRepairer {
	if node == nil || store == nil {
		return &noopClusterProjectionRepairer{}
	}
	if cfg.Interval <= 0 {
		cfg.Interval = defaultClusterProjectionRepairInterval
	}
	return &clusterProjectionRepairer{
		node:  node,
		store: store,
		cfg:   cfg,
	}
}

type clusterProjectionRepairer struct {
	node  *Node
	store ClusterQueryStore
	cfg   ClusterProjectionRepairerConfig

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
	start  bool
	stop   bool
}

func (r *clusterProjectionRepairer) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.start {
		return nil
	}
	repairCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.start = true

	if err := r.repairOnce(repairCtx); err != nil {
		cancel()
		r.cancel = nil
		r.start = false
		return err
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
					log.WarnContext(repairCtx, "cluster projection repair failed", "error", err)
				}
			}
		}
	}()

	return nil
}

func (r *clusterProjectionRepairer) Shutdown(ctx context.Context) error {
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

func (r *clusterProjectionRepairer) repairOnce(ctx context.Context) error {
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
	log.DebugContext(ctx, "cluster projection repair applied", "channels", len(counts))
	return nil
}
