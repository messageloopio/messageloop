package messageloopgo

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
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
	// PublishWith publishes a message with per-publish options.
	PublishWith(channel string, msg *Message, opts ...PublishOption) error
	// PublishWithAck publishes a message and waits for the server's PublishAck,
	// returning the broker-assigned offset.
	PublishWithAck(ctx context.Context, channel string, msg *Message, opts ...PublishOption) (uint64, error)
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
	// OnSurveyRequest sets the handler for survey requests from the server,
	// additionally receiving the request channel. When set it takes
	// precedence over the handler registered with OnSurvey.
	OnSurveyRequest(fn func(requestID, channel string, req *Message) (*Message, error))
	// Survey initiates a survey on an exact channel and waits for the
	// aggregated answers. The call waits on the caller's goroutine; the
	// receive loop fills the pending result. timeout<=0 lets the server
	// apply its policy cap.
	Survey(ctx context.Context, channel string, payload *Message, timeout time.Duration) ([]SurveyAnswer, error)
	// OnPresence sets the handler for presence events (join/leave).
	OnPresence(fn func(PresenceEvent))
	// OnPresenceSnapshot sets the handler for presence snapshots delivered
	// with Connected / SubscribeAck, and for the snapshot returned by a
	// Presence query.
	OnPresenceSnapshot(fn func(PresenceSnapshot))
	// OnGapNotice sets the handler for catch-up gap notices (C6). The notice
	// is informational and never advances the recovery cursor.
	OnGapNotice(fn func(GapNotice))
	// Presence queries the current presence snapshot of an exact channel and
	// waits for the server's reply on the caller's goroutine.
	Presence(ctx context.Context, channel string) (*PresenceSnapshot, error)
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

// subscriptionState tracks the per-channel subscription options that must be
// restored when the subscription is resumed after a reconnect.
type subscriptionState struct {
	ephemeral bool
	token     string
}

// ackPending tracks a pending publish waiting for its PublishAck. The once
// guard makes resolve/reject idempotent so concurrent delivery and disconnect
// or Close cleanup cannot double-send on the channel.
type ackPending struct {
	ch   chan ackResult
	once sync.Once
}

// ackResult is the outcome of a pending publish.
type ackResult struct {
	offset uint64
	err    error
}

// surveyPending tracks a pending client-initiated survey and its result
// channel. The once guard makes resolve idempotent so concurrent delivery and
// disconnect or Close cleanup cannot double-send on the channel.
type surveyPending struct {
	requestID string
	ch        chan surveyPendingResult
	once      sync.Once
}

// surveyPendingResult is the outcome of a pending survey: either a
// SurveyResult envelope or a top-level error.
type surveyPendingResult struct {
	result *clientpb.SurveyResult
	err    error
}

// resolve delivers the pending survey outcome, at most once.
func (s *surveyPending) resolve(res surveyPendingResult) {
	s.once.Do(func() { s.ch <- res })
}

// resolve delivers the broker-assigned offset, at most once.
func (a *ackPending) resolve(offset uint64) {
	a.once.Do(func() { a.ch <- ackResult{offset: offset} })
}

// reject fails the pending publish with err, at most once.
func (a *ackPending) reject(err error) {
	a.once.Do(func() { a.ch <- ackResult{err: err} })
}

// client is the implementation of the Client interface.
type client struct {
	mu                      sync.RWMutex
	ctx                     context.Context
	cancel                  context.CancelFunc
	transport               transport
	opts                    *Options
	sessionID               string
	connected               atomic.Bool
	closed                  atomic.Bool
	reconnecting            atomic.Bool
	generation              atomic.Uint64 // Connection generation, advanced on every reconnect
	connectedCh             chan struct{} // Closed when connection is established
	connectErrCh            chan error    // For connection errors
	handlerMu               sync.RWMutex
	msgHandler              func([]*Message)
	errorHandler            func(error)
	connectedHandler        func(string)
	reconnectingHandler     func(int)
	reconnectedHandler      func(string)
	surveyHandler           func(requestID string, req *Message) (*Message, error)
	surveyRequestHandler    func(requestID, channel string, req *Message) (*Message, error)
	presenceHandler         func(PresenceEvent)
	presenceSnapshotHandler func(PresenceSnapshot)
	gapNoticeHandler        func(GapNotice)
	pendingRPC              map[string]*rpcPending
	pendingRPCMu            sync.RWMutex
	pendingAck              map[string]*ackPending // Publish id -> pending publish awaiting its PublishAck
	pendingAckMu            sync.RWMutex
	pendingPresence         map[string]*rpcPending // PresenceQuery id -> pending query
	pendingPresenceMu       sync.RWMutex
	pendingSurvey           map[string]*surveyPending // Survey inbound id -> pending survey
	pendingSurveyMu         sync.RWMutex
	nextMsgID               atomic.Uint64
	subscriptions           map[string]*subscriptionState // Channel -> subscription state
	subMu                   sync.RWMutex
	pingCancel              context.CancelFunc
	pongCh                  chan struct{} // Signals pong receipt to the ping loop
	lastPong                atomic.Int64  // UnixNano of the last received pong

	// Session resumption state
	epoch          string
	channelOffsets map[string]uint64
	offsetMu       sync.RWMutex

	// Reconnection: stores connection parameters for re-dialing
	dialURL  string // WebSocket URL (empty for gRPC/QUIC)
	dialAddr string // gRPC address (empty for WebSocket/QUIC)
	dialQUIC string // QUIC host:port (empty for WebSocket/gRPC)

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

// DialQUIC creates a new QUIC client connecting to the specified host:port.
// QUIC requires TLS 1.3; pass WithInsecureSkipVerify when the server is
// running with transport.quic.insecure (self-signed).
func DialQUIC(addr string, opts ...Option) (Client, error) {
	options := defaultOptions()
	for _, opt := range opts {
		opt(options)
	}

	ctx, cancel := context.WithCancel(context.Background())

	trans, err := newQUICTransport(ctx, addr, options.Encoding, options.DialTimeout, quicTLSConfig(options))
	if err != nil {
		cancel()
		return nil, err
	}

	c := newClient(ctx, cancel, trans, options)
	c.dialQUIC = addr
	return c, nil
}

func quicTLSConfig(opts *Options) *tls.Config {
	var cfg *tls.Config
	if opts != nil && opts.TLSConfig != nil {
		cfg = opts.TLSConfig.Clone()
	} else {
		cfg = &tls.Config{}
	}
	if opts != nil && opts.InsecureSkipVerify {
		cfg.InsecureSkipVerify = true
	}
	return cfg
}

func (c *client) dialTransport() (transport, error) {
	if c.newTransport != nil {
		return c.newTransport()
	}
	if c.dialURL != "" {
		return newWSTransport(c.dialURL, c.opts.Encoding, c.opts.DialTimeout)
	}
	if c.dialQUIC != "" {
		return newQUICTransport(c.ctx, c.dialQUIC, c.opts.Encoding, c.opts.DialTimeout, quicTLSConfig(c.opts))
	}
	if c.dialAddr != "" {
		return newGRPCTransport(c.ctx, c.dialAddr)
	}
	return nil, fmt.Errorf("no dial address configured")
}

// newClient creates a new client with the given transport.
func newClient(ctx context.Context, cancel context.CancelFunc, trans transport, opts *Options) *client {
	c := &client{
		ctx:             ctx,
		cancel:          cancel,
		transport:       trans,
		opts:            opts,
		connectedCh:     make(chan struct{}),
		connectErrCh:    make(chan error, 1),
		pendingRPC:      make(map[string]*rpcPending),
		pendingAck:      make(map[string]*ackPending),
		pendingPresence: make(map[string]*rpcPending),
		pendingSurvey:   make(map[string]*surveyPending),
		subscriptions:   make(map[string]*subscriptionState),
		channelOffsets:  make(map[string]uint64),
		pongCh:          make(chan struct{}, 1),
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
	// Reuse the transport created by Dial/DialGRPC/DialQUIC for the first attempt;
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
		trans, err = c.dialTransport()
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
// carrying per-channel ephemeral flags and recovery cursors. The cursor is a
// v2 Position tagged with the broker stream epoch; a channel with no recorded
// offset resumes with a nil cursor (no hint), so the server falls back to its
// own recorded delivered position without flooding full history (§4.1).
func (c *client) resumeSubscriptions(epoch string) []*clientpb.Subscription {
	c.subMu.RLock()
	defer c.subMu.RUnlock()
	subs := make([]*clientpb.Subscription, 0, len(c.subscriptions))
	for ch, state := range c.subscriptions {
		sub := &clientpb.Subscription{
			Channel:   ch,
			Ephemeral: state.ephemeral,
			Token:     state.token,
			Recover:   true,
		}
		c.offsetMu.RLock()
		if offset, ok := c.channelOffsets[ch]; ok {
			sub.Cursor = Position(epoch, offset)
		}
		c.offsetMu.RUnlock()
		subs = append(subs, sub)
	}
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
				// Pending publishes / surveys / presence queries can no
				// longer complete on the lost connection: fail them so
				// callers can retry instead of hanging until their context
				// deadline.
				c.rejectPendingAcks(err)
				c.rejectPendingSurveys(err)
				c.rejectPendingPresence(err)
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
		// If the error references a pending RPC request, a pending
		// publish, a pending presence query or a pending survey, deliver it
		// to the waiting caller so the call fails fast with the server error
		// instead of hanging until the context deadline.
		if !c.deliverPending(msg) && !c.rejectPendingAck(msg) && !c.deliverPresence(msg) && !c.deliverSurveyError(msg) {
			if dis, ok := disconnectFromError(env.Error); ok {
				// The gRPC stream has no close frame: the server encodes the
				// numeric disconnect code in the error envelope metadata, and
				// the typed error keeps this path aligned with the WebSocket
				// close-frame path.
				c.handleError(dis, !c.connected.Load())
			} else {
				err := fmt.Errorf("server error: %s (code: %s)", env.Error.GetMessage(), env.Error.GetCode())
				c.handleError(err, !c.connected.Load())
			}
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
		c.handlePublishAck(env.PublishAck, msg)

	case *clientpb.OutboundMessage_Pong:
		// Handle pong response from server
		c.handlePong()

	case *clientpb.OutboundMessage_Ping:
		// The server probes us: answer with a pong carrying the same id.
		c.handleServerPing(msg)

	case *clientpb.OutboundMessage_PresenceEvent:
		c.handlePresenceEvent(env.PresenceEvent)

	case *clientpb.OutboundMessage_Presence:
		// Snapshot reply to our PresenceQuery, matched by the inbound id, or
		// an unsolicited snapshot pushed after Connected / on a dynamic
		// subscribe: dispatch it to OnPresenceSnapshot when it does not match
		// a pending query.
		if !c.deliverPresence(msg) {
			c.notifyPresenceSnapshot(env.Presence)
		}

	case *clientpb.OutboundMessage_RecoverComplete:
		c.handleRecoverComplete(env.RecoverComplete)

	case *clientpb.OutboundMessage_GapNotice:
		// A catch-up hole notification (C6): it is not part of the message
		// stream and never touches the per-channel cursor.
		c.handleGapNotice(env.GapNotice)

	case *clientpb.OutboundMessage_SurveyResult:
		c.handleSurveyResult(env.SurveyResult)

	case *clientpb.OutboundMessage_SubRefreshAck:
		// The server acknowledged our SubRefresh request; there is nothing
		// to do client-side.

	case *clientpb.OutboundMessage_SurveyRequest:
		c.handleSurveyRequest(env.SurveyRequest)

	case *clientpb.OutboundMessage_SurveyReply:
		// Outbound SurveyReply is a legacy server echo of a survey reply;
		// the SDK answers surveys with inbound SurveyReply messages and
		// aggregates client-initiated surveys via SurveyResult, so replies
		// from the server are ignored.
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
	c.epoch = connected.GetStreamEpoch()

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
		state := c.subscriptions[sub.GetChannel()]
		if state == nil {
			state = &subscriptionState{}
		}
		// The server's list is authoritative for which channels are
		// subscribed and for their ephemeral flag. Tokens are client-supplied
		// credentials the server does not persist, so the local token is kept
		// when the server does not echo one.
		state.ephemeral = sub.GetEphemeral()
		if sub.GetToken() != "" {
			state.token = sub.GetToken()
		}
		server[sub.GetChannel()] = sub.GetEphemeral()
		c.subscriptions[sub.GetChannel()] = state
	}
	for ch := range c.subscriptions {
		if _, ok := server[ch]; !ok {
			delete(c.subscriptions, ch)
		}
	}
	c.subMu.Unlock()

	// The server pushes presence snapshots as separate Presence envelopes
	// right after Connected (v2 has no presence list on Connected), and any
	// recovery replay follows the bare Connected frame as streamed
	// publications, so nothing else is dispatched here.
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

// handleSubscribeAck handles the SubscribeAck message: it writes back the
// authoritative subscription state and dispatches the presence snapshots that
// ride the ack. Recovery replays arrive as streamed Publication envelopes
// followed by one RecoverComplete per channel (§4.2), never inside the ack.
func (c *client) handleSubscribeAck(ack *clientpb.SubscribeAck) {
	for _, sub := range ack.GetSubscriptions() {
		state := c.subscriptions[sub.GetChannel()]
		if state == nil {
			state = &subscriptionState{}
		}
		state.ephemeral = sub.GetEphemeral()
		// The server echoes the subscription (including its token) in the
		// ack; keep the local token when it does not.
		if sub.GetToken() != "" {
			state.token = sub.GetToken()
		}
		c.subMu.Lock()
		c.subscriptions[sub.GetChannel()] = state
		c.subMu.Unlock()
	}

	// Presence snapshots ride the subscribe ack; dispatch after the
	// subscription state write-back above.
	for _, snap := range ack.GetPresence() {
		c.notifyPresenceSnapshot(snap)
	}
}

// handleRecoverComplete writes back the authoritative per-channel position the
// server echoes after replaying a channel's recovery, so the next reconnect
// resumes from the server-confirmed cursor. An unset position (fresh start
// with an empty batch, or a skipped/failed channel) leaves the cursor
// untouched: it is never treated as "0 means from the start".
func (c *client) handleRecoverComplete(complete *clientpb.RecoverComplete) {
	if complete == nil || complete.GetChannel() == "" {
		return
	}
	if off, set := posOffset(complete.GetPosition()); set {
		c.offsetMu.Lock()
		c.channelOffsets[complete.GetChannel()] = off
		c.offsetMu.Unlock()
	}
}

// handlePublication delivers one Publication envelop to the OnMessage
// handler. Replay and live publications share this single consumer path (§5).
// Only live (non-replay) messages advance the per-channel cursor: a replayed
// run waits for the RecoverComplete position so a reconnect resumes from the
// server-confirmed point instead of a mid-replay one.
func (c *client) handlePublication(pub *clientpb.Publication) {
	if pub == nil {
		return
	}
	msgs := make([]*Message, 0, len(pub.GetMessages()))
	for _, env := range pub.GetMessages() {
		if env == nil {
			continue
		}
		if !env.GetReplay() {
			if off, set := posOffset(env.GetPosition()); set {
				c.offsetMu.Lock()
				c.channelOffsets[env.GetChannel()] = off
				c.offsetMu.Unlock()
			}
		}
		msgs = append(msgs, messageFromEnv(env))
	}
	if len(msgs) == 0 {
		return
	}
	c.handlerMu.RLock()
	handler := c.msgHandler
	c.handlerMu.RUnlock()
	if handler != nil {
		handler(msgs)
	}
}

// handleGapNotice dispatches a catch-up gap notice to the OnGapNotice
// handler, if any. Without a handler the notice is silently ignored.
func (c *client) handleGapNotice(notice *clientpb.GapNotice) {
	if notice == nil {
		return
	}
	c.handlerMu.RLock()
	handler := c.gapNoticeHandler
	c.handlerMu.RUnlock()
	if handler != nil {
		handler(gapNoticeFromPB(notice))
	}
}

// notifyPresenceSnapshot dispatches one snapshot to the OnPresenceSnapshot
// handler, if any.
func (c *client) notifyPresenceSnapshot(snap *clientpb.PresenceSnapshot) {
	if snap == nil {
		return
	}
	c.handlerMu.RLock()
	handler := c.presenceSnapshotHandler
	c.handlerMu.RUnlock()
	if handler != nil {
		handler(presenceSnapshotFromPB(snap))
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

// handlePublishAck resolves the pending publish with the matching id, if any.
// The id is taken from the ack envelope and falls back to the message id; the
// server echoes the request id in both.
func (c *client) handlePublishAck(ack *clientpb.PublishAck, msg *clientpb.OutboundMessage) {
	if ack == nil {
		return
	}
	id := ack.GetId()
	if id == "" {
		id = msg.GetId()
	}
	if id == "" {
		return
	}

	c.pendingAckMu.Lock()
	ap, ok := c.pendingAck[id]
	if ok {
		delete(c.pendingAck, id)
	}
	c.pendingAckMu.Unlock()
	if ok {
		off, _ := posOffset(ack.GetPosition())
		ap.resolve(off)
	}
}

// rejectPendingAck rejects the pending publish with the matching id, if any,
// and removes the entry. It reports whether the error was routed to a pending
// publish. The delivery is once-guarded, so it cannot race with the ack
// delivery.
func (c *client) rejectPendingAck(msg *clientpb.OutboundMessage) bool {
	id := msg.GetId()
	if id == "" {
		return false
	}

	c.pendingAckMu.Lock()
	ap, ok := c.pendingAck[id]
	if ok {
		delete(c.pendingAck, id)
	}
	c.pendingAckMu.Unlock()
	if ok {
		ap.reject(fmt.Errorf("server error: %s (code: %s)", msg.GetError().GetMessage(), msg.GetError().GetCode()))
	}
	return ok
}

// rejectPendingPresence fails all pending Presence queries when the
// connection is lost before their snapshot arrives.
func (c *client) rejectPendingPresence(err error) {
	c.pendingPresenceMu.Lock()
	for id, rp := range c.pendingPresence {
		delete(c.pendingPresence, id)
		select {
		case rp.ch <- &clientpb.OutboundMessage{
			Envelope: &clientpb.OutboundMessage_Error{
				Error: &sharedv2.Error{
					Code:    "INTERNAL_ERROR",
					Type:    "server_error",
					Message: "connection lost before presence snapshot: " + err.Error(),
				},
			},
		}:
		default:
		}
	}
	c.pendingPresenceMu.Unlock()
}

// rejectPendingAcks fails all pending publishes when the connection is lost
// before their acks arrive.
func (c *client) rejectPendingAcks(err error) {
	c.pendingAckMu.Lock()
	for id, ap := range c.pendingAck {
		delete(c.pendingAck, id)
		ap.reject(fmt.Errorf("connection lost before publish ack: %w", err))
	}
	c.pendingAckMu.Unlock()
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

// WithSubscriptionToken sets the per-subscription authorization token
// forwarded to the server, which passes it to the subscribe ACL proxy for
// channel-level authorization. The default is empty (no token).
//
// Note: named WithSubscriptionToken (not WithToken) because WithToken is
// already taken by the client-level connect auth token (options.go).
func WithSubscriptionToken(token string) SubscribeOption {
	return func(s *clientpb.Subscription) {
		s.Token = token
	}
}

// WithRecover enables history recovery for the subscription: the server
// streams the channel's history as replay publications (same OnMessage path
// as live messages) followed by a RecoverComplete echoing the authoritative
// cursor. The optional cursor is the resume hint — typically
// WithRecover(Position(epoch, lastOffset)); a nil cursor means "no hint" and
// the server resumes from its own recorded delivered position (or skips when
// it has none) instead of flooding full history. There is no "offset 0 means
// from the start": use WithFresh for an explicit from-the-start replay.
func WithRecover(cursor *sharedv2.Position) SubscribeOption {
	return func(s *clientpb.Subscription) {
		s.Recover = true
		s.Cursor = cursor
	}
}

// WithFresh marks the subscription for an explicit from-the-start recovery:
// the server replays the whole history regardless of any cursor or
// server-recorded position. It implies recover=true.
func WithFresh() SubscribeOption {
	return func(s *clientpb.Subscription) {
		s.Recover = true
		s.Fresh = true
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
			Token:     c.subToken(ch),
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
	if s := c.subscriptions[ch]; s != nil {
		return s.ephemeral
	}
	return false
}

// subToken returns the token the given channel was last subscribed with.
func (c *client) subToken(ch string) string {
	c.subMu.RLock()
	defer c.subMu.RUnlock()
	if s := c.subscriptions[ch]; s != nil {
		return s.token
	}
	return ""
}

// PublishOption configures a single publish.
type PublishOption func(*clientpb.Publish)

// WithPublishToken sets the per-publish authorization token forwarded to the
// server, which passes it to the publish ACL proxy. The default is empty (no
// token).
func WithPublishToken(token string) PublishOption {
	return func(p *clientpb.Publish) {
		p.Token = token
	}
}

// PublishWith publishes a message with per-publish options.
func (c *client) PublishWith(channel string, msg *Message, opts ...PublishOption) error {
	if !c.connected.Load() {
		return fmt.Errorf("not connected")
	}

	payload, err := msg.ToPayload()
	if err != nil {
		return fmt.Errorf("failed to convert message: %w", err)
	}

	pub := &clientpb.Publish{
		Channel: channel,
		Payload: payload,
	}
	for _, opt := range opts {
		opt(pub)
	}

	pbMsg := &clientpb.InboundMessage{
		Id: c.generateID(),
		Envelope: &clientpb.InboundMessage_Publish{
			Publish: pub,
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

// PublishWithAck publishes a message and waits for the server's PublishAck,
// returning the broker-assigned offset. The caller's context bounds the wait:
// on cancellation or timeout the pending publish is dropped, and a lost
// connection fails all pending publishes so callers can retry.
func (c *client) PublishWithAck(ctx context.Context, channel string, msg *Message, opts ...PublishOption) (uint64, error) {
	if !c.connected.Load() {
		return 0, fmt.Errorf("not connected")
	}

	payload, err := msg.ToPayload()
	if err != nil {
		return 0, fmt.Errorf("failed to convert message: %w", err)
	}

	pub := &clientpb.Publish{
		Channel: channel,
		Payload: payload,
	}
	for _, opt := range opts {
		opt(pub)
	}

	id := c.generateID()
	ap := &ackPending{ch: make(chan ackResult, 1)}

	// Register the pending publish before sending so the ack can never be
	// missed between the send and the registration.
	c.pendingAckMu.Lock()
	c.pendingAck[id] = ap
	c.pendingAckMu.Unlock()
	defer func() {
		c.pendingAckMu.Lock()
		delete(c.pendingAck, id)
		c.pendingAckMu.Unlock()
	}()

	pbMsg := &clientpb.InboundMessage{
		Id: id,
		Envelope: &clientpb.InboundMessage_Publish{
			Publish: pub,
		},
	}

	c.mu.RLock()
	trans := c.transport
	c.mu.RUnlock()
	if err := trans.Send(c.ctx, pbMsg); err != nil {
		return 0, fmt.Errorf("publish failed: %w", err)
	}

	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case res := <-ap.ch:
		if res.err != nil {
			return 0, res.err
		}
		return res.offset, nil
	}
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

// OnSurveyRequest sets the handler for survey requests from the server,
// additionally receiving the request channel. When set it takes precedence
// over the OnSurvey handler. When no handler at all is set, the request
// payload is echoed back to the server.
func (c *client) OnSurveyRequest(fn func(requestID, channel string, req *Message) (*Message, error)) {
	c.handlerMu.Lock()
	c.surveyRequestHandler = fn
	c.handlerMu.Unlock()
}

// OnPresence sets the handler for presence events (join/leave).
func (c *client) OnPresence(fn func(PresenceEvent)) {
	c.handlerMu.Lock()
	c.presenceHandler = fn
	c.handlerMu.Unlock()
}

// OnPresenceSnapshot sets the handler for presence snapshots delivered with
// Connected / SubscribeAck and for the snapshot returned by a Presence query.
func (c *client) OnPresenceSnapshot(fn func(PresenceSnapshot)) {
	c.handlerMu.Lock()
	c.presenceSnapshotHandler = fn
	c.handlerMu.Unlock()
}

// OnGapNotice sets the handler for catch-up gap notices (C6): the server
// sends one when reconnect catch-up detected a hole on a subscribed channel.
// The notice never advances the per-channel recovery cursor. Without a
// handler the notice is silently ignored.
func (c *client) OnGapNotice(fn func(GapNotice)) {
	c.handlerMu.Lock()
	c.gapNoticeHandler = fn
	c.handlerMu.Unlock()
}

// handlePresenceEvent dispatches a presence event to the OnPresence handler.
// Unknown actions are still delivered.
func (c *client) handlePresenceEvent(ev *clientpb.PresenceEvent) {
	if ev == nil {
		return
	}
	c.handlerMu.RLock()
	handler := c.presenceHandler
	c.handlerMu.RUnlock()
	if handler != nil {
		handler(presenceEventFromPB(ev))
	}
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

// Presence queries the current presence snapshot of an exact channel. The
// server replies with a single snapshot matched by this query's id, which is
// returned and also dispatched to the OnPresenceSnapshot handler. An empty or
// wildcard channel is handed to the server, which rejects it (BAD_REQUEST);
// otherwise failures surface as an error carrying the server code/message.
// The wait happens on the caller's goroutine, so it must not be invoked
// synchronously from receive-loop callbacks (OnMessage, OnPresence,
// OnSurvey*, OnPresenceSnapshot).
func (c *client) Presence(ctx context.Context, channel string) (*PresenceSnapshot, error) {
	if !c.connected.Load() {
		return nil, fmt.Errorf("not connected")
	}

	id := c.generateID()
	rp := &rpcPending{ch: make(chan *clientpb.OutboundMessage, 1)}

	// Register the pending query before sending so the reply can never be
	// missed between the send and the registration.
	c.pendingPresenceMu.Lock()
	c.pendingPresence[id] = rp
	c.pendingPresenceMu.Unlock()
	defer func() {
		c.pendingPresenceMu.Lock()
		delete(c.pendingPresence, id)
		c.pendingPresenceMu.Unlock()
		rp.close()
	}()

	msg := &clientpb.InboundMessage{
		Id: id,
		Envelope: &clientpb.InboundMessage_PresenceQuery{
			PresenceQuery: &clientpb.PresenceQuery{Channel: channel},
		},
	}

	c.mu.RLock()
	trans := c.transport
	c.mu.RUnlock()
	if err := trans.Send(c.ctx, msg); err != nil {
		return nil, fmt.Errorf("presence query failed: %w", err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case outMsg := <-rp.ch:
		if outMsg == nil {
			return nil, fmt.Errorf("presence failed: no response")
		}
		if err := outMsg.GetError(); err != nil {
			return nil, fmt.Errorf("presence error: %s (code: %s)", err.GetMessage(), err.GetCode())
		}
		snap := outMsg.GetPresence()
		if snap == nil {
			return nil, fmt.Errorf("presence failed: no snapshot")
		}
		ps := presenceSnapshotFromPB(snap)
		c.handlerMu.RLock()
		handler := c.presenceSnapshotHandler
		c.handlerMu.RUnlock()
		if handler != nil {
			handler(ps)
		}
		return &ps, nil
	}
}

// deliverPresence routes a snapshot or error envelope whose id matches a
// pending PresenceQuery to the waiting caller, and removes the entry. It
// reports whether the message was routed. Delivery is non-blocking and
// happens under the pendingPresence write lock, mirroring deliverPending.
func (c *client) deliverPresence(msg *clientpb.OutboundMessage) bool {
	id := msg.GetId()
	if id == "" {
		return false
	}

	c.pendingPresenceMu.Lock()
	rp, ok := c.pendingPresence[id]
	if ok {
		delete(c.pendingPresence, id)
		select {
		case rp.ch <- msg:
		default:
			// Channel is full, discard
		}
	}
	c.pendingPresenceMu.Unlock()
	return ok
}

// SendSurveyReply sends a reply to a survey request issued by the server.
// When replyErr is non-nil it is carried in the reply's error field instead of
// the payload.
func (c *client) SendSurveyReply(ctx context.Context, requestID string, reply *Message, replyErr error) error {
	if !c.connected.Load() {
		return fmt.Errorf("not connected")
	}

	var payload *sharedv2.Payload
	if reply != nil {
		p, err := reply.ToPayload()
		if err != nil {
			return fmt.Errorf("failed to convert reply message: %w", err)
		}
		payload = p
	}

	var pbErr *sharedv2.Error
	if replyErr != nil {
		pbErr = &sharedv2.Error{
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
// the OnSurveyRequest handler (with channel), falling back to the OnSurvey
// handler (without channel), or echoing the payload back by default.
func (c *client) handleSurveyRequest(req *clientpb.SurveyRequest) {
	requestID := req.GetRequestId()
	channel := req.GetChannel()
	reqMsg := PayloadToMessage(req.GetPayload(), "")

	c.handlerMu.RLock()
	onRequest := c.surveyRequestHandler
	onSurvey := c.surveyHandler
	c.handlerMu.RUnlock()

	if onRequest != nil {
		reply, err := onRequest(requestID, channel, reqMsg)
		if err != nil {
			_ = c.SendSurveyReply(context.Background(), requestID, nil, err)
			return
		}
		_ = c.SendSurveyReply(context.Background(), requestID, reply, nil)
		return
	}

	if onSurvey != nil {
		reply, err := onSurvey(requestID, reqMsg)
		if err != nil {
			_ = c.SendSurveyReply(context.Background(), requestID, nil, err)
			return
		}
		_ = c.SendSurveyReply(context.Background(), requestID, reply, nil)
		return
	}

	// Default: echo the request payload back to the server so the initiator
	// collects an answer even when no application handler is registered.
	_ = c.SendSurveyReply(context.Background(), requestID, reqMsg, nil)
}

// Survey initiates a survey on an exact channel and waits for the aggregated
// answers. A timeout<=0 sends 0 and lets the server apply its policy cap. The
// wait happens on the caller's goroutine: the receive loop fills the pending
// result, so this must not be invoked synchronously from receive-loop
// callbacks (OnMessage, OnPresence, OnSurvey*, OnPresenceSnapshot).
//
// Completion conditions, first match wins:
//   - a SurveyResult carrying the generated request_id;
//   - a top-level error whose id equals this request's inbound id
//     (synchronous rejections such as SURVEY_DISABLED);
//   - a top-level error without a matchable id whose code is a survey
//     rejection code, when exactly one Survey() is in flight (server worker
//     failures may not echo the request id; the server allows one in-flight
//     survey per session).
//
// When the SurveyResult itself carries an error, the answers (if any) are
// returned alongside it. ctx cancellation/timeout, Close and disconnect all
// fail the pending survey.
func (c *client) Survey(ctx context.Context, channel string, payload *Message, timeout time.Duration) ([]SurveyAnswer, error) {
	if !c.connected.Load() {
		return nil, fmt.Errorf("not connected")
	}

	var pbPayload *sharedv2.Payload
	if payload != nil {
		p, err := payload.ToPayload()
		if err != nil {
			return nil, fmt.Errorf("failed to convert survey payload: %w", err)
		}
		pbPayload = p
	}

	requestID := uuid.NewString()
	id := c.generateID()
	sp := &surveyPending{requestID: requestID, ch: make(chan surveyPendingResult, 1)}

	// Register the pending survey before sending so the result can never be
	// missed between the send and the registration.
	c.pendingSurveyMu.Lock()
	c.pendingSurvey[id] = sp
	c.pendingSurveyMu.Unlock()
	defer func() {
		c.pendingSurveyMu.Lock()
		delete(c.pendingSurvey, id)
		c.pendingSurveyMu.Unlock()
	}()

	surveyReq := &clientpb.SurveyRequest{
		RequestId: requestID,
		Channel:   channel,
		Payload:   pbPayload,
	}
	if timeout > 0 {
		surveyReq.TimeoutMs = int32(timeout.Milliseconds())
	}

	msg := &clientpb.InboundMessage{
		Id: id,
		Envelope: &clientpb.InboundMessage_SurveyRequest{
			SurveyRequest: surveyReq,
		},
	}

	c.mu.RLock()
	trans := c.transport
	c.mu.RUnlock()
	if err := trans.Send(c.ctx, msg); err != nil {
		return nil, fmt.Errorf("survey failed: %w", err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-sp.ch:
		if res.err != nil {
			return nil, res.err
		}
		answers := make([]SurveyAnswer, 0, len(res.result.GetAnswers()))
		for _, a := range res.result.GetAnswers() {
			answers = append(answers, surveyAnswerFromPB(a))
		}
		if err := res.result.GetError(); err != nil {
			return answers, fmt.Errorf("survey error: %s (code: %s)", err.GetMessage(), err.GetCode())
		}
		return answers, nil
	}
}

// handleSurveyResult routes a SurveyResult envelope to the pending survey
// with the matching request_id, if any, and removes the entry. Results that
// arrive after the pending survey was cleaned up (ctx timeout, Close) are
// dropped.
func (c *client) handleSurveyResult(result *clientpb.SurveyResult) {
	if result == nil {
		return
	}

	c.pendingSurveyMu.Lock()
	var sp *surveyPending
	for id, p := range c.pendingSurvey {
		if p.requestID == result.GetRequestId() {
			sp = p
			delete(c.pendingSurvey, id)
			break
		}
	}
	c.pendingSurveyMu.Unlock()

	if sp != nil {
		sp.resolve(surveyPendingResult{result: result})
	}
}

// surveyRejectCodes are the top-level error codes the server may use to
// reject a client survey without echoing the request id (asynchronous worker
// failures).
var surveyRejectCodes = map[string]bool{
	"SURVEY_DISABLED":             true,
	"SURVEY_TOO_MANY_SUBSCRIBERS": true,
	"BAD_REQUEST":                 true,
	"PERMISSION_DENIED":           true,
	"RATE_LIMITED":                true,
	"INTERNAL_ERROR":              true,
}

// deliverSurveyError routes a top-level error envelope to a pending Survey.
// The id match covers synchronous rejections (the server echoes the inbound
// id); the no-id fallback covers worker failures, which are delivered only
// when exactly one Survey() is in flight — the server allows one in-flight
// survey per session. It reports whether the error was routed to a survey.
func (c *client) deliverSurveyError(msg *clientpb.OutboundMessage) bool {
	err := msg.GetError()
	if err == nil {
		return false
	}
	code := err.GetCode()

	c.pendingSurveyMu.Lock()
	defer c.pendingSurveyMu.Unlock()

	if id := msg.GetId(); id != "" {
		if sp, ok := c.pendingSurvey[id]; ok {
			delete(c.pendingSurvey, id)
			sp.resolve(surveyPendingResult{err: fmt.Errorf("survey error: %s (code: %s)", err.GetMessage(), code)})
			return true
		}
		return false
	}

	if len(c.pendingSurvey) == 1 && surveyRejectCodes[code] {
		for id, sp := range c.pendingSurvey {
			delete(c.pendingSurvey, id)
			sp.resolve(surveyPendingResult{err: fmt.Errorf("survey error: %s (code: %s)", err.GetMessage(), code)})
		}
		return true
	}
	return false
}

// rejectPendingSurveys fails all pending surveys when the connection is lost
// before their results arrive.
func (c *client) rejectPendingSurveys(err error) {
	c.pendingSurveyMu.Lock()
	for id, sp := range c.pendingSurvey {
		delete(c.pendingSurvey, id)
		sp.resolve(surveyPendingResult{err: fmt.Errorf("connection lost before survey result: %w", err)})
	}
	c.pendingSurveyMu.Unlock()
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
	trans, err := c.dialTransport()
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

	// Clean up pending presence queries
	c.pendingPresenceMu.Lock()
	for id, rp := range c.pendingPresence {
		delete(c.pendingPresence, id)
		rp.close()
	}
	c.pendingPresenceMu.Unlock()

	// Clean up pending surveys
	c.pendingSurveyMu.Lock()
	for id, sp := range c.pendingSurvey {
		delete(c.pendingSurvey, id)
		sp.resolve(surveyPendingResult{err: errors.New("client closed before survey result")})
	}
	c.pendingSurveyMu.Unlock()

	// Clean up pending publish acks
	c.pendingAckMu.Lock()
	for id, ap := range c.pendingAck {
		delete(c.pendingAck, id)
		ap.reject(errors.New("client closed before publish ack"))
	}
	c.pendingAckMu.Unlock()

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
			Error: &sharedv2.Error{
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

// handleServerPing answers a server-issued ping with a pong carrying the same
// id (an empty id still gets a pong), and records the exchange as liveness
// evidence through the same lastPong/pongCh path as handlePong, so the
// client's own PingTimeout cannot kill a connection the server is actively
// probing.
func (c *client) handleServerPing(msg *clientpb.OutboundMessage) {
	pong := &clientpb.InboundMessage{
		Id: msg.GetId(),
		Envelope: &clientpb.InboundMessage_Pong{
			Pong: &clientpb.Pong{},
		},
	}
	c.mu.RLock()
	trans := c.transport
	c.mu.RUnlock()
	if trans != nil {
		_ = trans.Send(c.ctx, pong)
	}
	c.handlePong()
}
