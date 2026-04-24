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
go test -race ./...         # With race detector (task test)
go test -v ./pkg/topics/... # Run tests for specific package
go test -v ./pkg/topics/... -run TestCSTrieMatcher # Run a single test
```

### Lint and vet
```bash
task vet   # go vet ./...
task lint  # golangci-lint run
```

### Integration tests (require running server)
- `pkg/websocket/integration_test.go` - Connect and publish via WebSocket
- `pkg/grpcstream/integration_test.go` - gRPC streaming e2e tests
- `pkg/grpcstream/port_integration_test.go` - Port-level gRPC tests

### Run the server
```bash
go run cmd/server/main.go --config ./config.yaml
```

### TypeScript SDK
```bash
cd sdks/ts && npm install && npm run build && npm test
```

### Other Task commands
```bash
task upgrade-lynx  # Update lynx framework and contrib deps
task release-tag   # Tag and push a release
task release-sdk-ts # Build and publish TypeScript SDK to npm
```

## Listener Model

The server exposes four distinct listeners:
- `transport.websocket.addr` — WebSocket client traffic (`:9080`, path `/ws`)
- `transport.grpc.addr` — Client gRPC streaming (`:9090`)
- `server.grpc_admin.addr` — Server-side gRPC admin API (`127.0.0.1:9091`, exposes `messageloop.server.v1.APIService`)
- `server.http.addr` — Health check and Prometheus metrics (`127.0.0.1:8080`)

## Configuration

Config structure defined in `config/config.go` with example in `config-example.yaml`:
- **server** - Admin HTTP address, admin gRPC address, heartbeat idle timeout (default: `300s`), RPC timeout (default: `30s`), per-user and per-client limits, built-in ACL rules, optional `require_auth`
- **transport** - WebSocket and client gRPC streaming listeners, plus optional TLS, compression, and write timeouts
- **broker** - Type selection (`memory` or `redis`) with Redis connection, stream, and history settings
- **cluster** - Optional Redis-backed distributed control plane with `enabled`, `node_id`, and `backend`
- **proxy** - Backend RPC proxy configurations with routes, timeout, and HTTP/gRPC backends

## Architecture

### Core Components

**Node** (`node.go`) - Central coordinator that manages Hub, Broker, PresenceStore, Cluster, Proxy, HeartbeatManager, ACL, Metrics, and Surveys.

**Client** (`client.go`) - Represents a single connection. Handles protocol messages:
- `Connect` - Initial authentication and session establishment
- `Publish` - Publish messages to channels
- `Subscribe` / `Unsubscribe` - Manage channel subscriptions
- `RpcRequest` - RPC-style request/response (proxied to backend with timeout protection)
- `Survey` / `SurveyResponse` - Broadcast a request to all channel subscribers and collect responses
- `Ping` / `Pong` - Connection keepalive

**Hub** (`hub.go`) - Sharded connection registry with 64 shards:
- `connShard` - Maps session IDs and user IDs to active clients
- `subShard` - Maps channels to subscribed clients
- `matcher` - Concurrent topic trie (`topics.CSTrieMatcher`) for wildcard subscriptions
- `maxConnsPerUser` - Per-user connection limit enforcement

**Broker** (`broker.go`) - Interface for pub/sub operations with pluggable implementations:
- **Memory broker** (`broker_memory.go`) - In-process pub/sub for single-node deployments
- **Redis broker** (`pkg/redisbroker/`) - Distributed broker using Redis Streams (persistent history) and Pub/Sub (real-time fan-out)
- `Publish` - Send data to a channel (optionally maintains history with offset+epoch)
- `Subscribe` / `Unsubscribe` - Manage node's channel subscriptions
- `History` - Retrieve message history from stream (supports filtering, pagination, `StreamPosition` with offset+epoch)
- `PublishJoin` / `PublishLeave` - Presence notifications

**Presence** (`presence.go`) - Separate interface from Broker for tracking which clients are in which channels:
- `MemoryPresenceStore` - Default in-memory implementation
- `RedisPresenceStore` (`pkg/redisbroker/presence_redis.go`) - Redis-backed for cluster deployments
- Used for channel occupancy queries and join/leave events

**Survey** (`survey.go`) - Request-response pattern across all subscribers of a channel:
- Sender sends a survey message; broker fans it out to all channel subscribers
- Responses are collected with a configurable timeout
- Used for member discovery, capability negotiation, distributed queries

**ACL** (`acl.go`) - Channel-level access control engine using glob-based pattern matching:
- Rules support AllowSubscribe, AllowPublish (by user ID, `*` for any), and DenyAll
- Evaluated on every Subscribe and Publish operation

**Proxy** (`proxy/`) - Backend service integration for RPC requests:
- Supports HTTP and gRPC backends
- Routes RPC requests based on channel patterns
- Three-tier timeout control: client-level, global, and proxy-specific

**Cluster** (`cluster.go`, `cluster_*.go`) - Optional distributed control plane:
- Redis-backed node discovery and state synchronization
- Command bus (`pkg/redisbroker/cluster_command_bus.go`) for cross-node commands
- Projection repair (`cluster_projection_repair.go`) for eventual consistency
- Presence aggregation across nodes
- Handles node join/leave, channel migration, and stale node cleanup

**Metrics** (`metrics.go`) - Prometheus instrumentation:
- Connection/subscription gauges, message publish/delivery counters
- Publish and RPC duration histograms, delivery failure counters
- Cluster-specific metrics (command dedupe hits, timeouts, projection repairs)

### Transports

The system abstracts connection handling via the **Transport** interface (`transport.go`):
- `Write` / `WriteMany` - Send bytes to client
- `Close` - Close connection with disconnect reason

**Write buffer pool** (`pool.go`) - Uses `sync.Pool` with 4 KB initial capacity to reduce allocations for outbound messages.

**WebSocket** (`pkg/websocket/`) - HTTP-upgraded WebSocket connections:
- Handler detects encoding from WebSocket subprotocol (`messageloop`, `messageloop+json`, `messageloop+proto`)
- Integrates with `lynx` framework for lifecycle management

**gRPC Stream** (`pkg/grpcstream/`) - Bidirectional gRPC streaming:
- `client_server.go` - Client streaming listener, manages per-connection streams
- `admin_server.go` - Admin gRPC API server (separate listener from client traffic)
- `server.go` - Shared server preparation, TLS loading, pre-bound listener lifecycle
- `cmd/server/runtime.go` - Bootstrap preflight that prepares gRPC listeners before `node.Run()`
- Uses custom `RawCodec` to avoid double-encoding messages
- Each client connection is a separate bidirectional stream

### Protocol

Client protocol messages defined in `shared/genproto/client/v1/`:
- `InboundMessage` - Client-to-server with oneof envelope: Connect, Subscribe, Publish, RpcRequest, Survey, Ping, etc.
- `OutboundMessage` - Server-to-client with oneof envelope: Connected, SubscribeAck, PublishAck, Publication, RpcReply, SurveyResult, etc.
- `Message` - Wrapper containing Channel, Id, Offset, and Payload
- `Publication` - Contains Envelopes (array of Message)

The `marshaler.go` re-exports shared marshalers from `shared/marshaler.go`:
- `JSONMarshaler{}` - Standard JSON encoding
- `ProtobufMarshaler{}` - Protobuf binary encoding
- `ProtoJSONMarshaler` - Protobuf JSON encoding

**Import convention:** Generated protobuf packages use short aliases: `clientpb`, `sharedpb`, `eventpb`, `serverpb`, `proxypb`.

### Topic Matching

`pkg/topics/` contains various topic matcher implementations for wildcard channel subscription:
- `matcher.go` - Interface for Subscribe/Unsubscribe/Lookup operations
- `cstrie.go` - Lock-free concurrent trie using CAS operations (default, used by Hub)
- `trie.go`, `naive.go`, `inverted_bitmap.go`, `optimized_inverted_bitmap.go` - Alternative implementations
- Topics use `.` delimiter and `*` wildcard

### Disconnect Handling

Typed `Disconnect` errors (`disconnect.go`) signal intentional connection termination with codes:
- **3000** - Connection closed (clean disconnect, or network loss)
- **3500-3509** - Terminal errors: InvalidToken, BadRequest, Stale, ForceNoReconnect, ConnectionLimit, ChannelLimit, InappropriateProtocol, PermissionDenied, NotAvailable, TooManyErrors
- **3511** - IdleTimeout (heartbeat detected inactivity)
- **3512** - SlowConsumer (client can't keep up with messages)

## Module Structure

- **Root package** (`*.go`) - Core types: Node, Client, Hub, Broker, Transport, Presence, Survey, ACL, Metrics
- `cmd/server/` - Server entry point using `lynx` framework
- `config/` - Configuration structures
- `shared/` - Separate Go module (`github.com/messageloopio/messageloop/shared`) with shared marshalers and generated protobuf Go code
- `protocol/` - Protobuf definitions (source)
- `genproto/` - Generated protobuf (local replace to shared/)
- `pkg/websocket/` - WebSocket transport implementation
- `pkg/grpcstream/` - gRPC streaming transport implementation
- `pkg/topics/` - Topic matching algorithms for wildcard subscriptions
- `pkg/redisbroker/` - Redis-based distributed broker implementation
- `proxy/` - RPC proxy backend integration
- `sdks/go/` - Go client SDK with client.go, websocket.go, grpc.go, proxy.go, mux.go, options.go
- `sdks/go/example/` - Go SDK example apps (basicwebsocket, basicgrpc, dynamicsub, protobuf, wsrpc, proxyserver)
- `sdks/ts/` - TypeScript/JavaScript client SDK

## Key Patterns

1. **Sharding** - Hub uses 64 shards, subscription locks use 16384 shards to reduce contention
2. **Protocol abstraction** - Core logic independent of transport; WebSocket and gRPC are pluggable
3. **Marshaler selection** - WebSocket negotiates encoding via subprotocol; gRPC uses protobuf only
4. **Disconnect handling** - Uses typed `Disconnect` errors implementing the `error` interface
5. **Payload format** - Publish and RPC operations use Payload with Binary/Text/Json data
6. **StreamPosition recovery** - History streams use offset + epoch semantics for reliable recovery
7. **Write pooling** - `sync.Pool` for byte buffers to reduce GC pressure on outbound writes
