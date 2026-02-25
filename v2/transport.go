package v2

import (
	"context"

	v2pb "github.com/messageloopio/messageloop/shared/genproto/v2"
)

// Transport is the abstraction over the underlying connection mechanism.
// It handles raw byte transmission and is decoupled from the Frame/Application layers.
// Implementations: WebSocket, gRPC streaming, TCP, QUIC.
type Transport interface {
	// Send sends encoded Frame bytes to the remote endpoint.
	Send(ctx context.Context, data []byte) error

	// Receive receives the next Frame's encoded bytes from the remote endpoint.
	// Blocks until data is available, the context is cancelled, or connection closes.
	Receive(ctx context.Context) ([]byte, error)

	// Close closes the transport connection with the given disconnect reason.
	Close(code v2pb.DisconnectCode, reason string) error

	// Done returns a channel that is closed when the transport connection ends.
	Done() <-chan struct{}
}
