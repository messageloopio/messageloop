package messageloop

import (
	"context"
	"errors"
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
		ctx:       ctx,
		node:      node,
		transport: t,
		session:   uuid.NewString(),
		marshaler: marshaler,
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
	case *clientpb.InboundMessage_PublishPayload:
		return c.onPublish(ctx, in, msg.PublishPayload)
	case *clientpb.InboundMessage_Subscribe:
		return c.onSubscribe(ctx, in, msg.Subscribe)
	case *clientpb.InboundMessage_RpcRequestPayload:
		return c.onRPC(ctx, in, msg.RpcRequestPayload)
	case *clientpb.InboundMessage_Unsubscribe:
		return c.onUnsubscribe(ctx, in, msg.Unsubscribe)
	case *clientpb.InboundMessage_Ping:
		return c.onPing(ctx, in, msg.Ping)
	case *clientpb.InboundMessage_SubRefresh:
		return c.onSubRefresh(ctx, in, msg.SubRefresh)
	}
	return nil
}

func (c *ClientSession) Channels() []string {
	return []string{}
}

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

	c.mu.Lock()
	c.authenticated = true
	c.node.addClient(c)
	c.mu.Unlock()

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

	proxyResp, err := c.node.ProxyRPC(ctx, channel, method, proxyReq)
	if err != nil {
		// No proxy configured or proxy failed - return error to client
		if err.Error() == "no proxy found for channel/method" {
			// No proxy configured - return original echo behavior
			return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
				out.Envelope = &clientpb.OutboundMessage_RpcReplyPayload{
					RpcReplyPayload: event,
				}
			}))
		}
		// Proxy error - return error to client
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

	// Return the proxy response
	return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		if proxyResp.Error != nil {
			out.Envelope = &clientpb.OutboundMessage_Error{
				Error: proxyResp.Error,
			}
		} else {
			out.Envelope = &clientpb.OutboundMessage_RpcReplyPayload{
				RpcReplyPayload: proxyResp.Event,
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
	subs := []string{}
	for _, ch := range sub.Subscriptions {
		if err := c.node.addSubscription(ch.Channel, subscriber{client: c, ephemeral: ch.Ephemeral}); err != nil {
			for _, s := range subs {
				_ = c.node.removeSubscription(s, c)
			}
			return err
		}
		subs = append(subs, ch.Channel)
	}
	return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_SubscribeAck{
			SubscribeAck: &clientpb.SubscribeAck{
				Subscriptions: lo.Map(subs, func(it string, i int) *clientpb.Subscription {
					return &clientpb.Subscription{
						Channel: it,
					}
				}),
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
	return errors.New("TODO")
}

func (c *ClientSession) onPing(ctx context.Context, in *clientpb.InboundMessage, ping *clientpb.Ping) error {
	return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Pong{
			Pong: &clientpb.Pong{},
		}
	}))
}

func (c *ClientSession) onSubRefresh(ctx context.Context, in *clientpb.InboundMessage, refresh *clientpb.SubRefresh) error {
	return errors.New("TODO")
}
