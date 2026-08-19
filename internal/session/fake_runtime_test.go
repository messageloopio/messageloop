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
	"github.com/messageloopio/messageloop/internal/stream"
	"github.com/messageloopio/messageloop/internal/survey"
	"github.com/messageloopio/messageloop/proxy"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	"github.com/prometheus/client_golang/prometheus"
)

// fakeRuntime is the in-package Runtime stub (PR-KA-D14 §3.6). Default
// methods are safe no-ops; Hub/Presence/Metrics are real so session_test
// and hub_test can exercise Close/Fence/Add without importing the root.
type fakeRuntime struct {
	hub         *Hub
	metrics     *metrics.Metrics
	presence    occupancy.PresenceStore
	authorizer  *authz.Authorizer
	limits      config.Limits
	requireAuth bool
	heartbeat   *HeartbeatManager
	maxSize     int

	deletedLease    bool
	deletedSnapshot bool
}

func newFakeRuntime() *fakeRuntime {
	auth, err := authz.NewAuthorizer(config.AuthorizerConfig{})
	if err != nil {
		panic(err)
	}
	return &fakeRuntime{
		hub:        NewHub(0, 0),
		metrics:    metrics.NewMetrics(prometheus.NewRegistry()),
		presence:   occupancy.NewMemoryPresenceStore(),
		authorizer: auth,
	}
}

func (f *fakeRuntime) Hub() *Hub                         { return f.hub }
func (f *fakeRuntime) Metrics() *metrics.Metrics         { return f.metrics }
func (f *fakeRuntime) Presence() occupancy.PresenceStore { return f.presence }
func (f *fakeRuntime) Authorizer() *authz.Authorizer     { return f.authorizer }
func (f *fakeRuntime) Limits() config.Limits             { return f.limits }
func (f *fakeRuntime) RequireAuth() bool                 { return f.requireAuth }
func (f *fakeRuntime) Heartbeat() *HeartbeatManager      { return f.heartbeat }

func (f *fakeRuntime) AddClient(c *Session) error { return f.hub.Add(c) }

func (f *fakeRuntime) AddSubscription(_ context.Context, ch string, sub Subscriber) error {
	if _, err := f.hub.AddSub(ch, sub); err != nil {
		return err
	}
	if sub.Session != nil && sub.Session.TrackChannel(ch) {
		_, _ = f.hub.RemoveSub(ch, sub.Session)
		return errSessionClosedForTrack
	}
	return nil
}

var errSessionClosedForTrack = errClosedClient

type errClosedClientT struct{}

func (errClosedClientT) Error() string { return "client is closed" }

var errClosedClient error = errClosedClientT{}

func (f *fakeRuntime) RemoveSubscription(ch string, c *Session) error {
	_, _ = f.hub.RemoveSub(ch, c)
	if c != nil {
		c.UntrackChannel(ch)
	}
	return nil
}

func (f *fakeRuntime) Publish(string, *stream.Publication) (uint64, error) { return 0, nil }
func (f *fakeRuntime) PublishTransient(string, *stream.Publication) error  { return nil }
func (f *fakeRuntime) ChannelPolicy(string) channel.ChannelPolicy {
	return channel.DefaultChannelPolicy()
}
func (f *fakeRuntime) MaxMessageSize() int { return f.maxSize }

func (f *fakeRuntime) UserPrincipal(userID string) authz.Principal {
	return authz.Principal{Kind: authz.PrincipalUser, UserID: userID}
}

func (f *fakeRuntime) ShouldTrackPresence(ch string, ephemeral bool) bool {
	return !ephemeral && !isWildcard(ch) && f.ChannelPolicy(ch).Presence
}

func (f *fakeRuntime) PresenceJoin(ctx context.Context, ch string, c *Session) {
	if c == nil || !f.ShouldTrackPresence(ch, false) {
		return
	}
	_ = f.presence.Add(ctx, ch, &occupancy.PresenceInfo{
		ClientID:        c.SessionID(),
		SessionID:       c.SessionID(),
		ConnectClientID: c.ClientID(),
		UserID:          c.UserID(),
		ConnectedAt:     c.ConnectedAt().UnixMilli(),
	})
}

func (f *fakeRuntime) PresenceLeave(ctx context.Context, ch, sessionID, _ string, ephemeral bool) {
	if !f.ShouldTrackPresence(ch, ephemeral) {
		return
	}
	_ = f.presence.Remove(ctx, ch, sessionID)
}

func (f *fakeRuntime) PresenceSnapshot(context.Context, string) *clientpb.PresenceSnapshot {
	return &clientpb.PresenceSnapshot{}
}

func (f *fakeRuntime) Survey(context.Context, string, []byte, time.Duration) ([]*survey.SurveyResult, error) {
	return nil, nil
}
func (f *fakeRuntime) AddSurveyResponse(context.Context, string, string, []byte, error) {}
func (f *fakeRuntime) CountMatchingSubscribers(context.Context, string) (int, error) {
	return 0, nil
}
func (f *fakeRuntime) BuildClientSurveyResult(string, string, []*survey.SurveyResult) *clientpb.SurveyResult {
	return &clientpb.SurveyResult{}
}

func (f *fakeRuntime) FindProxy(string, string) proxy.Proxy { return nil }
func (f *fakeRuntime) ProxyRPC(context.Context, string, string, *proxy.RPCProxyRequest) (*proxy.RPCProxyResponse, error) {
	return nil, nil
}
func (f *fakeRuntime) GetRPCTimeout() time.Duration { return 0 }

func (f *fakeRuntime) SyncClusterSessionState(context.Context, *Session) error { return nil }

func (f *fakeRuntime) DeleteClusterSessionState(_ context.Context, _ string) error {
	f.deletedLease = true
	f.deletedSnapshot = true
	return nil
}

func (f *fakeRuntime) AdjustClusterChannelSubscriptionsTimeout(string, int64) {}

func (f *fakeRuntime) ResumeRemoteSession(context.Context, *Session, string) (*cluster.ClusterSessionSnapshot, bool, error) {
	return nil, false, nil
}

func (f *fakeRuntime) RestoreSessionSubscriptions(context.Context, *Session, []cluster.ClusterSubscriptionSnapshot) []RestoreFailure {
	return nil
}

func (f *fakeRuntime) RestoreLocalSubscription(_ context.Context, ch string, sub Subscriber) error {
	if _, err := f.hub.AddSub(ch, sub); err != nil {
		return err
	}
	if sub.Session != nil {
		sub.Session.ForceTrackChannel(ch)
	}
	return nil
}

func (f *fakeRuntime) RemoveLocalSubscriptionOnly(ch string, s *Session, _ bool) (bool, error) {
	_, removed := f.hub.RemoveSub(ch, s)
	if removed && s != nil {
		s.UntrackChannel(ch)
	}
	return removed, nil
}

func (f *fakeRuntime) StreamEpoch() string { return "" }
func (f *fakeRuntime) RecoverState(*Session, []*clientpb.Subscription, *cluster.ClusterSessionSnapshot) clientpb.RecoverState {
	return clientpb.RecoverState_RECOVER_STATE_UNSPECIFIED
}
func (f *fakeRuntime) StreamRecoveries(context.Context, *Session, *clientpb.InboundMessage, []*clientpb.Subscription, *cluster.ClusterSessionSnapshot, string) {
}

// presenceJoin keeps the pre-D14 unexported name so session_test.go call
// sites stay byte-identical after the NewClient construction swap.
func (f *fakeRuntime) presenceJoin(ctx context.Context, ch string, c *Session) {
	f.PresenceJoin(ctx, ch, c)
}

var _ Runtime = (*fakeRuntime)(nil)
