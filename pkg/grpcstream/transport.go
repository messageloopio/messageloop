package grpcstream

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/messageloopio/messageloop"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// ErrTransportClosed is returned by WriteMany after the transport has been closed.
var ErrTransportClosed = errors.New("grpc transport is closed")

const (
	defaultWriteTimeout = 10 * time.Second
	// disconnectFrameTimeout bounds the enqueue of the disconnect frame in
	// Close. It must stay short so a backed-up send queue cannot block the
	// close path for a full write timeout; when it expires Close degrades to
	// a direct close without the frame.
	disconnectFrameTimeout = 1 * time.Second
)

type sendRequest struct {
	msg   rawFrame
	errCh chan<- error
	// disconnect marks the shutdown frame written by Close. It is still
	// delivered (not failed) when the worker drains the queue on exit, so
	// the client observes the disconnect reason.
	disconnect bool
}

type Transport struct {
	stream       grpc.BidiStreamingServer[clientpb.InboundMessage, clientpb.OutboundMessage]
	remoteAddr   string
	mu           sync.RWMutex
	closed       bool
	closeCh      chan struct{}
	closeOnce    sync.Once
	writeTimeout time.Duration
	sendCh       chan sendRequest
}

func (t *Transport) Write(message []byte) error {
	return t.WriteMany(message)
}

func (t *Transport) WriteMany(messages ...[]byte) error {
	for i := 0; i < len(messages); i++ {
		// Check if closed before enqueueing; the same lock is used by Close
		// to mark the transport closed, so a write cannot sneak past it.
		t.mu.RLock()
		closed := t.closed
		t.mu.RUnlock()
		if closed {
			return ErrTransportClosed
		}
		// Copy the message bytes because the caller may reuse the underlying
		// buffer (e.g. sync.Pool) after Write returns, while gRPC's transport
		// layer may still be reading the data asynchronously.
		copied := make(rawFrame, len(messages[i]))
		copy(copied, messages[i])
		if err := t.sendWithTimeout(copied); err != nil {
			return err
		}
	}
	return nil
}

func (t *Transport) sendWithTimeout(msg rawFrame) error {
	return t.sendWithBudget(sendRequest{msg: msg}, t.effectiveTimeout())
}

// sendWithBudget sends req, bounding the enqueue phase and the delivery-ack
// phase each with its own full budget. Giving the ack phase a fresh budget
// after a slow enqueue prevents healthy connections from being misjudged as
// slow consumers when the send queue briefly backs up.
func (t *Transport) sendWithBudget(req sendRequest, timeout time.Duration) error {
	errCh := make(chan error, 1)
	req.errCh = errCh
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case t.sendCh <- req:
	case <-timer.C:
		return fmt.Errorf("write timeout after %v", timeout)
	}
	// The enqueue phase may have consumed most of the budget; restart the
	// timer so the delivery phase is not cut short by a stale deadline.
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(timeout)
	select {
	case err := <-errCh:
		return err
	case <-t.closeCh:
		// The transport is closing; the worker may have exited before
		// processing this request, so do not wait out the write timeout.
		return ErrTransportClosed
	case <-timer.C:
		return fmt.Errorf("write timeout after %v", timeout)
	}
}

func (t *Transport) effectiveTimeout() time.Duration {
	if t.writeTimeout > 0 {
		return t.writeTimeout
	}
	return defaultWriteTimeout
}

func (t *Transport) Close(disconnect messageloop.Disconnect) error {
	var err error
	t.closeOnce.Do(func() {
		t.mu.Lock()
		if t.closed {
			t.mu.Unlock()
			return
		}
		t.closed = true
		t.mu.Unlock()

		// Queue the disconnect error frame through the send channel so it is
		// serialized with in-flight sends and delivered by the worker before
		// the worker is told to exit. When the queue is backed up this fails
		// fast (disconnectFrameTimeout) and Close degrades to a direct close
		// without the frame instead of blocking for the full write timeout.
		if writeErr := t.writeError(int32(disconnect.Code), disconnect.Reason); writeErr != nil {
			err = writeErr
		}
		close(t.closeCh)
	})
	return err
}

func (t *Transport) writeError(code int32, reason string) error {
	// The numeric disconnect code (3500-3512) is encoded into the error
	// envelope metadata because the gRPC stream has no close frame: the WS
	// path carries the code in the close frame, and without this the gRPC
	// client cannot tell the disconnect reasons apart.
	metadata := &structpb.Struct{Fields: map[string]*structpb.Value{
		"disconnect_code": structpb.NewNumberValue(float64(code)),
	}}
	msg := messageloop.MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Error{
			Error: &sharedpb.Error{
				Code:     "DISCONNECT_ERROR",
				Type:     "transport_error",
				Message:  reason,
				Metadata: metadata,
			},
		}
	})
	frame, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	return t.sendWithBudget(sendRequest{msg: rawFrame(frame), disconnect: true}, disconnectFrameTimeout)
}

var _ messageloop.Transport = new(Transport)

func newGRPCTransport(
	stream grpc.BidiStreamingServer[clientpb.InboundMessage, clientpb.OutboundMessage],
	remoteAddr string,
	writeTimeout time.Duration,
) *Transport {
	t := &Transport{
		stream:       stream,
		remoteAddr:   remoteAddr,
		closeCh:      make(chan struct{}),
		writeTimeout: writeTimeout,
		// Depth 1: the send channel is only a handoff from the session's
		// writer goroutine to the gRPC worker, never a second bounded buffer.
		// A deeper queue would delay the slow-consumer disconnect (3512) so
		// far that the client cannot observe it (PR-KA-B1 §7).
		sendCh: make(chan sendRequest, 1),
	}
	// Single worker goroutine serializes all sends to the gRPC stream. It is
	// shut down via closeCh; sendCh is never closed.
	go func() {
		for {
			select {
			case req := <-t.sendCh:
				req.errCh <- t.stream.SendMsg(req.msg)
			case <-t.closeCh:
				// The transport is closed; no new requests can be enqueued
				// once writers observe the closed flag, but requests that
				// passed the flag check just before Close may still arrive.
				// Drain the channel: fail leftover requests promptly so their
				// writers do not wait out the write timeout, and deliver the
				// disconnect frame so the client sees the close reason.
				for {
					select {
					case req := <-t.sendCh:
						if req.disconnect {
							req.errCh <- t.stream.SendMsg(req.msg)
						} else {
							req.errCh <- ErrTransportClosed
						}
					default:
						return
					}
				}
			}
		}
	}()
	return t
}

func (t *Transport) RemoteAddr() string {
	return t.remoteAddr
}
