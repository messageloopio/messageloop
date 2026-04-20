# Copilot Instructions for MessageLoop

## Build and Test Commands

- **Build**: `go build ./...`
- **Test All**: `go test ./...`
- **Test Package**: `go test -v ./pkg/topics/...`
- **Single Test**: `go test -v ./pkg/topics/ -run TestCSTrieMatcher`
- **Generate Protocol**: `task generate-protocol` (run `task init` first to install dependencies like `buf`)
- **Run Server**: `go run cmd/server/main.go --config ./config.yaml`
- **TypeScript SDK**: `cd sdks/ts && npm install && npm run build`

## High-Level Architecture

- **Node**: Central coordinator managing Hub, Broker, and Proxy components.
- **Hub**: Connection and subscription registry sharded into 64 shards to reduce lock contention.
- **Broker**: Abstract interface for Pub/Sub. Implementations:
  - `MemoryBroker`: In-process pub/sub for single-node deployments.
  - `RedisBroker`: Distributed pub/sub using Redis Streams and Pub/Sub.
- **Transports**: Pluggable connection handling abstraction:
  - `WebSocket`: Handles HTTP upgrades and negotiates encoding (json/proto).
  - `gRPC client streaming`: Handles bidirectional client sessions with custom `RawCodec` to avoid double-encoding.
  - `gRPC admin API`: Exposes `messageloop.server.v1.APIService` on a separate listener.
- **Protocol**: Protobuf-defined client envelopes and shared payloads.
  - `InboundMessage`/`OutboundMessage` contain operations such as Connect, Publish, Subscribe, RPC, and Survey.
  - `sharedpb.Payload` supports Binary, Text, or JSON data fields.

## Listener Model

- `transport.websocket.addr`: WebSocket client traffic
- `transport.grpc.addr`: client gRPC streaming traffic
- `server.grpc_admin.addr`: server-side gRPC admin API
- `server.http.addr`: health and Prometheus metrics

## Key Conventions

- **Imports**: Group into three sections separated by blank lines: Standard Library, Third-Party, and Local.
  - Use aliases for generated protobuf packages (e.g., `clientpb`, `sharedpb`) to avoid naming conflicts.
- **Error Handling**: Use typed `Disconnect` errors (range 3000-3509) for intentional connection termination.
  - Wrap errors with `fmt.Errorf("context: %w", err)` for chaining.
- **Concurrency**: Use `sync.Mutex` or `sync.RWMutex` embedded by value in structs.
- **Naming**:
  - Exported: PascalCase (e.g., `NewClientSession`)
  - Private: camelCase (e.g., `client`, `session`)
  - Interfaces: Capability-based (e.g., `Marshaler`, `Transport`)
- **Testing**:
  - Use `testify/assert` for assertions.
  - Use table-driven tests for multiple cases.
  - Place tests in `*_test.go` files within the same package.
- **Configuration**: Use `lynx` framework options pattern and `config` package structs.

## gRPC Package Structure

- `pkg/grpcstream/client_server.go`: client streaming listener/component
- `pkg/grpcstream/admin_server.go`: admin gRPC listener/component
- `pkg/grpcstream/server.go`: shared preparation, validation, TLS loading, and pre-bound listener lifecycle
- `cmd/server/runtime.go`: bootstrap preflight that prepares gRPC listeners before `node.Run(...)`

## MCP Servers

- **Redis**: A configuration for the Redis MCP server is available in `.github/mcp.json`.
  - Connects to: `redis://127.0.0.1:6379/10` (password protected).
  - Use this to inspect streams and keys during development.
