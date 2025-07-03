package messageloop

import (
	"context"
	"errors"
	clientv1 "github.com/deeploopdev/messageloop-protocol/gen/proto/go/client/v1"
	"google.golang.org/protobuf/proto"
	"sync"
)

func NewClient(ctx context.Context, node *Node, t Transport) (*Client, ClientCloseFunc, error) {
	client := &Client{
		ctx:       ctx,
		node:      node,
		transport: t,
	}
	return client, func() error {
		return client.close(Disconnect{})
	}, nil
}

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

func (c *Client) HandleMessage(in *clientv1.ClientMessage) error {

	c.mu.Lock()
	if c.status == statusClosed {
		c.mu.Unlock()
		return errors.New("client is closed")
	}
	c.mu.Unlock()

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
					Id:          in.Id,
					Channel:     publish.Channel,
					Offset:      0,
					Payload:     publish.Payload,
					PayloadJson: publish.PayloadJson,
				},
			}},
		},
	}
	bytes, err := proto.Marshal(out)
	if err != nil {
		return err
	}
	return c.transport.Write(bytes)
}
