package stream

import (
	"encoding/json"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"
)

// Local copies of two tiny root-package helpers, duplicated in PR-KA-D11 so
// this leaf package does not import the root package (which would create an
// import cycle through the root transition aliases). The root originals
// (client.go MarshalJSONStruct, hub.go isWildcard) stay in place until the
// root package is retired in KD-K26 phase three (D13).

// MarshalJSONStruct marshals a structpb.Struct into JSON bytes.
// The structpb protobuf text format (fields:{...}) is not valid JSON, so
// payloads must go through AsMap before json.Marshal.
func MarshalJSONStruct(s *structpb.Struct) ([]byte, error) {
	return json.Marshal(s.AsMap())
}

// isWildcard returns true if the channel pattern contains a wildcard character.
func isWildcard(ch string) bool {
	return strings.Contains(ch, "*")
}
