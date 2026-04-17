package grpcstream

import (
	"fmt"
	"sync"
	"time"

	"github.com/messageloopio/messageloop"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	"google.golang.org/grpc"
)

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
	// Check if closed using a read lock
	t.mu.RLock()
	if t.closed {
		t.mu.RUnlock()
		return nil
	}
	t.mu.RUnlock()

	for i := 0; i < len(messages); i++ {
		// Double-check after acquiring the read lock for each send
		t.mu.RLock()
		closed := t.closed
		t.mu.RUnlock()
		if closed {
			return nil
		}
		if err := t.sendWithTimeout(rawFrame(messages[i])); err != nil {
			return err
		}
	}
	return nil
}

func (t *Transport) sendWithTimeout(msg rawFrame) error {
	if t.writeTimeout <= 0 {
		return t.stream.SendMsg(msg)
	}
	errCh := make(chan error, 1)
	timer := time.NewTimer(t.writeTimeout)
	defer timer.Stop()
	select {
	case t.sendCh <- sendRequest{msg: msg, errCh: errCh}:
	case <-timer.C:
		return fmt.Errorf("write timeout after %v", t.writeTimeout)
	}
	select {
	case err := <-errCh:
		return err
	case <-timer.C:
		return fmt.Errorf("write timeout after %v", t.writeTimeout)
	}
}

func (t *Transport) Close(disconnect messageloop.Disconnect) error {
	var err error
	t.closeOnce.Do(func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.closed {
			err = nil
			return
		}
		t.writeError(int32(disconnect.Code), disconnect.Reason)
		time.Sleep(100 * time.Millisecond)
		close(t.closeCh)
		close(t.sendCh)
		t.closed = true
	})
	return err
}

func (t *Transport) writeError(code int32, reason string) {
	msg := messageloop.MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Error{
			Error: &sharedpb.Error{
				Code:    "DISCONNECT_ERROR",
				Type:    "transport_error",
				Message: reason,
			},
		}
	})
	_ = t.stream.Send(msg)
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
	// Single worker goroutine serializes all sends to the gRPC stream.
	go func() {
		for req := range t.sendCh {
			req.errCh <- t.stream.SendMsg(req.msg)
		}
	}()
	return t
}

func (t *Transport) RemoteAddr() string {
	return t.remoteAddr
}
