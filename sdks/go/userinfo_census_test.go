package messageloopgo

import (
	"reflect"
	"strings"
	"testing"

	proxypb "github.com/messageloopio/messageloop/shared/genproto/proxy/v2"
)

// normalizeFieldName collapses a field name to a comparison key that is
// insensitive to initialism style (client_id vs ClientID) but sensitive to
// additions, removals and real renames.
func normalizeFieldName(name string) string {
	return strings.ToLower(strings.ReplaceAll(name, "_", ""))
}

// TestSDKUserInfoMirrorCensus pins the server-side SDK's UserInfo mirror to
// the proxy.v2.UserInfo proto contract: one Go field per proto field. The
// root module's proxy.UserInfo is pinned by its own census
// (proxy/userinfo_census_test.go); this is the same red-line for the copy
// backend implementers program against, so a proto field added without the
// SDK mirror fails here instead of silently dropping data in ToProto.
// Deliberately stdlib-only: this module keeps no testify dependency.
func TestSDKUserInfoMirrorCensus(t *testing.T) {
	protoFields := make(map[string]bool)
	desc := (&proxypb.UserInfo{}).ProtoReflect().Descriptor()
	for i := 0; i < desc.Fields().Len(); i++ {
		protoFields[normalizeFieldName(desc.Fields().Get(i).TextName())] = true
	}
	if len(protoFields) == 0 {
		t.Fatal("proxy.v2.UserInfo descriptor reports no fields")
	}

	goFields := make(map[string]bool)
	goType := reflect.TypeOf(UserInfo{})
	for i := 0; i < goType.NumField(); i++ {
		goFields[normalizeFieldName(goType.Field(i).Name)] = true
	}

	if !reflect.DeepEqual(protoFields, goFields) {
		t.Errorf("sdks/go UserInfo must mirror proxy.v2.UserInfo exactly (one Go field per proto field):\nproto: %v\ngo:    %v", protoFields, goFields)
	}
}

// TestSDKUserInfoToProtoCarriesEveryField is the behavioral half: a fully
// populated SDK UserInfo must produce a proto message with every field set —
// a field added to the struct and proto but forgotten in ToProto fails here.
func TestSDKUserInfoToProtoCarriesEveryField(t *testing.T) {
	u := &UserInfo{
		ID:         "user-1",
		Username:   "alice",
		Token:      "token-1",
		ClientType: "web",
		ClientID:   "client-1",
		Namespace:  "acme",
	}

	got := u.ToProto()
	if got == nil {
		t.Fatal("ToProto returned nil for a non-nil UserInfo")
	}

	v := reflect.ValueOf(*got)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).String() == "" {
			t.Errorf("UserInfo.ToProto dropped field %s", v.Type().Field(i).Name)
		}
	}
}
