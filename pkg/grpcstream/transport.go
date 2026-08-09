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
)

// ErrTransportClosed is returned by WriteMany after the transport has been closed.
var ErrTransportClosed = errors.New("grpc transport is closed")

const defaultWriteTimeout = 10 * time.Second

type sendRequest struct {
	msg   rawFrame
	errCh chan<- error
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
	timeout := t.writeTimeout
	if timeout <= 0 {
		timeout = defaultWriteTimeout
	}
	errCh := make(chan error, 1)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case t.sendCh <- sendRequest{msg: msg, errCh: errCh}:
	case <-timer.C:
		return fmt.Errorf("write timeout after %v", timeout)
	}
	select {
	case err := <-errCh:
		return err
	case <-timer.C:
		return fmt.Errorf("write timeout after %v", timeout)
	}
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
		// the worker is told to exit.
		if writeErr := t.writeError(int32(disconnect.Code), disconnect.Reason); writeErr != nil {
			err = writeErr
		}
		close(t.closeCh)
	})
	return err
}

func (t *Transport) writeError(code int32, reason string) error {
	msg := messageloop.MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Error{
			Error: &sharedpb.Error{
				Code:    "DISCONNECT_ERROR",
				Type:    "transport_error",
				Message: reason,
			},
		}
	})
	frame, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	return t.sendWithTimeout(rawFrame(frame))
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
		sendCh:       make(chan sendRequest, 64),
	}
	// Single worker goroutine serializes all sends to the gRPC stream. It is
	// shut down via closeCh; sendCh is never closed.
	go func() {
		for {
			select {
			case req := <-t.sendCh:
				req.errCh <- t.stream.SendMsg(req.msg)
			case <-t.closeCh:
				return
			}
		}
	}()
	return t
}

func (t *Transport) RemoteAddr() string {
	return t.remoteAddr
}
