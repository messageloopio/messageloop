package messageloop

import (
	"context"
	"errors"
	protocol "github.com/deeplooplabs/messageloop-protocol"
	clientv1 "github.com/deeplooplabs/messageloop-protocol/gen/proto/go/client/v1"
	sharedv1 "github.com/deeplooplabs/messageloop-protocol/gen/proto/go/shared/v1"
	"github.com/google/uuid"
	"github.com/lynx-go/x/log"
	"github.com/samber/lo"
	"google.golang.org/protobuf/proto"
	"sync"
	"time"
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

func (c *Client) Send(ctx context.Context, msg *clientv1.ServerMessage) error {
	return c.write(ctx, msg)
}

func (c *Client) HandleMessage(ctx context.Context, in *clientv1.ClientMessage) error {
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
		_ = c.Send(ctx, BuildServerMessage(in, func(out *clientv1.ServerMessage) {
			out.Envelope = &clientv1.ServerMessage_Error{
				Error: &sharedv1.Error{
					Code:     501,
					Reason:   err.Error(),
					Message:  "服务异常",
					Metadata: nil,
				},
			}
		}))
		return err
	}
	return nil
}

func (c *Client) handleMessage(ctx context.Context, in *clientv1.ClientMessage) error {

	switch msg := in.Envelope.(type) {
	case *clientv1.ClientMessage_Connect:
		return c.onConnect(ctx, in, msg.Connect)
	case *clientv1.ClientMessage_Publish:
		return c.onPublish(ctx, in, msg.Publish)
	case *clientv1.ClientMessage_Subscribe:
		return c.onSubscribe(ctx, in, msg.Subscribe)
	case *clientv1.ClientMessage_RpcRequest:
		return c.onRPC(ctx, in, msg.RpcRequest)
	case *clientv1.ClientMessage_Unsubscribe:
		return c.onUnsubscribe(ctx, in, msg.Unsubscribe)
	case *clientv1.ClientMessage_Ping:
		return c.onPing(ctx, in, msg.Ping)
	case *clientv1.ClientMessage_SubRefresh:
		return c.onSubRefresh(ctx, in, msg.SubRefresh)
	}
	return nil
}

func (c *Client) Channels() []string {
	return []string{}
}

func (c *Client) onConnect(ctx context.Context, in *clientv1.ClientMessage, connect *clientv1.Connect) error {
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

	return c.Send(ctx, BuildServerMessage(in, func(out *clientv1.ServerMessage) {
		out.Envelope = &clientv1.ServerMessage_Connected{
			Connected: &clientv1.Connected{
				SessionId: c.SessionID(),
				Subscriptions: lo.Map(c.Channels(), func(it string, i int) *clientv1.Subscription {
					return &clientv1.Subscription{
						Channel: it,
					}
				}),
			},
		}
	}))
}

func BuildServerMessage(in *clientv1.ClientMessage, bodyFunc func(out *clientv1.ServerMessage)) *clientv1.ServerMessage {
	var out *clientv1.ServerMessage
	if in != nil {
		out = &clientv1.ServerMessage{
			Id:       in.Id,
			Metadata: in.Metadata,
			Time:     uint64(time.Now().UnixMilli()),
		}
	} else {
		out = &clientv1.ServerMessage{
			Id:       uuid.New().String(),
			Metadata: map[string]string{},
			Time:     uint64(time.Now().UnixMilli()),
		}
	}
	bodyFunc(out)
	return out
}

func (c *Client) payload(payloadBlob []byte, payloadString string) ([]byte, bool) {
	if len(payloadBlob) > 0 {
		return payloadBlob, true
	} else if len(payloadString) > 0 {
		return []byte(payloadString), false
	}
	return nil, true
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

func (c *Client) onRPC(ctx context.Context, in *clientv1.ClientMessage, req *clientv1.RPCRequest) error {
	return c.Send(ctx, BuildServerMessage(in, func(out *clientv1.ServerMessage) {
		out.Envelope = &clientv1.ServerMessage_RpcReply{
			RpcReply: &clientv1.RPCReply{
				Error:       nil,
				PayloadBlob: req.PayloadBlob,
				PayloadText: req.PayloadText,
			},
		}
	}))
}

func (c *Client) onPublish(ctx context.Context, in *clientv1.ClientMessage, pub *clientv1.Publish) error {
	if !c.Authenticated() {
		return DisconnectStale
	}

	payload, isBlob := c.payload(pub.PayloadBlob, pub.PayloadText)
	if err := c.node.Publish(pub.Channel, payload, WithClientInfo(c.ClientInfo()), WithAsBytes(isBlob)); err != nil {
		return err
	}
	return c.Send(ctx, BuildServerMessage(in, func(out *clientv1.ServerMessage) {
		out.Envelope = &clientv1.ServerMessage_PublishAck{
			PublishAck: &clientv1.PublishAck{
				Channel: pub.Channel,
				Offset:  0,
			},
		}
	}))
}

func (c *Client) onSubscribe(ctx context.Context, in *clientv1.ClientMessage, sub *clientv1.Subscribe) error {
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
	return c.Send(ctx, BuildServerMessage(in, func(out *clientv1.ServerMessage) {
		out.Envelope = &clientv1.ServerMessage_SubscribeAck{
			SubscribeAck: &clientv1.SubscribeAck{
				Subscriptions: lo.Map(subs, func(it string, i int) *clientv1.Subscription {
					return &clientv1.Subscription{
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

func (c *Client) onUnsubscribe(ctx context.Context, in *clientv1.ClientMessage, unsubscribe *clientv1.Unsubscribe) error {
	return errors.New("TODO")
}

func (c *Client) onPing(ctx context.Context, in *clientv1.ClientMessage, ping *clientv1.Ping) error {
	return c.Send(ctx, BuildServerMessage(in, func(out *clientv1.ServerMessage) {
		out.Envelope = &clientv1.ServerMessage_Pong{
			Pong: &clientv1.Pong{},
		}
	}))
}

func (c *Client) onSubRefresh(ctx context.Context, in *clientv1.ClientMessage, refresh *clientv1.SubRefresh) error {
	return errors.New("TODO")
}
