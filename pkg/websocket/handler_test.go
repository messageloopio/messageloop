package websocket

import (
	"testing"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/lynx-go/x/encoding/json"
	"github.com/messageloopio/messageloop"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

// TestHandler_NegotiationMatrix is the regression test for P1-B2: the
// negotiated subprotocol must select a marshaler whose frame type matches the
// frame type used for the connection. Before the fix the marshaler was picked
// from the client's offer list with substring matching, so offers like
// ["messageloop", "messageloop+proto"] negotiated "messageloop" (text frames)
// while selecting the protobuf marshaler — a connection that could never
// decode a single frame.
func TestHandler_NegotiationMatrix(t *testing.T) {
	h := NewHandler(nil, DefaultOptions())

	cases := []struct {
		name         string
		subProtocol  string // conn.Subprotocol() after negotiation
		wantBinary   bool
		wantType     func(t *testing.T, m messageloop.Marshaler)
	}{
		{
			name:        "plain messageloop",
			subProtocol: "messageloop",
			wantBinary:  false,
			wantType: func(t *testing.T, m messageloop.Marshaler) {
				assert.IsType(t, messageloop.ProtoJSONMarshaler, m)
			},
		},
		{
			name:        "json suffix",
			subProtocol: "messageloop+json",
			wantBinary:  false,
			wantType: func(t *testing.T, m messageloop.Marshaler) {
				assert.IsType(t, messageloop.ProtoJSONMarshaler, m)
			},
		},
		{
			name:        "proto suffix",
			subProtocol: "messageloop+proto",
			wantBinary:  true,
			wantType: func(t *testing.T, m messageloop.Marshaler) {
				assert.IsType(t, messageloop.ProtobufMarshaler{}, m)
			},
		},
		{
			name:        "no subprotocol",
			subProtocol: "",
			wantBinary:  false,
			wantType: func(t *testing.T, m messageloop.Marshaler) {
				assert.IsType(t, messageloop.ProtoJSONMarshaler, m)
			},
		},
		{
			name:        "unknown name containing proto must not match by substring",
			subProtocol: "messageloop-proto",
			wantBinary:  false,
			wantType: func(t *testing.T, m messageloop.Marshaler) {
				assert.IsType(t, messageloop.ProtoJSONMarshaler, m)
			},
		},
		{
			name:        "unknown random name",
			subProtocol: "xproto.unknown",
			wantBinary:  false,
			wantType: func(t *testing.T, m messageloop.Marshaler) {
				assert.IsType(t, messageloop.ProtoJSONMarshaler, m)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := h.marshaler(tc.subProtocol)
			tc.wantType(t, m)

			msgType := msgTypeFromSubprotocol(tc.subProtocol)
			if tc.wantBinary {
				assert.Equal(t, websocket.BinaryMessage, msgType)
			} else {
				assert.Equal(t, websocket.TextMessage, msgType)
			}

			// The marshaler must produce frames the negotiated frame type can
			// carry: protobuf bytes on binary frames, JSON on text frames.
			out := &clientpb.OutboundMessage{Id: uuid.NewString()}
			data, err := m.Marshal(out)
			require.NoError(t, err)
			if tc.wantBinary {
				require.NoError(t, messageloop.ProtobufMarshaler{}.Unmarshal(data, &clientpb.OutboundMessage{}))
			} else {
				require.NoError(t, messageloop.ProtoJSONMarshaler.Unmarshal(data, &clientpb.OutboundMessage{}))
			}
		})
	}
}

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
