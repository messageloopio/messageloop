package ws

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/lynx-go/x/encoding/json"
	"github.com/messageloopio/messageloop/shared"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
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
		wantType     func(t *testing.T, m shared.Marshaler)
	}{
		{
			name:        "plain messageloop",
			subProtocol: "messageloop",
			wantBinary:  false,
			wantType: func(t *testing.T, m shared.Marshaler) {
				assert.IsType(t, shared.ProtoJSONMarshaler, m)
			},
		},
		{
			name:        "json suffix",
			subProtocol: "messageloop+json",
			wantBinary:  false,
			wantType: func(t *testing.T, m shared.Marshaler) {
				assert.IsType(t, shared.ProtoJSONMarshaler, m)
			},
		},
		{
			name:        "proto suffix",
			subProtocol: "messageloop+proto",
			wantBinary:  true,
			wantType: func(t *testing.T, m shared.Marshaler) {
				assert.IsType(t, shared.ProtobufMarshaler{}, m)
			},
		},
		{
			name:        "no subprotocol",
			subProtocol: "",
			wantBinary:  false,
			wantType: func(t *testing.T, m shared.Marshaler) {
				assert.IsType(t, shared.ProtoJSONMarshaler, m)
			},
		},
		{
			name:        "unknown name containing proto must not match by substring",
			subProtocol: "messageloop-proto",
			wantBinary:  false,
			wantType: func(t *testing.T, m shared.Marshaler) {
				assert.IsType(t, shared.ProtoJSONMarshaler, m)
			},
		},
		{
			name:        "unknown random name",
			subProtocol: "xproto.unknown",
			wantBinary:  false,
			wantType: func(t *testing.T, m shared.Marshaler) {
				assert.IsType(t, shared.ProtoJSONMarshaler, m)
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
				require.NoError(t, shared.ProtobufMarshaler{}.Unmarshal(data, &clientpb.OutboundMessage{}))
			} else {
				require.NoError(t, shared.ProtoJSONMarshaler.Unmarshal(data, &clientpb.OutboundMessage{}))
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
					Id:       uuid.NewString(),
					Channel:  "/topic/test",
					Position: &sharedv2.Position{},
					Payload: &sharedv2.Payload{
						Data: &sharedv2.Payload_Json{
							Json: s,
						},
					},
				},
			}},
		},
	}
	data, err := shared.JSONMarshaler{}.Marshal(out)
	require.NoError(t, err)
	t.Logf("json marshal: %s", string(data))
	data, err = shared.ProtoJSONMarshaler.Marshal(out)
	require.NoError(t, err)
	t.Logf("protojson marshal: %s", string(data))
	// Verify the bytes payload matches
	t.Logf("payload bytes: %s", string(bytes))
}

// TestHeartbeat_ReadTimeoutFloorWhenProbing pins the read deadline formula:
// probing (idle>0 or ping>0) floors the timeout at max(2*idle, 3*ping, 10s)
// and configured values may raise but never lower it; a fully disabled
// heartbeat keeps 60s (no 10s floor) with configured as hard override.
func TestHeartbeat_ReadTimeoutFloorWhenProbing(t *testing.T) {
	cases := []struct {
		name           string
		idle           time.Duration
		ping           time.Duration
		configured     time.Duration
		want           time.Duration
	}{
		{
			name: "idle=15s ping=5s unconfigured floors at 2*idle",
			idle: 15 * time.Second,
			ping: 5 * time.Second,
			want: 30 * time.Second,
		},
		{
			name: "ping-only floors at 3*ping",
			ping: 5 * time.Second,
			want: 15 * time.Second,
		},
		{
			name: "idle and ping small floors at 10s",
			idle: 1 * time.Second,
			ping: 1 * time.Second,
			want: 10 * time.Second,
		},
		{
			name: "disabled heartbeat unconfigured keeps 60s",
			want: 60 * time.Second,
		},
		{
			name:       "disabled heartbeat configured overrides",
			configured: 90 * time.Second,
			want:       90 * time.Second,
		},
		{
			name:       "disabled heartbeat configured short is respected",
			configured: 45 * time.Second,
			want:       45 * time.Second,
		},
		{
			name:       "configured below floor is raised to floor",
			idle:       15 * time.Second,
			ping:       5 * time.Second,
			configured: 5 * time.Second,
			want:       30 * time.Second,
		},
		{
			name:       "configured above floor wins",
			idle:       15 * time.Second,
			ping:       5 * time.Second,
			configured: 45 * time.Second,
			want:       45 * time.Second,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, heartbeatReadTimeout(tc.idle, tc.ping, tc.configured))
		})
	}
}
