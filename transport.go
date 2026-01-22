package messageloop

type Transport interface {
	Write([]byte) error
	WriteMany(...[]byte) error
	Close(Disconnect) error
}
