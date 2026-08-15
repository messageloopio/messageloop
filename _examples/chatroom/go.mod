module github.com/messageloopio/messageloop/_examples/chatroom

go 1.26.5

require (
	github.com/messageloopio/messageloop/sdks/go v0.0.0
	github.com/messageloopio/messageloop/shared v0.2.0
	google.golang.org/grpc v1.79.1
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/quic-go/quic-go v0.61.0 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251202230838-ff82c1b0f217 // indirect
)

replace github.com/messageloopio/messageloop/sdks/go => ../../sdks/go

replace github.com/messageloopio/messageloop/shared => ../../shared
