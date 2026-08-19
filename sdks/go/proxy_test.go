package messageloopgo

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	proxypb "github.com/messageloopio/messageloop/shared/genproto/proxy/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

func newTestTextPayload(t *testing.T, s string) *sharedv2.Payload {
	t.Helper()
	m := NewMessageWithData("test", NewTextData(s))
	p, err := m.ToPayload()
	if err != nil {
		t.Fatalf("ToPayload: %v", err)
	}
	return p
}

// TestHandlerImplRPCOverride verifies P1-11: a custom RPC handler injected
// into HandlerImpl is used for dispatch instead of the embedded default
// implementation being called directly.
func TestHandlerImplRPCOverride(t *testing.T) {
	h := &HandlerImpl{}
	mux := NewRPCMux()
	mux.Handle("echo", func(ctx context.Context, req *RPCRequest) (*RPCResponse, error) {
		return &RPCResponse{Payload: req.Payload}, nil
	})
	h.RPCHandler = mux

	resp, err := h.RPC(context.Background(), &proxypb.RPCRequest{
		Id:      "req-1",
		Channel: "svc",
		Method:  "echo",
		Payload: newTestTextPayload(t, "hello"),
	})
	if err != nil {
		t.Fatalf("RPC failed: %v", err)
	}
	if resp.GetError() != nil {
		t.Fatalf("RPC returned error: %v", resp.GetError())
	}
	if resp.GetPayload() == nil || resp.GetPayload().GetText() != "hello" {
		t.Fatalf("RPC response payload = %v, want text hello", resp.GetPayload())
	}
}

// TestHandlerImplRPCDefault verifies the zero-value HandlerImpl still
// dispatches to the embedded default RPC implementation.
func TestHandlerImplRPCDefault(t *testing.T) {
	h := &HandlerImpl{}
	resp, err := h.RPC(context.Background(), &proxypb.RPCRequest{Id: "1", Channel: "svc", Method: "echo"})
	if err != nil {
		t.Fatalf("RPC failed: %v", err)
	}
	if resp.GetError() == nil || resp.GetError().GetCode() != "INTERNAL_ERROR" {
		t.Fatalf("RPC response = %v, want INTERNAL_ERROR", resp)
	}
}

// TestHandlerImplRPCPayloadConversionError verifies P1-11: a failure to
// convert the RPC response payload to protobuf is surfaced as an error
// instead of being silently swallowed.
func TestHandlerImplRPCPayloadConversionError(t *testing.T) {
	h := &HandlerImpl{}
	h.RPCHandler = &stubBadPayloadHandler{}

	_, err := h.RPC(context.Background(), &proxypb.RPCRequest{Id: "req-1", Channel: "svc", Method: "boom"})
	if err == nil {
		t.Fatal("RPC succeeded, want payload conversion error")
	}
	if status.Code(err) != codes.Internal {
		t.Fatalf("RPC error = %v, want Internal", err)
	}
}

type stubBadPayloadHandler struct{}

func (h *stubBadPayloadHandler) HandleRPC(ctx context.Context, req *RPCRequest) (*RPCResponse, error) {
	return &RPCResponse{
		Payload: &Message{Data: NewJSONData(map[string]any{"v": json.Number("not-a-number")})},
	}, nil
}

// TestHandlerImplAuthOverride verifies P1-11: a custom auth handler injected
// into HandlerImpl is used for dispatch.
func TestHandlerImplAuthOverride(t *testing.T) {
	h := &HandlerImpl{}
	h.AuthHandler = &stubAuthHandler{}

	resp, err := h.Authenticate(context.Background(), &proxypb.AuthenticateRequest{ClientId: "c1", Token: "t1"})
	if err != nil {
		t.Fatalf("Authenticate failed: %v", err)
	}
	if resp.GetUserInfo() == nil || resp.GetUserInfo().GetId() != "user-c1" {
		t.Fatalf("UserInfo id = %v, want user-c1", resp.GetUserInfo())
	}
}

type stubAuthHandler struct{}

func (h *stubAuthHandler) Authenticate(ctx context.Context, req *AuthenticateRequest) (*AuthenticateResponse, error) {
	return &AuthenticateResponse{
		UserInfo: &UserInfo{ID: "user-" + req.ClientID},
	}, nil
}

// TestHandlerImplACLOverride verifies P1-11: a custom ACL handler injected
// into HandlerImpl is used for SubscribeAcl/PublishAcl dispatch.
func TestHandlerImplACLOverride(t *testing.T) {
	h := &HandlerImpl{}
	h.ACLHandler = &denyACLHandler{}

	if _, err := h.SubscribeAcl(context.Background(), &proxypb.SubscribeAclRequest{Channel: "private.x"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("SubscribeAcl error = %v, want PermissionDenied", err)
	}
	if _, err := h.PublishAcl(context.Background(), &proxypb.PublishAclRequest{Channel: "private.x"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("PublishAcl error = %v, want PermissionDenied", err)
	}
}

type denyACLHandler struct{}

func (h *denyACLHandler) CheckSubscribeACL(ctx context.Context, channel, token string) error {
	return status.Error(codes.PermissionDenied, "denied")
}

func (h *denyACLHandler) CheckPublishACL(ctx context.Context, channel, token string) error {
	return status.Error(codes.PermissionDenied, "denied")
}

// TestHandlerImplLifecycleOverride verifies P1-11: a custom lifecycle handler
// injected into HandlerImpl is used for the notification hooks.
func TestHandlerImplLifecycleOverride(t *testing.T) {
	h := &HandlerImpl{}
	h.LifecycleHandler = &failingLifecycleHandler{}

	if _, err := h.OnConnected(context.Background(), &proxypb.OnConnectedRequest{SessionId: "s1"}); status.Code(err) != codes.Internal {
		t.Fatalf("OnConnected error = %v, want Internal", err)
	}
}

type failingLifecycleHandler struct{}

func (h *failingLifecycleHandler) OnConnected(ctx context.Context, sessionID, username string) error {
	return errors.New("lifecycle boom")
}

func (h *failingLifecycleHandler) OnDisconnected(ctx context.Context, sessionID, username string) error {
	return nil
}

func (h *failingLifecycleHandler) OnSubscribed(ctx context.Context, sessionID, channel, username string) error {
	return nil
}

func (h *failingLifecycleHandler) OnUnsubscribed(ctx context.Context, sessionID, channel, username string) error {
	return nil
}
