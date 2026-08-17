package messageloopgo

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/messageloopio/messageloop/shared"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	"github.com/quic-go/quic-go"
)

func TestQUICTLSConfig_InsecureSkipVerify(t *testing.T) {
	cfg := quicTLSConfig(&Options{InsecureSkipVerify: true})
	if !cfg.InsecureSkipVerify {
		t.Fatal("expected InsecureSkipVerify")
	}
}

func TestQUICTLSConfig_ClonesUserConfig(t *testing.T) {
	orig := &tls.Config{ServerName: "example.test"}
	opts := &Options{TLSConfig: orig, InsecureSkipVerify: true}
	cfg := quicTLSConfig(opts)
	if cfg.ServerName != "example.test" {
		t.Fatalf("ServerName = %q", cfg.ServerName)
	}
	if !cfg.InsecureSkipVerify {
		t.Fatal("expected InsecureSkipVerify to be applied on the clone")
	}
	if orig.InsecureSkipVerify {
		t.Fatal("original TLSConfig must not be mutated")
	}
}

func TestDialTransport_NoAddress(t *testing.T) {
	c := &client{opts: defaultOptions()}
	_, err := c.dialTransport()
	if err == nil {
		t.Fatal("expected error when no dial address is configured")
	}
}

func TestDisconnectFromApplicationError(t *testing.T) {
	err := &quic.ApplicationError{ErrorCode: 3511, ErrorMessage: "idle timeout"}
	var appErr *quic.ApplicationError
	if !errors.As(err, &appErr) {
		t.Fatal("expected ApplicationError")
	}
	de := &DisconnectError{Code: uint32(appErr.ErrorCode), Reason: appErr.ErrorMessage}
	if de.Code != 3511 || de.Reason != "idle timeout" {
		t.Fatalf("got %+v", de)
	}
}

func TestQUICFrameRoundTripThroughPipe(t *testing.T) {
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()

	msg := &clientpb.InboundMessage{Id: "1", Envelope: &clientpb.InboundMessage_Ping{Ping: &clientpb.Ping{}}}
	data, err := shared.JSONMarshaler{}.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- shared.WriteFrame(pw, data)
		_ = pw.Close()
	}()

	got, err := shared.ReadFrame(pr, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := <-errCh; writeErr != nil {
		t.Fatal(writeErr)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("frame mismatch")
	}

	// Drain: next read is EOF.
	_, err = shared.ReadFrame(pr, 4096)
	if err == nil {
		t.Fatal("expected EOF after writer close")
	}
}

func TestDialQUIC_Unreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := newQUICTransport(ctx, "127.0.0.1:1", EncodingJSON, 200*time.Millisecond, &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{shared.ALPNMessageLoopJSON},
	})
	if err == nil {
		t.Fatal("expected dial error against a closed port")
	}
}
