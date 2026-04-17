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
  - `gRPC`: Handles bidirectional streaming with custom `RawCodec` to avoid double-encoding.
- **Protocol**: Protobuf-defined client envelopes and shared payloads.
  - `InboundMessage`/`OutboundMessage` contain operations such as Connect, Publish, Subscribe, RPC, and Survey.
  - `sharedpb.Payload` supports Binary, Text, or JSON data fields.

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

## MCP Servers

- **Redis**: A configuration for the Redis MCP server is available in `.github/mcp.json`.
  - Connects to: `redis://127.0.0.1:6379/10` (password protected).
  - Use this to inspect streams and keys during development.
