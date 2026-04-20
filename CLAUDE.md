# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

MessageLoop is a realtime messaging platform server written in Go. It provides pub/sub messaging capabilities over WebSocket and gRPC streaming transports.

## Build and Test Commands

**Note:** This project uses [Task](https://taskfile.dev) for build automation. Install with `go install github.com/go-task/task/v3/cmd/task@latest`.

### Initialize development environment
```bash
task init  # Installs protoc-gen-go and buf
```

### Build
```bash
go build ./...
```

### Generate protocol buffers
```bash
task generate-protocol
```

### Run tests
```bash
go test ./...
go test ./pkg/topics/...  # Run tests for specific package
go test -v ./pkg/topics/... # Run with verbose output
go test -v ./pkg/topics/... -run TestCSTrieMatcher # Run a single test
```

### Run the server
```bash
go run cmd/server/main.go --config ./config.yaml
```

## Configuration

Config structure defined in `config/config.go` with example in `config-example.yaml`:
- **server** - Admin HTTP address (`127.0.0.1:8080`), admin gRPC address (`127.0.0.1:9091`), heartbeat idle timeout (default: `300s`), RPC timeout (default: `30s`), per-user and per-client limits, built-in ACL rules
- **transport** - WebSocket (`:9080` with `/ws` path) and client gRPC streaming (`:9090`) listeners, plus optional TLS, compression, and write timeouts
- **broker** - Type selection (`memory` or `redis`) with Redis connection, stream, and history settings
- **cluster** - Optional Redis-backed distributed control plane with `enabled`, `node_id`, and `backend`
- **proxy** - Backend proxy configurations with routes, timeout, and HTTP/gRPC backends

## Architecture

### Core Components

**Node** (`node.go`) - Central coordinator that manages:
- **Hub** - Sharded registry for client connections and channel subscriptions
- **Broker** - Pub/sub message broker
- **Proxy** - Backend RPC proxy integration
- Subscription locks (16384 shards) for channel-level concurrency control

**Client** (`client.go`) - Represents a single connection. Handles protocol messages:
- `Connect` - Initial authentication and session establishment
- `Publish` - Publish messages to channels
- `Subscribe` / `Unsubscribe` - Manage channel subscriptions
- `RpcRequest` - RPC-style request/response (proxied to backend with timeout protection)
- `Ping` / `Pong` - Connection keepalive

**Hub** (`hub.go`) - Sharded connection registry with 64 shards:
- `connShard` - Maps session IDs and user IDs to active clients
- `subShard` - Maps channels to subscribed clients

**Broker** (`broker.go`) - Interface for pub/sub operations with pluggable implementations:
- **Memory broker** (`broker_memory.go`) - In-memory pub/sub for single-node deployments
- **Redis broker** (`pkg/redisbroker/`) - Distributed broker using Redis Streams and Pub/Sub
- `Publish` - Send data to a channel (optionally maintains history)
- `Subscribe` / `Unsubscribe` - Manage node's channel subscriptions
- `History` - Retrieve message history from stream (supports filtering, pagination, `StreamPosition` with offset+epoch)
- `PublishJoin` / `PublishLeave` - Presence notifications

**Proxy** (`proxy/`) - Backend service integration for RPC requests:
- Supports HTTP and gRPC backends
- Routes RPC requests based on channel patterns
- Configured via YAML with timeout and endpoint settings
- Three-tier timeout control: client-level, global, and proxy-specific
- Automatic timeout detection with friendly error messages

### Transports

The system abstracts connection handling via the **Transport** interface (`transport.go`):
- `Write` / `WriteMany` - Send bytes to client
- `Close` - Close connection with disconnect reason

**WebSocket** (`websocket/`) - HTTP-upgraded WebSocket connections:
- Handler detects encoding from WebSocket subprotocol (`messageloop`, `messageloop+json`, `messageloop+proto`)
- Integrates with `lynx` framework for lifecycle management

**gRPC Stream** (`grpcstream/`) - Bidirectional gRPC streaming:
- Uses custom `RawCodec` to avoid double-encoding messages
- Each client connection is a separate bidirectional stream

### Protocol

The protocol uses a simple message format for pub/sub:

**Generated protobuf code** in `genproto/`:
- `v1/` - Client protocol messages (InboundMessage, OutboundMessage, Connect, Subscribe, etc.)
- `shared/v1/` - Shared error types
- `server/v1/` - Server API definitions
- `proxy/v1/` - Proxy protocol
- `event/v1/` - Event definitions

**Key protocol types:**
- `InboundMessage` - Client-to-server messages with oneof envelope for Connect, Subscribe, Publish, RpcRequest, etc.
- `OutboundMessage` - Server-to-client messages with oneof envelope for Connected, SubscribeAck, PublishAck, Publication, RpcReply, etc.
- `Message` - Wrapper containing Channel, Id, Offset, and Payload
- `Publication` - Contains Envelopes (array of Message)

**The `protocol` package** provides marshalers for JSON and protobuf encoding:
- `JSONMarshaler{}` - Standard JSON encoding
- `ProtobufMarshaler{}` - Protobuf binary encoding
- `ProtoJSONMarshaler` - Protobuf JSON encoding

**Note:** `Publish` and `RpcRequest` use Payload with Binary, Text, or Json data.

### Topic Matching

`pkg/topics/` contains various topic matcher implementations for wildcard channel subscription:
- `matcher.go` - Interface for Subscribe/Unsubscribe/Lookup operations
- `cstrie.go` - Lock-free concurrent trie using CAS operations
- `trie.go`, `naive.go`, `inverted_bitmap.go` - Alternative implementations
- Topics use `.` delimiter and `*` wildcard

## Key Patterns

1. **Sharding** - Hub uses 64 shards, subscription locks use 16384 shards to reduce contention
2. **Protocol abstraction** - Core logic independent of transport; WebSocket and gRPC are pluggable
3. **Marshaler selection** - WebSocket negotiates encoding via subprotocol; gRPC uses protobuf only
4. **Disconnect handling** - Uses typed `Disconnect` errors (`disconnect.go`) to signal intentional disconnection with codes (3000-3509)
5. **Payload format** - Publish and RPC operations use Payload with Binary/Text/Json data
6. **StreamPosition recovery** - History streams use offset + epoch semantics for reliable recovery

## TypeScript SDK

### Build
```bash
cd sdks/ts
npm install
npm run build
```

### Test
```bash
npm test
```

### Examples
- `sdks/ts/examples/browser/index.html` - Browser WebSocket example
- `sdks/ts/examples/node/client.ts` - Node.js example

## Dependencies

- `github.com/lynx-go/lynx` - Application framework providing lifecycle management
- `github.com/gorilla/websocket` - WebSocket implementation
- `google.golang.org/grpc` - gRPC framework
- `google.golang.org/protobuf` - Protobuf support
- `github.com/RoaringBitmap/roaring` - Compressed bitmap for topic matching
- `github.com/redis/go-redis/v9` - Redis client (for distributed broker option)

## Module Structure

- **Root package** (`*.go`) - Core types: Node, Client, Hub, Broker, Transport
- `cmd/server/` - Server entry point using `lynx` framework
- `pkg/websocket/` - WebSocket transport implementation
- `pkg/grpcstream/` - gRPC streaming transport implementation
- `pkg/topics/` - Topic matching algorithms for wildcard subscriptions
- `pkg/redisbroker/` - Redis-based distributed broker implementation
- `proxy/` - RPC proxy backend integration
- `sdks/go/` - Go client SDK for MessageLoop
- `sdks/go/example/` - Go SDK examples:
  - `basicwebsocket/` - Basic WebSocket connection example
  - `basicgrpc/` - Basic gRPC connection example
  - `dynamicsub/` - Dynamic subscription example
  - `protobuf/` - Protobuf encoding example
  - `wsrpc/` - WebSocket RPC example
  - `proxyserver/` - Proxy server example
- `sdks/ts/` - TypeScript/JavaScript client SDK for MessageLoop
- `protocol/` - Protobuf definitions (source)
- `genproto/` - Generated protobuf Go code (local replace)
- `config/` - Configuration structures
