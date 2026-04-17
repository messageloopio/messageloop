package messageloop

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/lynx-go/x/log"
)

const (
	defaultClusterNodeLeaseTTL           = 90 * time.Second
	defaultClusterNodeLeaseRenewInterval = 30 * time.Second
	defaultClusterSessionLeaseTTL        = 90 * time.Second
	defaultClusterSessionSnapshotTTL     = 24 * time.Hour
	defaultClusterQueryProjectionTTL     = 10 * time.Minute
)

var (
	// ErrClusterCommandUnsupported indicates the current cluster command bus cannot execute distributed commands.
	ErrClusterCommandUnsupported = errors.New("cluster command bus is not configured")
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
	SessionID      string                        `json:"session_id"`
	UserID         string                        `json:"user_id,omitempty"`
	ClientID       string                        `json:"client_id,omitempty"`
	Authenticated  bool                          `json:"authenticated"`
	Protocol       string                        `json:"protocol,omitempty"`
	ConnectedAt    int64                         `json:"connected_at,omitempty"`
	Subscriptions  []ClusterSubscriptionSnapshot `json:"subscriptions,omitempty"`
	ChannelOffsets map[string]uint64             `json:"channel_offsets,omitempty"`
	BrokerEpoch    string                        `json:"broker_epoch,omitempty"`
	AuthContext    map[string]string             `json:"auth_context,omitempty"`
	UpdatedAt      time.Time                     `json:"updated_at"`
}

// ClusterChannelInfo describes one shared subscription key projection.
type ClusterChannelInfo struct {
	Name        string `json:"name"`
	Subscribers int64  `json:"subscribers"`
}

// ClusterCommandHandler handles one incoming cluster command locally.
type ClusterCommandHandler func(ctx context.Context, cmd *ClusterCommand) (*ClusterCommandResult, error)

func (noopSessionDirectory) PutNodeLease(context.Context, *ClusterNodeLease, time.Duration) error {
	return nil
}

func (noopSessionDirectory) GetNodeLease(context.Context, string, string) (*ClusterNodeLease, error) {
	return nil, nil
}

func (noopSessionDirectory) PutSessionLease(context.Context, *ClusterSessionLease, time.Duration) error {
	return nil
}

func (noopSessionDirectory) CompareAndSwapSessionLease(context.Context, *ClusterSessionLease, *ClusterSessionLease, time.Duration) (bool, error) {
	return false, nil
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

// SessionDirectory returns the cluster session directory adapter.
func (r *ClusterRuntime) SessionDirectory() SessionDirectory {
	if r == nil {
		return &noopSessionDirectory{}
	}
	return r.deps.SessionDirectory
}

// CommandBus returns the cluster command bus adapter.
func (r *ClusterRuntime) CommandBus() ClusterCommandBus {
	if r == nil {
		return &noopClusterCommandBus{}
	}
	return r.deps.CommandBus
}

// QueryStore returns the cluster query store adapter.
func (r *ClusterRuntime) QueryStore() ClusterQueryStore {
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

func (n *Node) syncClusterSessionState(ctx context.Context, client *Client) error {
	if !n.ClusterEnabled() || client == nil {
		return nil
	}

	directory := n.clusterSessionDirectory()
	lease := n.clusterSessionLease(client)
	snapshot := n.clusterSessionSnapshot(client)

	if err := directory.PutSessionLease(ctx, lease, defaultClusterSessionLeaseTTL); err != nil {
		return err
	}
	return directory.PutSessionSnapshot(ctx, snapshot, defaultClusterSessionSnapshotTTL)
}

func (n *Node) deleteClusterSessionState(ctx context.Context, sessionID string) error {
	if !n.ClusterEnabled() || sessionID == "" {
		return nil
	}

	directory := n.clusterSessionDirectory()
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
	client.mu.RLock()
	defer client.mu.RUnlock()

	leaseVersion := client.clusterLeaseVersion
	if leaseVersion == 0 {
		leaseVersion = 1
	}

	return &ClusterSessionLease{
		SessionID:      client.session,
		NodeID:         n.ClusterNodeID(),
		IncarnationID:  n.ClusterIncarnationID(),
		UserID:         client.user,
		ClientID:       client.client,
		LeaseVersion:   leaseVersion,
		Authenticated:  client.authenticated,
		ConnectedAt:    client.connectedAt.UnixMilli(),
		LastActivityAt: client.lastActivity.UnixMilli(),
		ExpiresAt:      time.Now().Add(defaultClusterSessionLeaseTTL),
	}
}

func (n *Node) clusterSessionSnapshot(client *Client) *ClusterSessionSnapshot {
	client.mu.RLock()
	channels := make([]string, 0, len(client.subscribedChannels))
	for channel := range client.subscribedChannels {
		channels = append(channels, channel)
	}
	authenticated := client.authenticated
	sessionID := client.session
	userID := client.user
	clientID := client.client
	protocol := client.protocol
	connectedAt := client.connectedAt.UnixMilli()
	client.mu.RUnlock()

	sort.Strings(channels)
	subscriptions := make([]ClusterSubscriptionSnapshot, 0, len(channels))
	for _, channel := range channels {
		subscriptions = append(subscriptions, ClusterSubscriptionSnapshot{Channel: channel})
	}

	return &ClusterSessionSnapshot{
		SessionID:     sessionID,
		UserID:        userID,
		ClientID:      clientID,
		Authenticated: authenticated,
		Protocol:      protocol,
		ConnectedAt:   connectedAt,
		Subscriptions: subscriptions,
		AuthContext: map[string]string{
			"user_id":   userID,
			"client_id": clientID,
			"protocol":  protocol,
		},
		UpdatedAt: time.Now(),
	}
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
