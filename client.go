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
	protocol    string // ws or grpc
	connectedAt time.Time

	// Heartbeat fields
	lastActivity    time.Time
	heartbeatCancel context.CancelFunc

	// Tracks channels this client is subscribed to, for presence cleanup.
	subscribedChannels  map[string]struct{}
	clusterLeaseVersion uint64

	// Rate limiter for publish operations.
	publishLimiter *rate.Limiter

	// Survey field - stores the last received survey request ID
	lastSurveyRequestID string

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

	// Remove local subscriptions before clearing presence and hub state.
	// Cleanup runs with bounded concurrency: each channel keeps its own saga
	// ordering (RemoveSubscription serializes per-channel via subLock), while
	// the cluster-mode steps no longer serialize thousands of channels.
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

	// Remove presence for all subscribed channels.
	if len(channels) > 0 {
		presCtx := context.Background()
		for _, ch := range channels {
			_ = c.node.presence.Remove(presCtx, ch, sessionID)
			go c.node.PublishPresenceLeave(ch, sessionID, userID)
		}
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
		c.node.metrics.ConnectionsTotal.Dec()
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
			c.node.metrics.ConnectionsTotal.Dec()
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
	case *clientpb.InboundMessage_SubRefresh:
		return c.handleSubRefresh(ctx, in, msg.SubRefresh)
	case *clientpb.InboundMessage_SurveyRequest:
		return c.handleSurvey(ctx, in, msg.SurveyRequest)
	case *clientpb.InboundMessage_SurveyReply:
		return c.handleSurveyReply(ctx, in, msg.SurveyReply)
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

	// The session ID is needed by the auth proxy (authReq.SessionID) before
	// authentication, so it is set here. Takeover and state inheritance only
	// run after authentication succeeds: an unauthenticated connect must not
	// be able to evict a session that is still being served.
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
			SessionID:  c.session,
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

			// 3. Silently close old session (no presence leave, no sub removal)
			oldSession.closeQuiet()

			// 4. Replace session references in hub (sessions map + subShards)
			if err := c.node.hub.ReplaceSession(connect.SessionId, c); err != nil {
				return err
			}
		} else {
			var err error
			resumeSnapshot, resumed, err = c.node.resumeRemoteSession(ctx, c, connect.SessionId)
			if err != nil {
				return err
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
			return err
		}
		// Only a client that passed AddClient is counted in ConnectionsTotal;
		// close() decrements the gauge solely for such clients.
		c.MarkMetricsCharged()
	} else if err := c.node.syncClusterSessionState(ctx, c); err != nil {
		return err
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

	// Process subscriptions and handle recovery
	subs := connect.Subscriptions
	var pubs []*clientpb.Publication
	addedChannels := make([]string, 0, len(subs))

	// Enforce the per-client subscription limit, counting channels inherited
	// from a resumed session (they are already tracked in subscribedChannels).
	if limit := c.node.limits.MaxSubscriptionsPerClient; limit > 0 {
		c.mu.RLock()
		inheritedCount := len(c.subscribedChannels)
		c.mu.RUnlock()
		if inheritedCount+len(subs) > limit {
			return DisconnectChannelLimit
		}
	}

	// Get current broker epoch for recovery validation
	var currentEpoch string
	if epocher, ok := c.node.broker.(interface{ Epoch() string }); ok {
		currentEpoch = epocher.Epoch()
	}

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

		alreadySubscribed := c.hasSubscription(sub.Channel)
		if err := c.node.AddSubscription(ctx, sub.Channel, NewSubscriber(c, sub.Ephemeral)); err != nil {
			for _, channel := range addedChannels {
				_ = c.node.RemoveSubscription(channel, c)
				_ = c.node.presence.Remove(ctx, channel, c.session)
			}
			return err
		}
		if !alreadySubscribed {
			addedChannels = append(addedChannels, sub.Channel)
			_ = c.node.presence.Add(ctx, sub.Channel, &PresenceInfo{
				ClientID:    c.session,
				UserID:      c.user,
				ConnectedAt: c.connectedAt.UnixMilli(),
			})
			go c.node.PublishPresenceJoin(sub.Channel, c.session, c.user)
		}

		// Handle message recovery if requested
		if sub.Recover && sub.Offset > 0 {
			// Epoch validation: if the broker has restarted, the client's offset is invalid.
			// A client that carries no epoch (older SDK) cannot prove its offset belongs
			// to the current broker generation, so it is treated conservatively: recover
			// from the beginning instead of silently skipping messages.
			sinceOffset := sub.Offset + 1
			if currentEpoch != "" && sub.Epoch != currentEpoch {
				if sub.Epoch == "" {
					log.WarnContext(ctx, "client sent no epoch but broker epoch is set; recovering from the beginning",
						"channel", sub.Channel, "broker_epoch", currentEpoch)
				}
				// Epoch mismatch or unknown — recover from the beginning
				sinceOffset = 0
			}
			historyPubs, err := c.node.broker.History(sub.Channel, sinceOffset, 0)
			if err != nil {
				log.WarnContext(ctx, "failed to recover messages", "channel", sub.Channel, "error", err)
				continue
			}
			// Convert publications to protobuf format. The total number of
			// recovered messages is capped to keep the Connected envelope bounded.
			for _, pub := range historyPubs {
				if len(pubs) >= MaxRecoveredPublications {
					log.WarnContext(ctx, "recovery truncated",
						"channel", sub.Channel, "limit", MaxRecoveredPublications)
					break
				}
				pubs = append(pubs, &clientpb.Publication{
					Messages: []*clientpb.Message{
						{
							Id:      publicationID(sub.Channel, pub.Offset),
							Channel: sub.Channel,
							Offset:  pub.Offset,
							Payload: pub.PayloadProto(),
							Metadata: func() *sharedpb.Metadata {
								if len(pub.Metadata) == 0 {
									return nil
								}
								return &sharedpb.Metadata{Entries: pub.Metadata}
							}(),
						},
					},
				})
			}
		}
	}

	return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Connected{
			Connected: &clientpb.Connected{
				SessionId:     c.SessionID(),
				Resumed:       resumed,
				Epoch:         currentEpoch,
				Publications:  pubs,
				Subscriptions: c.subscriptionList(),
			},
		}
	}))
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
	return &ClientInfo{
		ClientID:    c.client,
		SessionID:   c.session,
		UserID:      c.user,
		RemoteAddr:  c.transport.RemoteAddr(),
		Protocol:    c.protocol,
		ConnectedAt: c.connectedAt.UnixMilli(),
	}
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
	pub := &Publication{}
	if publish.Payload != nil {
		pub.ContentType = publish.Payload.ContentType
		switch p := publish.Payload.Data.(type) {
		case *sharedpb.Payload_Json:
			// JSON data - marshal to bytes.
			data, err := MarshalJSONStruct(p.Json)
			if err != nil {
				return err
			}
			pub.Payload = data
			pub.Kind = PayloadKindJSON
		case *sharedpb.Payload_Binary:
			pub.Payload = p.Binary
			pub.Kind = PayloadKindBinary
		case *sharedpb.Payload_Text:
			pub.Payload = []byte(p.Text)
			pub.Kind = PayloadKindText
		}
	}
	if publish.Metadata != nil {
		pub.Metadata = publish.Metadata.Entries
	}
	pub.Id = in.Id

	if publish.Transient {
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
	// Enforce per-client subscription limit.
	if limit := c.node.limits.MaxSubscriptionsPerClient; limit > 0 {
		c.mu.RLock()
		currentCount := len(c.subscribedChannels)
		c.mu.RUnlock()
		if currentCount+len(sub.Subscriptions) > limit {
			return DisconnectChannelLimit
		}
	}

	subs := []*clientpb.Subscription{}
	addedChannels := make([]string, 0, len(sub.Subscriptions))
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

		if err := c.node.AddSubscription(ctx, ch.Channel, Subscriber{Client: c, Ephemeral: ch.Ephemeral}); err != nil {
			for _, channel := range addedChannels {
				if err := c.node.RemoveSubscription(channel, c); err != nil {
					log.WarnContext(ctx, "failed to rollback subscription", "channel", channel, "error", err)
				}
				_ = c.node.presence.Remove(ctx, channel, c.session)
			}
			return err
		}
		if !alreadySubscribed {
			addedChannels = append(addedChannels, ch.Channel)
		}
		subs = append(subs, ch)

		// Track presence and subscribed channel.
		if !alreadySubscribed {
			_ = c.node.presence.Add(ctx, ch.Channel, &PresenceInfo{
				ClientID:    c.session,
				UserID:      c.user,
				ConnectedAt: c.connectedAt.UnixMilli(),
			})
		}

		// Publish presence join event asynchronously
		if !alreadySubscribed {
			go c.node.PublishPresenceJoin(ch.Channel, c.session, c.user)
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
	return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_SubscribeAck{
			SubscribeAck: &clientpb.SubscribeAck{
				Subscriptions: subs,
			},
		}
	}))
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
	for _, sub := range unsubscribe.Subscriptions {
		alreadySubscribed := c.hasSubscription(sub.Channel)
		// Remove subscription
		_ = c.node.RemoveSubscription(sub.Channel, c)

		// Remove presence and untrack channel.
		if alreadySubscribed {
			_ = c.node.presence.Remove(ctx, sub.Channel, c.session)
		}

		// Publish presence leave event asynchronously
		if alreadySubscribed {
			go c.node.PublishPresenceLeave(sub.Channel, c.session, c.user)
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

	// Throttle the expensive presence/cluster refresh work: the refresh
	// goroutines run at most once per pingClusterRefreshInterval. The CAS
	// guard makes sure only one caller wins the window.
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

func (c *Client) handleSubRefresh(ctx context.Context, in *clientpb.InboundMessage, refresh *clientpb.SubRefresh) error {
	for _, ch := range refresh.Channels {
		p := c.node.FindProxy(ch, "subscribe")
		if p == nil {
			continue
		}
		aclReq := &proxy.SubscribeAclProxyRequest{Channel: ch}
		aclResp, err := p.SubscribeAcl(ctx, aclReq)
		if err != nil || aclResp.Error != nil {
			// ACL check failed — revoke subscription for this channel.
			_ = c.node.RemoveSubscription(ch, c)
			_ = c.node.presence.Remove(ctx, ch, c.session)
			go c.node.PublishPresenceLeave(ch, c.session, c.user)
		}
	}
	return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_SubRefreshAck{
			SubRefreshAck: &clientpb.SubRefreshAck{},
		}
	}))
}

// handleSurvey handles incoming survey requests from the server.
// The client should process the survey request and send a response back.
func (c *Client) handleSurvey(ctx context.Context, in *clientpb.InboundMessage, req *clientpb.SurveyRequest) error {
	c.ResetActivity()

	// Store the request ID for response routing
	c.mu.Lock()
	c.lastSurveyRequestID = req.RequestId
	c.mu.Unlock()

	// Extract payload from the survey request
	var payload []byte
	if req.Payload != nil {
		switch p := req.Payload.Data.(type) {
		case *sharedpb.Payload_Json:
			data, err := MarshalJSONStruct(p.Json)
			if err != nil {
				return err
			}
			payload = data
		case *sharedpb.Payload_Binary:
			payload = p.Binary
		case *sharedpb.Payload_Text:
			payload = []byte(p.Text)
		}
	}

	// Send survey response - by default, echo back the same payload
	// In a real implementation, the client application would handle this differently
	return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_SurveyReply{
			SurveyReply: &clientpb.SurveyReply{
				RequestId: req.RequestId,
				Payload: &sharedpb.Payload{
					Data: &sharedpb.Payload_Binary{
						Binary: payload,
					},
				},
			},
		}
	}))
}

// LastSurveyRequestID returns the last received survey request ID.
// This is useful for testing purposes.
func (c *Client) LastSurveyRequestID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastSurveyRequestID
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
		switch p := reply.Payload.Data.(type) {
		case *sharedpb.Payload_Json:
			payload, err = MarshalJSONStruct(p.Json)
			if err != nil {
				return err
			}
		case *sharedpb.Payload_Binary:
			payload = p.Binary
		case *sharedpb.Payload_Text:
			payload = []byte(p.Text)
		}
	}

	// Use request_id from reply, or fall back to stored request_id
	requestID := reply.RequestId
	if requestID == "" {
		c.mu.RLock()
		requestID = c.lastSurveyRequestID
		c.mu.RUnlock()
	}

	// Add the response to the survey (if the survey is still active)
	if requestID != "" {
		c.node.AddSurveyResponse(ctx, c.session, requestID, payload, err)
	}

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
		ClientID:    session,
		UserID:      user,
		ConnectedAt: connAt,
	}
	for _, ch := range channels {
		_ = c.node.presence.Add(c.ctx, ch, info)
	}
}
