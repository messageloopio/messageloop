package websocket

import (
	"testing"

	"github.com/deeplooplabs/messageloop"
	clientpb "github.com/deeplooplabs/messageloop/genproto/v1"
	"github.com/google/uuid"
	"github.com/lynx-go/x/encoding/json"
	"github.com/stretchr/testify/require"
)

func TestHandler_marshaler(t *testing.T) {
	payload := map[string]interface{}{
		"key_str": "value_str",
		"key_int": 123,
	}
	bytes, _ := json.Marshal(payload)
	out := &clientpb.OutboundMessage{
		Id:       uuid.NewString(),
		Metadata: map[string]string{},
		Envelope: &clientpb.OutboundMessage_Publication{
			Publication: &clientpb.Publication{Messages: []*clientpb.Message{
				{
					Id:           uuid.NewString(),
					Channel:      "/topic/test",
					Offset:       0,
					PayloadBytes: bytes,
					PayloadText:  string(bytes),
				},
			}},
		},
	}
	data, err := messageloop.JSONMarshaler{}.Marshal(out)
	require.NoError(t, err)
	t.Logf("json marshal: %s", string(data))
	data, err = messageloop.ProtoJSONMarshaler.Marshal(out)
	require.NoError(t, err)
	t.Logf("protojson marshal: %s", string(data))
}
