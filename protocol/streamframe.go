package shared

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	// FrameHeaderSize is the number of bytes in a stream-frame length prefix.
	FrameHeaderSize = 4

	// ALPNMessageLoop is the default ALPN identifier (JSON encoding).
	ALPNMessageLoop = "messageloop"
	// ALPNMessageLoopJSON selects protojson encoding.
	ALPNMessageLoopJSON = "messageloop+json"
	// ALPNMessageLoopProto selects binary protobuf encoding.
	ALPNMessageLoopProto = "messageloop+proto"
)

// ErrFrameTooLarge is returned when a length-prefixed frame exceeds the
// configured maximum size.
var ErrFrameTooLarge = errors.New("quic frame exceeds max size")

// ErrEmptyFrame is returned when a length prefix of zero is received.
var ErrEmptyFrame = errors.New("quic frame length is zero")

// ALPNProtocols is the server-side ALPN offer list. Order matters: TLS
// picks the first entry that the client also offered.
func ALPNProtocols() []string {
	return []string{ALPNMessageLoopProto, ALPNMessageLoopJSON, ALPNMessageLoop}
}

// MarshalerForALPN maps a negotiated ALPN identifier to a Marshaler.
// "messageloop+proto" speaks binary protobuf; every other negotiated value
// (including the bare "messageloop" alias) speaks protojson, matching the
// WebSocket subprotocol mapping.
func MarshalerForALPN(alpn string) Marshaler {
	if alpn == ALPNMessageLoopProto {
		return ProtobufMarshaler{}
	}
	return JSONMarshaler{}
}

// WriteFrame writes a single length-prefixed payload to w. The length is a
// big-endian uint32. The payload must be non-empty.
func WriteFrame(w io.Writer, payload []byte) error {
	if len(payload) == 0 {
		return ErrEmptyFrame
	}
	var hdr [FrameHeaderSize]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if err := writeFull(w, hdr[:]); err != nil {
		return err
	}
	return writeFull(w, payload)
}

// ReadFrame reads one length-prefixed payload from r. When maxSize > 0 and
// the advertised length exceeds it, ErrFrameTooLarge is returned and the
// payload is not consumed so the caller can close the stream.
func ReadFrame(r io.Reader, maxSize int) ([]byte, error) {
	var hdr [FrameHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return nil, ErrEmptyFrame
	}
	if maxSize > 0 && int(n) > maxSize {
		return nil, fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, n, maxSize)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func writeFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}
