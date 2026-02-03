# MessageLoop

A realtime messaging platform server written in Go, providing pub/sub messaging capabilities over WebSocket and gRPC streaming transports, using a CloudEvents-based protocol.

## Features

- **Multiple Transports** - WebSocket and gRPC streaming support
- **Flexible Encoding** - JSON and Protobuf message encoding
- **CloudEvents Protocol** - Standardized event format for message passing
- **Pub/Sub Messaging** - Channel-based publish/subscribe with wildcard support
- **RPC Integration** - Request/response messaging with backend proxy support
- **Distributed Broker** - Pluggable broker backend (in-memory or Redis Streams)
- **Sharded Architecture** - High-performance connection and subscription management
- **Client SDKs** - Go and TypeScript/JavaScript SDKs available

## Quick Start

### Installation

```bash
git clone https://github.com/deeplooplabs/messageloop.git
cd messageloop
go mod download
```

### Running the Server

```bash
go run cmd/server/main.go --config-dir ./configs
```

The server will start with:
- WebSocket transport on `ws://localhost:9080/ws`
- gRPC transport on `localhost:9090`

### Configuration

Create a configuration file (see `config-example.yaml`) or use the default settings:

```yaml
transport:
  websocket:
    address: ":9080"
  grpc:
    address: ":9090"

broker:
  type: "memory"  # or "redis"
  redis:
    addr: "localhost:6379"
```

## Client SDKs

### Go SDK

```go
import "github.com/deeplooplabs/messageloop/sdks/go"

client, err := messageloop.Dial(context.Background(),
    "ws://localhost:9080/ws",
    messageloop.WithClientId("my-client"),
)

// Subscribe to channels
err = client.Subscribe(context.Background(), "chat.messages")

// Publish a message
event := messageloop.NewCloudEvent(
    "/client",
    "chat.message",
    []byte(`{"text":"Hello!"}`),
)
err = client.Publish(context.Background(), "chat.messages", event)
```

See `sdks/go/example/` for more examples.

### TypeScript/JavaScript SDK

```typescript
import { dial, createCloudEvent } from "@messageloop/sdk";

const client = await dial("ws://localhost:9080/ws", [
  withClientId("my-client"),
]);

client.onMessage((events) => {
  events.forEach((msg) => console.log("Message:", msg.event.type));
});

const event = createCloudEvent({
  source: "/client",
  type: "chat.message",
  data: { text: "Hello!" },
});
await client.publish("chat.messages", event);
```

See `sdks/ts/` for more details and examples.

## Development

### Build

```bash
go build ./...
```

### Generate Protocol Buffers

```bash
# Install buf first
go install github.com/bufbuild/buf/cmd/buf@v1.63.0

# Generate
task generate-protocol
```

### Run Tests

```bash
# All tests
go test ./...

# Specific package
go test ./pkg/topics/...

# Verbose output
go test -v ./pkg/topics/...
```

### TypeScript SDK

```bash
cd sdks/ts
npm install
npm run build
npm test
```

## Architecture

MessageLoop is built with a modular, sharded architecture for high performance:

- **Node** - Central coordinator managing Hub, Broker, and Proxy
- **Hub** - 64-sharded connection registry for efficient client management
- **Broker** - Pluggable pub/sub backend (memory or Redis Streams)
- **Transport** - Abstracted connection handling (WebSocket/gRPC)
- **Proxy** - Backend service integration for RPC routing

### CloudEvents Protocol

All messaging uses CloudEvents format:
- `Publish` and `RpcRequest` wrap data in CloudEvents
- Supports `BinaryData` and `TextData` fields
- Standardized event metadata (type, source, id, time)

## Documentation

- [CLAUDE.md](CLAUDE.md) - Detailed architecture and development guide
- [sdks/go/](sdks/go/) - Go SDK documentation
- [sdks/ts/](sdks/ts/) - TypeScript SDK documentation

## License

[Add your license here]
