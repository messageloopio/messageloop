package messageloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop/pkg/topics"
	"github.com/messageloopio/messageloop/proxy"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	"github.com/samber/lo"
	"golang.org/x/time/rate"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func NewClient(ctx context.Context, node *Node, t Transport, marshaler Marshaler, opts ...ClientOption) (*Client, ClientCloseFunc, error) {
	client := &Client{
		ctx:                ctx,
		node:               node,
		transport:          t,
		session:            uuid.NewString(),
		marshaler:          marshaler,
		lastActivity:       time.Now(),
		connectedAt:        time.Now(),
		subscribedChannels: make(map[string]struct{}),
		surveyLimiter:      rate.NewLimiter(rate.Limit(1), 1),
	}

	// Apply options
	for _, opt := range opts {
		opt(client)
	}

	// Start heartbeat if configured
	if node.heartbeatManager != nil {
		node.heartbeatManager.Start(ctx, client)
	}

	return client, func() error {
		return client.close(Disconnect{})
	}, nil
}

// ClientOption is a functional option for Client
type ClientOption func(*Client)

func WithProtocol(protocol string) ClientOption {
	return func(c *Client) {
		c.protocol = protocol
	}
}

type ClientCloseFunc func() error

type ClientInfo struct {
	ClientID    string `json:"client_id"`
	SessionID   string `json:"session_id"`
	UserID      string `json:"user_id"`
	RemoteAddr  string `json:"remote_addr,omitempty"`
	Protocol    string `json:"protocol,omitempty"`
	UserAgent   string `json:"user_agent,omitempty"`
	ConnectedAt int64  `json:"connected_at,omitempty"`
}

type Client struct {
	mu            sync.RWMutex
	ctx           context.Context
	transport     Transport
	client        string // 客户端上传的
	session       string // 服务端生成
	user          string // 用户 ID
	status        status
	node          *Node
	marshaler     Marshaler
	authenticated bool

	// Connection metadata
	protocol    string // ws, grpc, or quic
	connectedAt time.Time

	// Heartbeat fields
	lastActivity    time.Time
	heartbeatCancel context.CancelFunc
	// pingDeadline is the one-shot timer armed after every outbound server
	// ping; it disconnects with 3511 when it fires unanswered (strategy B).
	// Guarded by mu. See heartbeat.go.
	pingDeadline *time.Timer
	// heartbeatDisconnectOnce makes the 3511 close idempotent: when the ping
	// deadline and the idle ticker race, exactly one caller issues the close
	// and counts heartbeat_idle_disconnects_total.
	heartbeatDisconnectOnce atomic.Bool

	// Tracks channels this client is subscribed to, for presence cleanup.
	subscribedChannels  map[string]struct{}
	clusterLeaseVersion uint64

	// Rate limiter for publish operations.
	publishLimiter *rate.Limiter

	// surveyInFlight guards against a second client survey while the first
	// worker is still collecting responses (KD-15: one survey per session).
	surveyInFlight atomic.Bool
	// surveyLimiter rate-limits client survey initiation: 1/s, burst 1.
	surveyLimiter *rate.Limiter

	// metricsCharged is set once AddClient has counted this connection in
	// ConnectionsTotal; close() only decrements the gauge when it is set.
	metricsCharged bool

	// lastClusterSyncNano is the UnixNano timestamp of the last presence /
	// cluster refresh triggered by a ping, used to throttle repeated syncs.
	lastClusterSyncNano atomic.Int64
}

func jsonLog(msg proto.Message) string {
	data, _ := ProtoJSONMarshaler.Marshal(msg)
	return string(data)
}

// MarshalJSONStruct marshals a structpb.Struct into JSON bytes.
// The structpb protobuf text format (fields:{...}) is not valid JSON, so
// payloads must go through AsMap before json.Marshal.
func MarshalJSONStruct(s *structpb.Struct) ([]byte, error) {
	return json.Marshal(s.AsMap())
}

func (c *Client) marshal(msg any) ([]byte, error) {
	return c.marshaler.Marshal(msg)
}

type status uint8

const (
	statusConnecting status = 1
	statusConnected  status = 2
	statusClosed     status = 3
)

func (c *Client) close(disconnect Disconnect) error {
	c.mu.Lock()
	if c.status == statusClosed {
		c.mu.Unlock()
		return nil
	}
	c.status = statusClosed
	if c.heartbeatCancel != nil {
		c.heartbeatCancel()
		c.heartbeatCancel = nil
	}
	channels := make([]string, 0, len(c.subscribedChannels))
	for ch := range c.subscribedChannels {
		channels = append(channels, ch)
	}
	sessionID := c.session
	userID := c.user
	metricsCharged := c.metricsCharged
	c.mu.Unlock()

	// Remove presence for all subscribed channels first, while the
	// subscriptions are still registered in the hub: ephemeral subscriptions
	// are identified this way and skipped (they never register presence or
	// publish join/leave events). Cleanup runs with bounded concurrency —
	// one goroutine per channel would explode on connections with thousands
	// of subscriptions (P1-A5).
	if len(channels) > 0 {
		const maxConcurrentPresence = 16
		presCtx := context.Background()
		work := make(chan string)
		var wg sync.WaitGroup
		for i := 0; i < maxConcurrentPresence; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for ch := range work {
					ephemeral := false
					if stored, ok := c.node.hub.LookupSubscriber(ch, c); ok {
						ephemeral = stored.Ephemeral
					}
					c.node.presenceLeave(presCtx, ch, sessionID, userID, ephemeral)
				}
			}()
		}
		for _, ch := range channels {
			work <- ch
		}
		close(work)
		wg.Wait()
	}

	// Remove local subscriptions before clearing hub state.
	if len(channels) > 0 {
		const maxConcurrentRemovals = 16
		work := make(chan string)
		var wg sync.WaitGroup
		for i := 0; i < maxConcurrentRemovals; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for ch := range work {
					if err := c.node.RemoveSubscription(ch, c); err != nil {
						log.WarnContext(context.Background(), "failed to remove subscription during close", "channel", ch, "session", sessionID, "error", err)
					}
				}
			}()
		}
		for _, ch := range channels {
			work <- ch
		}
		close(work)
		wg.Wait()
	}

	// Clean up session from hub. Only remove the hub entry (and the matching
	// cluster state) when this client still owns the session — a failed resume
	// or a takeover must not evict the session currently being served.
	if sessionID != "" {
		if c.node.hub.RemoveSessionIfMatches(sessionID, c) {
			if err := c.node.deleteClusterSessionState(context.Background(), sessionID); err != nil {
				log.WarnContext(context.Background(), "failed to delete cluster session state", "session", sessionID, "error", err)
			}
		}
	}

	if c.node.metrics != nil && metricsCharged {
		c.node.metrics.ConnectionsTotal.WithLabelValues(c.TransportLabel()).Dec()
	}

	// Notify proxy about disconnection
	p := c.node.FindProxy("", "disconnect")
	if p != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		disconnectedReq := &proxy.OnDisconnectedProxyRequest{
			SessionID: sessionID,
			Username:  userID,
		}
		_, _ = p.OnDisconnected(ctx, disconnectedReq) // Ignore error for notification
	}
	return c.transport.Close(disconnect)
}

// Close closes the client session with a disconnect reason.
// This is an exported method for use by external code.
func (c *Client) Close(disconnect Disconnect) error {
	return c.close(disconnect)
}

// closeQuiet silently closes the transport without removing subscriptions or publishing
// presence leave events. Used during session resumption where a new session takes over.
func (c *Client) closeQuiet() {
	c.mu.Lock()
	if c.status == statusClosed {
		c.mu.Unlock()
		return
	}
	c.status = statusClosed
	if c.heartbeatCancel != nil {
		c.heartbeatCancel()
		c.heartbeatCancel = nil
	}
	c.mu.Unlock()

	// Close transport silently — no presence cleanup, no hub removal
	_ = c.transport.Close(Disconnect{})
}

// MarkMetricsCharged records that AddClient succeeded, so close() only
// decrements the connection gauge for clients that were actually counted.
// If the client was already closed while AddClient was in flight, the gauge
// increment performed by AddClient is undone immediately instead of drifting.
func (c *Client) MarkMetricsCharged() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status == statusClosed {
		if c.node.metrics != nil {
			c.node.metrics.ConnectionsTotal.WithLabelValues(c.TransportLabel()).Dec()
		}
		return
	}
	c.metricsCharged = true
}

func (c *Client) ClientID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.client
}

func (c *Client) SessionID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.session
}

func (c *Client) UserID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.user
}

func (c *Client) Send(ctx context.Context, msg *clientpb.OutboundMessage) error {
	return c.write(ctx, msg)
}

func (c *Client) HandleMessage(ctx context.Context, in *clientpb.InboundMessage) error {
	c.mu.Lock()
	if c.status == statusClosed {
		c.mu.Unlock()
		return errors.New("client is closed")
	}
	// Reset activity while holding lock to prevent TOCTOU
	c.lastActivity = time.Now()
	// Any inbound frame answers an outstanding server ping (strategy B): stop
	// the ping deadline so business traffic keeps the connection alive — a
	// pong is not the only valid response.
	if c.pingDeadline != nil {
		c.pingDeadline.Stop()
		c.pingDeadline = nil
	}
	c.mu.Unlock()

	// Serialize the message body lazily: protojson.Marshal is expensive and
	// only needed when debug logging is actually enabled.
	if log.FromContext(ctx).Enabled(ctx, slog.LevelDebug) {
		log.DebugContext(ctx, "handling message", "message", jsonLog(in))
	}

	select {
	case <-c.ctx.Done():
		return nil
	default:
	}

	if err := c.handleMessage(ctx, in); err != nil {
		var dis Disconnect
		if errors.As(err, &dis) {
			_ = c.close(dis)
			return nil
		}
		_ = c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
			out.Envelope = &clientpb.OutboundMessage_Error{
				Error: &sharedpb.Error{
					Code:    "INTERNAL_ERROR",
					Type:    "server_error",
					Message: err.Error(),
				},
			}
		}))
		return err
	}
	return nil
}

func (c *Client) handleMessage(ctx context.Context, in *clientpb.InboundMessage) error {
	// Every inbound message except Connect requires an authenticated
	// session. Anonymous mode still authenticates through Connect (it simply
	// has no token), so this cannot reject anonymous clients.
	if _, isConnect := in.Envelope.(*clientpb.InboundMessage_Connect); !isConnect && !c.Authenticated() {
		return DisconnectInvalidToken
	}

	switch msg := in.Envelope.(type) {
	case *clientpb.InboundMessage_Connect:
		return c.handleConnect(ctx, in, msg.Connect)
	case *clientpb.InboundMessage_Publish:
		return c.handlePublish(ctx, in, msg.Publish)
	case *clientpb.InboundMessage_Subscribe:
		return c.handleSubscribe(ctx, in, msg.Subscribe)
	case *clientpb.InboundMessage_RpcRequest:
		return c.handleRPC(ctx, in, msg.RpcRequest)
	case *clientpb.InboundMessage_Unsubscribe:
		return c.handleUnsubscribe(ctx, in, msg.Unsubscribe)
	case *clientpb.InboundMessage_Ping:
		return c.handlePing(ctx, in, msg.Ping)
	case *clientpb.InboundMessage_Pong:
		return c.handlePong(ctx, in, msg.Pong)
	case *clientpb.InboundMessage_SubRefresh:
		return c.handleSubRefresh(ctx, in, msg.SubRefresh)
	case *clientpb.InboundMessage_SurveyRequest:
		return c.handleSurvey(ctx, in, msg.SurveyRequest)
	case *clientpb.InboundMessage_SurveyReply:
		return c.handleSurveyReply(ctx, in, msg.SurveyReply)
	case *clientpb.InboundMessage_PresenceQuery:
		return c.handlePresenceQuery(ctx, in, msg.PresenceQuery)
	default:
		// Unknown or empty envelope: reject instead of silently dropping.
		return DisconnectBadRequest
	}
}

const (
	SystemMethodAuthenticate = "$authenticate"
)

// pingClusterRefreshInterval throttles the presence / cluster state refresh
// triggered by client pings: pings arriving within this window only refresh
// lastActivity and are answered with a pong, avoiding a goroutine pair plus
// Redis round-trips per ping for malicious or chatty clients.
const pingClusterRefreshInterval = 10 * time.Second

func (c *Client) handleConnect(ctx context.Context, in *clientpb.InboundMessage, connect *clientpb.Connect) error {
	c.mu.RLock()
	authenticated := c.authenticated
	closed := c.status == statusClosed
	c.mu.RUnlock()

	if closed {
		return DisconnectConnectionClosed
	}

	if authenticated {
		return DisconnectBadRequest
	}

	// The requested session ID is staged before authentication so that the
	// takeover and state inheritance below can adopt it, but it is only kept
	// once authentication succeeds: an unauthenticated connect must not be
	// able to evict a session that is still being served. The auth proxy is
	// presented the original server-generated session ID instead of this
	// client-supplied value (it cannot be trusted before authentication).
	originalSessionID := c.session
	if connect.SessionId != "" {
		c.mu.Lock()
		c.session = connect.SessionId
		c.mu.Unlock()
	}

	// Proxy authentication - check if there's a proxy configured for authentication
	var p proxy.Proxy
	var authUser string
	if connect.Token != "" {
		p = c.node.FindProxy("", SystemMethodAuthenticate)
		if p == nil && c.node.requireAuth {
			// requireAuth is on but no proxy can verify the token: a non-empty
			// token must not bypass authentication.
			log.WarnContext(ctx, "authentication required but no auth proxy configured for token",
				"session", c.session)
			_ = c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
				out.Envelope = &clientpb.OutboundMessage_Error{
					Error: &sharedpb.Error{
						Code:    "AUTH_REQUIRED",
						Type:    "auth_error",
						Message: "authentication token is required",
					},
				}
			}))
			return DisconnectInvalidToken
		}
	} else if c.node.requireAuth {
		_ = c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
			out.Envelope = &clientpb.OutboundMessage_Error{
				Error: &sharedpb.Error{
					Code:    "AUTH_REQUIRED",
					Type:    "auth_error",
					Message: "authentication token is required",
				},
			}
		}))
		return DisconnectInvalidToken
	}
	if p != nil {
		authReq := &proxy.AuthenticateProxyRequest{
			ClientID:   connect.ClientId,
			Token:      connect.Token,
			ClientType: connect.ClientType,
			// The server-generated session ID, never the client-supplied one:
			// an unauthenticated client must not be able to feed an arbitrary
			// session ID to the authentication proxy (P2). The requested
			// session ID is only adopted after authentication succeeds.
			SessionID:  originalSessionID,
			RemoteAddr: c.transport.RemoteAddr(),
		}
		authResp, err := p.Authenticate(ctx, authReq)
		if err != nil {
			log.WarnContext(ctx, "proxy authentication failed", "error", err)
			_ = c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
				out.Envelope = &clientpb.OutboundMessage_Error{
					Error: &sharedpb.Error{
						Code:    "AUTH_ERROR",
						Type:    "auth_error",
						Message: err.Error(),
					},
				}
			}))
			return DisconnectInvalidToken
		}
		if authResp.Error != nil {
			log.WarnContext(ctx, "proxy authentication returned error", "error", authResp.Error)
			_ = c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
				out.Envelope = &clientpb.OutboundMessage_Error{
					Error: authResp.Error,
				}
			}))
			return DisconnectInvalidToken
		}
		// Store user info from proxy response
		if authResp.UserInfo != nil {
			authUser = authResp.UserInfo.ID
		}
	}

	// Resumption is only permitted when real authentication happened
	// (require_auth + a verified token via the auth proxy). In anonymous mode
	// a session id cannot be trusted — anyone could guess it — so it is
	// ignored and the connect starts a fresh session.
	resumeAllowed := c.node.requireAuth && p != nil
	if connect.SessionId != "" && !resumeAllowed {
		c.mu.Lock()
		c.session = originalSessionID
		c.mu.Unlock()
		log.WarnContext(ctx, "session takeover rejected: connect not authenticated, ignoring session id",
			"session", c.session, "provided_session", connect.SessionId)
	}

	// Check if this is a resumption attempt. Takeover and state inheritance
	// (writes to user/client/subscribedChannels) happen only after a successful
	// authentication, so a failed connect cannot evict or delete the session.
	resumed := false
	resumedLocal := false
	var resumeSnapshot *ClusterSessionSnapshot
	if connect.SessionId != "" && resumeAllowed {
		// Try to find the old session
		oldSession := c.node.hub.LookupSession(connect.SessionId)
		if oldSession != nil {
			resumed = true
			resumedLocal = true

			// 1. Copy state from the old session (no lock nesting: release
			// oldSession.mu before taking c.mu).
			oldSession.mu.Lock()
			oldChannels := make(map[string]struct{}, len(oldSession.subscribedChannels))
			for ch := range oldSession.subscribedChannels {
				oldChannels[ch] = struct{}{}
			}
			oldUser := oldSession.user
			oldClient := oldSession.client
			oldLeaseVersion := oldSession.clusterLeaseVersion
			oldMetricsCharged := oldSession.metricsCharged
			oldSession.mu.Unlock()

			// 2. Set inherited state on the new session
			c.mu.Lock()
			c.user = oldUser
			c.client = oldClient
			c.subscribedChannels = oldChannels
			// Transfer the connection gauge count from the old client: the old
			// client is closed quietly (no decrement) and the new client was
			// not counted by AddClient, so the gauge stays balanced.
			c.metricsCharged = oldMetricsCharged
			if oldLeaseVersion > 0 {
				c.clusterLeaseVersion = oldLeaseVersion + 1
			}
			c.mu.Unlock()

			// The authenticated user wins over the inherited one: apply it
			// before ReplaceSession so the per-user limit check inside the
			// hub sees the real user (a cross-user takeover must not bypass
			// maxConnsPerUser) and the session is registered in the
			// connShard of the user that will own it.
			if authUser != "" {
				c.mu.Lock()
				c.user = authUser
				c.mu.Unlock()
			}

			// 3. Silently close old session (no presence leave, no sub removal)
			oldSession.closeQuiet()

			// 4. Replace session references in hub (sessions map + subShards)
			if err := c.node.hub.ReplaceSession(connect.SessionId, c); err != nil {
				// Roll back the failed resume: the old session's transport is
				// already closed, so it must be fully evicted instead of
				// lingering as a zombie that keeps receiving broadcasts (its
				// subscriptions, presence and hub entry are cleaned up, plus
				// the cluster state).
				for ch := range oldChannels {
					ephemeral := false
					if stored, ok := c.node.hub.LookupSubscriber(ch, oldSession); ok {
						ephemeral = stored.Ephemeral
					}
					if rmErr := c.node.RemoveSubscription(ch, oldSession); rmErr != nil {
						log.WarnContext(ctx, "failed to remove subscription during resume rollback",
							"channel", ch, "session", connect.SessionId, "error", rmErr)
					}
					c.node.presenceLeave(ctx, ch, connect.SessionId, oldUser, ephemeral)
				}
				if c.node.hub.RemoveSessionIfMatches(connect.SessionId, oldSession) {
					if delErr := c.node.deleteClusterSessionState(context.Background(), connect.SessionId); delErr != nil {
						log.WarnContext(ctx, "failed to clean cluster session state after failed resume",
							"session", connect.SessionId, "error", delErr)
					}
				}
				return c.disconnectOnConnectError(ctx, err)
			}
		} else {
			var err error
			resumeSnapshot, resumed, err = c.node.resumeRemoteSession(ctx, c, connect.SessionId)
			if err != nil {
				return c.disconnectOnConnectError(ctx, err)
			}
		}
	}

	c.mu.Lock()
	c.authenticated = true
	// The authenticated user wins over the inherited one (matches the
	// pre-resume reordering semantics).
	if authUser != "" {
		c.user = authUser
	}
	if !resumed {
		c.client = connect.ClientId
	}
	if c.clusterLeaseVersion == 0 {
		c.clusterLeaseVersion = 1
	}
	if limit := c.node.limits.MaxPublishesPerSecond; limit > 0 {
		c.publishLimiter = rate.NewLimiter(rate.Limit(limit), limit)
	}
	c.mu.Unlock()

	if !resumed || !resumedLocal {
		if err := c.node.AddClient(c); err != nil {
			return c.disconnectOnConnectError(ctx, err)
		}
		// Only a client that passed AddClient is counted in ConnectionsTotal;
		// close() decrements the gauge solely for such clients.
		c.MarkMetricsCharged()
	} else if err := c.node.syncClusterSessionState(ctx, c); err != nil {
		return c.disconnectOnConnectError(ctx, err)
	}

	c.mu.Lock()
	// Only mark the connection connected when it is not closing: a
	// concurrent close() must not have its status resurrected by a connect
	// that is still in flight.
	if c.status != statusClosed {
		c.status = statusConnected
	}
	c.mu.Unlock()

	if resumeSnapshot != nil {
		if err := c.node.restoreSessionSubscriptions(ctx, c, resumeSnapshot.Subscriptions); err != nil {
			// Roll back the partially restored session: remove the hub
			// registration and the cluster lease/snapshot, then disconnect
			// the new connection. Without this the session lingers as a
			// zombie that cannot be resumed.
			c.node.hub.RemoveSession(c.SessionID())
			if delErr := c.node.deleteClusterSessionState(context.Background(), c.SessionID()); delErr != nil {
				log.WarnContext(ctx, "failed to clean cluster session state after restore failure",
					"session", c.SessionID(), "error", delErr)
			}
			return DisconnectStale
		}
	}

	// Notify proxy about client connection
	if p != nil {
		connectedReq := &proxy.OnConnectedProxyRequest{
			SessionID: c.session,
			Username:  connect.ClientId,
		}
		_, _ = p.OnConnected(ctx, connectedReq) // Ignore error for notification
	}

	// Process subscriptions and handle recovery
	subs := connect.Subscriptions
	var pubs []*clientpb.Publication
	addedChannels := make([]string, 0, len(subs))
	addedPresence := make([]string, 0, len(subs))

	// Get current broker epoch for recovery validation
	var currentEpoch string
	if epocher, ok := c.node.broker.(interface{ Epoch() string }); ok {
		currentEpoch = epocher.Epoch()
	}

	// Ordered recovery union (PR-03): ACL-passed request subscriptions first,
	// then snapshot-only channels from a resumed session. Snapshot-only
	// channels carry a synthetic recover subscription; the server-recorded
	// ChannelOffsets drives the actual cursor.
	recoverySubs := make([]*clientpb.Subscription, 0, len(subs))
	seenRecovery := make(map[string]struct{}, len(subs))

	for _, sub := range subs {
		// Per-channel ACL check. Denied channels get an error envelope and are
		// skipped; the connection stays up.
		if aclErr := c.checkSubscribeACL(ctx, in, sub); aclErr != nil {
			_ = c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
				out.Envelope = &clientpb.OutboundMessage_Error{
					Error: aclErr,
				}
			}))
			continue
		}

		if _, dup := seenRecovery[sub.Channel]; !dup {
			seenRecovery[sub.Channel] = struct{}{}
			recoverySubs = append(recoverySubs, sub)
		}

		alreadySubscribed := c.hasSubscription(sub.Channel)

		// Enforce the per-client subscription limit, counting only channels
		// that would actually be added: duplicates, ACL-denied channels, and
		// channels inherited from a resumed session do not count.
		if limit := c.node.limits.MaxSubscriptionsPerClient; limit > 0 && !alreadySubscribed {
			c.mu.RLock()
			currentCount := len(c.subscribedChannels)
			c.mu.RUnlock()
			if currentCount >= limit {
				// Roll back the channels already added in this round so the
				// hub is not left half-registered until close() runs.
				for _, channel := range addedChannels {
					_ = c.node.RemoveSubscription(channel, c)
				}
				for _, channel := range addedPresence {
					_ = c.node.presence.Remove(ctx, channel, c.session)
				}
				return DisconnectChannelLimit
			}
		}

		if err := c.node.AddSubscription(ctx, sub.Channel, NewSubscriber(c, sub.Ephemeral)); err != nil {
			for _, channel := range addedChannels {
				_ = c.node.RemoveSubscription(channel, c)
			}
			for _, channel := range addedPresence {
				_ = c.node.presence.Remove(ctx, channel, c.session)
			}
			return err
		}
		if !alreadySubscribed {
			addedChannels = append(addedChannels, sub.Channel)
			// Presence writers are gated by shouldTrackPresence: wildcard
			// patterns, ephemeral subscriptions and presence=false channels
			// never enter the store and never emit join events, so the
			// addedPresence rollback list only holds tracked channels.
			if c.node.shouldTrackPresence(sub.Channel, sub.Ephemeral) {
				addedPresence = append(addedPresence, sub.Channel)
				c.node.presenceJoin(ctx, sub.Channel, c)
			}
		}
	}

	// Cross-node resume: channels the snapshot subscribed but this Connect
	// request did not list are recovered too. They resume from the
	// server-recorded ChannelOffsets; a channel missing an offset is skipped
	// (never replayed from the beginning).
	if resumeSnapshot != nil {
		for _, snap := range resumeSnapshot.Subscriptions {
			if _, dup := seenRecovery[snap.Channel]; dup {
				continue
			}
			seenRecovery[snap.Channel] = struct{}{}
			recoverySubs = append(recoverySubs, &clientpb.Subscription{
				Channel: snap.Channel,
				Recover: true,
				Offset:  0,
			})
		}
	}

	// Recover every channel in the union through the shared helper: one
	// quota per Connect request, publications appended in union order
	// (request channels first, then snapshot-only channels).
	quota := newRecoverQuota()
	var results []*clientpb.RecoverResult
	recoveredAny := false
	truncatedAny := false
	for _, rs := range recoverySubs {
		rec := c.node.recoverSubscription(ctx, rs, resumeSnapshot, quota, "connect")
		pubs = append(pubs, rec.Publications...)
		results = append(results, rec.RecoverResult())
		if rec.Status == RecoverOK || rec.Status == RecoverTruncated || rec.Status == RecoverEpochReset {
			recoveredAny = true
		}
		if rec.Status == RecoverTruncated {
			truncatedAny = true
		}
	}

	return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Connected{
			Connected: &clientpb.Connected{
				SessionId:      c.SessionID(),
				Resumed:        resumed,
				Epoch:          currentEpoch,
				Publications:   pubs,
				Subscriptions:  c.subscriptionList(),
				Recovered:      recoveredAny,
				Truncated:      truncatedAny,
				RecoverResults: results,
				// One snapshot per currently subscribed tracked channel,
				// including channels restored from a resumed session.
				Presence: c.presenceSnapshots(ctx),
			},
		}
	}))
}

// presenceSnapshots builds the presence snapshots for every channel this
// session is currently subscribed to that is tracked for presence (exact,
// non-ephemeral, presence=true). Snapshot-only entries are omitted entirely
// rather than emitted as empty snapshots.
func (c *Client) presenceSnapshots(ctx context.Context) []*clientpb.PresenceSnapshot {
	var snapshots []*clientpb.PresenceSnapshot
	for _, sub := range c.subscriptionList() {
		ephemeral := false
		if stored, ok := c.node.hub.LookupSubscriber(sub.Channel, c); ok {
			ephemeral = stored.Ephemeral
		}
		if !c.node.shouldTrackPresence(sub.Channel, ephemeral) {
			continue
		}
		snapshots = append(snapshots, c.node.presenceSnapshot(ctx, sub.Channel))
	}
	return snapshots
}

// disconnectOnConnectError converts a non-terminal connect failure into a
// disconnect: a connect that fails mid-way must not leave a half-open
// connection that can neither serve traffic nor re-Connect (a second Connect
// would be rejected as BadRequest). Errors that are already Disconnect are
// returned unchanged so HandleMessage closes the connection with the original
// code (e.g. DisconnectStale for a failed resume claim); any other error
// closes the connection with DisconnectInternal.
func (c *Client) disconnectOnConnectError(ctx context.Context, err error) error {
	var dis Disconnect
	if errors.As(err, &dis) {
		return err
	}
	log.WarnContext(ctx, "connect failed, disconnecting client", "error", err)
	_ = c.close(DisconnectInternal)
	return err
}

// checkSubscribeACL evaluates the subscribe ACL for one channel and returns the
// error envelope to send to the client, or nil when the subscription is allowed.
func (c *Client) checkSubscribeACL(ctx context.Context, in *clientpb.InboundMessage, ch *clientpb.Subscription) *sharedpb.Error {
	p := c.node.FindProxy(ch.Channel, "subscribe")
	if p != nil {
		aclReq := &proxy.SubscribeAclProxyRequest{
			Channel:   ch.Channel,
			Token:     ch.Token,
			UserID:    c.user,
			SessionID: c.session,
		}
		aclResp, err := p.SubscribeAcl(ctx, aclReq)
		if err != nil {
			log.WarnContext(ctx, "proxy subscribe ACL check failed", "channel", ch.Channel, "error", err)
			return &sharedpb.Error{
				Code:    "ACL_ERROR",
				Type:    "acl_error",
				Message: err.Error(),
			}
		}
		if aclResp.Error != nil {
			log.WarnContext(ctx, "proxy subscribe ACL returned error", "channel", ch.Channel, "error", aclResp.Error)
			return aclResp.Error
		}
		return nil
	}
	if c.node.acl != nil {
		// Built-in ACL check (fallback when no proxy is configured)
		if !c.node.acl.CanSubscribe(ch.Channel, c.user) {
			log.WarnContext(ctx, "ACL denied subscribe", "channel", ch.Channel, "user", c.user)
			return &sharedpb.Error{
				Code:    "ACL_DENIED",
				Type:    "acl_error",
				Message: "subscribe denied by ACL rule",
			}
		}
	}
	return nil
}

func MakeOutboundMessage(in *clientpb.InboundMessage, bodyFunc func(out *clientpb.OutboundMessage)) *clientpb.OutboundMessage {
	var out *clientpb.OutboundMessage
	if in != nil {
		out = &clientpb.OutboundMessage{
			Id:   in.Id,
			Time: uint64(time.Now().UnixMilli()),
		}
	} else {
		out = &clientpb.OutboundMessage{
			Id:   uuid.New().String(),
			Time: uint64(time.Now().UnixMilli()),
		}
	}
	bodyFunc(out)
	return out
}

func (c *Client) ClientInfo() *ClientInfo {
	// c.client/c.session/c.user are written under c.mu by handleConnect and
	// the cluster resume path, so they must be read under the same lock.
	// connectedAt is immutable after construction and needs no protection.
	c.mu.RLock()
	info := &ClientInfo{
		ClientID:    c.client,
		SessionID:   c.session,
		UserID:      c.user,
		Protocol:    c.protocol,
		ConnectedAt: c.connectedAt.UnixMilli(),
	}
	c.mu.RUnlock()
	info.RemoteAddr = c.transport.RemoteAddr()
	return info
}

// TransportLabel returns the transport label value ("ws", "grpc", or "quic")
// for the connections metric. The protocol is fixed at construction
// (WithProtocol by the transport packages); anything unknown defaults to "ws".
func (c *Client) TransportLabel() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return MetricsTransportLabel(c.protocol)
}

func (c *Client) Authenticated() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.authenticated
}

func (c *Client) handleRPC(ctx context.Context, in *clientpb.InboundMessage, rpcReq *clientpb.RpcRequest) error {
	// Extract channel and method from RpcRequest
	channel := rpcReq.Channel
	method := rpcReq.Method

	if channel == "" {
		return errors.New("missing channel in RPC request")
	}

	// Apply RPC timeout from configuration or use default
	rpcTimeout := c.node.GetRPCTimeout()
	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	// Extract metadata
	var meta map[string]string
	if rpcReq.Metadata != nil {
		meta = rpcReq.Metadata.Entries
	}

	// Check if there's a proxy configured for this channel/method
	proxyReq := &proxy.RPCProxyRequest{
		ID:        in.Id,
		ClientID:  c.client,
		SessionID: c.session,
		UserID:    c.user,
		Channel:   channel,
		Method:    method,
		Payload:   rpcReq.Payload,
		Meta:      meta,
	}

	startTime := time.Now()
	proxyResp, err := c.node.ProxyRPC(rpcCtx, channel, method, proxyReq)
	duration := time.Since(startTime)

	if c.node.metrics != nil {
		c.node.metrics.RPCDuration.Observe(duration.Seconds())
	}

	if err != nil {
		// Check for timeout error
		if errors.Is(err, context.DeadlineExceeded) {
			log.WarnContext(ctx, "RPC request timeout",
				"channel", channel,
				"method", method,
				"timeout", rpcTimeout,
				"duration", duration,
			)
			return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
				out.Envelope = &clientpb.OutboundMessage_Error{
					Error: &sharedpb.Error{
						Code:    "RPC_TIMEOUT",
						Type:    "timeout",
						Message: fmt.Sprintf("RPC request timeout after %v", duration),
					},
				}
			}))
		}

		// No proxy configured - return echo behavior
		if errors.Is(err, proxy.ErrNoProxyFound) {
			return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
				out.Envelope = &clientpb.OutboundMessage_RpcReply{
					RpcReply: &clientpb.RpcReply{
						RequestId: in.Id,
						Payload:   rpcReq.Payload,
						Metadata:  rpcReq.Metadata,
					},
				}
			}))
		}

		// Proxy error - return error to client
		log.WarnContext(ctx, "RPC proxy error",
			"channel", channel,
			"method", method,
			"error", err,
			"duration", duration,
		)
		return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
			out.Envelope = &clientpb.OutboundMessage_Error{
				Error: &sharedpb.Error{
					Code:    "PROXY_ERROR",
					Type:    "proxy_error",
					Message: err.Error(),
				},
			}
		}))
	}

	// Log successful RPC
	log.DebugContext(ctx, "RPC request completed",
		"channel", channel,
		"method", method,
		"duration", duration,
	)

	return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		if proxyResp.Error != nil {
			out.Envelope = &clientpb.OutboundMessage_Error{
				Error: proxyResp.Error,
			}
		} else {
			out.Envelope = &clientpb.OutboundMessage_RpcReply{
				RpcReply: &clientpb.RpcReply{
					RequestId: in.Id,
					Payload:   proxyResp.Payload,
					Metadata:  &sharedpb.Metadata{Entries: proxyResp.Meta},
				},
			}
		}
	}))
}

func (c *Client) handlePublish(ctx context.Context, in *clientpb.InboundMessage, publish *clientpb.Publish) error {
	if !c.Authenticated() {
		// An unauthenticated publish is an auth problem, not a stale
		// (auth-timeout) connection: use the invalid-token code.
		return DisconnectInvalidToken
	}

	if c.publishLimiter != nil && !c.publishLimiter.Allow() {
		return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
			out.Envelope = &clientpb.OutboundMessage_Error{
				Error: &sharedpb.Error{
					Code:    "RATE_LIMITED",
					Type:    "rate_limit",
					Message: "publish rate limit exceeded",
				},
			}
		}))
	}

	channel := publish.Channel
	if channel == "" {
		return errors.New("missing channel in publish message")
	}

	// Proxy ACL check - check if there's a proxy configured for publish ACL
	p := c.node.FindProxy(channel, "publish")
	if p != nil {
		aclReq := &proxy.PublishAclProxyRequest{
			Channel:   channel,
			Token:     publish.Token,
			UserID:    c.user,
			SessionID: c.session,
		}
		aclResp, err := p.PublishAcl(ctx, aclReq)
		if err != nil {
			log.WarnContext(ctx, "proxy publish ACL check failed", "channel", channel, "error", err)
			return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
				out.Envelope = &clientpb.OutboundMessage_Error{
					Error: &sharedpb.Error{
						Code:    "ACL_ERROR",
						Type:    "acl_error",
						Message: err.Error(),
					},
				}
			}))
		}
		if aclResp.Error != nil {
			log.WarnContext(ctx, "proxy publish ACL returned error", "channel", channel, "error", aclResp.Error)
			return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
				out.Envelope = &clientpb.OutboundMessage_Error{
					Error: aclResp.Error,
				}
			}))
		}
	} else if c.node.acl != nil {
		// Built-in ACL check (fallback when no proxy is configured)
		if !c.node.acl.CanPublish(channel, c.user) {
			log.WarnContext(ctx, "ACL denied publish", "channel", channel, "user", c.user)
			return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
				out.Envelope = &clientpb.OutboundMessage_Error{
					Error: &sharedpb.Error{
						Code:    "ACL_DENIED",
						Type:    "acl_error",
						Message: "publish denied by ACL rule",
					},
				}
			}))
		}
	}

	// Extract data from Payload, preserving the original oneof variant.
	pub, err := PublicationFromPayload(in.Id, nil, publish.Payload)
	if err != nil {
		return err
	}
	if publish.Metadata != nil {
		pub.Metadata = publish.Metadata.Entries
	}

	pol := c.node.ChannelPolicy(channel)
	forceTransient := publish.Transient || pol.TransientOnly || !pol.History
	if forceTransient {
		// Channel policy forces transient delivery (e.g. game tick
		// channels): the publish must still succeed — no error, ack with
		// offset 0 — it just never writes history. Count the forced
		// conversions (a client-declared transient is not forced).
		if !publish.Transient && c.node.metrics != nil {
			c.node.metrics.ChannelPolicyTransientForced.Inc()
		}
		if err := c.node.PublishTransient(channel, pub); err != nil {
			return err
		}
		return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
			out.Envelope = &clientpb.OutboundMessage_PublishAck{
				PublishAck: &clientpb.PublishAck{
					Id:     in.Id,
					Offset: 0,
				},
			}
		}))
	}

	offset, err := c.node.Publish(channel, pub)
	if err != nil {
		return err
	}
	return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_PublishAck{
			PublishAck: &clientpb.PublishAck{
				Id:     in.Id,
				Offset: offset,
			},
		}
	}))
}

func (c *Client) handleSubscribe(ctx context.Context, in *clientpb.InboundMessage, sub *clientpb.Subscribe) error {
	subs := []*clientpb.Subscription{}
	addedChannels := make([]string, 0, len(sub.Subscriptions))
	addedPresence := make([]string, 0, len(sub.Subscriptions))

	// Get current broker epoch for recovery validation.
	var currentEpoch string
	if epocher, ok := c.node.broker.(interface{ Epoch() string }); ok {
		currentEpoch = epocher.Epoch()
	}
	// One quota shared by every channel in this Subscribe request.
	quota := newRecoverQuota()
	var pubs []*clientpb.Publication
	var results []*clientpb.RecoverResult

	for _, ch := range sub.Subscriptions {
		alreadySubscribed := c.hasSubscription(ch.Channel)
		// Proxy ACL check - check if there's a proxy configured for subscription ACL
		if aclErr := c.checkSubscribeACL(ctx, in, ch); aclErr != nil {
			_ = c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
				out.Envelope = &clientpb.OutboundMessage_Error{
					Error: aclErr,
				}
			}))
			continue
		}

		// Enforce the per-client subscription limit, counting only channels
		// that would actually be added: duplicates and ACL-denied channels
		// do not count toward the limit.
		if limit := c.node.limits.MaxSubscriptionsPerClient; limit > 0 && !alreadySubscribed {
			c.mu.RLock()
			currentCount := len(c.subscribedChannels)
			c.mu.RUnlock()
			if currentCount >= limit {
				return DisconnectChannelLimit
			}
		}

		if err := c.node.AddSubscription(ctx, ch.Channel, Subscriber{Client: c, Ephemeral: ch.Ephemeral}); err != nil {
			for _, channel := range addedChannels {
				if err := c.node.RemoveSubscription(channel, c); err != nil {
					log.WarnContext(ctx, "failed to rollback subscription", "channel", channel, "error", err)
				}
			}
			for _, channel := range addedPresence {
				_ = c.node.presence.Remove(ctx, channel, c.session)
			}
			return err
		}
		if !alreadySubscribed {
			addedChannels = append(addedChannels, ch.Channel)
		}
		subs = append(subs, ch)

		// Track presence for tracked subscriptions only (shouldTrackPresence
		// excludes ephemeral, wildcard and presence=false channels): they
		// never publish a join event. A re-subscribe does not join again.
		if !alreadySubscribed && c.node.shouldTrackPresence(ch.Channel, ch.Ephemeral) {
			addedPresence = append(addedPresence, ch.Channel)
			c.node.presenceJoin(ctx, ch.Channel, c)
		}

		// Notify proxy about subscription
		if p := c.node.FindProxy(ch.Channel, "subscribe"); p != nil {
			subscribedReq := &proxy.OnSubscribedProxyRequest{
				SessionID: c.session,
				Channel:   ch.Channel,
				Username:  c.user,
			}
			_, _ = p.OnSubscribed(ctx, subscribedReq) // Ignore error for notification
		}

		// Recover through the shared helper (PR-03): every ACL-passed,
		// successfully subscribed channel gets a result; a re-subscribe with
		// recover=true is a legitimate catch-up. The subscription stays even
		// when recovery fails.
		rec := c.node.recoverSubscription(ctx, ch, nil, quota, "subscribe")
		pubs = append(pubs, rec.Publications...)
		results = append(results, rec.RecoverResult())
	}
	return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_SubscribeAck{
			SubscribeAck: &clientpb.SubscribeAck{
				Subscriptions:  subs,
				Publications:   pubs,
				RecoverResults: results,
				Epoch:          currentEpoch,
				// Catch-up snapshot for every channel in this request that
				// is tracked for presence, including re-subscribes.
				Presence: c.snapshotForChannels(ctx, subs),
			},
		}
	}))
}

// snapshotForChannels builds presence snapshots for the requested channels,
// skipping wildcard, ephemeral and presence=false subscriptions.
func (c *Client) snapshotForChannels(ctx context.Context, subs []*clientpb.Subscription) []*clientpb.PresenceSnapshot {
	var snapshots []*clientpb.PresenceSnapshot
	for _, sub := range subs {
		ephemeral := false
		if stored, ok := c.node.hub.LookupSubscriber(sub.Channel, c); ok {
			ephemeral = stored.Ephemeral
		}
		if !c.node.shouldTrackPresence(sub.Channel, ephemeral) {
			continue
		}
		snapshots = append(snapshots, c.node.presenceSnapshot(ctx, sub.Channel))
	}
	return snapshots
}

func (c *Client) write(ctx context.Context, msg proto.Message) error {
	// Serialize the message body lazily: protojson.Marshal is expensive and
	// only needed when debug logging is actually enabled.
	if log.FromContext(ctx).Enabled(ctx, slog.LevelDebug) {
		log.DebugContext(ctx, "sending message", "message", jsonLog(msg))
	}
	buf := getBuffer()
	defer putBuffer(buf)
	var err error
	*buf, err = c.marshaler.MarshalAppend((*buf)[:0], msg)
	if err != nil {
		return err
	}
	log.DebugContext(ctx, "message marshaled", "size", len(*buf))
	err = c.transport.Write(*buf)
	if err != nil {
		log.ErrorContext(ctx, "failed to write to transport", err)
		go func() { _ = c.close(DisconnectSlowConsumer) }()
	} else {
		log.DebugContext(ctx, "message written to transport successfully")
	}
	return err
}

func (c *Client) handleUnsubscribe(ctx context.Context, in *clientpb.InboundMessage, unsubscribe *clientpb.Unsubscribe) error {
	// Publish presence leave events with bounded concurrency: one goroutine
	// per channel would explode on a batch of thousands of unsubscribes.
	const maxConcurrentPresenceEvents = 16
	sem := make(chan struct{}, maxConcurrentPresenceEvents)
	var wg sync.WaitGroup
	for _, sub := range unsubscribe.Subscriptions {
		alreadySubscribed := c.hasSubscription(sub.Channel)
		// The unsubscribe request carries no ephemeral flag, so the stored
		// subscription decides: ephemeral subscriptions never registered
		// presence and must not publish a leave event.
		ephemeral := false
		if stored, ok := c.node.hub.LookupSubscriber(sub.Channel, c); ok {
			ephemeral = stored.Ephemeral
		}
		// Remove subscription
		_ = c.node.RemoveSubscription(sub.Channel, c)

		// Remove presence and publish leave only when this subscription was
		// actually tracked (shouldTrackPresence excludes ephemeral, wildcard
		// and presence=false channels). The unsubscribe request carries no
		// ephemeral flag, so the stored subscription decides.
		if alreadySubscribed && c.node.shouldTrackPresence(sub.Channel, ephemeral) {
			sem <- struct{}{}
			wg.Add(1)
			go func(channel string) {
				defer func() {
					<-sem
					wg.Done()
				}()
				c.node.presenceLeave(ctx, channel, c.session, c.user, ephemeral)
			}(sub.Channel)
		}

		// Notify proxy about unsubscription
		p := c.node.FindProxy(sub.Channel, "unsubscribe")
		if p != nil {
			unsubscribedReq := &proxy.OnUnsubscribedProxyRequest{
				SessionID: c.session,
				Channel:   sub.Channel,
				Username:  c.user,
			}
			_, _ = p.OnUnsubscribed(ctx, unsubscribedReq) // Ignore error for notification
		}
	}
	wg.Wait()
	return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_UnsubscribeAck{
			UnsubscribeAck: &clientpb.UnsubscribeAck{
				Subscriptions: unsubscribe.Subscriptions,
			},
		}
	}))
}

func (c *Client) handlePing(ctx context.Context, in *clientpb.InboundMessage, ping *clientpb.Ping) error {
	c.ResetActivity()
	c.throttledClusterRefresh()

	log.DebugContext(ctx, "received ping, sending pong", "message_id", in.Id)
	err := c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Pong{
			Pong: &clientpb.Pong{},
		}
	}))
	if err != nil {
		log.ErrorContext(ctx, "failed to send pong", err)
	} else {
		log.DebugContext(ctx, "pong sent successfully", "message_id", in.Id)
	}
	return err
}

// handlePong handles inbound pong replies to server-initiated pings. No
// reply is sent: the pong itself only proves liveness. It must run the same
// throttled presence / cluster refresh as handlePing — a client that only
// answers server pings (and never sends its own) would otherwise let the
// Redis session lease and presence member TTLs expire.
func (c *Client) handlePong(ctx context.Context, in *clientpb.InboundMessage, pong *clientpb.Pong) error {
	c.ResetActivity()
	c.throttledClusterRefresh()
	return nil
}

// throttledClusterRefresh runs the expensive presence/cluster refresh work
// at most once per pingClusterRefreshInterval. The CAS guard makes sure only
// one caller wins the window. Shared by handlePing and handlePong so the
// two liveness paths refresh identically.
func (c *Client) throttledClusterRefresh() {
	now := time.Now().UnixNano()
	if last := c.lastClusterSyncNano.Load(); now-last >= int64(pingClusterRefreshInterval) &&
		c.lastClusterSyncNano.CompareAndSwap(last, now) {
		go c.refreshPresence()
		go func() {
			clusterCtx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
			defer cancel()
			if err := c.node.syncClusterSessionState(clusterCtx, c); err != nil {
				log.WarnContext(clusterCtx, "failed to refresh cluster session state", "session", c.session, "error", err)
			}
		}()
	}
}

func (c *Client) handleSubRefresh(ctx context.Context, in *clientpb.InboundMessage, refresh *clientpb.SubRefresh) error {
	// Publish presence leave events with bounded concurrency, same as
	// handleUnsubscribe: one goroutine per revoked channel would explode on a
	// batch of thousands of channels.
	const maxConcurrentPresenceEvents = 16
	sem := make(chan struct{}, maxConcurrentPresenceEvents)
	var wg sync.WaitGroup
	for _, ch := range refresh.Channels {
		p := c.node.FindProxy(ch, "subscribe")
		if p == nil {
			continue
		}
		aclReq := &proxy.SubscribeAclProxyRequest{Channel: ch}
		aclResp, err := p.SubscribeAcl(ctx, aclReq)
		if err != nil || aclResp.Error != nil {
			// ACL check failed — revoke subscription for this channel.
			ephemeral := false
			if stored, ok := c.node.hub.LookupSubscriber(ch, c); ok {
				ephemeral = stored.Ephemeral
			}
			_ = c.node.RemoveSubscription(ch, c)
			if c.node.shouldTrackPresence(ch, ephemeral) {
				sem <- struct{}{}
				wg.Add(1)
				go func(channel string) {
					defer func() {
						<-sem
						wg.Done()
					}()
					c.node.presenceLeave(ctx, channel, c.session, c.user, ephemeral)
				}(ch)
			}
		}
	}
	wg.Wait()
	return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_SubRefreshAck{
			SubRefreshAck: &clientpb.SubRefreshAck{},
		}
	}))
}

// handleSurvey starts a client-initiated survey on an exact channel. Every
// rejection path sends a top-level error envelope (no disconnect, no
// subscription revocation) and returns; the accepted path never blocks the
// read loop — the session is marked in-flight and a worker goroutine runs
// Node.Survey asynchronously, then sends the aggregated SurveyResult
// (KD-15: the initiator's own SurveyReply arrives as the next inbound frame
// on the same connection, so waiting here would deadlock the read loop).
func (c *Client) handleSurvey(ctx context.Context, in *clientpb.InboundMessage, req *clientpb.SurveyRequest) error {
	ch := req.GetChannel()
	if ch == "" || isWildcard(ch) {
		return c.sendSurveyError(ctx, in, "BAD_REQUEST", "request_error", "survey channel must be an exact channel")
	}
	if !c.sessionCoversChannel(ch) {
		return c.sendSurveyError(ctx, in, "PERMISSION_DENIED", "acl_error", "survey denied: channel not covered by session")
	}
	pol := c.node.ChannelPolicy(ch)
	if !pol.Survey {
		return c.sendSurveyError(ctx, in, "SURVEY_DISABLED", "policy_error", "survey disabled by channel policy")
	}
	if c.node.acl != nil && !c.node.acl.CanSurvey(ch, c.user) {
		return c.sendSurveyError(ctx, in, "PERMISSION_DENIED", "acl_error", "survey denied by ACL rule")
	}
	if !c.surveyInFlight.CompareAndSwap(false, true) {
		return c.sendSurveyError(ctx, in, "RATE_LIMITED", "rate_limit", "a survey is already in flight for this session")
	}
	if !c.surveyLimiter.Allow() {
		c.surveyInFlight.Store(false)
		return c.sendSurveyError(ctx, in, "RATE_LIMITED", "rate_limit", "survey rate limit exceeded")
	}

	var payload []byte
	if req.Payload != nil {
		pub, err := PublicationFromPayload("", nil, req.Payload)
		if err != nil {
			c.surveyInFlight.Store(false)
			return err
		}
		payload = pub.Payload
	}

	// timeout = clamp(req.TimeoutMs, 100ms, min(policy.MaxSurveyTimeout||5s, 10s)).
	// TimeoutMs <= 0 uses the policy cap (5s default).
	timeout := pol.MaxSurveyTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if timeout > 10*time.Second {
		timeout = 10 * time.Second
	}
	if req.TimeoutMs > 0 {
		requested := time.Duration(req.TimeoutMs) * time.Millisecond
		if requested > timeout {
			requested = timeout
		}
		if requested < 100*time.Millisecond {
			requested = 100 * time.Millisecond
		}
		timeout = requested
	}

	// Fast path: the local subscriber set already exceeds the cap, so the
	// survey can never run — reject synchronously with zero outbound
	// SurveyRequests. The worker re-checks the cluster-wide count.
	if limit := pol.MaxSurveySubscribers; limit > 0 && len(c.node.hub.GetMatchingSubscribers(ch)) > limit {
		c.surveyInFlight.Store(false)
		return c.sendSurveyError(ctx, in, "SURVEY_TOO_MANY_SUBSCRIBERS", "survey_error", "survey refused: too many subscribers")
	}

	requestID := req.RequestId
	if requestID == "" {
		requestID = uuid.NewString()
	}
	c.runSurveyWorker(requestID, ch, payload, timeout)
	return nil
}

// sendSurveyError sends a top-level error envelope for a rejected client
// survey, counts it in survey_client_total, and returns nil (no disconnect).
func (c *Client) sendSurveyError(ctx context.Context, in *clientpb.InboundMessage, code, errType, message string) error {
	if c.node.metrics != nil {
		c.node.metrics.SurveyClientTotal.WithLabelValues(code).Inc()
	}
	return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Error{
			Error: &sharedpb.Error{Code: code, Type: errType, Message: message},
		}
	}))
}

// sendSurveyTopError is the worker-side twin of sendSurveyError for
// asynchronously discovered failures (no inbound message id to echo).
func (c *Client) sendSurveyTopError(code, errType, message string) {
	if c.node.metrics != nil {
		c.node.metrics.SurveyClientTotal.WithLabelValues(code).Inc()
	}
	_ = c.Send(c.ctx, MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Error{
			Error: &sharedpb.Error{Code: code, Type: errType, Message: message},
		}
	}))
}

// runSurveyWorker runs the survey off the read loop: cluster-wide subscriber
// count preflight, Node.Survey (local + cluster aggregation), answer
// truncation, then the outbound SurveyResult. The in-flight flag is cleared
// when the worker finishes.
func (c *Client) runSurveyWorker(requestID, channel string, payload []byte, timeout time.Duration) {
	go func() {
		defer c.surveyInFlight.Store(false)
		ctx := c.ctx

		total, err := c.node.countMatchingSubscribers(ctx, channel)
		if err != nil {
			log.WarnContext(ctx, "survey subscriber count failed", "channel", channel, "error", err)
			c.sendSurveyTopError("INTERNAL_ERROR", "server_error", "survey subscriber count failed: "+err.Error())
			return
		}
		if limit := c.node.ChannelPolicy(channel).MaxSurveySubscribers; limit > 0 && total > limit {
			c.sendSurveyTopError("SURVEY_TOO_MANY_SUBSCRIBERS", "survey_error", "survey refused: too many subscribers")
			return
		}

		results, err := c.node.Survey(ctx, channel, payload, timeout)
		if err != nil {
			log.WarnContext(ctx, "survey execution failed", "channel", channel, "error", err)
			c.sendSurveyTopError("INTERNAL_ERROR", "server_error", err.Error())
			return
		}
		if c.node.metrics != nil {
			c.node.metrics.SurveyClientTotal.WithLabelValues("ok").Inc()
		}
		result := c.node.buildClientSurveyResult(requestID, channel, results)
		if err := c.Send(ctx, MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
			out.Envelope = &clientpb.OutboundMessage_SurveyResult{SurveyResult: result}
		})); err != nil {
			log.WarnContext(ctx, "failed to send survey result", "request_id", requestID, "error", err)
		}
	}()
}

// handleSurveyReply handles incoming survey replies from clients.
// This is called when a client sends a SurveyReply back to the server.
func (c *Client) handleSurveyReply(ctx context.Context, in *clientpb.InboundMessage, reply *clientpb.SurveyReply) error {
	c.ResetActivity()

	// Extract payload from the survey reply
	var payload []byte
	var err error
	if reply.Error != nil {
		err = fmt.Errorf("%s: %s", reply.Error.Code, reply.Error.Message)
	}
	if reply.Payload != nil {
		pub, convErr := PublicationFromPayload("", nil, reply.Payload)
		if convErr != nil {
			return convErr
		}
		payload = pub.Payload
		// Legacy behavior: a successfully converted JSON payload resets the
		// reply error (the old inline code reused one variable for both);
		// kept for exact equivalence with the pre-refactor semantics.
		if pub.Kind == PayloadKindJSON {
			err = nil
		}
	}

	// The reply must carry its own request ID: it is the server-generated
	// survey id from the outbound SurveyRequest (or the initiator's id for
	// its own survey), and AddSurveyResponse routes on it.
	if reply.RequestId == "" {
		log.WarnContext(ctx, "survey reply without request id dropped", "session", c.session)
		return nil
	}

	// Add the response to the survey (if the survey is still active)
	c.node.AddSurveyResponse(ctx, c.session, reply.RequestId, payload, err)

	return nil
}

// Heartbeat-related methods

// setHeartbeatCancel sets the heartbeat cancel function.
func (c *Client) setHeartbeatCancel(cancel context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.heartbeatCancel = cancel
}

// ResetActivity resets the last activity timestamp to now.
func (c *Client) ResetActivity() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastActivity = time.Now()
}

// ForceTestIDs overrides the session, user, and client IDs for testing
// purposes. It also marks the client authenticated so test clients that are
// wired directly (bypassing Connect) can still exercise message handlers.
func (c *Client) ForceTestIDs(sessionID, userID, clientID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.session = sessionID
	c.user = userID
	c.client = clientID
	c.authenticated = true
	if c.clusterLeaseVersion == 0 {
		c.clusterLeaseVersion = 1
	}
}

func (c *Client) hasSubscription(channel string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.subscribedChannels[channel]
	return ok
}

func (c *Client) subscriptionList() []*clientpb.Subscription {
	c.mu.RLock()
	channels := make([]string, 0, len(c.subscribedChannels))
	for channel := range c.subscribedChannels {
		channels = append(channels, channel)
	}
	c.mu.RUnlock()
	slices.Sort(channels)
	return lo.Map(channels, func(channel string, _ int) *clientpb.Subscription {
		return &clientpb.Subscription{Channel: channel}
	})
}

// sessionCoversChannel reports whether the session is subscribed to ch
// exactly or through a matching wildcard pattern. PresenceQuery requires
// coverage before serving a snapshot: a broad ACL must not let a session
// peek into channels it never subscribed to.
func (c *Client) sessionCoversChannel(ch string) bool {
	if c.hasSubscription(ch) {
		return true
	}
	for _, pattern := range c.subscriptionList() {
		if isWildcard(pattern.Channel) && topics.Match(pattern.Channel, ch) {
			return true
		}
	}
	return false
}

// handlePresenceQuery serves one PresenceQuery with the current presence
// snapshot. Rejections surface as top-level error envelopes without
// disconnecting: the subscription state is untouched.
func (c *Client) handlePresenceQuery(ctx context.Context, in *clientpb.InboundMessage, query *clientpb.PresenceQuery) error {
	ch := query.GetChannel()
	if ch == "" || isWildcard(ch) {
		return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
			out.Envelope = &clientpb.OutboundMessage_Error{
				Error: &sharedpb.Error{
					Code:    "BAD_REQUEST",
					Type:    "request_error",
					Message: "presence query channel must be an exact channel",
				},
			}
		}))
	}
	if !c.sessionCoversChannel(ch) {
		return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
			out.Envelope = &clientpb.OutboundMessage_Error{
				Error: &sharedpb.Error{
					Code:    "PERMISSION_DENIED",
					Type:    "acl_error",
					Message: "presence query denied: channel not covered by session",
				},
			}
		}))
	}
	if !c.node.ChannelPolicy(ch).Presence {
		return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
			out.Envelope = &clientpb.OutboundMessage_Error{
				Error: &sharedpb.Error{
					Code:    "POLICY_DENIED",
					Type:    "policy_error",
					Message: "presence query denied by channel policy",
				},
			}
		}))
	}
	if c.node.acl != nil && !c.node.acl.CanSubscribe(ch, c.user) {
		return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
			out.Envelope = &clientpb.OutboundMessage_Error{
				Error: &sharedpb.Error{
					Code:    "PERMISSION_DENIED",
					Type:    "acl_error",
					Message: "presence query denied by ACL rule",
				},
			}
		}))
	}
	return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Presence{
			Presence: c.node.presenceSnapshot(ctx, ch),
		}
	}))
}

// refreshPresence re-adds presence entries for all subscribed channels to reset TTL.
func (c *Client) refreshPresence() {
	c.mu.RLock()
	channels := make([]string, 0, len(c.subscribedChannels))
	for ch := range c.subscribedChannels {
		channels = append(channels, ch)
	}
	session := c.session
	user := c.user
	connAt := c.connectedAt.UnixMilli()
	c.mu.RUnlock()

	if len(channels) == 0 {
		return
	}
	info := &PresenceInfo{
		ClientID:        session,
		SessionID:       session,
		ConnectClientID: c.ClientID(),
		UserID:          user,
		ConnectedAt:     connAt,
	}
	for _, ch := range channels {
		// Ephemeral, wildcard and presence=false subscriptions never
		// register presence, so their TTL must not be refreshed here either.
		ephemeral := false
		if stored, ok := c.node.hub.LookupSubscriber(ch, c); ok {
			ephemeral = stored.Ephemeral
		}
		if !c.node.shouldTrackPresence(ch, ephemeral) {
			continue
		}
		_ = c.node.presence.Add(c.ctx, ch, info)
	}
}
