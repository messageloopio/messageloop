package messageloopgo

import (
	"fmt"

	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
	"google.golang.org/protobuf/types/known/structpb"
)

// DisconnectError is a typed disconnect error signaled by the server. The
// numeric code is the same code carried in WebSocket close frames; on gRPC,
// where the stream has no close frame, the server encodes the code in the
// DISCONNECT_ERROR envelope metadata and the SDK maps it back to this type so
// both transports surface the same error.
type DisconnectError struct {
	// Code is the numeric disconnect code (3000, 3500-3513).
	Code uint32
	// Reason is a short human-readable description of the disconnect.
	Reason string
}

// Error implements the error interface.
func (e *DisconnectError) Error() string {
	if e == nil {
		return "disconnected"
	}
	if e.Reason == "" {
		return fmt.Sprintf("disconnected (code: %d)", e.Code)
	}
	return fmt.Sprintf("disconnected: %s (code: %d)", e.Reason, e.Code)
}

// disconnectFromError extracts a typed DisconnectError from a DISCONNECT_ERROR
// envelope's metadata. The gRPC transport has no close frame, so the server
// encodes the numeric disconnect code as the structpb Number value
// "disconnect_code" (pkg/grpcstream/transport.go). It reports false when the
// metadata is missing or malformed so the caller keeps its existing behavior.
func disconnectFromError(e *sharedv2.Error) (*DisconnectError, bool) {
	if e == nil || e.GetMetadata() == nil {
		return nil, false
	}
	v, ok := e.GetMetadata().GetFields()["disconnect_code"]
	if _, isNumber := v.GetKind().(*structpb.Value_NumberValue); !ok || !isNumber {
		return nil, false
	}
	code := uint32(v.GetNumberValue())
	if code == 0 {
		return nil, false
	}
	return &DisconnectError{Code: code, Reason: e.GetMessage()}, true
}
