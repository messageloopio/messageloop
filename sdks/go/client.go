package messageloopgo

import (
	"context"
	"errors"
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
	// Publish publishes a message to a channel. Pass transient=true to skip
	// persistence and only deliver to currently connected subscribers.
	Publish(channel string, msg *Message, transient ...bool) error
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
	// SubscribeWith subscribes to a single channel with per-subscription options.
	SubscribeWith(channel string, opts ...SubscribeOption) error
	// SubRefresh asks the server to re-validate the subscriptions for the given
	// channels (e.g. after an ACL change on the backend).
	SubRefresh(ctx context.Context, channels ...string) error
	// SendSurveyReply sends a reply to a survey request issued by the server.
	SendSurveyReply(ctx context.Context, requestID string, reply *Message, replyErr error) error
	// OnSurvey sets the handler for survey requests from the server. The
	// handler returns the response payload. When no handler is set, the
	// request payload is echoed back to the server.
	OnSurvey(fn func(requestID string, req *Message) (*Message, error))
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
	handlerMu           sync.RWMutex
	msgHandler          func([]*Message)
	errorHandler        func(error)
	connectedHandler    func(string)
	reconnectingHandler func(int)
	reconnectedHandler  func(string)
	surveyHandler       func(requestID string, req *Message) (*Message, error)
	pendingRPC          map[string]*rpcPending
	pendingRPCMu        sync.RWMutex
	nextMsgID           atomic.Uint64
	subscriptions       map[string]bool // Channel -> ephemeral flag
	subMu               sync.RWMutex
	pingCancel          context.CancelFunc
	pongCh              chan struct{} // Signals pong receipt to the ping loop
	lastPong            atomic.Int64  // UnixNano of the last received pong

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
		pongCh:         make(chan struct{}, 1),
	}
	return c
}

// Connect connects to the server and starts the receive loop.
//
// Every attempt advances the connection generation and uses a fresh
// transport, mirroring reconnect(): a failed attempt closes its transport and
// terminates its receive loop, so a retry never leaks a receive loop or
// double-delivers from a superseded connection.
func (c *client) Connect(ctx context.Context) error {
	// Reuse the transport created by Dial/DialGRPC for the first attempt;
	// later attempts always dial a fresh transport and supersede the old one.
	c.mu.RLock()
	old := c.transport
	c.mu.RUnlock()

	var trans transport
	if c.generation.Load() == 0 && old != nil {
		trans = old
	} else {
		if old != nil {
			_ = old.Close()
		}
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
			return fmt.Errorf("dial failed: %w", err)
		}
	}

	gen := c.generation.Add(1)
	c.mu.Lock()
	c.transport = trans
	c.connectedCh = make(chan struct{})
	c.connectErrCh = make(chan error, 1)
	c.mu.Unlock()

	// First attempt: plain connect with auto-subscribe channels.
	// Later attempts: resume the previous session with recovery offsets.
	connectMsg := c.buildConnectMessage(c.generation.Load() > 1)

	ctx, cancel := context.WithTimeout(ctx, c.connectionTimeout())
	defer cancel()

	if err := trans.Send(ctx, connectMsg); err != nil {
		_ = trans.Close()
		return fmt.Errorf("send connect failed: %w", err)
	}

	// Start receive loop
	go c.receiveLoop(trans, gen)

	// Wait for connection to be established or an error. Close() closes both
	// channels after setting the closed flag, so a closed channel is
	// indistinguishable from a legitimately established connection unless the
	// closed flag (or the channel's ok value) is checked: a zero-value nil
	// receive from the closed connectErrCh must not be reported as success.
	c.mu.RLock()
	connCh := c.connectedCh
	errCh := c.connectErrCh
	c.mu.RUnlock()

	select {
	case <-connCh:
		if c.closed.Load() {
			_ = trans.Close()
			return errors.New("client closed")
		}
		return nil
	case err, ok := <-errCh:
		_ = trans.Close()
		if !ok {
			return errors.New("client closed")
		}
		return err
	case <-ctx.Done():
		_ = trans.Close()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("connection timeout")
		}
		return ctx.Err()
	}
}

// buildConnectMessage builds the Connect message for an attempt. When resume
// is true the message carries the previous session ID, epoch and per-channel
// recovery offsets so the server can resume the session.
func (c *client) buildConnectMessage(resume bool) *clientpb.InboundMessage {
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

	if !resume {
		// Add auto-subscribe channels
		if len(c.opts.AutoSubscribe) > 0 {
			subs := make([]*clientpb.Subscription, len(c.opts.AutoSubscribe))
			for i, ch := range c.opts.AutoSubscribe {
				subs[i] = &clientpb.Subscription{
					Channel: ch,
				}
			}
			connectMsg.GetConnect().Subscriptions = subs
		}
		return connectMsg
	}

	c.mu.RLock()
	sessionID := c.sessionID
	epoch := c.epoch
	c.mu.RUnlock()
	connectMsg.GetConnect().SessionId = sessionID
	connectMsg.GetConnect().Subscriptions = c.resumeSubscriptions(epoch)
	return connectMsg
}

// resumeSubscriptions builds the subscription list for a re-connecting client,
// carrying per-channel ephemeral flags, recovery offsets and the broker epoch.
func (c *client) resumeSubscriptions(epoch string) []*clientpb.Subscription {
	c.subMu.RLock()
	subs := make([]*clientpb.Subscription, 0, len(c.subscriptions))
	for ch, ephemeral := range c.subscriptions {
		sub := &clientpb.Subscription{
			Channel:   ch,
			Ephemeral: ephemeral,
			Recover:   true,
			Epoch:     epoch,
		}
		c.offsetMu.RLock()
		if offset, ok := c.channelOffsets[ch]; ok {
			sub.Offset = offset
		}
		c.offsetMu.RUnlock()
		subs = append(subs, sub)
	}
	c.subMu.RUnlock()
	return subs
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
				// Ignore failures from superseded transports: a newer Connect
				// or reconnect has already replaced this transport, and its
				// close must not trigger error callbacks or reconnection.
				c.mu.RLock()
				current := c.transport == trans
				c.mu.RUnlock()
				if !current {
					return
				}
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

	case *clientpb.OutboundMessage_SubRefreshAck:
		// The server acknowledged our SubRefresh request; there is nothing
		// to do client-side.

	case *clientpb.OutboundMessage_SurveyRequest:
		c.handleSurveyRequest(env.SurveyRequest)

	case *clientpb.OutboundMessage_SurveyReply:
		// The SDK does not initiate surveys, so replies from the server to a
		// survey we never sent are ignored.
	}
}

// handleConnected handles the Connected message.
func (c *client) handleConnected(connected *clientpb.Connected, gen uint64) {
	// Drop stale Connected responses from a superseded connection: they
	// would otherwise reset the reconnecting flag and session bookkeeping.
	// Also bail out if the client is already closed: Close() may have closed
	// and nilled connectedCh already.
	c.mu.Lock()
	if c.closed.Load() || c.generation.Load() != gen {
		c.mu.Unlock()
		return
	}

	c.sessionID = connected.GetSessionId()
	c.epoch = connected.GetEpoch()

	// Signal that the connection is established. Closing the channel under
	// the same lock as Close() serializes the two: Close() closes and nils
	// the channel under this lock, so neither close(nil) nor double-close
	// can occur, and the connected flag cannot be set after Close() has
	// completed its own store (which follows closed.Store(true)).
	if c.connectedCh != nil {
		select {
		case <-c.connectedCh:
			// Already closed
		default:
			close(c.connectedCh)
		}
	}
	c.connected.Store(true)
	c.mu.Unlock()

	wasReconnecting := c.reconnecting.Swap(false)

	// Track subscriptions. The server always returns the authoritative
	// subscription list (even for resumed sessions, where channels may have
	// been restored from a cluster snapshot), so write it back unconditionally.
	c.subMu.Lock()
	server := make(map[string]bool, len(connected.GetSubscriptions()))
	for _, sub := range connected.GetSubscriptions() {
		server[sub.GetChannel()] = sub.GetEphemeral()
		c.subscriptions[sub.GetChannel()] = sub.GetEphemeral()
	}
	for ch := range c.subscriptions {
		if _, ok := server[ch]; !ok {
			delete(c.subscriptions, ch)
		}
	}
	c.subMu.Unlock()

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
		if len(msgs) > 0 {
			c.handlerMu.RLock()
			handler := c.msgHandler
			c.handlerMu.RUnlock()
			if handler != nil {
				handler(msgs)
			}
		}
	}

	// Start ping loop
	c.startPingLoop()

	if wasReconnecting {
		c.handlerMu.RLock()
		reconnected := c.reconnectedHandler
		c.handlerMu.RUnlock()
		if reconnected != nil {
			reconnected(c.sessionID)
		}
	}
	c.handlerMu.RLock()
	connectedHandler := c.connectedHandler
	c.handlerMu.RUnlock()
	if connectedHandler != nil {
		connectedHandler(c.sessionID)
	}
}

// handleSubscribeAck handles the SubscribeAck message.
func (c *client) handleSubscribeAck(ack *clientpb.SubscribeAck) {
	for _, sub := range ack.GetSubscriptions() {
		c.subMu.Lock()
		c.subscriptions[sub.GetChannel()] = sub.GetEphemeral()
		c.subMu.Unlock()
	}
}

// handleUnsubscribeAck handles the UnsubscribeAck message.
func (c *client) handleUnsubscribeAck(ack *clientpb.UnsubscribeAck) {
	for _, sub := range ack.GetSubscriptions() {
		c.subMu.Lock()
		delete(c.subscriptions, sub.GetChannel())
		c.subMu.Unlock()
		// Drop the recovery offset so a later re-subscribe does not resume
		// from a stale offset and re-deliver history from the unsubscribed
		// period.
		c.offsetMu.Lock()
		delete(c.channelOffsets, sub.GetChannel())
		c.offsetMu.Unlock()
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
	if len(msgs) > 0 {
		c.handlerMu.RLock()
		handler := c.msgHandler
		c.handlerMu.RUnlock()
		if handler != nil {
			handler(msgs)
		}
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
	c.handlerMu.RLock()
	handler := c.errorHandler
	c.handlerMu.RUnlock()
	if handler != nil {
		handler(err)
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

// SubscribeOption configures a single subscription created by SubscribeWith.
type SubscribeOption func(*clientpb.Subscription)

// WithEphemeral marks the subscription as ephemeral: the server does not
// register presence for the channel and does not persist the subscription
// across reconnects. The default is false (persistent subscription).
func WithEphemeral(ephemeral bool) SubscribeOption {
	return func(s *clientpb.Subscription) {
		s.Ephemeral = ephemeral
	}
}

// Subscribe subscribes to channels.
func (c *client) Subscribe(channels ...string) error {
	subs := make([]*clientpb.Subscription, len(channels))
	for i, ch := range channels {
		subs[i] = &clientpb.Subscription{
			Channel: ch,
		}
	}
	return c.sendSubscribe(subs)
}

// SubscribeWith subscribes to a single channel with per-subscription options.
func (c *client) SubscribeWith(channel string, opts ...SubscribeOption) error {
	sub := &clientpb.Subscription{
		Channel: channel,
	}
	for _, opt := range opts {
		opt(sub)
	}
	return c.sendSubscribe([]*clientpb.Subscription{sub})
}

// sendSubscribe sends a Subscribe message with the given subscriptions.
func (c *client) sendSubscribe(subs []*clientpb.Subscription) error {
	if !c.connected.Load() {
		return fmt.Errorf("not connected")
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
			Ephemeral: c.isEphemeral(ch),
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

// isEphemeral reports whether the given channel was last subscribed with the
// ephemeral flag.
func (c *client) isEphemeral(ch string) bool {
	c.subMu.RLock()
	defer c.subMu.RUnlock()
	return c.subscriptions[ch]
}

// Publish publishes a message to a channel. The optional transient flag, when
// true, skips persistence and only delivers to currently connected subscribers.
func (c *client) Publish(channel string, msg *Message, transient ...bool) error {
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
				Channel:   channel,
				Payload:   payload,
				Transient: len(transient) > 0 && transient[0],
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

	// Apply the configured default timeout when the caller did not set a
	// deadline, so a dead connection cannot hang the call indefinitely.
	if c.opts.RPCTimeout > 0 {
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, c.opts.RPCTimeout)
			defer cancel()
		}
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
	c.handlerMu.Lock()
	c.msgHandler = fn
	c.handlerMu.Unlock()
}

// OnError sets the error handler.
func (c *client) OnError(fn func(error)) {
	c.handlerMu.Lock()
	c.errorHandler = fn
	c.handlerMu.Unlock()
}

// OnConnected sets the connected handler.
func (c *client) OnConnected(fn func(string)) {
	c.handlerMu.Lock()
	c.connectedHandler = fn
	c.handlerMu.Unlock()
}

// OnReconnecting sets the handler called before each reconnect attempt.
func (c *client) OnReconnecting(fn func(attempt int)) {
	c.handlerMu.Lock()
	c.reconnectingHandler = fn
	c.handlerMu.Unlock()
}

// OnReconnected sets the handler called after a successful reconnect.
func (c *client) OnReconnected(fn func(sessionID string)) {
	c.handlerMu.Lock()
	c.reconnectedHandler = fn
	c.handlerMu.Unlock()
}

// OnSurvey sets the handler for survey requests from the server. The handler
// receives the survey request ID and payload and returns the response payload.
// When no handler is set, the request payload is echoed back to the server.
func (c *client) OnSurvey(fn func(requestID string, req *Message) (*Message, error)) {
	c.handlerMu.Lock()
	c.surveyHandler = fn
	c.handlerMu.Unlock()
}

// SubRefresh asks the server to re-validate the subscriptions for the given
// channels (e.g. after an ACL change on the backend).
func (c *client) SubRefresh(ctx context.Context, channels ...string) error {
	if !c.connected.Load() {
		return fmt.Errorf("not connected")
	}

	msg := &clientpb.InboundMessage{
		Id: c.generateID(),
		Envelope: &clientpb.InboundMessage_SubRefresh{
			SubRefresh: &clientpb.SubRefresh{
				Channels: channels,
			},
		},
	}

	c.mu.RLock()
	trans := c.transport
	c.mu.RUnlock()
	if err := trans.Send(ctx, msg); err != nil {
		return fmt.Errorf("sub refresh failed: %w", err)
	}

	return nil
}

// SendSurveyReply sends a reply to a survey request issued by the server.
// When replyErr is non-nil it is carried in the reply's error field instead of
// the payload.
func (c *client) SendSurveyReply(ctx context.Context, requestID string, reply *Message, replyErr error) error {
	if !c.connected.Load() {
		return fmt.Errorf("not connected")
	}

	var payload *sharedpb.Payload
	if reply != nil {
		p, err := reply.ToPayload()
		if err != nil {
			return fmt.Errorf("failed to convert reply message: %w", err)
		}
		payload = p
	}

	var pbErr *sharedpb.Error
	if replyErr != nil {
		pbErr = &sharedpb.Error{
			Code:    "SURVEY_REPLY_ERROR",
			Type:    "survey_error",
			Message: replyErr.Error(),
		}
	}

	msg := &clientpb.InboundMessage{
		Id: c.generateID(),
		Envelope: &clientpb.InboundMessage_SurveyReply{
			SurveyReply: &clientpb.SurveyReply{
				RequestId: requestID,
				Payload:   payload,
				Error:     pbErr,
			},
		},
	}

	c.mu.RLock()
	trans := c.transport
	c.mu.RUnlock()
	if err := trans.Send(ctx, msg); err != nil {
		return fmt.Errorf("survey reply failed: %w", err)
	}

	return nil
}

// handleSurveyRequest handles a SurveyRequest from the server, dispatching to
// the user survey handler or echoing the payload back by default.
func (c *client) handleSurveyRequest(req *clientpb.SurveyRequest) {
	requestID := req.GetRequestId()
	reqMsg := PayloadToMessage(req.GetPayload(), "")

	c.handlerMu.RLock()
	handler := c.surveyHandler
	c.handlerMu.RUnlock()

	if handler == nil {
		// Default: echo the request payload back, mirroring the server's own
		// default survey behavior.
		_ = c.SendSurveyReply(context.Background(), requestID, reqMsg, nil)
		return
	}

	reply, err := handler(requestID, reqMsg)
	if err != nil {
		_ = c.SendSurveyReply(context.Background(), requestID, nil, err)
		return
	}
	_ = c.SendSurveyReply(context.Background(), requestID, reply, nil)
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
			c.handlerMu.RLock()
			errorHandler := c.errorHandler
			c.handlerMu.RUnlock()
			if errorHandler != nil {
				errorHandler(fmt.Errorf("reconnect failed after %d attempts", c.opts.ReconnectMaxAttempts))
			}
			return
		}

		c.handlerMu.RLock()
		reconnectingHandler := c.reconnectingHandler
		errorHandler := c.errorHandler
		c.handlerMu.RUnlock()
		if reconnectingHandler != nil {
			reconnectingHandler(attempt)
		}

		select {
		case <-c.ctx.Done():
			c.reconnecting.Store(false)
			return
		case <-time.After(delay):
		}

		if err := c.reconnect(); err != nil {
			if errorHandler != nil {
				errorHandler(fmt.Errorf("reconnect attempt %d failed: %w", attempt, err))
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
	connectMsg.GetConnect().Subscriptions = c.resumeSubscriptions(epoch)

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
		if c.closed.Load() {
			_ = trans.Close()
			return errors.New("client closed")
		}
		return nil
	case err, ok := <-errCh:
		_ = trans.Close()
		if !ok {
			return errors.New("client closed")
		}
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

// BuildPublishMessage builds a Publish message. The optional transient flag,
// when true, skips persistence and only delivers to currently connected
// subscribers.
//
// Note: a Message whose payload cannot be converted to protobuf (e.g. invalid
// JSON data) is silently serialized with an empty payload because this
// constructor cannot return an error.
func BuildPublishMessage(channel string, msg *Message, transient ...bool) *clientpb.InboundMessage {
	payload, _ := msg.ToPayload() // Ignore error for backward compatibility
	return &clientpb.InboundMessage{
		Envelope: &clientpb.InboundMessage_Publish{
			Publish: &clientpb.Publish{
				Channel:   channel,
				Payload:   payload,
				Transient: len(transient) > 0 && transient[0],
			},
		},
	}
}

// BuildRPCMessage builds an RPC request message.
//
// Note: a Message whose payload cannot be converted to protobuf (e.g. invalid
// JSON data) is silently serialized with an empty payload because this
// constructor cannot return an error.
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
//
// Note: a Message whose payload cannot be converted to protobuf (e.g. invalid
// JSON data) is silently serialized with an empty payload because this
// constructor cannot return an error.
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

	c.lastPong.Store(time.Now().UnixNano())

	go c.pingLoop(pingCtx)
}

// pingLoop sends ping messages at regular intervals and watches for pong
// timeouts. After each ping a deadline of PingTimeout is armed; if the pong
// for that ping does not arrive in time, the connection is assumed half-open
// and the transport is closed so the receive loop observes the failure and,
// when enabled, the reconnect flow takes over.
func (c *client) pingLoop(ctx context.Context) {
	ticker := time.NewTicker(c.opts.PingInterval)
	defer ticker.Stop()

	var timer *time.Timer
	var timerCh <-chan time.Time
	var armedAt time.Time

	// arm starts the pong deadline for the ping just sent.
	arm := func() {
		if c.opts.PingTimeout <= 0 {
			timerCh = nil
			armedAt = time.Time{}
			return
		}
		if timer == nil {
			timer = time.NewTimer(c.opts.PingTimeout)
		} else if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(c.opts.PingTimeout)
		timerCh = timer.C
		armedAt = time.Now()
	}

	// disarm cancels the pending pong deadline.
	disarm := func() {
		timerCh = nil
		armedAt = time.Time{}
		if timer != nil {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.pongCh:
			// The pong for the in-flight ping arrived: the connection is
			// alive, so the pending deadline no longer applies. The next ping
			// re-arms it.
			disarm()
		case <-timerCh:
			// The deadline fired without a pong. A pong recorded after the
			// deadline was armed means the pong and the deadline raced in the
			// select above — the connection is fine. Otherwise the connection
			// is half-open: close the transport so the receive loop triggers
			// the reconnect flow.
			if armedAt.IsZero() || c.lastPong.Load() >= armedAt.UnixNano() {
				continue
			}
			c.mu.RLock()
			trans := c.transport
			c.mu.RUnlock()
			if trans != nil {
				_ = trans.Close()
			}
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
			arm()
		}
	}
}

// handlePong handles a pong response from the server, recording its arrival
// for the ping loop's pong timeout detection.
func (c *client) handlePong() {
	c.lastPong.Store(time.Now().UnixNano())
	select {
	case c.pongCh <- struct{}{}:
	default:
	}
}
