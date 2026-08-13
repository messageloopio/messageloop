package messageloop

import (
	"math"
	"testing"

	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

// --- A1: PublicationFromPayload shared conversion ---

func TestPublicationFromPayload_Variants(t *testing.T) {
	t.Run("binary", func(t *testing.T) {
		pub, err := PublicationFromPayload("id-1", map[string]string{"k": "v"},
			&sharedpb.Payload{ContentType: "application/octet-stream", Data: &sharedpb.Payload_Binary{Binary: []byte{1, 2, 3}}})
		require.NoError(t, err)
		require.Equal(t, PayloadKindBinary, pub.Kind)
		require.Equal(t, []byte{1, 2, 3}, pub.Payload)
		require.Equal(t, "application/octet-stream", pub.ContentType)
		require.Equal(t, "id-1", pub.Id)
		require.Equal(t, map[string]string{"k": "v"}, pub.Metadata)
	})

	t.Run("text", func(t *testing.T) {
		pub, err := PublicationFromPayload("", nil, &sharedpb.Payload{Data: &sharedpb.Payload_Text{Text: "hello"}})
		require.NoError(t, err)
		require.Equal(t, PayloadKindText, pub.Kind)
		require.Equal(t, []byte("hello"), pub.Payload)
	})

	t.Run("json", func(t *testing.T) {
		st, err := structpb.NewStruct(map[string]any{"a": "b"})
		require.NoError(t, err)
		pub, err := PublicationFromPayload("", nil, &sharedpb.Payload{Data: &sharedpb.Payload_Json{Json: st}})
		require.NoError(t, err)
		require.Equal(t, PayloadKindJSON, pub.Kind)
		require.JSONEq(t, `{"a":"b"}`, string(pub.Payload))
	})

	t.Run("nil payload", func(t *testing.T) {
		pub, err := PublicationFromPayload("id", nil, nil)
		require.NoError(t, err)
		require.NotNil(t, pub)
		require.Nil(t, pub.Payload)
		require.Equal(t, PayloadKindBinary, pub.Kind) // zero value, matches legacy call sites
	})
}

// TestPublicationFromPayload_JSONNonFinite pins the JSON variant behavior
// for non-finite numbers. Protobuf >= 1.36 sanitizes NaN/Inf in AsMap to the
// strings "NaN"/"Infinity", so the MarshalJSONStruct error path is no longer
// reachable through the public structpb API (<= 1.33 leaked raw NaN, which
// made json.Marshal fail). PublicationFromPayload keeps the error path for
// defensive robustness: any future marshal failure still surfaces to the
// converged call sites instead of being silently dropped.
func TestPublicationFromPayload_JSONNonFinite(t *testing.T) {
	st, err := structpb.NewStruct(map[string]any{"n": math.NaN()})
	require.NoError(t, err)
	pub, err := PublicationFromPayload("id", nil, &sharedpb.Payload{Data: &sharedpb.Payload_Json{Json: st}})
	require.NoError(t, err)
	require.Equal(t, PayloadKindJSON, pub.Kind)
	require.JSONEq(t, `{"n":"NaN"}`, string(pub.Payload))
}
