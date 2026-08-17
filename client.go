package messageloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop/pkg/topics"
	"github.com/messageloopio/messageloop/proxy"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
	"github.com/samber/lo"
	"golang.org/x/time/rate"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func NewClient(ctx context.Context, node *Node, t Transport, marshaler Marshaler, opts ...ClientOption) (*Session, ClientCloseFunc, error) {
	att := &Attachment{
		Transport: t,
		Marshaler: marshaler,
	}
	client := &Session{
		ctx:                ctx,
		node:               node,
		attachment:         att,
		loopAtt:            att,
		state:              SessionAuthenticating,
		out:                newSendQueue(),
		session:            uuid.NewString(),
		lastActivity:       time.Now(),
		connectedAt:        time.Now(),
		subscribedChannels: make(map[string]struct{}),
		surveyLimiter:      rate.NewLimiter(rate.Limit(1), 1),
	}

	// Apply options
	for _, opt := range opts {
		opt(client)
	}
	// The protocol travels with the attachment: Attach overwrites the
	// session protocol from the bound attachment, so the initial one must
	// carry the negotiated value too.
	client.attachment.Protocol = client.protocol

	// Start heartbeat if configured. It is restarted by Attach (a resume
	// replaces both the attachment and the connection context).
	if node.heartbeatManager != nil {
		node.heartbeatManager.Start(ctx, client)
	}

	// The close func is bound to this connection's attachment: when a local
	// resume replaces the attachment, the read loop of the superseded
	// connection must not tear the session down (it belongs to the resumed
	// session now).
	return client, func() error {
		return client.closeFromAttachment(att)
	}, nil
}

// ClientOption is a functional option for Client
type ClientOption func(*Session)

func WithProtocol(protocol string) ClientOption {
	return func(c *Session) {
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

func (c *Session) marshal(msg any) ([]byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.attachmentMarshalerLocked().Marshal(msg)
}

// MarkMetricsCharged records that AddClient succeeded, so Close only
// decrements the connection gauge for clients that were actually counted.
// If the client was already closed while AddClient was in flight, the gauge
// increment performed by AddClient is undone immediately instead of drifting.
func (c *Session) MarkMetricsCharged() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == SessionClosed {
		if c.node.metrics != nil {
			c.node.metrics.ConnectionsTotal.WithLabelValues(c.TransportLabel()).Dec()
		}
		return
	}
	c.metricsCharged = true
}

func (c *Session) ClientID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.client
}

func (c *Session) SessionID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.session
}

func (c *Session) UserID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.user
}

func (c *Session) Send(ctx context.Context, msg *clientpb.OutboundMessage) error {
	s := c.canonical()
	return s.enqueue(ctx, msg)
}

func (c *Session) HandleMessage(ctx context.Context, in *clientpb.InboundMessage) error {
	s := c.canonical()
	s.mu.Lock()
	if s.state == SessionClosed {
		s.mu.Unlock()
		return errors.New("session is closed")
	}
	// Reset activity while holding lock to prevent TOCTOU
	s.lastActivity = time.Now()
	// Any inbound frame answers an outstanding server ping (strategy B): stop
	// the ping deadline so business traffic keeps the connection alive — a
	// pong is not the only valid response.
	if s.pingDeadline != nil {
		s.pingDeadline.Stop()
		s.pingDeadline = nil
	}
	s.mu.Unlock()

	// Serialize the message body lazily: protojson.Marshal is expensive and
	// only needed when debug logging is actually enabled.
	if log.FromContext(ctx).Enabled(ctx, slog.LevelDebug) {
		log.DebugContext(ctx, "handling message", "message", jsonLog(in))
	}

	select {
	case <-s.ctx.Done():
		return nil
	default:
	}

	if err := s.handleMessage(ctx, in); err != nil {
		var dis Disconnect
		if errors.As(err, &dis) {
			c.closeFromLoop(dis)
			return nil
		}
		_ = s.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
			out.Envelope = &clientpb.OutboundMessage_Error{
				Error: &sharedv2.Error{
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

func (c *Session) handleMessage(ctx context.Context, in *clientpb.InboundMessage) error {
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

func (c *Session) handleConnect(ctx context.Context, in *clientpb.InboundMessage, connect *clientpb.Connect) error {
	c.mu.RLock()
	authenticated := c.authenticated
	closed := c.state == SessionClosed
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
					Error: &sharedv2.Error{
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
				Error: &sharedv2.Error{
					Code:    "AUTH_REQUIRED",
					Type:    "auth_error",
					Message: "authentication token is required",
				},
			}
		}))
		return DisconnectInvalidToken
	}
	if p != nil {
		// Copy the attachment under the lock: a concurrent Close may clear
		// it while the connect is in flight. A cleared attachment means the
		// session is closing, so the connect fails with the same code as the
		// closed-session check at the top of this function.
		c.mu.RLock()
		att := c.attachment
		c.mu.RUnlock()
		if att == nil || att.Transport == nil {
			return DisconnectConnectionClosed
		}
		authReq := &proxy.AuthenticateProxyRequest{
			ClientID:   connect.ClientId,
			Token:      connect.Token,
			ClientType: connect.ClientType,
			// The server-generated session ID, never the client-supplied one:
			// an unauthenticated client must not be able to feed an arbitrary
			// session ID to the authentication proxy (P2). The requested
			// session ID is only adopted after authentication succeeds.
			SessionID:  originalSessionID,
			RemoteAddr: att.Transport.RemoteAddr(),
		}
		authResp, err := p.Authenticate(ctx, authReq)
		if err != nil {
			log.WarnContext(ctx, "proxy authentication failed", "error", err)
			_ = c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
				out.Envelope = &clientpb.OutboundMessage_Error{
					Error: &sharedv2.Error{
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
					Error: sharedErrorV2(authResp.Error),
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
	// happen only after a successful authentication, so a failed connect
	// cannot evict or delete the session.
	resumed := false
	resumedLocal := false
	var resumeSnapshot *ClusterSessionSnapshot
	if connect.SessionId != "" && resumeAllowed {
		// Try to find the old session
		existing := c.node.hub.LookupSession(connect.SessionId)
		if existing != nil {
			resumed = true
			resumedLocal = true

			// Check the per-user connection limit BEFORE detaching the old
			// attachment (§6): a cross-user resume must not tear the old
			// session down when the target user has no slot left. On failure
			// the old session stays Attached and this connection is closed.
			if authUser != "" && authUser != existing.UserID() {
				if err := c.node.hub.PrepareSessionUser(connect.SessionId, existing, authUser); err != nil {
					return c.disconnectOnConnectError(ctx, err)
				}
			}

			// The authenticated user wins over the inherited one; the session
			// object is stable, so only the identity fields move (the
			// subscription state already lives on the resumed session).
			existing.mu.Lock()
			existing.user = authUser
			existing.ctx = ctx
			if existing.clusterLeaseVersion == 0 {
				existing.clusterLeaseVersion = 1
			} else {
				// Same-node resume bumps the local version; the cluster sync
				// below persists it (A1: same-fence Bind, version +1 is still
				// "this node").
				existing.clusterLeaseVersion++
			}
			existing.mu.Unlock()

			// Local takeover: tear off the old attachment, bind the new one.
			// Nothing is left, nothing is unbound, subscriptions are not
			// touched — the same Session object keeps serving.
			existing.Detach(Disconnect{})

			c.mu.RLock()
			tempAtt := c.attachment
			c.mu.RUnlock()
			if tempAtt == nil {
				return c.disconnectOnConnectError(ctx, errors.New("attach: session closed during connect"))
			}
			newAtt := &Attachment{
				Transport: tempAtt.Transport,
				Marshaler: tempAtt.Marshaler,
				Protocol:  tempAtt.Protocol,
			}
			if err := existing.Attach(newAtt); err != nil {
				// §5: an Attach failure after Detach is a real close — the
				// directory must not be held by a session with no attachment.
				_ = existing.Close(DisconnectInternal)
				return c.disconnectOnConnectError(ctx, err)
			}

			// The temporary Authenticating session never enters the hub: it
			// becomes a read-loop shell delegating to the resumed session.
			c.mu.Lock()
			c.delegate = existing
			c.attachment = nil
			c.stopHeartbeatLocked()
			if c.pingDeadline != nil {
				c.pingDeadline.Stop()
				c.pingDeadline = nil
			}
			c.mu.Unlock()

			return existing.finishConnect(ctx, in, connect, resumed, true, nil, p, authUser)
		} else {
			var err error
			resumeSnapshot, resumed, err = c.node.resumeRemoteSession(ctx, c, connect.SessionId)
			if err != nil {
				return c.disconnectOnConnectError(ctx, err)
			}
		}
	}

	// Bind the initial attachment: from here the session is Attached and the
	// writer goroutine starts draining the queue. The Connected frame is sent
	// by finishConnect once Connect completes (§5: Connected implies
	// Attached). The attachment is read under the lock: a concurrent Close
	// may have cleared it while the connect was in flight.
	if !resumedLocal {
		c.mu.RLock()
		att := c.attachment
		c.mu.RUnlock()
		if att == nil {
			return c.disconnectOnConnectError(ctx, errors.New("attach: session closed during connect"))
		}
		if err := c.Attach(att); err != nil {
			return c.disconnectOnConnectError(ctx, err)
		}
	}

	return c.finishConnect(ctx, in, connect, resumed, resumedLocal, resumeSnapshot, p, authUser)
}

// finishConnect completes a successful Connect on the canonical session: it
// registers the session, processes the requested subscriptions, performs
// recovery and sends the Connected envelope. For a local resume it runs on
// the resumed session object (the new connection's Authenticating session is
// only a shell by then).
func (c *Session) finishConnect(ctx context.Context, in *clientpb.InboundMessage, connect *clientpb.Connect, resumed, resumedLocal bool, resumeSnapshot *ClusterSessionSnapshot, p proxy.Proxy, authUser string) error {
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
		// Close decrements the gauge solely for such clients.
		c.MarkMetricsCharged()
	} else if err := c.node.syncClusterSessionState(ctx, c); err != nil {
		return c.disconnectOnConnectError(ctx, err)
	}

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

	// Process subscriptions and streamed recovery: every recover=true channel
	// is replayed through the shared Replayer after the bare Connected frame.
	subs := connect.Subscriptions
	addedChannels := make([]string, 0, len(subs))
	addedPresence := make([]string, 0, len(subs))

	// Get current broker epoch for recovery validation and Connected.
	currentEpoch := c.node.streamEpoch()

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
			// Unroutable patterns and malformed topics fail the single
			// channel softly: an error envelope, the channel is skipped (it
			// stays out of Connected.Subscriptions), the channels already
			// added in this Connect stay, and the Connect itself succeeds
			// (A3 §7).
			if errors.Is(err, ErrPatternNotRoutable) || errors.Is(err, topics.ErrBadTopic) {
				log.WarnContext(ctx, "connect initial subscription skipped",
					"channel", sub.Channel, "error", err)
				c.sendSubscribeRequestError(ctx, in, sub.Channel, err)
				continue
			}
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
			})
		}
	}

	// Send the bare Connected first (no publications, no recover results),
	// then the per-channel replay stream (§4.2). Presence snapshots travel as
	// individual presence envelopes right after the Connected frame (v2 has no
	// presence list on Connected).
	if err := c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Connected{
			Connected: &clientpb.Connected{
				SessionId:     c.SessionID(),
				Resumed:       resumed,
				StreamEpoch:   currentEpoch,
				Subscriptions: c.subscriptionList(),
				AcceptedCaps:  connect.Caps,
			},
		}
	})); err != nil {
		return err
	}
	for _, snap := range c.presenceSnapshots(ctx) {
		if err := c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
			out.Envelope = &clientpb.OutboundMessage_Presence{Presence: snap}
		})); err != nil {
			return err
		}
	}

	// One quota per Connect request shared by every channel in the union,
	// streamed in union order (request channels first, then snapshot-only).
	c.node.streamRecoveries(ctx, c, in, recoverySubs, resumeSnapshot, "connect")
	return nil
}

// presenceSnapshots builds the presence snapshots for every channel this
// session is currently subscribed to that is tracked for presence (exact,
// non-ephemeral, presence=true). Snapshot-only entries are omitted entirely
// rather than emitted as empty snapshots.
func (c *Session) presenceSnapshots(ctx context.Context) []*clientpb.PresenceSnapshot {
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
func (c *Session) disconnectOnConnectError(ctx context.Context, err error) error {
	var dis Disconnect
	if errors.As(err, &dis) {
		return err
	}
	log.WarnContext(ctx, "connect failed, disconnecting client", "error", err)
	_ = c.Close(DisconnectInternal)
	return err
}

// sendSubscribeRequestError sends a top-level request error envelope for a
// channel that could not be subscribed (unroutable pattern / bad topic). The
// connection stays up and every other channel is unaffected (A3 §7).
func (c *Session) sendSubscribeRequestError(ctx context.Context, in *clientpb.InboundMessage, channel string, err error) {
	code := "BAD_REQUEST"
	if errors.Is(err, ErrPatternNotRoutable) {
		code = "PATTERN_NOT_ROUTABLE"
	}
	_ = c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Error{
			Error: &sharedv2.Error{
				Code:    code,
				Type:    "request_error",
				Message: fmt.Sprintf("cannot subscribe to channel %q: %v", channel, err),
			},
		}
	}))
}

// checkSubscribeACL evaluates one subscription through the Authorizer and the
// proxy (PR-KA-A4 §8.1) and returns the error envelope to send to the client,
// or nil when the subscription is allowed. Order: routability, static Decide,
// then the proxy — a proxy approval must never bypass a static deny.
func (c *Session) checkSubscribeACL(ctx context.Context, in *clientpb.InboundMessage, ch *clientpb.Subscription) *sharedv2.Error {
	// 1. Routability before authorization: the subscription key must compile
	// on the live bus (A3). The same code pair as A3: PATTERN_NOT_ROUTABLE /
	// BAD_REQUEST, and the connection stays up.
	if _, err := CompileInterest(ch.Channel); err != nil {
		code := "BAD_REQUEST"
		if errors.Is(err, ErrPatternNotRoutable) {
			code = "PATTERN_NOT_ROUTABLE"
		}
		return &sharedv2.Error{
			Code:    code,
			Type:    "request_error",
			Message: fmt.Sprintf("cannot subscribe to channel %q: %v", ch.Channel, err),
		}
	}

	// 2. Static authorization: language inclusion against the authorizer
	// rules. This runs before the proxy, so a proxy that allows can never
	// punch a hole in a static deny.
	if dec := c.node.authorizer.Decide(c.node.userPrincipal(c.user), ActionSubscribePattern, ch.Channel); !dec.Allow {
		log.WarnContext(ctx, "ACL denied subscribe", "channel", ch.Channel, "user", c.user, "reason", dec.Reason)
		return &sharedv2.Error{
			Code:    "ACL_DENIED",
			Type:    "acl_error",
			Message: "subscribe denied by ACL rule",
		}
	}

	// 3. Proxy: an additional gate asked only when a route matches. The
	// proxy may reject this single request, but its approval does not
	// replace step 2 (no TOCTOU into AllowLang).
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
			return &sharedv2.Error{
				Code:    "ACL_ERROR",
				Type:    "acl_error",
				Message: err.Error(),
			}
		}
		if aclResp.Error != nil {
			log.WarnContext(ctx, "proxy subscribe ACL returned error", "channel", ch.Channel, "error", aclResp.Error)
			return sharedErrorV2(aclResp.Error)
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

// sharedErrorV2 converts a shared.v1 error (auth/ACL proxies still speak
// protocol/proxy/v1) into the client-v2 wire error the runtime now emits.
func sharedErrorV2(e *sharedpb.Error) *sharedv2.Error {
	if e == nil {
		return nil
	}
	return &sharedv2.Error{
		Code:    e.GetCode(),
		Type:    e.GetType(),
		Message: e.GetMessage(),
	}
}

// payloadV2toV1 converts a client-v2 payload into the shared.v1 payload the
// proxy RPC path still consumes. The shapes are identical except for the
// package path.
func payloadV2toV1(p *sharedv2.Payload) *sharedpb.Payload {
	if p == nil {
		return nil
	}
	out := &sharedpb.Payload{ContentType: p.GetContentType()}
	switch d := p.Data.(type) {
	case *sharedv2.Payload_Json:
		out.Data = &sharedpb.Payload_Json{Json: d.Json}
	case *sharedv2.Payload_Binary:
		out.Data = &sharedpb.Payload_Binary{Binary: d.Binary}
	case *sharedv2.Payload_Text:
		out.Data = &sharedpb.Payload_Text{Text: d.Text}
	}
	return out
}

// payloadV1toV2 converts a shared.v1 payload (proxy / admin responses) into
// the client-v2 payload the runtime now emits.
func payloadV1toV2(p *sharedpb.Payload) *sharedv2.Payload {
	if p == nil {
		return nil
	}
	out := &sharedv2.Payload{ContentType: p.GetContentType()}
	switch d := p.Data.(type) {
	case *sharedpb.Payload_Json:
		out.Data = &sharedv2.Payload_Json{Json: d.Json}
	case *sharedpb.Payload_Binary:
		out.Data = &sharedv2.Payload_Binary{Binary: d.Binary}
	case *sharedpb.Payload_Text:
		out.Data = &sharedv2.Payload_Text{Text: d.Text}
	}
	return out
}

func (c *Session) ClientInfo() *ClientInfo {
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
	c.mu.RLock()
	att := c.attachment
	c.mu.RUnlock()
	if att != nil && att.Transport != nil {
		info.RemoteAddr = att.Transport.RemoteAddr()
	}
	return info
}

func (c *Session) Authenticated() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.authenticated
}

func (c *Session) handleRPC(ctx context.Context, in *clientpb.InboundMessage, rpcReq *clientpb.RpcRequest) error {
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
		// The proxy protocol (protocol/proxy/v1) still speaks shared.v1, so
		// the client-v2 request payload is bridged before invoking it.
		Payload:   payloadV2toV1(rpcReq.Payload),
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
					Error: &sharedv2.Error{
						Code:    "RPC_TIMEOUT",
						Type:    "timeout",
						Message: fmt.Sprintf("RPC request timeout after %v", duration),
					},
				}
			}))
		}

		// No proxy configured: soft failure NO_PROXY (PR-KA-A4 §8.3). The
		// request is no longer echoed as an RpcReply.
		if errors.Is(err, proxy.ErrNoProxyFound) {
			return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
				out.Envelope = &clientpb.OutboundMessage_Error{
					Error: &sharedv2.Error{
						Code:    "NO_PROXY",
						Type:    "request_error",
						Message: "no proxy configured for channel/method",
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
				Error: &sharedv2.Error{
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
				Error: sharedErrorV2(proxyResp.Error),
			}
		} else {
			out.Envelope = &clientpb.OutboundMessage_RpcReply{
				RpcReply: &clientpb.RpcReply{
					RequestId: in.Id,
					Payload:   payloadV1toV2(proxyResp.Payload),
					Metadata:  &sharedv2.Metadata{Entries: proxyResp.Meta},
				},
			}
		}
	}))
}

func (c *Session) handlePublish(ctx context.Context, in *clientpb.InboundMessage, publish *clientpb.Publish) error {
	if !c.Authenticated() {
		// An unauthenticated publish is an auth problem, not a stale
		// (auth-timeout) connection: use the invalid-token code.
		return DisconnectInvalidToken
	}

	if c.publishLimiter != nil && !c.publishLimiter.Allow() {
		return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
			out.Envelope = &clientpb.OutboundMessage_Error{
				Error: &sharedv2.Error{
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

	// Static authorization first (PR-KA-A4 §8.1): a proxy that allows must
	// never override a static deny.
	if dec := c.node.authorizer.Decide(c.node.userPrincipal(c.user), ActionPublish, channel); !dec.Allow {
		log.WarnContext(ctx, "ACL denied publish", "channel", channel, "user", c.user, "reason", dec.Reason)
		return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
			out.Envelope = &clientpb.OutboundMessage_Error{
				Error: &sharedv2.Error{
					Code:    "ACL_DENIED",
					Type:    "acl_error",
					Message: "publish denied by ACL rule",
				},
			}
		}))
	}

	// Proxy ACL check: an additional gate, asked only when a route matches.
	// The proxy may reject this single request; its approval cannot bypass
	// the static Decide above.
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
					Error: &sharedv2.Error{
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
					Error: sharedErrorV2(aclResp.Error),
				}
			}))
		}
	}

	// Extract data from Payload, preserving the original oneof variant.
	pub, err := PublicationFromPayloadV2(in.Id, nil, publish.Payload)
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
					Id: in.Id,
					// Transient / no-history: the position offset stays unset
					// (KD-K11), never 0-means-offset.
					Position: positionFrom(c.node.streamEpoch(), 0, false),
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
				Id:       in.Id,
				Position: positionFrom(c.node.streamEpoch(), offset, true),
			},
		}
	}))
}

func (c *Session) handleSubscribe(ctx context.Context, in *clientpb.InboundMessage, sub *clientpb.Subscribe) error {
	subs := []*clientpb.Subscription{}
	addedChannels := make([]string, 0, len(sub.Subscriptions))
	addedPresence := make([]string, 0, len(sub.Subscriptions))

	// Get current broker epoch for the SubscribeAck.
	currentEpoch := c.node.streamEpoch()

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

		if err := c.node.AddSubscription(ctx, ch.Channel, Subscriber{Session: c, Ephemeral: ch.Ephemeral}); err != nil {
			// Unroutable patterns and malformed topics fail the single
			// channel softly: a top-level error envelope, no rollback of
			// the channels already added in this request, no disconnect
			// (A3 §7).
			if errors.Is(err, ErrPatternNotRoutable) || errors.Is(err, topics.ErrBadTopic) {
				c.sendSubscribeRequestError(ctx, in, ch.Channel, err)
				continue
			}
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
	}

	// Send the bare SubscribeAck (no publications), then stream every
	// recover=true channel of this request through the shared Replayer: one
	// quota per Subscribe request (§4.2). A re-subscribe with recover=true is
	// a legitimate catch-up; the subscription stays even when recovery fails.
	if err := c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		ack := &clientpb.SubscribeAck{
			Subscriptions: subs,
			Recover:       c.node.recoverState(c, subs, nil),
			StreamEpoch:   currentEpoch,
			// Catch-up snapshot for every channel in this request that
			// is tracked for presence, including re-subscribes.
			Presence: c.snapshotForChannels(ctx, subs),
		}
		out.Envelope = &clientpb.OutboundMessage_SubscribeAck{SubscribeAck: ack}
	})); err != nil {
		return err
	}
	c.node.streamRecoveries(ctx, c, in, subs, nil, "subscribe")
	return nil
}

// snapshotForChannels builds presence snapshots for the requested channels,
// skipping wildcard, ephemeral and presence=false subscriptions.
func (c *Session) snapshotForChannels(ctx context.Context, subs []*clientpb.Subscription) []*clientpb.PresenceSnapshot {
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

func (c *Session) handleUnsubscribe(ctx context.Context, in *clientpb.InboundMessage, unsubscribe *clientpb.Unsubscribe) error {
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

func (c *Session) handlePing(ctx context.Context, in *clientpb.InboundMessage, ping *clientpb.Ping) error {
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
func (c *Session) handlePong(ctx context.Context, in *clientpb.InboundMessage, pong *clientpb.Pong) error {
	c.ResetActivity()
	c.throttledClusterRefresh()
	return nil
}

// throttledClusterRefresh runs the expensive presence/cluster refresh work
// at most once per pingClusterRefreshInterval. The CAS guard makes sure only
// one caller wins the window. Shared by handlePing and handlePong so the
// two liveness paths refresh identically.
func (c *Session) throttledClusterRefresh() {
	now := time.Now().UnixNano()
	if last := c.lastClusterSyncNano.Load(); now-last >= int64(pingClusterRefreshInterval) &&
		c.lastClusterSyncNano.CompareAndSwap(last, now) {
		go c.refreshPresence()
		go func() {
			clusterCtx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
			defer cancel()
			if err := c.node.syncClusterSessionState(clusterCtx, c); err != nil {
				if errors.Is(err, ErrSessionFenced) {
					// Another node claimed the session: this attachment's
					// fencing is gone. Disconnect (3502) without unbinding
					// the directory lease or deleting cluster state — the
					// new owner is serving the session.
					log.WarnContext(clusterCtx, "session fenced by another owner, disconnecting", "session", c.session)
					c.disconnectFenced()
					return
				}
				log.WarnContext(clusterCtx, "failed to refresh cluster session state", "session", c.session, "error", err)
			}
		}()
	}
}

// disconnectFenced closes a client whose session fencing was invalidated by
// another owner (ErrSessionFenced from the directory refresh). It runs the
// Fence verb: local subscriptions and the hub registration are dropped, but
// the Directory is not unbound and no presence leave is emitted — the session
// now belongs to the new owner, and touching the directory would clobber it.
func (c *Session) disconnectFenced() {
	_ = c.Fence(DisconnectStale)
}

func (c *Session) handleSubRefresh(ctx context.Context, in *clientpb.InboundMessage, refresh *clientpb.SubRefresh) error {
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
func (c *Session) handleSurvey(ctx context.Context, in *clientpb.InboundMessage, req *clientpb.SurveyRequest) error {
	ch := req.GetChannel()
	if ch == "" || isWildcard(ch) {
		return c.sendSurveyError(ctx, in, "BAD_REQUEST", "request_error", "survey channel must be an exact channel")
	}
	if !c.sessionCoversChannel(ch) {
		return c.sendSurveyError(ctx, in, "PERMISSION_DENIED", "acl_error", "survey denied: channel not covered by session")
	}
	// Authorizer decides survey: it already combines the Effects.Survey
	// gate with the allow_survey rules and deny_all (PR-KA-A4 §8.1).
	dec := c.node.authorizer.Decide(c.node.userPrincipal(c.user), ActionSurvey, ch)
	if !dec.Allow {
		if !dec.Effects.Survey {
			return c.sendSurveyError(ctx, in, "SURVEY_DISABLED", "policy_error", "survey disabled by channel policy")
		}
		return c.sendSurveyError(ctx, in, "PERMISSION_DENIED", "acl_error", "survey denied by ACL rule")
	}
	pol := dec.Effects
	if !c.surveyInFlight.CompareAndSwap(false, true) {
		return c.sendSurveyError(ctx, in, "RATE_LIMITED", "rate_limit", "a survey is already in flight for this session")
	}
	if !c.surveyLimiter.Allow() {
		c.surveyInFlight.Store(false)
		return c.sendSurveyError(ctx, in, "RATE_LIMITED", "rate_limit", "survey rate limit exceeded")
	}

	var payload []byte
	if req.Payload != nil {
		pub, err := PublicationFromPayloadV2("", nil, req.Payload)
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
func (c *Session) sendSurveyError(ctx context.Context, in *clientpb.InboundMessage, code, errType, message string) error {
	if c.node.metrics != nil {
		c.node.metrics.SurveyClientTotal.WithLabelValues(code).Inc()
	}
	return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Error{
			Error: &sharedv2.Error{Code: code, Type: errType, Message: message},
		}
	}))
}

// sendSurveyTopError is the worker-side twin of sendSurveyError for
// asynchronously discovered failures (no inbound message id to echo).
func (c *Session) sendSurveyTopError(code, errType, message string) {
	if c.node.metrics != nil {
		c.node.metrics.SurveyClientTotal.WithLabelValues(code).Inc()
	}
	_ = c.Send(c.ctx, MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Error{
			Error: &sharedv2.Error{Code: code, Type: errType, Message: message},
		}
	}))
}

// runSurveyWorker runs the survey off the read loop: cluster-wide subscriber
// count preflight, Node.Survey (local + cluster aggregation), answer
// truncation, then the outbound SurveyResult. The in-flight flag is cleared
// when the worker finishes.
func (c *Session) runSurveyWorker(requestID, channel string, payload []byte, timeout time.Duration) {
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
func (c *Session) handleSurveyReply(ctx context.Context, in *clientpb.InboundMessage, reply *clientpb.SurveyReply) error {
	c.ResetActivity()

	// Extract payload from the survey reply
	var payload []byte
	var err error
	if reply.Error != nil {
		err = fmt.Errorf("%s: %s", reply.Error.Code, reply.Error.Message)
	}
	if reply.Payload != nil {
		pub, convErr := PublicationFromPayloadV2("", nil, reply.Payload)
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
func (c *Session) setHeartbeatCancel(cancel context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.heartbeatCancel = cancel
}

// ResetActivity resets the last activity timestamp to now.
func (c *Session) ResetActivity() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastActivity = time.Now()
}

// ForceTestIDs overrides the session, user, and client IDs for testing
// purposes. It also marks the client authenticated and attaches the initial
// attachment (starting the writer goroutine) so test clients that are wired
// directly (bypassing Connect) can still exercise message handlers and
// observe synchronous writes.
func (c *Session) ForceTestIDs(sessionID, userID, clientID string) {
	c.mu.Lock()
	c.session = sessionID
	c.user = userID
	c.client = clientID
	c.authenticated = true
	if c.clusterLeaseVersion == 0 {
		c.clusterLeaseVersion = 1
	}
	att := c.attachment
	c.mu.Unlock()
	if c.state == SessionAuthenticating {
		_ = c.Attach(att)
	}
}

func (c *Session) hasSubscription(channel string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.subscribedChannels[channel]
	return ok
}

func (c *Session) subscriptionList() []*clientpb.Subscription {
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
func (c *Session) sessionCoversChannel(ch string) bool {
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
func (c *Session) handlePresenceQuery(ctx context.Context, in *clientpb.InboundMessage, query *clientpb.PresenceQuery) error {
	ch := query.GetChannel()
	if ch == "" || isWildcard(ch) {
		return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
			out.Envelope = &clientpb.OutboundMessage_Error{
				Error: &sharedv2.Error{
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
				Error: &sharedv2.Error{
					Code:    "PERMISSION_DENIED",
					Type:    "acl_error",
					Message: "presence query denied: channel not covered by session",
				},
			}
		}))
	}
	dec := c.node.authorizer.Decide(c.node.userPrincipal(c.user), ActionPresence, ch)
	if !dec.Allow {
		if !dec.Effects.Presence {
			return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
				out.Envelope = &clientpb.OutboundMessage_Error{
					Error: &sharedv2.Error{
						Code:    "POLICY_DENIED",
						Type:    "policy_error",
						Message: "presence query denied by channel policy",
					},
				}
			}))
		}
		return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
			out.Envelope = &clientpb.OutboundMessage_Error{
				Error: &sharedv2.Error{
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
func (c *Session) refreshPresence() {
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
