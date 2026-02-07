# AGENTS.md

This file provides guidance for agentic coding agents operating in this repository.

## Project Overview

MessageLoop is a realtime messaging platform server written in Go. It provides pub/sub messaging over WebSocket and gRPC, using CloudEvents-based protocol.

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
go run cmd/server/main.go --config-dir ./configs
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

    cloudevents "github.com/cloudevents/sdk-go/binding/format/protobuf/v2/pb"
    sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
    clientpb "github.com/messageloopio/messageloop/shared/genproto/v1"
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

- Use typed `Disconnect` errors for intentional disconnection with codes (3000-3509 range)
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
- Use options pattern for optional parameters (see `PublishOption`, `WithClientDesc`)

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

### CloudEvents Usage

- Use `cloudevents.CloudEvent` for Publish and RPC operations
- Access data via `event.GetBinaryData()` and `event.GetTextData()`
- CloudEvents are wrapped in `InboundMessage` and `OutboundMessage` envelopes

## Architecture Patterns

- **Sharding**: Hub uses 64 shards, subscription locks use 16384 shards
- **Protocol abstraction**: Core logic independent of transport (WebSocket/gRPC)
- **Marshaler pattern**: `Marshaler` interface with `JSONMarshaler` and `ProtobufMarshaler`
- **Disconnect handling**: Typed errors for graceful disconnection with codes

## Key Files

- `client.go`: Client session handling, message routing
- `hub.go`: Connection registry with sharding
- `broker.go`: Pub/sub interface with memory/Redis implementations
- `node.go`: Central coordinator
- `pkg/topics/`: Topic matcher implementations (cstrie, trie, naive, inverted_bitmap)
