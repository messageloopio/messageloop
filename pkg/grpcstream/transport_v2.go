package grpcstream

import (
	"context"
	"io"
	"sync"

	v2 "github.com/messageloopio/messageloop/v2"
	v2pb "github.com/messageloopio/messageloop/shared/genproto/v2"
	"google.golang.org/grpc"
)

// TransportV2 implements the v2.Transport interface over a gRPC bidirectional stream.
// The stream carries StreamFrame messages, where each StreamFrame.Data field holds
// an already-encoded v2 Frame (Protobuf or JSON bytes). This avoids double-encoding.
type TransportV2 struct {
	stream    grpc.BidiStreamingServer[v2pb.StreamFrame, v2pb.StreamFrame]
	mu        sync.Mutex
	closed    bool
	closeOnce sync.Once
	done      chan struct{}
}

var _ v2.Transport = (*TransportV2)(nil)

// NewTransportV2 wraps a gRPC bidi stream as a v2.Transport.
func NewTransportV2(stream grpc.BidiStreamingServer[v2pb.StreamFrame, v2pb.StreamFrame]) *TransportV2 {
	return &TransportV2{
		stream: stream,
		done:   make(chan struct{}),
	}
}

// Send wraps the encoded Frame bytes in a StreamFrame and sends via gRPC.
func (t *TransportV2) Send(_ context.Context, data []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	return t.stream.Send(&v2pb.StreamFrame{Data: data})
}

// Receive reads the next StreamFrame from the gRPC stream and returns its Data bytes.
// Returns io.EOF (wrapped) when the stream ends normally.
func (t *TransportV2) Receive(ctx context.Context) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		sf, err := t.stream.Recv()
		if err != nil {
			ch <- result{nil, err}
			return
		}
		ch <- result{sf.Data, nil}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			t.signalDone()
		}
		if r.err == io.EOF {
			return nil, r.err
		}
		return r.data, r.err
	}
}

// Close marks the transport as closed and signals Done.
// For gRPC, the server stream ends when the handler returns —
// callers should cancel the stream's context after calling Close.
func (t *TransportV2) Close(_ v2pb.DisconnectCode, _ string) error {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closed = true
		t.mu.Unlock()
		t.signalDone()
	})
	return nil
}

// Done returns a channel closed when the transport connection ends.
func (t *TransportV2) Done() <-chan struct{} {
	return t.done
}

func (t *TransportV2) signalDone() {
	select {
	case <-t.done:
	default:
		close(t.done)
	}
}
