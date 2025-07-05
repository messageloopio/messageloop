package websocket

import (
	"github.com/deeploopdev/messageloop"
	clientv1 "github.com/deeploopdev/messageloop-protocol/gen/proto/go/client/v1"
	"github.com/google/uuid"
	"github.com/lynx-go/x/encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestHandler_marshaler(t *testing.T) {
	payload := map[string]interface{}{
		"key_str": "value_str",
		"key_int": 123,
	}
	bytes, _ := json.Marshal(payload)
	out := &clientv1.ServerMessage{
		Id:      uuid.NewString(),
		Headers: map[string]string{},
		Body: &clientv1.ServerMessage_Publication{
			Publication: &clientv1.Publication{Messages: []*clientv1.Message{
				{
					Id:            uuid.NewString(),
					Channel:       "/topic/test",
					Offset:        0,
					PayloadBytes:  bytes,
					PayloadString: string(bytes),
				},
			}},
		},
	}
	data, err := messageloop.DefaultJSONMarshaler.Marshal(out)
	require.NoError(t, err)
	t.Logf("json marshal: %s", string(data))
	data, err = messageloop.DefaultProtoJsonMarshaler.Marshal(out)
	require.NoError(t, err)
	t.Logf("protojson marshal: %s", string(data))
}
