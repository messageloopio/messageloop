# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

MessageLoop is a realtime messaging platform server written in Go. It provides pub/sub messaging capabilities over WebSocket and gRPC streaming transports, using a CloudEvents-based protocol.

## Build and Test Commands

### Build
```bash
go build ./...
```

### Generate protocol buffers
```bash
task generate-protocol  # Requires buf: go install github.com/bufbuild/buf/cmd/buf@v1.63.0
```

### Run tests
```bash
go test ./...
go test ./pkg/topics/...  # Run tests for specific package
go test -v ./pkg/topics/... # Run with verbose output
```

### Run the server
```bash
go run cmd/server/main.go --config-dir ./configs
```

## Configuration

Config structure defined in `config/config.go` with example in `config-example.yaml`:
- **Transport** - WebSocket address (`:9080`) and gRPC address (`:9090`)
- **Broker** - Type selection (`memory` or `redis`) with Redis connection settings
- **Proxies** - Backend proxy configurations for RPC routing (HTTP or gRPC)

## Architecture

### Core Components

**Node** (`node.go`) - Central coordinator that manages:
- **Hub** - Sharded registry for client connections and channel subscriptions
- **Broker** - Pub/sub message broker
- **Proxy** - Backend RPC proxy integration
- Subscription locks (16384 shards) for channel-level concurrency control

**Client** (`client.go`) - Represents a single connection. Handles protocol messages:
- `Connect` - Initial authentication and session establishment
- `Publish` - Publish CloudEvents to channels
- `Subscribe` / `Unsubscribe` - Manage channel subscriptions
- `RpcRequest` - RPC-style request/response using CloudEvents (proxied to backend)
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

The protocol uses CloudEvents for message passing:

**Generated protobuf code** in `genproto/`:
- `v1/` - Client protocol messages (InboundMessage, OutboundMessage, Connect, Subscribe, etc.)
- `shared/v1/` - Shared error types
- `server/v1/` - Server API definitions
- `proxy/v1/` - Proxy protocol
- `event/v1/` - Event definitions
- `includes/cloudevents/` - CloudEvent protobuf definitions

**Key protocol types:**
- `InboundMessage` - Client-to-server messages with oneof envelope for Connect, Subscribe, Publish (CloudEvent), RpcRequest (CloudEvent), etc.
- `OutboundMessage` - Server-to-client messages with oneof envelope for Connected, SubscribeAck, PublishAck, Publication, RpcReply (CloudEvent), etc.
- `Message` - Wrapper containing Channel, Id, Offset, and Event (CloudEvent)
- `Publication` - Contains Envelopes (array of Message)

**The `protocol` package** provides marshalers for JSON and protobuf encoding:
- `JSONMarshaler{}` - Standard JSON encoding
- `ProtobufMarshaler{}` - Protobuf binary encoding
- `ProtoJSONMarshaler` - Protobuf JSON encoding

**Note:** The protocol is CloudEvents-based. `Publish` and `RpcRequest` use CloudEvents with `BinaryData` or `TextData` fields.

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
5. **CloudEvents** - Publish and RPC operations wrap data in CloudEvents with BinaryData/TextData fields
6. **StreamPosition recovery** - History streams use offset + epoch semantics for reliable recovery

## Dependencies

- `github.com/cloudevents/sdk-go/binding/format/protobuf/v2` - CloudEvent protobuf format
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
- `protocol/` - Protobuf definitions (source)
- `genproto/` - Generated protobuf Go code (local replace)
- `config/` - Configuration structures
