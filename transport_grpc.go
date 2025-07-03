package messageloop

type grpcTransport struct {
}

func (gt *grpcTransport) Write(bytes []byte) error {
	//TODO implement me
	panic("implement me")
}

func (gt *grpcTransport) WriteMany(bytes ...[]byte) error {
	//TODO implement me
	panic("implement me")
}

func (gt *grpcTransport) Close(disconnect Disconnect) error {
	//TODO implement me
	panic("implement me")
}

var _ Transport = new(grpcTransport)
