package websocket

import (
	"testing"

	"github.com/fleetlit/messageloop"
	clientpb "github.com/fleetlit/messageloop/genproto/v1"
	"github.com/google/uuid"
	cloudevents "github.com/cloudevents/sdk-go/binding/format/protobuf/v2/pb"
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
			Publication: &clientpb.Publication{Envelopes: []*clientpb.Message{
				{
					Id:      uuid.NewString(),
					Channel: "/topic/test",
					Offset:  0,
					Payload: &cloudevents.CloudEvent{
						Id:          uuid.NewString(),
						Source:      "test-source",
						Type:        "test.type",
						SpecVersion: "1.0",
						Data: &cloudevents.CloudEvent_TextData{
							TextData: string(bytes),
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
}
