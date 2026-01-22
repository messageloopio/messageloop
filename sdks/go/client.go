package messageloopgo

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/cloudevents/sdk-go/binding/format/protobuf/v2/pb"
	sharedpb "github.com/fleetlit/messageloop/genproto/shared/v1"
	clientpb "github.com/fleetlit/messageloop/genproto/v1"
	"google.golang.org/protobuf/proto"
)

// transport is the interface for sending/receiving messages.
type transport interface {
	Send(ctx context.Context, msg *clientpb.InboundMessage) error
	Recv(ctx context.Context) (*clientpb.OutboundMessage, error)
	Close() error
}

// Client is the MessageLoop client interface.
type Client interface {
	// Connect connects to the server
	Connect(ctx context.Context) error
	// Close closes the connection
	Close() error
	// Subscribe subscribes to channels
	Subscribe(channels ...string) error
	// Unsubscribe unsubscribes from channels
	Unsubscribe(channels ...string) error
	// Publish publishes a message to a channel
	Publish(channel string, event *pb.CloudEvent) error
	// RPC sends an RPC request and waits for a response
	RPC(ctx context.Context, channel, method string, req, resp *pb.CloudEvent) error
	// OnMessage sets the message handler
	OnMessage(fn func([]*Message))
	// OnError sets the error handler
	OnError(fn func(error))
	// OnConnected sets the connected handler
	OnConnected(fn func(sessionID string))
	// SessionID returns the session ID
	SessionID() string
	// IsConnected returns the connection status
	IsConnected() bool
}

// client is the implementation of the Client interface.
type client struct {
	mu               sync.RWMutex
	ctx              context.Context
	cancel           context.CancelFunc
	transport        transport
	opts             *Options
	sessionID        string
	connected        atomic.Bool
	closed           atomic.Bool
	connectedCh      chan struct{} // Closed when connection is established
	connectErrCh     chan error    // For connection errors
	msgHandler       func([]*Message)
	errorHandler     func(error)
	connectedHandler func(string)
	pendingRPC       map[string]chan *clientpb.OutboundMessage
	pendingRPCMu     sync.RWMutex
	nextMsgID        atomic.Uint64
	subscriptions    map[string]bool
	subMu            sync.RWMutex
}

// Dial creates a new WebSocket client connecting to the specified URL.
func Dial(url string, opts ...Option) (Client, error) {
	options := defaultOptions()
	for _, opt := range opts {
		opt(options)
	}

	ctx, cancel := context.WithCancel(context.Background())

	trans, err := newWSTransport(url, options.Encoding, options.DialTimeout)
	if err != nil {
		cancel()
		return nil, err
	}

	return newClient(ctx, cancel, trans, options), nil
}

// DialGRPC creates a new gRPC client connecting to the specified address.
func DialGRPC(addr string, opts ...Option) (Client, error) {
	options := defaultOptions()
	for _, opt := range opts {
		opt(options)
	}

	ctx, cancel := context.WithCancel(context.Background())

	trans, err := newGRPCTransport(ctx, addr)
	if err != nil {
		cancel()
		return nil, err
	}

	return newClient(ctx, cancel, trans, options), nil
}

// newClient creates a new client with the given transport.
func newClient(ctx context.Context, cancel context.CancelFunc, trans transport, opts *Options) *client {
	c := &client{
		ctx:           ctx,
		cancel:        cancel,
		transport:     trans,
		opts:          opts,
		connectedCh:   make(chan struct{}),
		connectErrCh:  make(chan error, 1),
		pendingRPC:    make(map[string]chan *clientpb.OutboundMessage),
		subscriptions: make(map[string]bool),
	}
	return c
}

// Connect connects to the server and starts the receive loop.
func (c *client) Connect(ctx context.Context) error {
	// Reset the connected channel for reconnection attempts
	c.mu.Lock()
	c.connectedCh = make(chan struct{})
	c.connectErrCh = make(chan error, 1)
	c.mu.Unlock()

	// Send Connect message
	connectMsg := &clientpb.InboundMessage{
		Id:       c.generateID(),
		Metadata: make(map[string]string),
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId:   c.opts.ClientID,
				ClientType: c.opts.ClientType,
				Token:      c.opts.Token,
				Version:    c.opts.Version,
			},
		},
	}

	// Add auto-subscribe channels
	if len(c.opts.AutoSubscribe) > 0 {
		subs := make([]*clientpb.Subscription, len(c.opts.AutoSubscribe))
		for i, ch := range c.opts.AutoSubscribe {
			subs[i] = &clientpb.Subscription{
				Channel:   ch,
				Ephemeral: false,
			}
		}
		connectMsg.GetConnect().Subscriptions = subs
	}

	if err := c.transport.Send(ctx, connectMsg); err != nil {
		return fmt.Errorf("send connect failed: %w", err)
	}

	// Start receive loop
	go c.receiveLoop()

	// Wait for connection to be established or an error
	select {
	case <-c.connectedCh:
		return nil
	case err := <-c.connectErrCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(30 * time.Second):
		return fmt.Errorf("connection timeout")
	}
}

// receiveLoop is the main receive loop.
func (c *client) receiveLoop() {
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		msg, err := c.transport.Recv(c.ctx)
		if err != nil {
			if !c.closed.Load() {
				isConnError := !c.connected.Load()
				c.handleError(fmt.Errorf("receive error: %w", err), isConnError)
			}
			return
		}

		c.handleMessage(msg)
	}
}

// handleMessage handles an incoming message from the server.
func (c *client) handleMessage(msg *clientpb.OutboundMessage) {
	switch env := msg.GetEnvelope().(type) {
	case *clientpb.OutboundMessage_Connected:
		c.handleConnected(env.Connected)

	case *clientpb.OutboundMessage_Error:
		err := fmt.Errorf("server error: %s (code: %s)", env.Error.GetMessage(), env.Error.GetCode())
		c.handleError(err, !c.connected.Load())

	case *clientpb.OutboundMessage_SubscribeAck:
		c.handleSubscribeAck(env.SubscribeAck)

	case *clientpb.OutboundMessage_UnsubscribeAck:
		c.handleUnsubscribeAck(env.UnsubscribeAck)

	case *clientpb.OutboundMessage_Publication:
		c.handlePublication(env.Publication)

	case *clientpb.OutboundMessage_RpcReply:
		c.handleRPCReply(msg, env.RpcReply)

	case *clientpb.OutboundMessage_PublishAck:
		// PublishAck is handled via RPC reply mechanism
		// or we can just log it

	case *clientpb.OutboundMessage_Pong:
		// Pong is handled by the ping mechanism
	}
}

// handleConnected handles the Connected message.
func (c *client) handleConnected(connected *clientpb.Connected) {
	c.mu.Lock()
	c.sessionID = connected.GetSessionId()
	ch := c.connectedCh
	c.mu.Unlock()
	c.connected.Store(true)

	// Signal that connection is established
	select {
	case <-ch:
		// Already closed
	default:
		close(ch)
	}

	// Track subscriptions
	for _, sub := range connected.GetSubscriptions() {
		c.subMu.Lock()
		c.subscriptions[sub.GetChannel()] = true
		c.subMu.Unlock()
	}

	// Handle initial publications
	for _, pub := range connected.GetPublications() {
		messages := wrapPublication(pub)
		if c.msgHandler != nil && len(messages) > 0 {
			c.msgHandler(messages)
		}
	}

	if c.connectedHandler != nil {
		c.connectedHandler(c.sessionID)
	}
}

// handleSubscribeAck handles the SubscribeAck message.
func (c *client) handleSubscribeAck(ack *clientpb.SubscribeAck) {
	for _, sub := range ack.GetSubscriptions() {
		c.subMu.Lock()
		c.subscriptions[sub.GetChannel()] = true
		c.subMu.Unlock()
	}
}

// handleUnsubscribeAck handles the UnsubscribeAck message.
func (c *client) handleUnsubscribeAck(ack *clientpb.UnsubscribeAck) {
	for _, sub := range ack.GetSubscriptions() {
		c.subMu.Lock()
		delete(c.subscriptions, sub.GetChannel())
		c.subMu.Unlock()
	}
}

// handlePublication handles the Publication message.
func (c *client) handlePublication(pub *clientpb.Publication) {
	messages := wrapPublication(pub)
	if c.msgHandler != nil && len(messages) > 0 {
		c.msgHandler(messages)
	}
}

// handleRPCReply handles the RPC reply message.
func (c *client) handleRPCReply(msg *clientpb.OutboundMessage, event *pb.CloudEvent) {
	id := msg.GetId()

	c.pendingRPCMu.RLock()
	ch, ok := c.pendingRPC[id]
	c.pendingRPCMu.RUnlock()

	if ok {
		select {
		case ch <- msg:
		default:
			// Channel is full or closed, discard
		}
	}
}

// handleError handles an error.
func (c *client) handleError(err error, isConnError bool) {
	if c.errorHandler != nil {
		c.errorHandler(err)
	}
	// If this is a connection error (error during connection handshake), notify the Connect method
	if isConnError {
		c.mu.Lock()
		ch := c.connectErrCh
		c.mu.Unlock()
		select {
		case ch <- err:
		default:
			// Channel already has an error or is closed
		}
	}
}

// Subscribe subscribes to channels.
func (c *client) Subscribe(channels ...string) error {
	if !c.connected.Load() {
		return fmt.Errorf("not connected")
	}

	subs := make([]*clientpb.Subscription, len(channels))
	for i, ch := range channels {
		subs[i] = &clientpb.Subscription{
			Channel:   ch,
			Ephemeral: false,
		}
	}

	msg := &clientpb.InboundMessage{
		Id: c.generateID(),
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{
				Subscriptions: subs,
			},
		},
	}

	if err := c.transport.Send(c.ctx, msg); err != nil {
		return fmt.Errorf("subscribe failed: %w", err)
	}

	return nil
}

// Unsubscribe unsubscribes from channels.
func (c *client) Unsubscribe(channels ...string) error {
	if !c.connected.Load() {
		return fmt.Errorf("not connected")
	}

	subs := make([]*clientpb.Subscription, len(channels))
	for i, ch := range channels {
		subs[i] = &clientpb.Subscription{
			Channel:   ch,
			Ephemeral: false,
		}
	}

	msg := &clientpb.InboundMessage{
		Id: c.generateID(),
		Envelope: &clientpb.InboundMessage_Unsubscribe{
			Unsubscribe: &clientpb.Unsubscribe{
				Subscriptions: subs,
			},
		},
	}

	if err := c.transport.Send(c.ctx, msg); err != nil {
		return fmt.Errorf("unsubscribe failed: %w", err)
	}

	return nil
}

// Publish publishes a message to a channel.
func (c *client) Publish(channel string, event *pb.CloudEvent) error {
	if !c.connected.Load() {
		return fmt.Errorf("not connected")
	}

	msg := &clientpb.InboundMessage{
		Id:      c.generateID(),
		Channel: channel,
		Envelope: &clientpb.InboundMessage_Publish{
			Publish: event,
		},
	}

	if err := c.transport.Send(c.ctx, msg); err != nil {
		return fmt.Errorf("publish failed: %w", err)
	}

	return nil
}

// RPC sends an RPC request and waits for a response.
func (c *client) RPC(ctx context.Context, channel, method string, req, resp *pb.CloudEvent) error {
	if !c.connected.Load() {
		return fmt.Errorf("not connected")
	}

	id := c.generateID()
	ch := make(chan *clientpb.OutboundMessage, 1)

	c.pendingRPCMu.Lock()
	c.pendingRPC[id] = ch
	c.pendingRPCMu.Unlock()

	defer func() {
		c.pendingRPCMu.Lock()
		delete(c.pendingRPC, id)
		c.pendingRPCMu.Unlock()
		close(ch)
	}()

	msg := &clientpb.InboundMessage{
		Id:      id,
		Channel: channel,
		Method:  method,
		Envelope: &clientpb.InboundMessage_RpcRequest{
			RpcRequest: req,
		},
	}

	if err := c.transport.Send(c.ctx, msg); err != nil {
		return fmt.Errorf("rpc send failed: %w", err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case outMsg := <-ch:
		if outMsg == nil {
			return fmt.Errorf("rpc failed: no response")
		}

		if err := outMsg.GetError(); err != nil {
			return fmt.Errorf("rpc error: %s (code: %s)", err.GetMessage(), err.GetCode())
		}

		reply := outMsg.GetRpcReply()
		if reply == nil {
			return fmt.Errorf("rpc failed: no reply")
		}

		// Copy reply to resp
		if resp != nil {
			proto.Merge(resp, reply)
		}

		return nil
	}
}

// OnMessage sets the message handler.
func (c *client) OnMessage(fn func([]*Message)) {
	c.msgHandler = fn
}

// OnError sets the error handler.
func (c *client) OnError(fn func(error)) {
	c.errorHandler = fn
}

// OnConnected sets the connected handler.
func (c *client) OnConnected(fn func(string)) {
	c.connectedHandler = fn
}

// SessionID returns the session ID.
func (c *client) SessionID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sessionID
}

// IsConnected returns the connection status.
func (c *client) IsConnected() bool {
	return c.connected.Load()
}

// Close closes the connection.
func (c *client) Close() error {
	c.closed.Store(true)
	c.connected.Store(false)
	c.cancel()

	// Close connection channels
	c.mu.Lock()
	if c.connectedCh != nil {
		select {
		case <-c.connectedCh:
		default:
			close(c.connectedCh)
		}
		c.connectedCh = nil
	}
	if c.connectErrCh != nil {
		close(c.connectErrCh)
		c.connectErrCh = nil
	}
	c.mu.Unlock()

	// Clean up pending RPCs
	c.pendingRPCMu.Lock()
	for id, ch := range c.pendingRPC {
		delete(c.pendingRPC, id)
		close(ch)
	}
	c.pendingRPCMu.Unlock()

	return c.transport.Close()
}

// generateID generates a unique message ID.
func (c *client) generateID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), c.nextMsgID.Add(1))
}

// BuildConnectMessage builds a Connect message.
func BuildConnectMessage(opts *Options) *clientpb.InboundMessage {
	subs := make([]*clientpb.Subscription, 0)
	if len(opts.AutoSubscribe) > 0 {
		subs = make([]*clientpb.Subscription, len(opts.AutoSubscribe))
		for i, ch := range opts.AutoSubscribe {
			subs[i] = &clientpb.Subscription{
				Channel:   ch,
				Ephemeral: false,
			}
		}
	}

	return &clientpb.InboundMessage{
		Id:       "",
		Metadata: make(map[string]string),
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId:      opts.ClientID,
				ClientType:    opts.ClientType,
				Token:         opts.Token,
				Version:       opts.Version,
				Subscriptions: subs,
			},
		},
	}
}

// BuildSubscribeMessage builds a Subscribe message.
func BuildSubscribeMessage(channels ...string) *clientpb.InboundMessage {
	subs := make([]*clientpb.Subscription, len(channels))
	for i, ch := range channels {
		subs[i] = &clientpb.Subscription{
			Channel:   ch,
			Ephemeral: false,
		}
	}

	return &clientpb.InboundMessage{
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{
				Subscriptions: subs,
			},
		},
	}
}

// BuildUnsubscribeMessage builds an Unsubscribe message.
func BuildUnsubscribeMessage(channels ...string) *clientpb.InboundMessage {
	subs := make([]*clientpb.Subscription, len(channels))
	for i, ch := range channels {
		subs[i] = &clientpb.Subscription{
			Channel:   ch,
			Ephemeral: false,
		}
	}

	return &clientpb.InboundMessage{
		Envelope: &clientpb.InboundMessage_Unsubscribe{
			Unsubscribe: &clientpb.Unsubscribe{
				Subscriptions: subs,
			},
		},
	}
}

// BuildPublishMessage builds a Publish message.
func BuildPublishMessage(channel string, event *pb.CloudEvent) *clientpb.InboundMessage {
	return &clientpb.InboundMessage{
		Channel: channel,
		Envelope: &clientpb.InboundMessage_Publish{
			Publish: event,
		},
	}
}

// BuildRPCMessage builds an RPC request message.
func BuildRPCMessage(channel, method string, event *pb.CloudEvent) *clientpb.InboundMessage {
	return &clientpb.InboundMessage{
		Channel: channel,
		Method:  method,
		Envelope: &clientpb.InboundMessage_RpcRequest{
			RpcRequest: event,
		},
	}
}

// BuildErrorMessage builds an Error message.
func BuildErrorMessage(code, msgType, message string) *clientpb.OutboundMessage {
	return &clientpb.OutboundMessage{
		Envelope: &clientpb.OutboundMessage_Error{
			Error: &sharedpb.Error{
				Code:    code,
				Type:    msgType,
				Message: message,
			},
		},
	}
}

// BuildConnectedMessage builds a Connected message.
func BuildConnectedMessage(sessionID string, subscriptions []*clientpb.Subscription) *clientpb.OutboundMessage {
	return &clientpb.OutboundMessage{
		Envelope: &clientpb.OutboundMessage_Connected{
			Connected: &clientpb.Connected{
				SessionId:     sessionID,
				Subscriptions: subscriptions,
			},
		},
	}
}

// BuildSubscribeAckMessage builds a SubscribeAck message.
func BuildSubscribeAckMessage(subscriptions []*clientpb.Subscription) *clientpb.OutboundMessage {
	return &clientpb.OutboundMessage{
		Envelope: &clientpb.OutboundMessage_SubscribeAck{
			SubscribeAck: &clientpb.SubscribeAck{
				Subscriptions: subscriptions,
			},
		},
	}
}

// BuildPublicationMessage builds a Publication message.
func BuildPublicationMessage(messages []*clientpb.Message) *clientpb.OutboundMessage {
	return &clientpb.OutboundMessage{
		Envelope: &clientpb.OutboundMessage_Publication{
			Publication: &clientpb.Publication{
				Envelopes: messages,
			},
		},
	}
}

// BuildRPCReplyMessage builds an RPC reply message.
func BuildRPCReplyMessage(id string, event *pb.CloudEvent) *clientpb.OutboundMessage {
	return &clientpb.OutboundMessage{
		Id: id,
		Envelope: &clientpb.OutboundMessage_RpcReply{
			RpcReply: event,
		},
	}
}
