package runtime

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/lynx-go/x/log"
)

const (
	defaultClusterNodeLeaseTTL           = 90 * time.Second
	defaultClusterNodeLeaseRenewInterval = 30 * time.Second
	// defaultClusterSessionLeaseTTL is the expected session lease TTL for the
	// default heartbeat config (idle=300s, ping=0): the owning node renews
	// the lease only on ping/pong refreshes (throttled to every 10s), so a
	// lease shorter than the idle timeout would let a live but idle session
	// be taken over. sessionLeaseTTL() recomputes the TTL from the heartbeat
	// config and can shrink below this constant for second-scale heartbeats.
	defaultClusterSessionLeaseTTL    = 600 * time.Second
	defaultClusterSessionSnapshotTTL = 24 * time.Hour
	defaultClusterQueryProjectionTTL = 10 * time.Minute
)

func (noopSessionDirectory) PutNodeLease(context.Context, *ClusterNodeLease, time.Duration) error {
	return nil
}

func (noopSessionDirectory) GetNodeLease(context.Context, string, string) (*ClusterNodeLease, error) {
	return nil, nil
}

// CompareAndSwapSessionLease on the noop directory always succeeds: there is
// no remote directory to conflict with, so the local sync must never be
// fenced by a lease it cannot even read back.
func (noopSessionDirectory) CompareAndSwapSessionLease(context.Context, *ClusterSessionLease, *ClusterSessionLease, time.Duration) (bool, error) {
	return true, nil
}

// CompareAndSwapSessionState on the noop directory always succeeds, like the
// lease-only CAS above: with no remote directory there is nothing to write
// atomically.
func (noopSessionDirectory) CompareAndSwapSessionState(context.Context, *ClusterSessionLease, *ClusterSessionLease, *ClusterSessionSnapshot, time.Duration, time.Duration) (bool, error) {
	return true, nil
}

func (noopSessionDirectory) GetSessionLease(context.Context, string) (*ClusterSessionLease, error) {
	return nil, nil
}

func (noopSessionDirectory) DeleteSessionLease(context.Context, string) error {
	return nil
}

func (noopSessionDirectory) PutSessionSnapshot(context.Context, *ClusterSessionSnapshot, time.Duration) error {
	return nil
}

func (noopSessionDirectory) GetSessionSnapshot(context.Context, string) (*ClusterSessionSnapshot, error) {
	return nil, nil
}

func (noopSessionDirectory) DeleteSessionSnapshot(context.Context, string) error {
	return nil
}

func (noopSessionDirectory) AddUserSession(context.Context, string, string, time.Duration) error {
	return nil
}

func (noopSessionDirectory) RemoveUserSession(context.Context, string, string) error {
	return nil
}

func (noopSessionDirectory) ListUserSessions(context.Context, string) ([]string, error) {
	return nil, nil
}

func (noopClusterCommandBus) SetHandler(ClusterCommandHandler) {}

func (noopClusterCommandBus) SendCommand(context.Context, *ClusterCommand) (*ClusterCommandResult, error) {
	return nil, ErrClusterCommandUnsupported
}

func (noopClusterCommandBus) BroadcastCommand(context.Context, *ClusterCommand) ([]*ClusterCommandResult, error) {
	return nil, ErrClusterCommandUnsupported
}

func (noopClusterQueryStore) AdjustChannelSubscriptions(context.Context, string, int64, time.Duration) error {
	return nil
}

func (noopClusterQueryStore) ReplaceNodeChannels(context.Context, map[string]int64, time.Duration) error {
	return nil
}

func (noopClusterQueryStore) ListChannels(context.Context) ([]ClusterChannelInfo, error) {
	return nil, nil
}

func (noopClusterQueryStore) ListNodeProjections(context.Context) ([]ClusterNodeProjection, error) {
	return nil, nil
}

func (noopClusterQueryStore) DeleteNodeProjection(context.Context, string, string) error {
	return nil
}

// SessionDirectory returns the cluster session directory adapter.
func (r *Cluster) SessionDirectory() SessionDirectory {
	if r == nil {
		return &noopSessionDirectory{}
	}
	return r.deps.SessionDirectory
}

// CommandBus returns the cluster command bus adapter.
func (r *Cluster) CommandBus() ClusterCommandBus {
	if r == nil {
		return &noopClusterCommandBus{}
	}
	return r.deps.CommandBus
}

// QueryStore returns the cluster query store adapter.
func (r *Cluster) QueryStore() ClusterQueryStore {
	if r == nil {
		return &noopClusterQueryStore{}
	}
	return r.deps.QueryStore
}

// ClusterEnabled reports whether distributed control-plane behavior is enabled.
func (n *Node) ClusterEnabled() bool {
	return n.cluster != nil && n.cluster.Enabled()
}

// ClusterNodeID returns the configured cluster node identifier.
func (n *Node) ClusterNodeID() string {
	if n.cluster == nil {
		return ""
	}
	return n.cluster.NodeID()
}

// ClusterIncarnationID returns the generated cluster incarnation identifier.
func (n *Node) ClusterIncarnationID() string {
	if n.cluster == nil {
		return ""
	}
	return n.cluster.IncarnationID()
}

func (n *Node) clusterSessionDirectory() SessionDirectory {
	if n.cluster == nil {
		return &noopSessionDirectory{}
	}
	return n.cluster.SessionDirectory()
}

func (n *Node) clusterCommandBus() ClusterCommandBus {
	if n.cluster == nil {
		return &noopClusterCommandBus{}
	}
	return n.cluster.CommandBus()
}

func (n *Node) clusterQueryStore() ClusterQueryStore {
	if n.cluster == nil {
		return &noopClusterQueryStore{}
	}
	return n.cluster.QueryStore()
}

// syncClusterSessionState writes the cluster-visible lease and snapshot for a
// client session (AddClient, subscription saga, throttled ping/pong refresh).
// The lease is never blindly PUT: it is claimed or refreshed with
// CompareAndSwapSessionLease so a fencing taken over by another node is never
// written back (KD-K4). A refresh keeps the lease version unchanged; only
// resumeRemoteSession bumps it during a cross-node takeover.
func (n *Node) syncClusterSessionState(ctx context.Context, client *Client) error {
	if !n.ClusterEnabled() || client == nil {
		return nil
	}

	directory := n.clusterSessionDirectory()
	desired := n.clusterSessionLease(client)
	snapshot := n.clusterSessionSnapshot(client)

	current, err := directory.GetSessionLease(ctx, desired.SessionID)
	if err != nil {
		return err
	}

	// First registration: the directory has no record, so claim it with
	// CAS(expected=nil). A blind SET could overwrite a lease another node
	// registered in the meantime.
	if current == nil {
		ok, err := n.compareAndSwapSessionState(ctx, directory, nil, desired, snapshot)
		if err != nil {
			return err
		}
		if !ok {
			if n.metrics != nil {
				n.metrics.BindFencedTotal.Inc()
			}
			return ErrSessionFenced
		}
		return nil
	}

	// The directory records a different fencing (another node's CAS won the
	// session): this attachment is fenced and must not write anything back.
	if current.NodeID != n.ClusterNodeID() || current.IncarnationID != n.ClusterIncarnationID() {
		if n.metrics != nil {
			n.metrics.BindRefreshFailTotal.Inc()
		}
		return ErrSessionFenced
	}
	// A directory version newer than the local one means this attachment is
	// stale (a newer generation already synced). An equal version is the
	// same-fence refresh: TTL / LastActivity / UserID are refreshed and the
	// lease version stays unchanged. A local version strictly greater is the
	// local-takeover persist path: handleConnect bumps the version on a
	// same-node resume and this write records that bump without creating a
	// new one (refresh never increments).
	if current.LeaseVersion > desired.LeaseVersion {
		if n.metrics != nil {
			n.metrics.BindRefreshFailTotal.Inc()
		}
		return ErrSessionFenced
	}

	ok, err := n.compareAndSwapSessionState(ctx, directory, current, desired, snapshot)
	if err != nil {
		return err
	}
	if !ok {
		if n.metrics != nil {
			n.metrics.BindRefreshFailTotal.Inc()
		}
		return ErrSessionFenced
	}
	return nil
}

// compareAndSwapSessionState runs the lease CAS and the snapshot write as one
// atomic step when the directory implements SessionStateCompareAndSwapper
// (the Redis directory does, via a single Lua script; PR-KA-D10 §1.2).
// Directories without the extension fall back to the two-step CAS +
// PutSessionSnapshot, which is NOT atomic: a stale in-flight refresh can land
// its snapshot after another node won the lease between the two writes. That
// residual window is accepted for the fakes and the noop directory; the
// production Redis path never takes it.
func (n *Node) compareAndSwapSessionState(ctx context.Context, directory SessionDirectory, expected, desired *ClusterSessionLease, snapshot *ClusterSessionSnapshot) (bool, error) {
	if cas, ok := directory.(SessionStateCompareAndSwapper); ok {
		return cas.CompareAndSwapSessionState(ctx, expected, desired, snapshot, n.sessionLeaseTTL(), defaultClusterSessionSnapshotTTL)
	}
	ok, err := directory.CompareAndSwapSessionLease(ctx, expected, desired, n.sessionLeaseTTL())
	if err != nil || !ok {
		return ok, err
	}
	return true, directory.PutSessionSnapshot(ctx, snapshot, defaultClusterSessionSnapshotTTL)
}

func (n *Node) deleteClusterSessionState(ctx context.Context, sessionID string) error {
	if !n.ClusterEnabled() || sessionID == "" {
		return nil
	}

	directory := n.clusterSessionDirectory()

	// Ownership check: only delete state that this node incarnation owns, or
	// whose lease is already gone/expired. A fresh lease identifying another
	// node incarnation means the session is still being served there, and the
	// state must be left intact.
	lease, err := directory.GetSessionLease(ctx, sessionID)
	if err != nil {
		return err
	}
	if lease != nil && lease.ExpiresAt.After(time.Now()) &&
		(lease.NodeID != "" || lease.IncarnationID != "") &&
		(lease.NodeID != n.ClusterNodeID() || lease.IncarnationID != n.ClusterIncarnationID()) {
		return nil
	}

	if err := directory.DeleteSessionLease(ctx, sessionID); err != nil {
		return err
	}
	return directory.DeleteSessionSnapshot(ctx, sessionID)
}

func (n *Node) adjustClusterChannelSubscriptions(ctx context.Context, channel string, delta int64) error {
	if !n.ClusterEnabled() || channel == "" || delta == 0 {
		return nil
	}
	return n.clusterQueryStore().AdjustChannelSubscriptions(ctx, channel, delta, defaultClusterQueryProjectionTTL)
}

func (n *Node) clusterSessionLease(client *Client) *ClusterSessionLease {
	id := client.SnapshotIdentity()

	leaseVersion := id.LeaseVersion
	if leaseVersion == 0 {
		leaseVersion = 1
	}

	return &ClusterSessionLease{
		SessionID:      id.SessionID,
		NodeID:         n.ClusterNodeID(),
		IncarnationID:  n.ClusterIncarnationID(),
		UserID:         id.UserID,
		ClientID:       id.ClientID,
		LeaseVersion:   leaseVersion,
		Authenticated:  id.Authenticated,
		ConnectedAt:    id.ConnectedAt.UnixMilli(),
		LastActivityAt: id.LastActivity.UnixMilli(),
		ExpiresAt:      time.Now().Add(n.sessionLeaseTTL()),
	}
}

func (n *Node) clusterSessionSnapshot(client *Client) *ClusterSessionSnapshot {
	id := client.SnapshotIdentity()
	channels := client.SubscribedChannels()
	authenticated := id.Authenticated
	sessionID := id.SessionID
	userID := id.UserID
	clientID := id.ClientID
	protocol := id.Protocol
	connectedAt := id.ConnectedAt.UnixMilli()

	sort.Strings(channels)
	subscriptions := make([]ClusterSubscriptionSnapshot, 0, len(channels))
	// Per-channel last delivered offset: read back from the hub subscriber
	// record (the same re-read pattern as the ephemeral flag). Zero offsets
	// (nothing delivered, or transient-only) are omitted from the snapshot.
	channelOffsets := make(map[string]uint64, len(channels))
	for _, channel := range channels {
		ephemeral := false
		var deliveredOffset uint64
		if sub, ok := n.hub.LookupSubscriber(channel, client); ok {
			ephemeral = sub.Ephemeral
			deliveredOffset = sub.DeliveredOffset
		}
		subscriptions = append(subscriptions, ClusterSubscriptionSnapshot{Channel: channel, Ephemeral: ephemeral})
		if deliveredOffset > 0 {
			channelOffsets[channel] = deliveredOffset
		}
	}

	snapshot := &ClusterSessionSnapshot{
		SessionID:     sessionID,
		UserID:        userID,
		ClientID:      clientID,
		Authenticated: authenticated,
		Protocol:      protocol,
		ConnectedAt:   connectedAt,
		Subscriptions: subscriptions,
		// ChannelOffsets feeds the exact cross-node resume: the resuming
		// node recovers from ChannelOffsets[ch]+1 (see the field comment).
		ChannelOffsets: channelOffsets,
		// BrokerEpoch lets the resuming node detect broker history
		// invalidation across the resume (see the field comment).
		AuthContext: map[string]string{
			"user_id":   userID,
			"client_id": clientID,
			"protocol":  protocol,
		},
		UpdatedAt: time.Now(),
	}
	if epochBroker, ok := n.broker.(interface{ Epoch() string }); ok {
		snapshot.BrokerEpoch = epochBroker.Epoch()
	}
	return snapshot
}

// ClusterNodeLeaseManagerConfig configures the generic node lease renewer.
type ClusterNodeLeaseManagerConfig struct {
	NodeID        string
	IncarnationID string
	TTL           time.Duration
	RenewInterval time.Duration
}

// NewClusterNodeLeaseManager creates a generic node lease renewer backed by SessionDirectory.
func NewClusterNodeLeaseManager(directory SessionDirectory, cfg ClusterNodeLeaseManagerConfig) ClusterNodeLeaseManager {
	if directory == nil || cfg.NodeID == "" || cfg.IncarnationID == "" {
		return &noopClusterNodeLeaseManager{}
	}
	if cfg.TTL <= 0 {
		cfg.TTL = defaultClusterNodeLeaseTTL
	}
	if cfg.RenewInterval <= 0 {
		cfg.RenewInterval = defaultClusterNodeLeaseRenewInterval
	}
	return &clusterNodeLeaseManager{
		directory: directory,
		config:    cfg,
	}
}

type clusterNodeLeaseManager struct {
	directory SessionDirectory
	config    ClusterNodeLeaseManagerConfig

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
	start  bool
	stop   bool
}

func (m *clusterNodeLeaseManager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.start {
		return nil
	}
	leaseCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.start = true

	if err := m.renewOnce(leaseCtx); err != nil {
		cancel()
		m.cancel = nil
		m.start = false
		return err
	}

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(m.config.RenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
				if err := m.renewOnce(leaseCtx); err != nil {
					log.WarnContext(leaseCtx, "cluster node lease renewal failed", "node_id", m.config.NodeID, "incarnation_id", m.config.IncarnationID, "error", err)
				}
			}
		}
	}()

	return nil
}

func (m *clusterNodeLeaseManager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	if m.stop {
		m.mu.Unlock()
		return nil
	}
	m.stop = true
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.wg.Wait()
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}

func (m *clusterNodeLeaseManager) renewOnce(ctx context.Context) error {
	lease := &ClusterNodeLease{
		NodeID:        m.config.NodeID,
		IncarnationID: m.config.IncarnationID,
		StartedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(m.config.TTL),
	}
	return m.directory.PutNodeLease(ctx, lease, m.config.TTL)
}
