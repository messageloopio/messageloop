package proxy

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	proxypb "github.com/messageloopio/messageloop/shared/genproto/proxy/v2"
)

// normalizeFieldName collapses a field name to a comparison key that is
// insensitive to initialism style (client_id vs ClientID) but sensitive to
// additions, removals and real renames.
func normalizeFieldName(name string) string {
	return strings.ToLower(strings.ReplaceAll(name, "_", ""))
}

// TestUserInfoMirrorCensus pins the UserInfo mirror against the proto
// contract: proxy.UserInfo must carry exactly one Go field per
// proxy.v2.UserInfo proto field. The proto is the wire contract; this Go
// struct is the server-side mirror a fourth module (the server-side SDK)
// also copies. A proto field added without its mirror here (or a leftover
// Go field) fails this census instead of silently dropping data in
// FromProtoAuthenticateResponse.
func TestUserInfoMirrorCensus(t *testing.T) {
	protoFields := make(map[string]bool)
	desc := (&proxypb.UserInfo{}).ProtoReflect().Descriptor()
	for i := 0; i < desc.Fields().Len(); i++ {
		protoFields[normalizeFieldName(desc.Fields().Get(i).TextName())] = true
	}
	require.NotEmpty(t, protoFields)

	goFields := make(map[string]bool)
	goType := reflect.TypeOf(UserInfo{})
	for i := 0; i < goType.NumField(); i++ {
		goFields[normalizeFieldName(goType.Field(i).Name)] = true
	}

	assert.Equal(t, protoFields, goFields,
		"proxy.UserInfo must mirror proxy.v2.UserInfo exactly (one Go field per proto field)")
}

// TestUserInfoFromProtoCarriesEveryField is the behavioral half of the
// census: a fully populated AuthenticateResponse must arrive with every
// mirror field non-empty. A field added to the proto and the struct but
// forgotten in FromProtoAuthenticateResponse stays empty and fails here.
func TestUserInfoFromProtoCarriesEveryField(t *testing.T) {
	resp := &proxypb.AuthenticateResponse{
		UserInfo: &proxypb.UserInfo{
			Id:         "user-1",
			Username:   "alice",
			Token:      "token-1",
			ClientType: "web",
			ClientId:   "client-1",
			Namespace:  "acme",
		},
	}

	got := FromProtoAuthenticateResponse(resp)
	require.NotNil(t, got.UserInfo)

	v := reflect.ValueOf(*got.UserInfo)
	for i := 0; i < v.NumField(); i++ {
		require.NotEmpty(t, v.Field(i).String(),
			"FromProtoAuthenticateResponse dropped field %s", v.Type().Field(i).Name)
	}
}

// TestUserInfoProtoRoundTrip guards the inverse direction used by transport
// tests: the shared converter must survive a proto marshal/unmarshal cycle.
func TestUserInfoProtoRoundTrip(t *testing.T) {
	resp := &proxypb.AuthenticateResponse{
		UserInfo: &proxypb.UserInfo{
			Id: "user-1", Username: "alice", Token: "token-1",
			ClientType: "web", ClientId: "client-1", Namespace: "acme",
		},
	}
	data, err := proto.Marshal(resp)
	require.NoError(t, err)

	var decoded proxypb.AuthenticateResponse
	require.NoError(t, proto.Unmarshal(data, &decoded))

	got := FromProtoAuthenticateResponse(&decoded)
	require.NotNil(t, got.UserInfo)
	assert.Equal(t, "acme", got.UserInfo.Namespace)
	assert.Equal(t, "user-1", got.UserInfo.ID)
}
