package websocket

import (
	"testing"

	"github.com/google/uuid"
	"github.com/lynx-go/x/encoding/json"
	"github.com/messageloopio/messageloop"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestHandler_marshaler(t *testing.T) {
	payload := map[string]interface{}{
		"key_str": "value_str",
		"key_int": 123,
	}
	bytes, _ := json.Marshal(payload)

	// Create a Struct from the payload
	s, err := structpb.NewStruct(payload)
	require.NoError(t, err)

	out := &clientpb.OutboundMessage{
		Id: uuid.NewString(),
		Envelope: &clientpb.OutboundMessage_Publication{
			Publication: &clientpb.Publication{Messages: []*clientpb.Message{
				{
					Id:      uuid.NewString(),
					Channel: "/topic/test",
					Offset:  0,
					Payload: &sharedpb.Payload{
						Data: &sharedpb.Payload_Json{
							Json: s,
						},
					},
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
	// Verify the bytes payload matches
	t.Logf("payload bytes: %s", string(bytes))
}
