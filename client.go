package messageloop

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/binding/format/protobuf/v2/pb"
	sharedpb "github.com/deeplooplabs/messageloop/genproto/shared/v1"
	clientpb "github.com/deeplooplabs/messageloop/genproto/v1"
	"github.com/deeplooplabs/messageloop/proxy"
	"github.com/google/uuid"
	"github.com/lynx-go/x/log"
	"github.com/samber/lo"
	"google.golang.org/protobuf/proto"
)

func NewClientSession(ctx context.Context, node *Node, t Transport, marshaler Marshaler) (*ClientSession, ClientCloseFunc, error) {
	client := &ClientSession{
		ctx:          ctx,
		node:         node,
		transport:    t,
		session:      uuid.NewString(),
		marshaler:    marshaler,
		lastActivity: time.Now(),
	}

	// Start heartbeat if configured
	if node.heartbeatManager != nil {
		node.heartbeatManager.Start(ctx, client)
	}

	return client, func() error {
		return client.close(Disconnect{})
	}, nil
}

type EncodingType int

const (
	EncodingTypeJSON     EncodingType = 1
	EncodingTypeProtobuf EncodingType = 2
)

type ClientCloseFunc func() error

type ClientDesc struct {
	ClientID  string `json:"client_id"`
	SessionID string `json:"session_id"`
	UserID    string `json:"user_id"`
}

type ClientSession struct {
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

	// Heartbeat fields
	lastActivity    time.Time
	heartbeatCancel context.CancelFunc

	// Survey field - stores the last received survey request ID
	lastSurveyRequestID string
}

func jsonLog(msg proto.Message) string {
	data, _ := ProtoJSONMarshaler.Marshal(msg)
	return string(data)
}

func (c *ClientSession) marshal(msg any) ([]byte, error) {
	return c.marshaler.Marshal(msg)
}

type status uint8

const (
	statusConnecting status = 1
	statusConnected  status = 2
	statusClosed     status = 3
)

func (c *ClientSession) close(disconnect Disconnect) error {
	c.mu.Lock()
	if c.heartbeatCancel != nil {
		c.heartbeatCancel()
		c.heartbeatCancel = nil
	}
	c.mu.Unlock()

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

func (c *ClientSession) ClientID() string {
	return c.client
}

func (c *ClientSession) SessionID() string {
	return c.session
}

func (c *ClientSession) UserID() string {
	return c.user
}

func (c *ClientSession) Send(ctx context.Context, msg *clientpb.OutboundMessage) error {
	return c.write(ctx, msg)
}

func (c *ClientSession) HandleMessage(ctx context.Context, in *clientpb.InboundMessage) error {
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

func (c *ClientSession) handleMessage(ctx context.Context, in *clientpb.InboundMessage) error {

	switch msg := in.Envelope.(type) {
	case *clientpb.InboundMessage_Connect:
		return c.onConnect(ctx, in, msg.Connect)
	case *clientpb.InboundMessage_Publish:
		return c.onPublish(ctx, in, msg.Publish)
	case *clientpb.InboundMessage_Subscribe:
		return c.onSubscribe(ctx, in, msg.Subscribe)
	case *clientpb.InboundMessage_RpcRequest:
		return c.onRPC(ctx, in, msg.RpcRequest)
	case *clientpb.InboundMessage_Unsubscribe:
		return c.onUnsubscribe(ctx, in, msg.Unsubscribe)
	case *clientpb.InboundMessage_Ping:
		return c.onPing(ctx, in, msg.Ping)
	case *clientpb.InboundMessage_SubRefresh:
		return c.onSubRefresh(ctx, in, msg.SubRefresh)
	case *clientpb.InboundMessage_SurveyRequest:
		return c.onSurvey(ctx, in, msg.SurveyRequest)
	case *clientpb.InboundMessage_SurveyResponse:
		return c.onSurveyResponse(ctx, in, msg.SurveyResponse)
	}
	return nil
}

func (c *ClientSession) Channels() []string {
	return []string{}
}

const (
	SystemMethodAuthenticate = "$authenticate"
)

func (c *ClientSession) onConnect(ctx context.Context, in *clientpb.InboundMessage, connect *clientpb.Connect) error {
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

	// Proxy authentication - check if there's a proxy configured for authentication
	var p proxy.Proxy
	if connect.Token != "" {
		p = c.node.FindProxy("", SystemMethodAuthenticate)
		if p != nil {
			authReq := &proxy.AuthenticateProxyRequest{
				Username:   connect.ClientId, // Use client_id as username
				Password:   connect.Token,    // Use token as password
				ClientType: connect.ClientType,
				ClientID:   connect.ClientId,
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
	c.node.addClient(c)
	c.mu.Unlock()

	// Notify proxy about client connection
	if p != nil {
		connectedReq := &proxy.OnConnectedProxyRequest{
			SessionID: c.session,
			Username:  connect.ClientId,
		}
		_, _ = p.OnConnected(ctx, connectedReq) // Ignore error for notification
	}

	return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Connected{
			Connected: &clientpb.Connected{
				SessionId: c.SessionID(),
				Subscriptions: lo.Map(c.Channels(), func(it string, i int) *clientpb.Subscription {
					return &clientpb.Subscription{
						Channel: it,
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
			Id:       in.Id,
			Metadata: in.Metadata,
			Time:     uint64(time.Now().UnixMilli()),
		}
	} else {
		out = &clientpb.OutboundMessage{
			Id:       uuid.New().String(),
			Metadata: map[string]string{},
			Time:     uint64(time.Now().UnixMilli()),
		}
	}
	bodyFunc(out)
	return out
}

func (c *ClientSession) ClientInfo() *ClientDesc {
	return &ClientDesc{
		ClientID:  c.client,
		SessionID: c.session,
		UserID:    c.user,
	}
}

func (c *ClientSession) Authenticated() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.authenticated
}

func (c *ClientSession) onRPC(ctx context.Context, in *clientpb.InboundMessage, event *cloudevents.CloudEvent) error {
	// Extract channel and method from the InboundMessage
	channel := in.Channel
	method := in.Method

	// Fallback to event fields if not set in InboundMessage
	if channel == "" && event != nil {
		channel = event.Source
	}
	if method == "" && event != nil {
		method = event.Type
	}

	// Apply RPC timeout from configuration or use default
	rpcTimeout := c.node.GetRPCTimeout()
	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	// Check if there's a proxy configured for this channel/method
	proxyReq := &proxy.RPCProxyRequest{
		ID:        in.Id,
		ClientID:  c.client,
		SessionID: c.session,
		UserID:    c.user,
		Channel:   channel,
		Method:    method,
		Event:     event,
		Meta:      in.Metadata,
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

		// No proxy configured or proxy failed - return error to client
		if errors.Is(err, proxy.ErrNoProxyFound) {
			// No proxy configured - return original echo behavior
			return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
				out.Envelope = &clientpb.OutboundMessage_RpcReply{
					RpcReply: event,
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

	// Return the proxy response
	return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		if proxyResp.Error != nil {
			out.Envelope = &clientpb.OutboundMessage_Error{
				Error: proxyResp.Error,
			}
		} else {
			out.Envelope = &clientpb.OutboundMessage_RpcReply{
				RpcReply: proxyResp.Event,
			}
		}
	}))
}

func (c *ClientSession) onPublish(ctx context.Context, in *clientpb.InboundMessage, event *cloudevents.CloudEvent) error {
	if !c.Authenticated() {
		return DisconnectStale
	}

	// Extract channel from InboundMessage
	channel := in.Channel
	if channel == "" {
		// Fallback to event source if channel is not set
		if event != nil && event.Source != "" {
			channel = event.Source
		}
	}

	// Extract data from CloudEvent
	var data []byte
	if event != nil {
		if binaryData := event.GetBinaryData(); len(binaryData) > 0 {
			data = binaryData
		} else if textData := event.GetTextData(); textData != "" {
			data = []byte(textData)
		}
	}

	if err := c.node.Publish(channel, data, WithClientDesc(c.ClientInfo()), WithAsBytes(true)); err != nil {
		return err
	}
	return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_PublishAck{
			PublishAck: &clientpb.PublishAck{
				Offset: 0,
			},
		}
	}))
}

func (c *ClientSession) onSubscribe(ctx context.Context, in *clientpb.InboundMessage, sub *clientpb.Subscribe) error {
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

		if err := c.node.addSubscription(ctx, ch.Channel, subscriber{client: c, ephemeral: ch.Ephemeral}); err != nil {
			for _, s := range subs {
				if rmErr := c.node.removeSubscription(s.Channel, c); rmErr != nil {
					log.WarnContext(ctx, "failed to rollback subscription", "channel", s.Channel, "error", rmErr)
				}
			}
			return err
		}
		subs = append(subs, ch)

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

func (c *ClientSession) write(ctx context.Context, msg proto.Message) error {
	log.DebugContext(ctx, "sending message", "message", jsonLog(msg))
	bytes, err := c.marshal(msg)
	if err != nil {
		return err
	}
	return c.transport.Write(bytes)
}

func (c *ClientSession) onUnsubscribe(ctx context.Context, in *clientpb.InboundMessage, unsubscribe *clientpb.Unsubscribe) error {
	for _, sub := range unsubscribe.Subscriptions {
		// Remove subscription
		_ = c.node.removeSubscription(sub.Channel, c)

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

func (c *ClientSession) onPing(ctx context.Context, in *clientpb.InboundMessage, ping *clientpb.Ping) error {
	c.ResetActivity()
	return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Pong{
			Pong: &clientpb.Pong{},
		}
	}))
}

func (c *ClientSession) onSubRefresh(ctx context.Context, in *clientpb.InboundMessage, refresh *clientpb.SubRefresh) error {
	// SubRefresh is used to refresh subscriptions, currently just acknowledges the refresh
	// The proxy is notified through OnSubscribed/OnUnsubscribed, so no additional action needed here
	return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_SubRefreshAck{
			SubRefreshAck: &clientpb.SubRefreshAck{},
		}
	}))
}

// onSurvey handles incoming survey requests from the server.
// The client should process the survey request and send a response back.
func (c *ClientSession) onSurvey(ctx context.Context, in *clientpb.InboundMessage, req *clientpb.SurveyRequest) error {
	c.ResetActivity()

	// Store the request ID for response routing
	c.mu.Lock()
	c.lastSurveyRequestID = req.RequestId
	c.mu.Unlock()

	// Extract payload from the survey request
	var payload []byte
	if req.Payload != nil {
		if binaryData := req.Payload.GetBinaryData(); len(binaryData) > 0 {
			payload = binaryData
		} else if textData := req.Payload.GetTextData(); textData != "" {
			payload = []byte(textData)
		}
	}

	// Send survey response - by default, echo back the same payload
	// In a real implementation, the client application would handle this differently
	responseEvent := &cloudevents.CloudEvent{
		Id:          uuid.NewString(),
		Source:      in.Channel,
		SpecVersion: "1.0",
		Type:        "com.messageloop.survey.response",
		Data: &cloudevents.CloudEvent_BinaryData{
			BinaryData: payload,
		},
	}

	return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_SurveyResponse{
			SurveyResponse: &clientpb.SurveyResponse{
				RequestId: req.RequestId,
				Payload:   responseEvent,
			},
		}
	}))
}

// LastSurveyRequestID returns the last received survey request ID.
// This is useful for testing purposes.
func (c *ClientSession) LastSurveyRequestID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastSurveyRequestID
}

// onSurveyResponse handles incoming survey responses from clients.
// This is called when a client sends a SurveyResponse back to the server.
func (c *ClientSession) onSurveyResponse(ctx context.Context, in *clientpb.InboundMessage, resp *clientpb.SurveyResponse) error {
	c.ResetActivity()

	// Extract payload from the survey response
	var payload []byte
	var respErr error
	if resp.Error != nil {
		respErr = fmt.Errorf("%s: %s", resp.Error.Code, resp.Error.Message)
	}
	if resp.Payload != nil {
		if binaryData := resp.Payload.GetBinaryData(); len(binaryData) > 0 {
			payload = binaryData
		} else if textData := resp.Payload.GetTextData(); textData != "" {
			payload = []byte(textData)
		}
	}

	// Use request_id from response, or fall back to stored request_id
	requestID := resp.RequestId
	if requestID == "" {
		c.mu.RLock()
		requestID = c.lastSurveyRequestID
		c.mu.RUnlock()
	}

	// Add the response to the survey (if the survey is still active)
	if requestID != "" {
		c.node.AddSurveyResponse(ctx, c.session, requestID, payload, respErr)
	}

	return nil
}

// Heartbeat-related methods

// setHeartbeatCancel sets the heartbeat cancel function.
func (c *ClientSession) setHeartbeatCancel(cancel context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.heartbeatCancel = cancel
}

// ResetActivity resets the last activity timestamp to now.
func (c *ClientSession) ResetActivity() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastActivity = time.Now()
}
