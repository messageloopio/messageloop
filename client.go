package messageloop

import (
	"context"
	"errors"
	clientv1 "github.com/deeploopdev/messageloop-protocol/gen/proto/go/client/v1"
	"github.com/google/uuid"
	"github.com/lynx-go/x/log"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"sync"
)

func NewClient(ctx context.Context, node *Node, t Transport, marshaler Marshaler) (*Client, ClientCloseFunc, error) {
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

type Client struct {
	mu        sync.RWMutex
	connectMu sync.Mutex // allows syncing connect with disconnect.
	ctx       context.Context
	transport Transport
	uid       string
	session   string
	user      string
	info      []byte
	status    status
	node      *Node
	marshaler Marshaler
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
	// TODO
	return nil
}

func (c *Client) ID() string {
	return c.session
}

func marshalJson(msg proto.Message) string {
	bytes, _ := protojson.Marshal(msg)
	return string(bytes)
}

func (c *Client) HandleMessage(ctx context.Context, in *clientv1.ClientMessage) error {

	c.mu.Lock()
	if c.status == statusClosed {
		c.mu.Unlock()
		return errors.New("client is closed")
	}
	c.mu.Unlock()

	log.DebugContext(ctx, "handling message", "message", marshalJson(in))

	select {
	case <-c.ctx.Done():
		return nil
	default:
	}

	switch msg := in.Body.(type) {
	case *clientv1.ClientMessage_Publish:
		if err := c.onPublish(in, msg.Publish); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) onPublish(in *clientv1.ClientMessage, publish *clientv1.Publish) error {
	out := &clientv1.ServerMessage{
		Id:      in.Id,
		Headers: in.Headers,
		Body: &clientv1.ServerMessage_Publication{
			Publication: &clientv1.Publication{Messages: []*clientv1.Message{
				{
					Id:            in.Id,
					Channel:       publish.Channel,
					Offset:        0,
					PayloadBytes:  publish.PayloadBytes,
					PayloadString: publish.PayloadString,
				},
			}},
		},
	}
	return c.write(out)
}

func (c *Client) write(msg any) error {
	bytes, err := c.marshal(msg)
	if err != nil {
		return err
	}
	return c.transport.Write(bytes)
}
