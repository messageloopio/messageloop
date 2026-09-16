# AGENTS.md

This file provides guidance for agentic coding agents operating in this repository.

## Project Overview

MessageLoop is a realtime messaging platform server written in Go. It provides pub/sub messaging over WebSocket and gRPC using protobuf-defined message envelopes and shared payload types.

**Namespaces (multi-tenant, P1)**: every client-visible channel is namespaced — `ns:topic` (e.g. `acme:chat.room1`). `:` participates in topic segment matching alongside `.` (see `pkg/topics`). The namespace is resolved at connect time from the auth proxy response (`UserInfo.namespace`), falling back to the static `server.namespace` (mandatory when `require_auth` is disabled); out-of-namespace channel operations are rejected with `NAMESPACE_MISMATCH`, and cross-namespace resume is refused (3500). Per-user connection limits and the server API/cluster user index are scoped to `(namespace, user)`.

Current listener model:

- WebSocket client traffic on `transport.websocket.addr`.
- Client gRPC streaming on `transport.grpc.addr`.
- Optional client QUIC on `transport.quic.addr` (UDP, TLS 1.3; empty addr disables it).
- Optional client KCP on `transport.kcp.addr` (UDP, KCP reliability layer with a TLS overlay; empty addr disables it).
- Server-side gRPC API (Server API) on `server.api.addr`.
- Admin HTTP health/metrics on `server.http.addr`.

## Build Commands

```bash
# Build all packages
go build ./...

# Run all tests
go test ./...

# Run tests for specific package
go test ./pkg/topics/...

# Run tests with verbose output
go test -v ./pkg/topics/...

# Run a single test
go test -v ./pkg/topics/... -run TestCSTrieMatcher

# Generate protocol buffers (requires buf)
task generate-protocol

# Initialize dev environment (installs protoc-gen-go and buf)
task init

# Run the server
go run ./cmd/server --config ./config.yaml
```

## Code Style Guidelines

### Imports

Organize imports in three groups separated by blank lines:
1. Standard library
2. Third-party dependencies
3. Local imports (this project)

Use aliases for protobuf packages to keep code clean:
```go
import (
    "context"
    "errors"
    "fmt"
    "sync"
    "time"

    serverv2 "github.com/messageloopio/messageloop/shared/genproto/server/v2"
    sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
    clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
    "github.com/messageloopio/messageloop/proxy"
    "github.com/lynx-go/x/log"
    "github.com/samber/lo"
    "google.golang.org/protobuf/proto"
)
```

### Naming Conventions

- **Exported types/functions/constants**: PascalCase (e.g., `NewClientSession`, `EncodingTypeJSON`)
- **Unexported variables/fields**: camelCase (e.g., `client`, `session`, `heartbeatCancel`)
- **Interfaces**: Simple names describing capability (e.g., `Broker`, `Transport`, `Marshaler`, `Matcher`)
- **Errors**: `Disconnect` type with typed codes (e.g., `DisconnectBadRequest`, `DisconnectStale`)
- **Constants**: Group related constants with `const` block, use iota for enum-like values

### Error Handling

- Use typed `Disconnect` errors for intentional disconnection with codes (3000-3514 range)
- Wrap errors with `fmt.Errorf("context: %w", err)` for error chaining
- Use `errors.As()` for type assertion on error types
- Use `errors.Is()` for sentinel error comparison
- Never suppress errors with `_` unless intentionally ignoring
- Log errors at the appropriate level before returning

### Types and Structs

- Use `type X struct { ... }` for structs
- Use `type EncodingType int` with iota-based constants for enums
- Use `type MyFunc func()` for function types
- Embed `sync.Mutex` or `sync.RWMutex` by value for struct-level locking
- Use package-level documentation for exported types

### Function Organization

- Receiver methods grouped by type: `(c *ClientSession)`, `(h *Hub)`
- Put related private helpers below their public counterparts
- Keep functions focused and under ~100 lines when possible
- Use options pattern for optional parameters (see `PublishOption`, `WithClientInfo`)

### Comments

- Add doc comments for all exported types, functions, and constants
- Comment non-obvious logic inline
- Use `//` for single-line comments, `/* */` for multi-line
- Prefix receiver method comments with type name: `// Send writes a message to the client`

### Testing

- Test files: `*_test.go` in same package
- Test functions: `TestXxx(t *testing.T)` pattern
- Use `testify/assert` for assertions: `assert.NoError(t, err)`
- Helper functions in test files (e.g., `assertEqual`)
- Include benchmarks: `BenchmarkXxx(b *testing.B)`
- Use table-driven tests for multiple test cases

Example test pattern:
```go
func TestCSTrieMatcher(t *testing.T) {
    assert := assert.New(t)
    m := NewCSTrieMatcher()
    sub, err := m.Subscribe("forex.*", subscriber)
    assert.NoError(err)
    // ...
}
```

### Payload Usage

- Client protocol messages use `InboundMessage` and `OutboundMessage` envelopes.
- Publish and RPC payloads use `sharedpb.Payload` with binary, text, or JSON variants.
- SDK-level helpers wrap payloads as `Message` plus typed `Data` helpers rather than CloudEvents.

## Architecture Patterns

- **Sharding**: Hub uses 64 shards, subscription locks use 16384 shards
- **Protocol abstraction**: Core logic independent of transport (WebSocket/gRPC)
- **Split gRPC surfaces**: Client streaming and Server API RPCs run on separate listeners but share the same in-process `Node`
- **Marshaler pattern**: `Marshaler` interface with `JSONMarshaler` and `ProtobufMarshaler`
- **Disconnect handling**: Typed errors for graceful disconnection with codes

## Key Files

- `internal/session/client.go`: Client session handling, message routing
- `internal/session/hub.go`: Connection registry with sharding
- `internal/stream/broker.go`: Pub/sub interface with memory/Redis implementations
- `internal/runtime/node.go`: Central coordinator (Node, Cluster facade, recover)
- `cmd/server/main.go`: Bootstrap wiring and listener setup
- `cmd/server/runtime.go`: gRPC preflight and startup ordering helpers
- `pkg/transport/grpc/client_server.go`: Client gRPC streaming server component
- `internal/serverapi/server.go`: Server API gRPC server component
- `pkg/transport/grpc/server.go`: Shared gRPC server preparation and listener lifecycle
- `pkg/transport/quic/`: Optional QUIC client transport (length-prefixed frames over one bidirectional stream)
- `pkg/transport/kcp/`: Optional KCP client transport (length-prefixed frames over a TLS-secured KCP session)
- `pkg/topics/`: Topic matcher implementations (cstrie, trie, naive, inverted_bitmap)
