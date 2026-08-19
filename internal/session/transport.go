package session

// Transport abstracts a client connection (WebSocket, gRPC stream) as a
// channel of already-framed inbound messages and a WriteMessage sink.
type Transport interface {
	Write([]byte) error
	WriteMany(...[]byte) error
	Close(Disconnect) error
	RemoteAddr() string
}
