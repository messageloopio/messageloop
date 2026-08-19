package messageloopgo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/types/known/structpb"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

// TestClientDisconnectCodeEnvelope verifies that a DISCONNECT_ERROR envelope
// carrying the numeric disconnect code in its metadata surfaces as the same
// typed DisconnectError as the WebSocket close-frame path, with the same
// numeric code (task 3 / A2).
func TestClientDisconnectCodeEnvelope(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())

	handlerErr := make(chan error, 1)
	c.OnError(func(err error) { handlerErr <- err })

	const code = 3507 // DisconnectPermissionDenied
	c.handleMessage(&clientpb.OutboundMessage{
		Envelope: &clientpb.OutboundMessage_Error{
			Error: &sharedpb.Error{
				Code:     "DISCONNECT_ERROR",
				Type:     "transport_error",
				Message:  "permission denied",
				Metadata: &structpb.Struct{Fields: map[string]*structpb.Value{"disconnect_code": structpb.NewNumberValue(code)}},
			},
		},
	}, 0)

	select {
	case err := <-handlerErr:
		var de *DisconnectError
		if !errors.As(err, &de) {
			t.Fatalf("error = %v (%T), want *DisconnectError", err, err)
		}
		if de.Code != code {
			t.Fatalf("disconnect code = %d, want %d", de.Code, code)
		}
		if de.Reason != "permission denied" {
			t.Fatalf("disconnect reason = %q, want %q", de.Reason, "permission denied")
		}

		// The WS path must produce the same type and value: feed the same
		// code through a close frame and compare.
		wsErr := closeFrameError(websocket.FormatCloseMessage(code, "permission denied"))
		var wsDe *DisconnectError
		if !errors.As(wsErr, &wsDe) {
			t.Fatalf("close frame error = %v (%T), want *DisconnectError", wsErr, wsErr)
		}
		if wsDe.Code != de.Code {
			t.Fatalf("WS path code = %d, gRPC path code = %d, want equal", wsDe.Code, de.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("error handler was not called for DISCONNECT_ERROR envelope")
	}
}

// TestClientDisconnectCodeMissingMetadata verifies that an error envelope
// without the disconnect_code metadata keeps the pre-existing plain error
// behavior (task 3 requirement: no new error path from missing metadata).
func TestClientDisconnectCodeMissingMetadata(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())

	handlerErr := make(chan error, 1)
	c.OnError(func(err error) { handlerErr <- err })

	c.handleMessage(&clientpb.OutboundMessage{
		Envelope: &clientpb.OutboundMessage_Error{
			Error: &sharedpb.Error{
				Code:    "GENERIC",
				Type:    "server_error",
				Message: "generic failure",
			},
		},
	}, 0)

	select {
	case err := <-handlerErr:
		var de *DisconnectError
		if errors.As(err, &de) {
			t.Fatalf("error = %v, want plain error without metadata", err)
		}
		if !strings.Contains(err.Error(), "generic failure") {
			t.Fatalf("error = %v, want the server message preserved", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("error handler was not called")
	}
}

// TestClientDisconnectCodeNonNumberMetadata verifies that a disconnect_code
// metadata entry that is not a Number value (malformed) keeps the existing
// plain error behavior instead of panicking or producing a typed error.
func TestClientDisconnectCodeNonNumberMetadata(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())

	handlerErr := make(chan error, 1)
	c.OnError(func(err error) { handlerErr <- err })

	c.handleMessage(&clientpb.OutboundMessage{
		Envelope: &clientpb.OutboundMessage_Error{
			Error: &sharedpb.Error{
				Code:    "DISCONNECT_ERROR",
				Type:    "transport_error",
				Message: "weird",
				Metadata: &structpb.Struct{Fields: map[string]*structpb.Value{
					"disconnect_code": structpb.NewStringValue("3507"),
				}},
			},
		},
	}, 0)

	select {
	case err := <-handlerErr:
		var de *DisconnectError
		if errors.As(err, &de) {
			t.Fatalf("error = %v, want plain error for non-number metadata", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("error handler was not called")
	}
}

// TestWSTransportCloseFrameDisconnectCode verifies the WebSocket path surfaces
// a typed DisconnectError carrying the numeric close code, matching the
// disconnect code the gRPC path decodes from the DISCONNECT_ERROR metadata.
func TestWSTransportCloseFrameDisconnectCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(3507, "permission denied"))
		_ = conn.Close()
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	trans, err := newWSTransport(wsURL, EncodingJSON, 5*time.Second)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer func() { _ = trans.Close() }()

	_, err = trans.Recv(context.Background())
	var de *DisconnectError
	if !errors.As(err, &de) {
		t.Fatalf("Recv error = %v (%T), want *DisconnectError", err, err)
	}
	if de.Code != 3507 {
		t.Fatalf("close code = %d, want 3507", de.Code)
	}
	if de.Reason != "permission denied" {
		t.Fatalf("close reason = %q, want %q", de.Reason, "permission denied")
	}
}

// TestClientDisconnectErrorAfterAckMissingMetadata verifies the disconnect
// parsing never panics on an empty error envelope (nil metadata and message).
func TestClientDisconnectErrorNilSafety(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())

	handlerErr := make(chan error, 1)
	c.OnError(func(err error) { handlerErr <- err })

	c.handleMessage(&clientpb.OutboundMessage{
		Envelope: &clientpb.OutboundMessage_Error{
			Error: &sharedpb.Error{Code: "DISCONNECT_ERROR", Type: "transport_error"},
		},
	}, 0)

	select {
	case err := <-handlerErr:
		if err == nil {
			t.Fatal("expected a non-nil error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("error handler was not called")
	}
}

// TestClientDisconnectErrorFormat locks the error string of DisconnectError so
// callers can rely on the code being visible in logs.
func TestClientDisconnectErrorFormat(t *testing.T) {
	e := &DisconnectError{Code: 3512, Reason: "slow consumer"}
	got := e.Error()
	if !strings.Contains(got, "3512") || !strings.Contains(got, "slow consumer") {
		t.Fatalf("DisconnectError.Error() = %q, want code and reason", got)
	}
}
