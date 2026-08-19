package runtime

import (
	"context"
	"testing"

	"github.com/messageloopio/messageloop/internal/protocol"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVersionGate_RejectsUnsupportedVersions proves the Connect version gate
// is fail-closed: empty, non-numeric and non-generation-2 versions are
// answered with a VERSION_UNSUPPORTED Error followed by a 3514 disconnect,
// and the rejected connect never stages the client-supplied session ID nor
// reaches authentication.
func TestVersionGate_RejectsUnsupportedVersions(t *testing.T) {
	versions := []string{"", "1.0.0", "3.0.0", "abc", "2x"}
	for _, version := range versions {
		t.Run("version="+version, func(t *testing.T) {
			assert := assert.New(t)
			ctx := context.Background()
			node := NewNode(nil)
			transport := &capturingTransport{}

			client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
			require.NoError(t, err)
			serverSessionID := client.SessionID()

			msg := &clientpb.InboundMessage{
				Id: "msg-1",
				Envelope: &clientpb.InboundMessage_Connect{
					Connect: &clientpb.Connect{
						Version:   version,
						ClientId:  "rogue-client",
						SessionId: "rogue-session",
					},
				},
			}

			err = client.HandleMessage(ctx, msg)
			require.NoError(t, err)

			// Error frame goes out before the disconnect: VERSION_UNSUPPORTED.
			msgs := outboundMessages(t, transport)
			require.NotEmpty(t, msgs, "an Error frame must be sent before disconnecting")
			errFrame := msgs[0].GetError()
			require.NotNil(t, errFrame, "first outbound frame must be an Error, got %v", msgs[0].Envelope)
			assert.Equal("VERSION_UNSUPPORTED", errFrame.GetCode())
			assert.Equal("version_error", errFrame.GetType())

			// Then the transport is closed with the 3514 disconnect.
			assert.True(transport.isClosed(), "transport must be closed after a rejected version")
			assert.Equal(DisconnectUnsupportedVersion.Code, transport.getCloseReason().Code)

			// Nothing was staged: not authenticated, client-supplied session ID
			// not adopted, no Connected frame sent.
			assert.False(client.Authenticated())
			assert.Equal(serverSessionID, client.SessionID())
			for _, m := range msgs {
				assert.Nil(m.GetConnected())
			}
		})
	}
}

// TestVersionGate_AcceptsGeneration2 proves any version whose major generation
// is 2 passes the gate and completes a normal connect.
func TestVersionGate_AcceptsGeneration2(t *testing.T) {
	versions := []string{"2", "2.0.0", "2.1.3"}
	for _, version := range versions {
		t.Run("version="+version, func(t *testing.T) {
			assert := assert.New(t)
			ctx := context.Background()
			node := NewNode(nil)
			transport := &capturingTransport{}

			client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
			require.NoError(t, err)

			msg := &clientpb.InboundMessage{
				Id: "msg-1",
				Envelope: &clientpb.InboundMessage_Connect{
					Connect: &clientpb.Connect{Version: version},
				},
			}

			err = client.HandleMessage(ctx, msg)
			require.NoError(t, err)

			msgs := outboundMessages(t, transport)
			require.NotEmpty(t, msgs)
			assert.NotNil(msgs[0].GetConnected(), "generation-2 version must connect, got %v", msgs[0].Envelope)
			assert.True(client.Authenticated())
			assert.False(transport.isClosed())
		})
	}
}

// TestProtocolGenerationOK pins the parsing rule directly: only the decimal
// integer before the first dot matters, and only generation 2 is accepted.
func TestProtocolGenerationOK(t *testing.T) {
	assert := assert.New(t)
	for _, ok := range []string{"2", "2.0.0", "2.1.3", "2.0.0-rc.1"} {
		assert.True(protocol.GenerationOK(ok), "%q must be accepted", ok)
	}
	for _, rejected := range []string{"", "1", "1.0.0", "3.0.0", "0.9", "abc", "2x", ".2", " 2"} {
		assert.False(protocol.GenerationOK(rejected), "%q must be rejected", rejected)
	}
}
