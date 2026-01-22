module github.com/fleetlit/messageloop/sdks/go

go 1.25

require (
	github.com/cloudevents/sdk-go/binding/format/protobuf/v2 v2.16.2
	github.com/fleetlit/messageloop/genproto v1.0.0
	github.com/gorilla/websocket v1.5.3
	google.golang.org/grpc v1.78.0
	google.golang.org/protobuf v1.36.11
)

require (
	golang.org/x/net v0.47.0 // indirect
	golang.org/x/sys v0.38.0 // indirect
	golang.org/x/text v0.31.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251029180050-ab9386a59fda // indirect
)

replace github.com/fleetlit/messageloop/genproto => ../../genproto
