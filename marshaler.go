package messageloop

import (
	"github.com/messageloopio/messageloop/shared"
)

// Re-export Marshaler types from shared package for backward compatibility.
type (
	Marshaler          = shared.Marshaler
	JSONMarshaler      = shared.JSONMarshaler
	ProtobufMarshaler  = shared.ProtobufMarshaler
	MarshalTypeError   = shared.MarshalTypeError
	UnmarshalTypeError = shared.UnmarshalTypeError
)

// Re-export shared marshalers and errors.
var (
	ProtoJSONMarshaler = shared.ProtoJSONMarshaler
	Marshalers         = shared.Marshalers
)
