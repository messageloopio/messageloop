package stream

import (
	"encoding/json"

	"google.golang.org/protobuf/types/known/structpb"
)

// MarshalJSONStruct marshals a structpb.Struct into JSON bytes.
// The structpb protobuf text format (fields:{...}) is not valid JSON, so
// payloads must go through AsMap before json.Marshal. This is the single
// home of the helper (the former session/runtime copies were removed with
// the root transition aliases they worked around).
func MarshalJSONStruct(s *structpb.Struct) ([]byte, error) {
	return json.Marshal(s.AsMap())
}
