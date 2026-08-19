package runtime

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/messageloopio/messageloop/config"
	"github.com/messageloopio/messageloop/internal/authz"
	"github.com/messageloopio/messageloop/internal/channel"
	"github.com/messageloopio/messageloop/internal/cluster"
	"github.com/messageloopio/messageloop/internal/metrics"
	"github.com/messageloopio/messageloop/internal/occupancy"
	"github.com/messageloopio/messageloop/internal/session"
	"github.com/messageloopio/messageloop/internal/stream"
	"github.com/messageloopio/messageloop/internal/survey"
	"github.com/messageloopio/messageloop/proxy"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
)

// nodeRuntime adapts *Node onto session.Runtime without exporting Node methods
// (PR-KA-D14 §3.4). Each accessor reads the live field so late injection
// (SetMetrics) is visible.
type nodeRuntime struct{ n *Node }

var _ session.Runtime = nodeRuntime{}

func (r nodeRuntime) Hub() *session.Hub { return r.n.hub }

func (r nodeRuntime) Metrics() *metrics.Metrics { return r.n.metrics }

func (r nodeRuntime) Presence() occupancy.PresenceStore { return r.n.presence }

func (r nodeRuntime) Authorizer() *authz.Authorizer { return r.n.authorizer }

func (r nodeRuntime) Limits() config.Limits { return r.n.limits }

func (r nodeRuntime) RequireAuth() bool { return r.n.requireAuth }

func (r nodeRuntime) Heartbeat() *session.HeartbeatManager { return r.n.heartbeatManager }

func (r nodeRuntime) AddClient(c *session.Session) error { return r.n.AddClient(c) }

func (r nodeRuntime) AddSubscription(ctx context.Context, ch string, sub session.Subscriber) error {
	return r.n.AddSubscription(ctx, ch, sub)
}

func (r nodeRuntime) RemoveSubscription(ch string, c *session.Session) error {
	return r.n.RemoveSubscription(ch, c)
}

func (r nodeRuntime) Publish(ch string, pub *stream.Publication) (uint64, error) {
	return r.n.Publish(ch, pub)
}

func (r nodeRuntime) PublishTransient(ch string, pub *stream.Publication) error {
	return r.n.PublishTransient(ch, pub)
}

func (r nodeRuntime) ChannelPolicy(ch string) channel.ChannelPolicy {
	return r.n.ChannelPolicy(ch)
}

func (r nodeRuntime) MaxMessageSize() int { return r.n.MaxMessageSize() }

func (r nodeRuntime) UserPrincipal(userID string) authz.Principal {
	return r.n.userPrincipal(userID)
}

func (r nodeRuntime) ShouldTrackPresence(ch string, ephemeral bool) bool {
	return r.n.shouldTrackPresence(ch, ephemeral)
}

func (r nodeRuntime) PresenceJoin(ctx context.Context, ch string, c *session.Session) {
	r.n.presenceJoin(ctx, ch, c)
}

func (r nodeRuntime) PresenceLeave(ctx context.Context, ch, sessionID, userID string, ephemeral bool) {
	r.n.presenceLeave(ctx, ch, sessionID, userID, ephemeral)
}

func (r nodeRuntime) PresenceSnapshot(ctx context.Context, ch string) *clientpb.PresenceSnapshot {
	return r.n.presenceSnapshot(ctx, ch)
}

func (r nodeRuntime) Survey(ctx context.Context, channel string, payload []byte, timeout time.Duration) ([]*survey.SurveyResult, error) {
	return r.n.Survey(ctx, channel, payload, timeout)
}

func (r nodeRuntime) AddSurveyResponse(ctx context.Context, sessionID, requestID string, payload []byte, err error) {
	r.n.AddSurveyResponse(ctx, sessionID, requestID, payload, err)
}

func (r nodeRuntime) CountMatchingSubscribers(ctx context.Context, ch string) (int, error) {
	return r.n.countMatchingSubscribers(ctx, ch)
}

func (r nodeRuntime) BuildClientSurveyResult(requestID, channel string, results []*survey.SurveyResult) *clientpb.SurveyResult {
	return r.n.buildClientSurveyResult(requestID, channel, results)
}

func (r nodeRuntime) FindProxy(channel, method string) proxy.Proxy {
	return r.n.FindProxy(channel, method)
}

func (r nodeRuntime) ProxyRPC(ctx context.Context, channel, method string, req *proxy.RPCProxyRequest) (*proxy.RPCProxyResponse, error) {
	return r.n.ProxyRPC(ctx, channel, method, req)
}

func (r nodeRuntime) GetRPCTimeout() time.Duration { return r.n.GetRPCTimeout() }

func (r nodeRuntime) SyncClusterSessionState(ctx context.Context, c *session.Session) error {
	return r.n.syncClusterSessionState(ctx, c)
}

func (r nodeRuntime) DeleteClusterSessionState(ctx context.Context, sessionID string) error {
	return r.n.deleteClusterSessionState(ctx, sessionID)
}

func (r nodeRuntime) AdjustClusterChannelSubscriptionsTimeout(channel string, delta int64) {
	r.n.adjustClusterChannelSubscriptionsTimeout(channel, delta)
}

func (r nodeRuntime) ResumeRemoteSession(ctx context.Context, c *session.Session, sessionID string) (*cluster.ClusterSessionSnapshot, bool, error) {
	return r.n.resumeRemoteSession(ctx, c, sessionID)
}

func (r nodeRuntime) RestoreSessionSubscriptions(ctx context.Context, c *session.Session, subs []cluster.ClusterSubscriptionSnapshot) []session.RestoreFailure {
	raw := r.n.restoreSessionSubscriptions(ctx, c, subs)
	out := make([]session.RestoreFailure, len(raw))
	for i, f := range raw {
		out[i] = session.RestoreFailure{Channel: f.channel, Err: f.err}
	}
	return out
}

func (r nodeRuntime) RestoreLocalSubscription(ctx context.Context, ch string, sub session.Subscriber) error {
	return r.n.restoreLocalSubscription(ctx, ch, sub)
}

func (r nodeRuntime) RemoveLocalSubscriptionOnly(ch string, s *session.Session, updateMetrics bool) (bool, error) {
	return r.n.removeLocalSubscriptionOnly(ch, s, updateMetrics)
}

func (r nodeRuntime) StreamEpoch() string { return r.n.streamEpoch() }

func (r nodeRuntime) RecoverState(c *session.Session, subs []*clientpb.Subscription, snapshot *cluster.ClusterSessionSnapshot) clientpb.RecoverState {
	return r.n.recoverState(c, subs, snapshot)
}

func (r nodeRuntime) StreamRecoveries(ctx context.Context, c *session.Session, in *clientpb.InboundMessage, subs []*clientpb.Subscription, snapshot *cluster.ClusterSessionSnapshot, path string) {
	r.n.streamRecoveries(ctx, c, in, subs, snapshot, path)
}

// NewClient is the root-package thin wrapper (PR-KA-D14 §3.4). Signature is
// unchanged so transports keep calling messageloop.NewClient.
func NewClient(ctx context.Context, node *Node, t Transport, marshaler Marshaler, opts ...ClientOption) (*Session, ClientCloseFunc, error) {
	return session.NewClient(ctx, nodeRuntime{node}, t, marshaler, opts...)
}

// Copies of tiny helpers that lived in hub.go so root files (node.go,
// recover.go) keep compiling without touching their bodies. Same bytes as
// the session-package originals (D11 local-copy precedent).

func index(s string, numBuckets int) int {
	if numBuckets == 1 {
		return 0
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(s))
	return int(hash.Sum64() % uint64(numBuckets))
}

func isWildcard(ch string) bool {
	return strings.Contains(ch, "*")
}

func publicationID(channel string, offset uint64) string {
	return fmt.Sprintf("%s-%d", channel, offset)
}

const broadcastParallelLimit = 64

// pingClusterRefreshInterval is a root copy of the session-package const so
// node.go (lease TTL formula) and client_fix_test.go keep compiling.
const pingClusterRefreshInterval = 10 * time.Second
