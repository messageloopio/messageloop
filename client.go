package messageloop

import (
	"context"
	"errors"
	"sync"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/binding/format/protobuf/v2/pb"
	clientpb "github.com/deeplooplabs/messageloop/genproto/v1"
	sharedpb "github.com/deeplooplabs/messageloop/genproto/shared/v1"
	"github.com/deeplooplabs/messageloop/protocol"
	"github.com/google/uuid"
	"github.com/lynx-go/x/log"
	"github.com/samber/lo"
	"google.golang.org/protobuf/proto"
)

func NewClient(ctx context.Context, node *Node, t Transport, marshaler protocol.Marshaler) (*Client, ClientCloseFunc, error) {
	client := &Client{
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

type ClientInfo struct {
	ClientID  string `json:"client_id"`
	SessionID string `json:"session_id"`
	UserID    string `json:"user_id"`
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
	marshaler     protocol.Marshaler
	authenticated bool
}

func jsonLog(msg proto.Message) string {
	data, _ := protocol.ProtoJSONMarshaler.Marshal(msg)
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
	return c.transport.Close(disconnect)
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
		_ = c.Send(ctx, BuildOutboundMessage(in, func(out *clientpb.OutboundMessage) {
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
	}
	return nil
}

func (c *Client) Channels() []string {
	return []string{}
}

func (c *Client) onConnect(ctx context.Context, in *clientpb.InboundMessage, connect *clientpb.Connect) error {
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

	return c.Send(ctx, BuildOutboundMessage(in, func(out *clientpb.OutboundMessage) {
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

func BuildOutboundMessage(in *clientpb.InboundMessage, bodyFunc func(out *clientpb.OutboundMessage)) *clientpb.OutboundMessage {
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

func (c *Client) ClientInfo() *ClientInfo {
	return &ClientInfo{
		ClientID:  c.client,
		SessionID: c.session,
		UserID:    c.user,
	}
}

func (c *Client) Authenticated() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.authenticated
}

func (c *Client) onRPC(ctx context.Context, in *clientpb.InboundMessage, event *cloudevents.CloudEvent) error {
	// Echo back the RPC request as the reply for now
	return c.Send(ctx, BuildOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_RpcReply{
			RpcReply: event,
		}
	}))
}

func (c *Client) onPublish(ctx context.Context, in *clientpb.InboundMessage, event *cloudevents.CloudEvent) error {
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

	if err := c.node.Publish(channel, data, WithClientInfo(c.ClientInfo()), WithAsBytes(true)); err != nil {
		return err
	}
	return c.Send(ctx, BuildOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_PublishAck{
			PublishAck: &clientpb.PublishAck{
				Offset: 0,
			},
		}
	}))
}

func (c *Client) onSubscribe(ctx context.Context, in *clientpb.InboundMessage, sub *clientpb.Subscribe) error {
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
	return c.Send(ctx, BuildOutboundMessage(in, func(out *clientpb.OutboundMessage) {
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

func (c *Client) write(ctx context.Context, msg proto.Message) error {
	log.DebugContext(ctx, "sending message", "message", jsonLog(msg))
	bytes, err := c.marshal(msg)
	if err != nil {
		return err
	}
	return c.transport.Write(bytes)
}

func (c *Client) onUnsubscribe(ctx context.Context, in *clientpb.InboundMessage, unsubscribe *clientpb.Unsubscribe) error {
	return errors.New("TODO")
}

func (c *Client) onPing(ctx context.Context, in *clientpb.InboundMessage, ping *clientpb.Ping) error {
	return c.Send(ctx, BuildOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Pong{
			Pong: &clientpb.Pong{},
		}
	}))
}

func (c *Client) onSubRefresh(ctx context.Context, in *clientpb.InboundMessage, refresh *clientpb.SubRefresh) error {
	return errors.New("TODO")
}
