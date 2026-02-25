package v2

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	v2pb "github.com/messageloopio/messageloop/shared/genproto/v2"
)

// ClientV2 represents a single v2 protocol connection.
// It handles the Frame layer (ID, ACK, dedup) and dispatches application messages.
type ClientV2 struct {
	mu        sync.RWMutex
	transport Transport
	encoder   Encoder
	idGen     FrameIDGenerator
	dedup     *DedupCache

	sessionID string
	clientSeq atomic.Uint64 // server-side seq tracker (for outbound frames)

	// onMessage is called for each valid inbound application message.
	onMessage func(msg *v2pb.Message)

	// onDisconnect is called when the connection closes.
	onDisconnect func(code v2pb.DisconnectCode, reason string)

	ctx    context.Context
	cancel context.CancelFunc
}

// ClientV2Option is a functional option for ClientV2.
type ClientV2Option func(*ClientV2)

// WithOnMessage sets the inbound message handler.
func WithOnMessage(fn func(msg *v2pb.Message)) ClientV2Option {
	return func(c *ClientV2) { c.onMessage = fn }
}

// WithOnDisconnect sets the disconnect handler.
func WithOnDisconnect(fn func(code v2pb.DisconnectCode, reason string)) ClientV2Option {
	return func(c *ClientV2) { c.onDisconnect = fn }
}

// NewClientV2 creates a new ClientV2.
func NewClientV2(
	sessionID string,
	transport Transport,
	encoder Encoder,
	opts ...ClientV2Option,
) *ClientV2 {
	ctx, cancel := context.WithCancel(context.Background())
	c := &ClientV2{
		sessionID: sessionID,
		transport: transport,
		encoder:   encoder,
		idGen:     &UUIDGenerator{},
		dedup:     NewDedupCache(),
		ctx:       ctx,
		cancel:    cancel,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Start begins the receive loop. Blocks until the connection closes.
func (c *ClientV2) Start() {
	defer c.cancel()
	for {
		data, err := c.transport.Receive(c.ctx)
		if err != nil {
			return
		}
		if err := c.handleFrame(data); err != nil {
			// Log or handle frame-level errors; continue for recoverable errors
			continue
		}
	}
}

// Send sends an application message wrapped in a Frame.
// flags controls ACK requirements etc.
func (c *ClientV2) Send(ctx context.Context, msg *v2pb.Message, flags uint32) error {
	payload, err := c.encoder.EncodeMessage(msg)
	if err != nil {
		return err
	}

	frame := &v2pb.Frame{
		FrameId:   c.idGen.Generate(),
		SessionId: c.sessionID,
		Timestamp: time.Now().UnixMilli(),
		Flags:     flags,
		Encoding:  c.encoder.Encoding(),
		Payload:   payload,
	}

	data, err := c.encoder.EncodeFrame(frame)
	if err != nil {
		return err
	}

	return c.transport.Send(ctx, data)
}

// Publish sends a Publication message to the client.
func (c *ClientV2) Publish(ctx context.Context, channel string, data []byte, msgType string, position *v2pb.StreamPosition) error {
	msg := &v2pb.Message{
		Payload: &v2pb.Message_Publication{
			Publication: &v2pb.Publication{
				Channel:  channel,
				Data:     data,
				Type:     msgType,
				Position: position,
			},
		},
	}
	return c.Send(ctx, msg, 0)
}

// Disconnect sends a Disconnect message and closes the transport.
func (c *ClientV2) Disconnect(code v2pb.DisconnectCode, reason string, reconnectAllowed bool) error {
	msg := &v2pb.Message{
		Payload: &v2pb.Message_Disconnect{
			Disconnect: &v2pb.Disconnect{
				Code:             code,
				Reason:           reason,
				ReconnectAllowed: reconnectAllowed,
			},
		},
	}
	// Best-effort: send disconnect message before closing
	_ = c.Send(c.ctx, msg, 0)
	return c.transport.Close(code, reason)
}

// SessionID returns the session ID for this client.
func (c *ClientV2) SessionID() string {
	return c.sessionID
}

// Done returns a channel closed when this client's context is cancelled.
func (c *ClientV2) Done() <-chan struct{} {
	return c.ctx.Done()
}

// handleFrame processes a raw inbound frame.
func (c *ClientV2) handleFrame(data []byte) error {
	frame, err := c.encoder.DecodeFrame(data)
	if err != nil {
		return err
	}

	// Deduplication: ignore already-processed client frames
	if frame.ClientSeq > 0 && c.dedup.IsDuplicate(frame.SessionId, frame.ClientSeq) {
		// Send ACK even for duplicates so client stops retrying
		if frame.Flags&uint32(v2pb.FrameFlags_FLAG_ACK_REQUIRED) != 0 {
			_ = c.sendACK(frame.FrameId)
		}
		return nil
	}

	// Send ACK if required
	if frame.Flags&uint32(v2pb.FrameFlags_FLAG_ACK_REQUIRED) != 0 {
		if err := c.sendACK(frame.FrameId); err != nil {
			return err
		}
	}

	// Decode and dispatch application message
	if len(frame.Payload) == 0 {
		return nil
	}

	msg, err := c.encoder.DecodeMessage(frame.Payload)
	if err != nil {
		return err
	}

	if c.onMessage != nil {
		c.onMessage(msg)
	}
	return nil
}

// sendACK sends an ACK frame for the given frame ID.
func (c *ClientV2) sendACK(frameID string) error {
	ack := &v2pb.Frame{
		FrameId:   c.idGen.Generate(),
		SessionId: c.sessionID,
		Timestamp: time.Now().UnixMilli(),
		Flags:     uint32(v2pb.FrameFlags_FLAG_IS_ACK),
		Encoding:  c.encoder.Encoding(),
		// Payload carries the original frame_id being ACKed
		Payload: []byte(frameID),
	}
	data, err := c.encoder.EncodeFrame(ack)
	if err != nil {
		return err
	}
	return c.transport.Send(c.ctx, data)
}
