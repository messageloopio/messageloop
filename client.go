package messageloop

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop/proxy"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	"github.com/samber/lo"
	"google.golang.org/protobuf/proto"
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
	connectMu     sync.Mutex // allows syncing connect with disconnect.
	ctx           context.Context
	transport     Transport
	client        string // 客户端上传的
	session       string // 服务端生成
	user          string // 用户 ID
	info          []byte
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
	subscribedChannels map[string]struct{}

	// Survey field - stores the last received survey request ID
	lastSurveyRequestID string
}

func jsonLog(msg proto.Message) string {
	data, _ := ProtoJSONMarshaler.Marshal(msg)
	return string(data)
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
	c.mu.Unlock()

	// Remove presence for all subscribed channels.
	if len(channels) > 0 {
		presCtx := context.Background()
		for _, ch := range channels {
			_ = c.node.presence.Remove(presCtx, ch, c.session)
		}
	}

	// Clean up session from hub
	if c.session != "" {
		c.node.hub.RemoveSession(c.session)
	}

	// Notify proxy about disconnection
	p := c.node.FindProxy("", "disconnect")
	if p != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		disconnectedReq := &proxy.OnDisconnectedProxyRequest{
			SessionID: c.session,
			Username:  c.user,
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

func (c *Client) ClientID() string {
	return c.client
}

func (c *Client) SessionID() string {
	return c.session
}

func (c *Client) UserID() string {
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

	log.DebugContext(ctx, "handling message", "message", jsonLog(in))

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
	}
	return nil
}

const (
	SystemMethodAuthenticate = "$authenticate"
)

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

	// Check if this is a resumption attempt
	resumed := false
	if connect.SessionId != "" {
		// Try to find the old session
		oldSession := c.node.hub.LookupSession(connect.SessionId)
		if oldSession != nil {
			resumed = true
		}
	}

	// Proxy authentication - check if there's a proxy configured for authentication
	var p proxy.Proxy
	if connect.Token != "" {
		p = c.node.FindProxy("", SystemMethodAuthenticate)
		if p != nil {
			authReq := &proxy.AuthenticateProxyRequest{
				ClientID:   connect.ClientId,
				Token:      connect.Token,
				ClientType: connect.ClientType,
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
				c.user = authResp.UserInfo.ID
			}
		}
	}

	c.mu.Lock()
	c.authenticated = true
	c.client = connect.ClientId
	c.node.AddClient(c)
	c.mu.Unlock()

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

	for _, sub := range subs {
		// Add subscription
		c.node.hub.addSub(sub.Channel, NewSubscriber(c, sub.Ephemeral))

		// Handle message recovery if requested
		if sub.Recover && sub.Offset > 0 {
			historyPubs, err := c.node.broker.History(sub.Channel, sub.Offset+1, 0)
			if err != nil {
				log.WarnContext(ctx, "failed to recover messages", "channel", sub.Channel, "error", err)
				continue
			}
			// Convert publications to protobuf format
			for _, pub := range historyPubs {
				payload := &sharedpb.Payload{}
				if len(pub.Payload) > 0 {
					payload.Data = &sharedpb.Payload_Binary{
						Binary: pub.Payload,
					}
				}
				pubs = append(pubs, &clientpb.Publication{
					Messages: []*clientpb.Message{
						{
							Id:      uuid.New().String(),
							Channel: sub.Channel,
							Offset:  pub.Offset,
							Payload: payload,
						},
					},
				})
			}
		}
	}

	return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Connected{
			Connected: &clientpb.Connected{
				SessionId:    c.SessionID(),
				Resumed:      resumed,
				Publications: pubs,
				Subscriptions: lo.Map(subs, func(it *clientpb.Subscription, i int) *clientpb.Subscription {
					return &clientpb.Subscription{
						Channel: it.Channel,
					}
				}),
			},
		}
	}))
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
		return DisconnectStale
	}

	channel := publish.Channel
	if channel == "" {
		return errors.New("missing channel in publish message")
	}

	// Proxy ACL check - check if there's a proxy configured for publish ACL
	p := c.node.FindProxy(channel, "publish")
	if p != nil && publish.Token != "" {
		aclReq := &proxy.PublishAclProxyRequest{
			Channel: channel,
			Token:   publish.Token,
		}
		aclResp, err := p.PublishAcl(ctx, aclReq)
		if err != nil {
			log.WarnContext(ctx, "proxy publish ACL check failed", "channel", channel, "error", err)
			_ = c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
				out.Envelope = &clientpb.OutboundMessage_Error{
					Error: &sharedpb.Error{
						Code:    "ACL_ERROR",
						Type:    "acl_error",
						Message: err.Error(),
					},
				}
			}))
			return DisconnectInvalidToken
		}
		if aclResp.Error != nil {
			log.WarnContext(ctx, "proxy publish ACL returned error", "channel", channel, "error", aclResp.Error)
			_ = c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
				out.Envelope = &clientpb.OutboundMessage_Error{
					Error: aclResp.Error,
				}
			}))
			return DisconnectInvalidToken
		}
	}

	// Extract data from Payload
	var data []byte
	var isText bool
	if publish.Payload != nil {
		switch p := publish.Payload.Data.(type) {
		case *sharedpb.Payload_Json:
			// JSON data - marshal to bytes
			data = []byte(p.Json.String())
			isText = true
		case *sharedpb.Payload_Binary:
			data = p.Binary
			isText = false
		}
	}

	if err := c.node.Publish(channel, data, isText); err != nil {
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

func (c *Client) handleSubscribe(ctx context.Context, in *clientpb.InboundMessage, sub *clientpb.Subscribe) error {
	subs := []*clientpb.Subscription{}
	for _, ch := range sub.Subscriptions {
		// Proxy ACL check - check if there's a proxy configured for subscription ACL
		p := c.node.FindProxy(ch.Channel, "subscribe")
		if p != nil && ch.Token != "" {
			aclReq := &proxy.SubscribeAclProxyRequest{
				Channel: ch.Channel,
				Token:   ch.Token,
			}
			aclResp, err := p.SubscribeAcl(ctx, aclReq)
			if err != nil {
				log.WarnContext(ctx, "proxy subscribe ACL check failed", "channel", ch.Channel, "error", err)
				_ = c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
					out.Envelope = &clientpb.OutboundMessage_Error{
						Error: &sharedpb.Error{
							Code:    "ACL_ERROR",
							Type:    "acl_error",
							Message: err.Error(),
						},
					}
				}))
				return DisconnectInvalidToken
			}
			if aclResp.Error != nil {
				log.WarnContext(ctx, "proxy subscribe ACL returned error", "channel", ch.Channel, "error", aclResp.Error)
				_ = c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
					out.Envelope = &clientpb.OutboundMessage_Error{
						Error: aclResp.Error,
					}
				}))
				return DisconnectInvalidToken
			}
		}

		if err := c.node.AddSubscription(ctx, ch.Channel, Subscriber{Client: c, Ephemeral: ch.Ephemeral}); err != nil {
			for _, s := range subs {
				if err := c.node.RemoveSubscription(s.Channel, c); err != nil {
					log.WarnContext(ctx, "failed to rollback subscription", "channel", s.Channel, "error", err)
				}
			}
			return err
		}
		subs = append(subs, ch)

		// Track presence and subscribed channel.
		_ = c.node.presence.Add(ctx, ch.Channel, &PresenceInfo{
			ClientID:    c.session,
			UserID:      c.user,
			ConnectedAt: c.connectedAt.UnixMilli(),
		})
		c.mu.Lock()
		c.subscribedChannels[ch.Channel] = struct{}{}
		c.mu.Unlock()

		// Notify proxy about subscription
		if p != nil {
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
	log.DebugContext(ctx, "sending message", "message", jsonLog(msg))
	bytes, err := c.marshal(msg)
	if err != nil {
		return err
	}
	log.DebugContext(ctx, "message marshaled", "size", len(bytes))
	err = c.transport.Write(bytes)
	if err != nil {
		log.ErrorContext(ctx, "failed to write to transport", err)
	} else {
		log.DebugContext(ctx, "message written to transport successfully")
	}
	return err
}

func (c *Client) handleUnsubscribe(ctx context.Context, in *clientpb.InboundMessage, unsubscribe *clientpb.Unsubscribe) error {
	for _, sub := range unsubscribe.Subscriptions {
		// Remove subscription
		_ = c.node.RemoveSubscription(sub.Channel, c)

		// Remove presence and untrack channel.
		_ = c.node.presence.Remove(ctx, sub.Channel, c.session)
		c.mu.Lock()
		delete(c.subscribedChannels, sub.Channel)
		c.mu.Unlock()

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
	// SubRefresh is used to refresh subscriptions, currently just acknowledges the refresh
	// The proxy is notified through OnSubscribed/OnUnsubscribed, so no additional action needed here
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
			payload = []byte(p.Json.String())
		case *sharedpb.Payload_Binary:
			payload = p.Binary
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
			payload = []byte(p.Json.String())
		case *sharedpb.Payload_Binary:
			payload = p.Binary
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
