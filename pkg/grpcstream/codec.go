package grpcstream

import (
	"fmt"

	"google.golang.org/protobuf/proto"
)

type rawFrame []byte

// RawCodec allows sending Protobuf encoded Pushes without
// additional wrapping and marshaling.
type RawCodec struct{}

func (c *RawCodec) Marshal(v interface{}) ([]byte, error) {
	out, ok := v.(rawFrame)
	if !ok {
		vv, ok := v.(proto.Message)
		if !ok {
			return nil, fmt.Errorf("failed to marshal, message is %T, want proto.Message", v)
		}
		return proto.Marshal(vv)
	}
	return out, nil
}

func (c *RawCodec) Unmarshal(data []byte, v interface{}) error {
	vv, ok := v.(proto.Message)
	if !ok {
		return fmt.Errorf("failed to unmarshal, message is %T, want proto.Message", v)
	}
	return proto.Unmarshal(data, vv)
}

// Name returns the codec name used as the gRPC content-subtype. The name is
// package-prefixed instead of the default "proto" so that this codec is never
// registered in the process-global codec registry under the default name
// (which would override the standard proto codec for every gRPC connection in
// the process). The codec is wired per-server via ForceServerCodec in
// prepareServer, so the name is only a protocol label; it must match the
// codec name used by the Go SDK client.
func (c *RawCodec) Name() string {
	return "messageloop-proto"
}
