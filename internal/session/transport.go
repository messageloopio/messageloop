package session

import "errors"

// Transport abstracts a client connection (WebSocket, gRPC stream) as a
// channel of already-framed inbound messages and a WriteMessage sink.
type Transport interface {
	Write([]byte) error
	WriteMany(...[]byte) error
	Close(Disconnect) error
	RemoteAddr() string
}

// ErrPeerGone marks a transport write error whose shape means the peer went
// away (WebSocket normal/going-away close, gRPC Canceled/Unavailable). The
// adapter that produces such errors wraps them with this sentinel at the
// seam, so the session's write-error classification (handleWriteError →
// Disconnect 3000 vs 3512) never has to import transport libraries to sniff
// their error shapes.
var ErrPeerGone = errors.New("peer closed the connection")
