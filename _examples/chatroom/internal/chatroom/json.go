package chatroom

import (
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/types/known/structpb"
)

// marshalJSON encodes v with encoding/json.
func marshalJSON(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal json: %w", err)
	}
	return b, nil
}

// mustStruct converts JSON bytes into a protobuf Struct.
func mustStruct(b []byte) *structpb.Struct {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		// Unreachable: b always comes from marshalJSON of a plain struct.
		panic(err)
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		panic(err)
	}
	return s
}
