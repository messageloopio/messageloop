module github.com/messageloopio/messageloop/sdks/go

go 1.26.0

require (
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.3
	github.com/messageloopio/messageloop/shared v0.2.0
	github.com/quic-go/quic-go v0.62.0
	github.com/xtaci/kcp-go/v5 v5.6.72
	google.golang.org/grpc v1.83.2
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/klauspost/cpuid/v2 v2.2.10 // indirect
	github.com/klauspost/reedsolomon v1.12.0 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/tjfoc/gmsm v1.4.1 // indirect
	go.opentelemetry.io/otel/metric v1.45.0 // indirect
	go.opentelemetry.io/otel/sdk v1.45.0 // indirect
	go.opentelemetry.io/otel/trace v1.45.0 // indirect
	golang.org/x/crypto v0.55.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	golang.org/x/time v0.16.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260911204522-f61a6ca850bd // indirect
)

// tjfoc/gmsm v1.4.1 (frozen, go 1.14) drags grpc@v1.31.0 ->
// go-control-plane@v0.9.4 -> pre-split google.golang.org/genproto@2019 into
// the module graph. Together with the genproto/googleapis/rpc module this
// breaks workspace-mode builds with "ambiguous import". Force the post-split
// monolith, which no longer contains the googleapis packages.
replace google.golang.org/genproto => google.golang.org/genproto v0.0.0-20260911204522-f61a6ca850bd

replace github.com/messageloopio/messageloop/shared => ./../../shared
