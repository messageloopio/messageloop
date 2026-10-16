package shared

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestWriteReadFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte(`{"id":"1","connect":{}}`)
	if err := WriteFrame(&buf, payload); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}

	got, err := ReadFrame(&buf, 0)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %q, want %q", got, payload)
	}
}

func TestReadFrameTooLarge(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, bytes.Repeat([]byte("x"), 32)); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}

	_, err := ReadFrame(&buf, 16)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("got %v, want ErrFrameTooLarge", err)
	}
}

func TestReadFrameEmpty(t *testing.T) {
	_, err := ReadFrame(bytes.NewReader([]byte{0, 0, 0, 0}), 0)
	if !errors.Is(err, ErrEmptyFrame) {
		t.Fatalf("got %v, want ErrEmptyFrame", err)
	}
}

func TestWriteFrameEmpty(t *testing.T) {
	err := WriteFrame(io.Discard, nil)
	if !errors.Is(err, ErrEmptyFrame) {
		t.Fatalf("got %v, want ErrEmptyFrame", err)
	}
}

func TestReadFrameEOF(t *testing.T) {
	_, err := ReadFrame(bytes.NewReader(nil), 0)
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("got %v, want EOF", err)
	}
}

func TestMarshalerForALPN(t *testing.T) {
	if got := MarshalerForALPN(ALPNMessageLoopProto).Name(); got != "proto" {
		t.Fatalf("proto alpn: got %q", got)
	}
	if got := MarshalerForALPN(ALPNMessageLoopJSON).Name(); got != "json" {
		t.Fatalf("json alpn: got %q", got)
	}
	if got := MarshalerForALPN(ALPNMessageLoop).Name(); got != "json" {
		t.Fatalf("default alpn: got %q", got)
	}
	if got := MarshalerForALPN("").Name(); got != "json" {
		t.Fatalf("empty alpn: got %q", got)
	}
}
