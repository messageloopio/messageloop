package messageloopgo

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
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
	Publish(channel string, msg *Message) error
	// RPC sends an RPC request and waits for a response
	RPC(ctx context.Context, channel, method string, req, resp *Message) error
	// OnMessage sets the message handler
	OnMessage(fn func([]*Message))
	// OnError sets the error handler
	OnError(fn func(error))
	// OnConnected sets the connected handler
	OnConnected(fn func(sessionID string))
	// OnReconnecting sets the reconnecting handler, called before each attempt.
	OnReconnecting(fn func(attempt int))
	// OnReconnected sets the reconnected handler, called after successful reconnect.
	OnReconnected(fn func(sessionID string))
	// SessionID returns the session ID
	SessionID() string
	// IsConnected returns the connection status
	IsConnected() bool
}

// rpcPending tracks a pending RPC call and its response channel.
// The channel close is guarded by sync.Once so concurrent close from
// RPC's defer and Client.Close cannot double-close and panic.
type rpcPending struct {
	ch   chan *clientpb.OutboundMessage
	once sync.Once
}

// close closes the response channel exactly once.
func (r *rpcPending) close() {
	r.once.Do(func() { close(r.ch) })
}

// client is the implementation of the Client interface.
type client struct {
	mu                  sync.RWMutex
	ctx                 context.Context
	cancel              context.CancelFunc
	transport           transport
	opts                *Options
	sessionID           string
	connected           atomic.Bool
	closed              atomic.Bool
	reconnecting        atomic.Bool
	generation          atomic.Uint64 // Connection generation, advanced on every reconnect
	connectedCh         chan struct{} // Closed when connection is established
	connectErrCh        chan error    // For connection errors
	msgHandler          func([]*Message)
	errorHandler        func(error)
	connectedHandler    func(string)
	reconnectingHandler func(int)
	reconnectedHandler  func(string)
	pendingRPC          map[string]*rpcPending
	pendingRPCMu        sync.RWMutex
	nextMsgID           atomic.Uint64
	subscriptions       map[string]bool
	subMu               sync.RWMutex
	pingCancel          context.CancelFunc

	// Session resumption state
	epoch          string
	channelOffsets map[string]uint64
	offsetMu       sync.RWMutex

	// Reconnection: stores connection parameters for re-dialing
	dialURL  string // WebSocket URL (empty for gRPC)
	dialAddr string // gRPC address (empty for WebSocket)

	// newTransport overrides the transport factory used by reconnect (tests).
	newTransport func() (transport, error)
	// connectTimeout overrides the default connect timeout when non-zero (tests).
	connectTimeout time.Duration
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

	c := newClient(ctx, cancel, trans, options)
	c.dialURL = url
	return c, nil
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

	c := newClient(ctx, cancel, trans, options)
	c.dialAddr = addr
	return c, nil
}

// newClient creates a new client with the given transport.
func newClient(ctx context.Context, cancel context.CancelFunc, trans transport, opts *Options) *client {
	c := &client{
		ctx:            ctx,
		cancel:         cancel,
		transport:      trans,
		opts:           opts,
		connectedCh:    make(chan struct{}),
		connectErrCh:   make(chan error, 1),
		pendingRPC:     make(map[string]*rpcPending),
		subscriptions:  make(map[string]bool),
		channelOffsets: make(map[string]uint64),
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
		Id: c.generateID(),
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

	c.mu.RLock()
	trans := c.transport
	c.mu.RUnlock()
	if err := trans.Send(ctx, connectMsg); err != nil {
		return fmt.Errorf("send connect failed: %w", err)
	}

	// Start receive loop
	go c.receiveLoop(trans, 0)

	// Wait for connection to be established or an error
	select {
	case <-c.connectedCh:
		return nil
	case err := <-c.connectErrCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(c.connectionTimeout()):
		return fmt.Errorf("connection timeout")
	}
}

// receiveLoop is the main receive loop. It is bound to a single transport
// and its connection generation, so it never reads from a superseded
// transport after a reconnect swaps in a new one.
func (c *client) receiveLoop(trans transport, gen uint64) {
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		msg, err := trans.Recv(c.ctx)
		if err != nil {
			if !c.closed.Load() {
				isConnError := !c.connected.Load()
				c.handleError(fmt.Errorf("receive error: %w", err), isConnError)
				c.connected.Store(false)
				// Attempt reconnection if enabled
				if c.opts.AutoReconnect && !c.closed.Load() {
					go c.reconnectLoop()
				}
			}
			return
		}

		c.handleMessage(msg, gen)
	}
}

// handleMessage handles an incoming message from the server.
func (c *client) handleMessage(msg *clientpb.OutboundMessage, gen uint64) {
	switch env := msg.GetEnvelope().(type) {
	case *clientpb.OutboundMessage_Connected:
		c.handleConnected(env.Connected, gen)

	case *clientpb.OutboundMessage_Error:
		// If the error references a pending RPC request, deliver it to the
		// RPC caller so the call fails fast with the server error instead of
		// hanging until the context deadline.
		if !c.deliverPending(msg) {
			err := fmt.Errorf("server error: %s (code: %s)", env.Error.GetMessage(), env.Error.GetCode())
			c.handleError(err, !c.connected.Load())
		}

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
		// Handle pong response from server
		c.handlePong()
	}
}

// handleConnected handles the Connected message.
func (c *client) handleConnected(connected *clientpb.Connected, gen uint64) {
	// Drop stale Connected responses from a superseded connection: they
	// would otherwise reset the reconnecting flag and session bookkeeping.
	if c.generation.Load() != gen {
		return
	}

	c.mu.Lock()
	c.sessionID = connected.GetSessionId()
	c.epoch = connected.GetEpoch()
	resumed := connected.GetResumed()
	ch := c.connectedCh
	c.mu.Unlock()
	c.connected.Store(true)

	wasReconnecting := c.reconnecting.Swap(false)

	// Signal that connection is established
	select {
	case <-ch:
		// Already closed
	default:
		close(ch)
	}

	// Track subscriptions (skip if session was resumed — server preserved them)
	if !resumed {
		for _, sub := range connected.GetSubscriptions() {
			c.subMu.Lock()
			c.subscriptions[sub.GetChannel()] = true
			c.subMu.Unlock()
		}
	}

	// Handle initial publications (recovery messages)
	for _, pub := range connected.GetPublications() {
		// Update offsets from recovered publications
		for _, env := range pub.GetMessages() {
			if env != nil && env.GetOffset() > 0 {
				c.offsetMu.Lock()
				c.channelOffsets[env.GetChannel()] = env.GetOffset()
				c.offsetMu.Unlock()
			}
		}
		msgs := wrapPublicationToMessages(pub)
		if c.msgHandler != nil && len(msgs) > 0 {
			c.msgHandler(msgs)
		}
	}

	// Start ping loop
	c.startPingLoop()

	if wasReconnecting && c.reconnectedHandler != nil {
		c.reconnectedHandler(c.sessionID)
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
	// Update per-channel offsets for session resumption
	for _, env := range pub.GetMessages() {
		if env != nil && env.GetOffset() > 0 {
			c.offsetMu.Lock()
			c.channelOffsets[env.GetChannel()] = env.GetOffset()
			c.offsetMu.Unlock()
		}
	}
	msgs := wrapPublicationToMessages(pub)
	if c.msgHandler != nil && len(msgs) > 0 {
		c.msgHandler(msgs)
	}
}

// handleRPCReply handles the RPC reply message.
func (c *client) handleRPCReply(msg *clientpb.OutboundMessage, reply *clientpb.RpcReply) {
	c.deliverPending(msg)
}

// deliverPending delivers msg to the pending RPC with the matching ID, if
// any, and removes the entry. It reports whether the message was routed to a
// pending RPC.
//
// The delivery is non-blocking and happens under the pendingRPC write lock:
// RPC's deferred cleanup and Client.Close only close the response channel
// while holding (or after acquiring) the same lock, so the send cannot race
// with the channel close.
func (c *client) deliverPending(msg *clientpb.OutboundMessage) bool {
	id := msg.GetId()
	if id == "" {
		return false
	}

	c.pendingRPCMu.Lock()
	rp, ok := c.pendingRPC[id]
	if ok {
		delete(c.pendingRPC, id)
		select {
		case rp.ch <- msg:
		default:
			// Channel is full, discard
		}
	}
	c.pendingRPCMu.Unlock()
	return ok
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

	c.mu.RLock()
	trans := c.transport
	c.mu.RUnlock()
	if err := trans.Send(c.ctx, msg); err != nil {
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

	c.mu.RLock()
	trans := c.transport
	c.mu.RUnlock()
	if err := trans.Send(c.ctx, msg); err != nil {
		return fmt.Errorf("unsubscribe failed: %w", err)
	}

	return nil
}

// Publish publishes a message to a channel.
func (c *client) Publish(channel string, msg *Message) error {
	if !c.connected.Load() {
		return fmt.Errorf("not connected")
	}

	// Convert Message to Payload
	payload, err := msg.ToPayload()
	if err != nil {
		return fmt.Errorf("failed to convert message: %w", err)
	}

	pbMsg := &clientpb.InboundMessage{
		Id: c.generateID(),
		Envelope: &clientpb.InboundMessage_Publish{
			Publish: &clientpb.Publish{
				Channel: channel,
				Payload: payload,
			},
		},
	}

	c.mu.RLock()
	trans := c.transport
	c.mu.RUnlock()
	if err := trans.Send(c.ctx, pbMsg); err != nil {
		return fmt.Errorf("publish failed: %w", err)
	}

	return nil
}

// RPC sends an RPC request and waits for a response.
func (c *client) RPC(ctx context.Context, channel, method string, req, resp *Message) error {
	if !c.connected.Load() {
		return fmt.Errorf("not connected")
	}

	// Convert request Message to Payload
	reqPayload, err := req.ToPayload()
	if err != nil {
		return fmt.Errorf("failed to convert request message: %w", err)
	}

	id := c.generateID()
	rp := &rpcPending{ch: make(chan *clientpb.OutboundMessage, 1)}

	c.pendingRPCMu.Lock()
	c.pendingRPC[id] = rp
	c.pendingRPCMu.Unlock()

	defer func() {
		c.pendingRPCMu.Lock()
		delete(c.pendingRPC, id)
		c.pendingRPCMu.Unlock()
		rp.close()
	}()

	msg := &clientpb.InboundMessage{
		Id: id,
		Envelope: &clientpb.InboundMessage_RpcRequest{
			RpcRequest: &clientpb.RpcRequest{
				Channel: channel,
				Method:  method,
				Payload: reqPayload,
			},
		},
	}

	c.mu.RLock()
	trans := c.transport
	c.mu.RUnlock()
	if err := trans.Send(c.ctx, msg); err != nil {
		return fmt.Errorf("rpc send failed: %w", err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case outMsg := <-rp.ch:
		if outMsg == nil {
			return fmt.Errorf("rpc failed: no response")
		}

		if err := outMsg.GetError(); err != nil {
			return fmt.Errorf("rpc error: %s (code: %s)", err.GetMessage(), err.GetCode())
		}

		pbReply := outMsg.GetRpcReply()
		if pbReply == nil {
			return fmt.Errorf("rpc failed: no reply")
		}

		// Check for error in reply
		if pbReply.GetError() != nil {
			return fmt.Errorf("rpc error: %s (code: %s)", pbReply.GetError().GetMessage(), pbReply.GetError().GetCode())
		}

		// Convert Payload reply to Message
		if resp != nil && pbReply.GetPayload() != nil {
			replyMsg := PayloadToMessage(pbReply.GetPayload(), "")
			// Copy the reply message to resp
			*resp = *replyMsg
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

// OnReconnecting sets the handler called before each reconnect attempt.
func (c *client) OnReconnecting(fn func(attempt int)) {
	c.reconnectingHandler = fn
}

// OnReconnected sets the handler called after a successful reconnect.
func (c *client) OnReconnected(fn func(sessionID string)) {
	c.reconnectedHandler = fn
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

// reconnectLoop attempts to reconnect with exponential backoff.
func (c *client) reconnectLoop() {
	if !c.reconnecting.CompareAndSwap(false, true) {
		return // Already reconnecting
	}

	// Stop the ping loop during reconnection
	c.mu.Lock()
	if c.pingCancel != nil {
		c.pingCancel()
		c.pingCancel = nil
	}
	c.mu.Unlock()

	delay := c.opts.ReconnectInitialDelay
	for attempt := 1; ; attempt++ {
		if c.closed.Load() {
			c.reconnecting.Store(false)
			return
		}
		if c.opts.ReconnectMaxAttempts > 0 && attempt > c.opts.ReconnectMaxAttempts {
			c.reconnecting.Store(false)
			if c.errorHandler != nil {
				c.errorHandler(fmt.Errorf("reconnect failed after %d attempts", c.opts.ReconnectMaxAttempts))
			}
			return
		}

		if c.reconnectingHandler != nil {
			c.reconnectingHandler(attempt)
		}

		select {
		case <-c.ctx.Done():
			c.reconnecting.Store(false)
			return
		case <-time.After(delay):
		}

		if err := c.reconnect(); err != nil {
			if c.errorHandler != nil {
				c.errorHandler(fmt.Errorf("reconnect attempt %d failed: %w", attempt, err))
			}
			delay = time.Duration(float64(delay) * c.opts.ReconnectBackoffFactor)
			if delay > c.opts.ReconnectMaxDelay {
				delay = c.opts.ReconnectMaxDelay
			}
			continue
		}
		return // reconnect succeeded, handleConnected will clear reconnecting flag
	}
}

// reconnect creates a new transport and sends a Connect with session resumption.
func (c *client) reconnect() error {
	// Close old transport
	c.mu.RLock()
	old := c.transport
	c.mu.RUnlock()
	_ = old.Close()

	// Create new transport
	var trans transport
	var err error
	if c.newTransport != nil {
		trans, err = c.newTransport()
	} else if c.dialURL != "" {
		trans, err = newWSTransport(c.dialURL, c.opts.Encoding, c.opts.DialTimeout)
	} else if c.dialAddr != "" {
		trans, err = newGRPCTransport(c.ctx, c.dialAddr)
	} else {
		return fmt.Errorf("no dial address configured")
	}
	if err != nil {
		return err
	}

	// Every new transport advances the connection generation so stale
	// Connected responses from superseded connections can be recognized.
	gen := c.generation.Add(1)
	c.mu.Lock()
	c.transport = trans
	c.mu.Unlock()

	// Build Connect message with session resumption
	c.mu.RLock()
	sessionID := c.sessionID
	epoch := c.epoch
	c.mu.RUnlock()

	connectMsg := &clientpb.InboundMessage{
		Id: c.generateID(),
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId:   c.opts.ClientID,
				ClientType: c.opts.ClientType,
				Token:      c.opts.Token,
				Version:    c.opts.Version,
				SessionId:  sessionID,
			},
		},
	}

	// Build subscriptions with recovery offsets
	c.subMu.RLock()
	subs := make([]*clientpb.Subscription, 0, len(c.subscriptions))
	for ch := range c.subscriptions {
		sub := &clientpb.Subscription{
			Channel: ch,
			Recover: true,
			Epoch:   epoch,
		}
		c.offsetMu.RLock()
		if offset, ok := c.channelOffsets[ch]; ok {
			sub.Offset = offset
		}
		c.offsetMu.RUnlock()
		subs = append(subs, sub)
	}
	c.subMu.RUnlock()
	connectMsg.GetConnect().Subscriptions = subs

	// Reset connection channels
	c.mu.Lock()
	c.connectedCh = make(chan struct{})
	c.connectErrCh = make(chan error, 1)
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(c.ctx, c.connectionTimeout())
	defer cancel()

	c.mu.RLock()
	cur := c.transport
	c.mu.RUnlock()
	if err := cur.Send(ctx, connectMsg); err != nil {
		_ = trans.Close()
		return fmt.Errorf("send connect failed: %w", err)
	}

	// Start receive loop
	go c.receiveLoop(trans, gen)

	// Wait for connection
	c.mu.RLock()
	connCh := c.connectedCh
	errCh := c.connectErrCh
	c.mu.RUnlock()

	select {
	case <-connCh:
		return nil
	case err := <-errCh:
		_ = trans.Close()
		return err
	case <-ctx.Done():
		_ = trans.Close()
		return fmt.Errorf("reconnect timeout")
	}
}

// connectionTimeout returns the timeout for a single connection attempt,
// or the test override when set.
func (c *client) connectionTimeout() time.Duration {
	if c.connectTimeout > 0 {
		return c.connectTimeout
	}
	return 30 * time.Second
}

// Close closes the connection.
func (c *client) Close() error {
	c.closed.Store(true)
	c.connected.Store(false)

	// Cancel ping loop
	c.mu.Lock()
	if c.pingCancel != nil {
		c.pingCancel()
		c.pingCancel = nil
	}
	c.mu.Unlock()

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
	for id, rp := range c.pendingRPC {
		delete(c.pendingRPC, id)
		rp.close()
	}
	c.pendingRPCMu.Unlock()

	c.mu.RLock()
	trans := c.transport
	c.mu.RUnlock()
	return trans.Close()
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
		Id: "",
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
func BuildPublishMessage(channel string, msg *Message) *clientpb.InboundMessage {
	payload, _ := msg.ToPayload() // Ignore error for backward compatibility
	return &clientpb.InboundMessage{
		Envelope: &clientpb.InboundMessage_Publish{
			Publish: &clientpb.Publish{
				Channel: channel,
				Payload: payload,
			},
		},
	}
}

// BuildRPCMessage builds an RPC request message.
func BuildRPCMessage(channel, method string, msg *Message) *clientpb.InboundMessage {
	payload, _ := msg.ToPayload() // Ignore error for backward compatibility
	return &clientpb.InboundMessage{
		Envelope: &clientpb.InboundMessage_RpcRequest{
			RpcRequest: &clientpb.RpcRequest{
				Channel: channel,
				Method:  method,
				Payload: payload,
			},
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
				Messages: messages,
			},
		},
	}
}

// BuildRPCReplyMessage builds an RPC reply message.
func BuildRPCReplyMessage(id string, msg *Message) *clientpb.OutboundMessage {
	payload, _ := msg.ToPayload() // Ignore error for backward compatibility
	return &clientpb.OutboundMessage{
		Id: id,
		Envelope: &clientpb.OutboundMessage_RpcReply{
			RpcReply: &clientpb.RpcReply{
				RequestId: id,
				Payload:   payload,
			},
		},
	}
}

// Ping loop and heartbeat methods

// startPingLoop starts the ping loop if configured.
func (c *client) startPingLoop() {
	if c.opts.PingInterval <= 0 {
		return
	}

	c.mu.Lock()
	if c.pingCancel != nil {
		c.mu.Unlock()
		return
	}

	pingCtx, cancel := context.WithCancel(c.ctx)
	c.pingCancel = cancel
	c.mu.Unlock()

	go c.pingLoop(pingCtx)
}

// pingLoop sends ping messages at regular intervals.
func (c *client) pingLoop(ctx context.Context) {
	ticker := time.NewTicker(c.opts.PingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !c.connected.Load() {
				return
			}

			pingMsg := &clientpb.InboundMessage{
				Id: c.generateID(),
				Envelope: &clientpb.InboundMessage_Ping{
					Ping: &clientpb.Ping{},
				},
			}

			c.mu.RLock()
			trans := c.transport
			c.mu.RUnlock()
			if err := trans.Send(ctx, pingMsg); err != nil {
				// Log error but don't break the loop
				// The connection will be closed by receive loop if there's a real error
				continue
			}
		}
	}
}

// handlePong handles a pong response from the server.
func (c *client) handlePong() {
	// Pong received - the connection is alive
	// Could add more sophisticated tracking here if needed
}
