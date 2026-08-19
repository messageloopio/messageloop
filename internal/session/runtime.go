// Package session is the Session Plane (KD-K26 phase three (b), PR-KA-D14):
// Session/Hub/Transport/Heartbeat plus the Runtime seam that inverts
// Session's former *Node field. Callers import this package directly
// (D15 deleted the root aliases).
package session

import (
	"context"
	"time"

	"github.com/messageloopio/messageloop/config"
	"github.com/messageloopio/messageloop/internal/authz"
	"github.com/messageloopio/messageloop/internal/channel"
	"github.com/messageloopio/messageloop/internal/cluster"
	"github.com/messageloopio/messageloop/internal/metrics"
	"github.com/messageloopio/messageloop/internal/occupancy"
	"github.com/messageloopio/messageloop/internal/protocol"
	"github.com/messageloopio/messageloop/internal/stream"
	"github.com/messageloopio/messageloop/internal/survey"
	"github.com/messageloopio/messageloop/proxy"
	"github.com/messageloopio/messageloop/shared"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

// Local type aliases so the git-mv'd files keep using short names (body
// stays byte-identical except package/import, s.node→s.rt, and §3.5).
// These alias internal/* and shared — they are not root-package aliases.
type (
	Disconnect                  = protocol.Disconnect
	ChannelPolicy               = channel.ChannelPolicy
	Action                      = authz.Action
	Principal                   = authz.Principal
	Authorizer                  = authz.Authorizer
	Decision                    = authz.Decision
	PresenceInfo                = occupancy.PresenceInfo
	PresenceStore               = occupancy.PresenceStore
	Publication                 = stream.Publication
	ClusterSessionSnapshot      = cluster.ClusterSessionSnapshot
	ClusterSubscriptionSnapshot = cluster.ClusterSubscriptionSnapshot
	Metrics                     = metrics.Metrics
	Marshaler                   = shared.Marshaler
	JSONMarshaler               = shared.JSONMarshaler
	ProtobufMarshaler           = shared.ProtobufMarshaler
	SurveyResult                = survey.SurveyResult
)

var (
	DisconnectConnectionClosed      = protocol.DisconnectConnectionClosed
	DisconnectInvalidToken          = protocol.DisconnectInvalidToken
	DisconnectBadRequest            = protocol.DisconnectBadRequest
	DisconnectStale                 = protocol.DisconnectStale
	DisconnectForceNoReconnect      = protocol.DisconnectForceNoReconnect
	DisconnectConnectionLimit       = protocol.DisconnectConnectionLimit
	DisconnectChannelLimit          = protocol.DisconnectChannelLimit
	DisconnectInappropriateProtocol = protocol.DisconnectInappropriateProtocol
	DisconnectPermissionDenied      = protocol.DisconnectPermissionDenied
	DisconnectNotAvailable          = protocol.DisconnectNotAvailable
	DisconnectTooManyErrors         = protocol.DisconnectTooManyErrors
	DisconnectIdleTimeout           = protocol.DisconnectIdleTimeout
	DisconnectSlowConsumer          = protocol.DisconnectSlowConsumer
	DisconnectInternal              = protocol.DisconnectInternal
	DisconnectUnsupportedVersion    = protocol.DisconnectUnsupportedVersion

	DefaultChannelPolicy     = channel.DefaultChannelPolicy
	ErrPatternNotRoutable    = channel.ErrPatternNotRoutable
	ErrHistoryDisabled       = channel.ErrHistoryDisabled
	PublicationFromPayloadV2 = stream.PublicationFromPayloadV2
	CompileInterest          = channel.CompileInterest
	ErrSessionFenced         = cluster.ErrSessionFenced
	MetricsTransportLabel    = metrics.MetricsTransportLabel
	ProtoJSONMarshaler       = shared.ProtoJSONMarshaler
	NewMemoryPresenceStore   = occupancy.NewMemoryPresenceStore
)

const (
	ActionSubscribePattern = authz.ActionSubscribePattern
	ActionPublish          = authz.ActionPublish
	ActionRecover          = authz.ActionRecover
	ActionPresence         = authz.ActionPresence
	ActionSurvey           = authz.ActionSurvey

	PayloadKindBinary = stream.PayloadKindBinary
	PayloadKindText   = stream.PayloadKindText
	PayloadKindJSON   = stream.PayloadKindJSON
)

// clusterEvictRollbackTimeout bounds the re-subscription rollback after a
// partially failed session takeover eviction. Moved from cluster_resume.go
// with the Fence path (PR-KA-D14 §3.5); the root definition is deleted.
const clusterEvictRollbackTimeout = 5 * time.Second

// Runtime 是 Session 对节点编排层的依赖缝(KD-K26 阶段三(b),PR-KA-D14)。
// 访问器每次调用时读取,容忍 SetMetrics 等晚期注入;编排方法由根包
// nodeRuntime 适配器委托到 *Node(含未导出方法)。
type Runtime interface {
	// 装配访问器
	Hub() *Hub
	Metrics() *metrics.Metrics
	Presence() occupancy.PresenceStore
	Authorizer() *authz.Authorizer
	Limits() config.Limits
	RequireAuth() bool
	Heartbeat() *HeartbeatManager

	// 连接与订阅编排
	AddClient(c *Session) error
	AddSubscription(ctx context.Context, ch string, sub Subscriber) error
	RemoveSubscription(ch string, c *Session) error

	// 发布与频道策略
	Publish(ch string, pub *stream.Publication) (uint64, error)
	PublishTransient(ch string, pub *stream.Publication) error
	ChannelPolicy(ch string) channel.ChannelPolicy
	MaxMessageSize() int

	// 身份
	UserPrincipal(userID string) authz.Principal

	// presence 编排
	ShouldTrackPresence(ch string, ephemeral bool) bool
	PresenceJoin(ctx context.Context, ch string, c *Session)
	PresenceLeave(ctx context.Context, ch, sessionID, userID string, ephemeral bool)
	PresenceSnapshot(ctx context.Context, ch string) *clientpb.PresenceSnapshot

	// survey 编排
	Survey(ctx context.Context, channel string, payload []byte, timeout time.Duration) ([]*survey.SurveyResult, error)
	AddSurveyResponse(ctx context.Context, sessionID, requestID string, payload []byte, err error)
	CountMatchingSubscribers(ctx context.Context, ch string) (int, error)
	BuildClientSurveyResult(requestID, channel string, results []*survey.SurveyResult) *clientpb.SurveyResult

	// proxy
	FindProxy(channel, method string) proxy.Proxy
	ProxyRPC(ctx context.Context, channel, method string, req *proxy.RPCProxyRequest) (*proxy.RPCProxyResponse, error)
	GetRPCTimeout() time.Duration

	// cluster 编排
	SyncClusterSessionState(ctx context.Context, c *Session) error
	DeleteClusterSessionState(ctx context.Context, sessionID string) error
	AdjustClusterChannelSubscriptionsTimeout(channel string, delta int64)
	ResumeRemoteSession(ctx context.Context, c *Session, sessionID string) (*cluster.ClusterSessionSnapshot, bool, error)
	RestoreSessionSubscriptions(ctx context.Context, c *Session, subs []cluster.ClusterSubscriptionSnapshot) []RestoreFailure
	RestoreLocalSubscription(ctx context.Context, ch string, sub Subscriber) error
	RemoveLocalSubscriptionOnly(ch string, s *Session, updateMetrics bool) (bool, error)

	// recovery 编排
	StreamEpoch() string
	RecoverState(c *Session, subs []*clientpb.Subscription, snapshot *cluster.ClusterSessionSnapshot) clientpb.RecoverState
	StreamRecoveries(ctx context.Context, c *Session, in *clientpb.InboundMessage, subs []*clientpb.Subscription, snapshot *cluster.ClusterSessionSnapshot, path string)
}

// RestoreFailure 是一频道订阅恢复失败的结果(映射根包未导出的
// clusterRestoreFailure,跨包接口不能暴露未导出类型)。
type RestoreFailure struct {
	Channel string
	Err     error
}

// IdentitySnapshot is a consistent read of the session identity fields
// (PR-KA-D14 §3.3). Taken under RLock.
type IdentitySnapshot struct {
	SessionID     string
	UserID        string
	ClientID      string
	Protocol      string
	Authenticated bool
	ConnectedAt   time.Time
	LastActivity  time.Time
	LeaseVersion  uint64
}

// ConnectedAt returns the session's connect timestamp.
func (s *Session) ConnectedAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.connectedAt
}

// SnapshotIdentity returns a consistent copy of the identity fields.
func (s *Session) SnapshotIdentity() IdentitySnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return IdentitySnapshot{
		SessionID:     s.session,
		UserID:        s.user,
		ClientID:      s.client,
		Protocol:      s.protocol,
		Authenticated: s.authenticated,
		ConnectedAt:   s.connectedAt,
		LastActivity:  s.lastActivity,
		LeaseVersion:  s.clusterLeaseVersion,
	}
}

// SubscribedChannels returns a copy of the session's subscribed channel set.
func (s *Session) SubscribedChannels() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	channels := make([]string, 0, len(s.subscribedChannels))
	for ch := range s.subscribedChannels {
		channels = append(channels, ch)
	}
	return channels
}

// TrackChannel records ch on the session. Returns true (and does not write)
// when the session is already closed — the subscribe saga must abort so
// Close's snapshot cannot miss the channel.
func (s *Session) TrackChannel(ch string) (closed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == SessionClosed {
		return true
	}
	s.subscribedChannels[ch] = struct{}{}
	return false
}

// ForceTrackChannel records ch unconditionally (rollback / restore hydrate).
func (s *Session) ForceTrackChannel(ch string) {
	s.mu.Lock()
	s.subscribedChannels[ch] = struct{}{}
	s.mu.Unlock()
}

// UntrackChannel drops ch from the session's subscribed set.
func (s *Session) UntrackChannel(ch string) {
	s.mu.Lock()
	delete(s.subscribedChannels, ch)
	s.mu.Unlock()
}

// AdoptIdentity replaces the session ID triple, rebuilds subscribedChannels
// from subscriptions, and writes clusterLeaseVersion. Empty userID/clientID
// are left unchanged; a zero leaseVersion only fills 1 when the current
// value is still 0 (resume takeover, cluster_resume.go original).
func (s *Session) AdoptIdentity(sessionID, userID, clientID string, subscriptions []string, leaseVersion uint64) {
	s.mu.Lock()
	s.session = sessionID
	if userID != "" {
		s.user = userID
	}
	if clientID != "" {
		s.client = clientID
	}
	s.subscribedChannels = make(map[string]struct{}, len(subscriptions))
	for _, ch := range subscriptions {
		s.subscribedChannels[ch] = struct{}{}
	}
	if leaseVersion > 0 {
		s.clusterLeaseVersion = leaseVersion
	} else if s.clusterLeaseVersion == 0 {
		s.clusterLeaseVersion = 1
	}
	s.mu.Unlock()
}

// HasSubscription reports whether the session tracks ch. Exported so
// leave-root tests that called the unexported hasSubscription keep working
// after the package move (spec §3.6 "零命中" missed method call sites).
func (s *Session) HasSubscription(channel string) bool {
	return s.hasSubscription(channel)
}

// SubscriptionList is the exported form of subscriptionList for leave-root tests.
func (s *Session) SubscriptionList() []*clientpb.Subscription {
	return s.subscriptionList()
}

// Attachment returns the current transport binding (leave-root tests / Attach).
func (s *Session) Attachment() *Attachment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.attachment
}

// MarkAuthenticated sets the authenticated flag (leave-root tests used to
// write the unexported field under mu).
func (s *Session) MarkAuthenticated() {
	s.mu.Lock()
	s.authenticated = true
	s.mu.Unlock()
}

// SetUserIDForTest overwrites the user id (leave-root tests wrote s.user).
func (s *Session) SetUserIDForTest(userID string) {
	s.mu.Lock()
	s.user = userID
	s.mu.Unlock()
}

// SetClientIDForTest overwrites the client id (leave-root tests wrote s.client).
func (s *Session) SetClientIDForTest(clientID string) {
	s.mu.Lock()
	s.client = clientID
	s.mu.Unlock()
}

// Marshal is the exported form of marshal for leave-root tests.
func (s *Session) Marshal(msg any) ([]byte, error) { return s.marshal(msg) }

// HandleRPC is the exported form of handleRPC for leave-root tests.
func (s *Session) HandleRPC(ctx context.Context, in *clientpb.InboundMessage, rpcReq *clientpb.RpcRequest) error {
	return s.handleRPC(ctx, in, rpcReq)
}

// HandleUnsubscribe is the exported form of handleUnsubscribe for leave-root tests.
func (s *Session) HandleUnsubscribe(ctx context.Context, in *clientpb.InboundMessage, unsubscribe *clientpb.Unsubscribe) error {
	return s.handleUnsubscribe(ctx, in, unsubscribe)
}

// ThrottledClusterRefresh is the exported form of throttledClusterRefresh.
func (s *Session) ThrottledClusterRefresh() { s.throttledClusterRefresh() }

// SetLastClusterSyncNanoForTest writes lastClusterSyncNano (leave-root tests).
func (s *Session) SetLastClusterSyncNanoForTest(nano int64) { s.lastClusterSyncNano.Store(nano) }

// SetJitterForTest pins heartbeat jitter (heartbeat_test.go).
func (hm *HeartbeatManager) SetJitterForTest(fn func(time.Duration) time.Duration) {
	hm.jitter = fn
}

// Sessions returns a snapshot of hub sessions (ReplaceRules + node_test).
func (h *Hub) Sessions() []*Session {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*Session, 0, len(h.sessions))
	for _, s := range h.sessions {
		out = append(out, s)
	}
	return out
}

// newHub keeps the pre-export name for in-package tests (hub_test.go).
var newHub = NewHub

// positionFrom is a local copy of the root recover.go helper so this package
// does not import the root (import cycle). Same bytes as recover.go.
func positionFrom(epoch string, offset uint64, set bool) *sharedv2.Position {
	p := &sharedv2.Position{StreamEpoch: epoch}
	if set {
		off := offset
		p.Offset = &off
	}
	return p
}
