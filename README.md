# MessageLoop

MessageLoop is a real-time messaging server written in Go. It provides channel-based pub/sub, request/response RPC, presence, history, and session-aware connection management over WebSocket and gRPC.

The project supports a simple single-node setup with in-memory components and can be extended with Redis for distributed delivery, durable history, and an optional Redis-backed control plane for multi-node deployments.

## Highlights

- WebSocket and gRPC client transports
- JSON and protobuf wire encodings
- Channel pub/sub with wildcard topic matching
- Request/response RPC routed to HTTP or gRPC backends
- Presence tracking plus join/leave presence events
- Message history via in-memory ring buffers or Redis Streams
- Session resumption support for reconnecting clients
- Built-in ACL rules plus proxy-backed auth and ACL checks
- Per-user and per-client limits
- Prometheus metrics and health checks
- Optional Redis-backed cluster control plane for multi-node operation

## Architecture At A Glance

| Component | Responsibility |
| --- | --- |
| `Node` | Central coordinator for transports, broker, proxy, presence, surveys, and the cluster control plane |
| `Hub` | Sharded in-memory registry for sessions and subscriptions |
| `Broker` | Pub/sub backend, implemented by in-memory broker or Redis broker |
| `Transport` | Connection abstraction used by WebSocket and gRPC streaming servers |
| `Proxy` | RPC/auth/ACL/lifecycle delegation to HTTP or gRPC backends |
| `Cluster` | Optional Redis-backed control plane for multi-node session ownership and coordination |

## Quick Start

### Requirements

- Go 1.25+
- Redis 7+ if you want Redis broker or cluster features
- `task` and `buf` only if you need to regenerate protobuf code

### Run A Single Node With In-Memory Broker

Create a local config file:

```yaml
server:
  http:
    addr: ":8080"
  grpc_admin:
    addr: "127.0.0.1:9091"

transport:
  websocket:
    addr: ":9080"
    path: "/ws"
    check_origin: true
  grpc:
    addr: ":9090"

broker:
  type: memory
```

Start the server:

```bash
go run cmd/server/main.go --config ./config.yaml
```

Default endpoints:

- WebSocket: `ws://localhost:9080/ws`
- gRPC streaming: `localhost:9090`
- gRPC admin API: `127.0.0.1:9091`
- Health: `http://localhost:8080/health`
- Prometheus metrics: `http://localhost:8080/metrics`

### Run With Redis Broker

Use Redis when you need cross-node publish delivery or Redis-backed history:

```yaml
broker:
  type: redis
  redis:
    addr: 127.0.0.1:6379
    db: 10
    history_ttl: 24h
    stream_max_length: 10000
```

Notes:

- The in-memory broker keeps a ring buffer history per channel with a default size of `256` messages.
- The Redis broker stores history in Redis Streams with a default TTL of `24h` and a default max length of `10000` entries.
- `config-example.yaml` contains a fuller Redis-based server configuration.

### Enable The Distributed Control Plane

Redis broker and Redis control plane are related but not the same:

- Redis broker gives you distributed channel delivery and Redis-backed history.
- Enabling `cluster` adds cross-node session ownership, remote session takeover and resume, cluster-wide survey, cluster command dedupe, and projection repair.

Example:

```yaml
broker:
  type: redis
  redis:
    addr: 127.0.0.1:6379
    db: 10

cluster:
  enabled: true
  node_id: node-a
  backend: redis
```

Operational requirements:

- Every process in the same cluster must share the same Redis namespace and broker settings.
- `cluster.node_id` must be unique per logical node.
- Session-targeted admin operations and cluster-wide survey only become cluster-aware when `cluster.enabled: true`.

## Configuration Overview

MessageLoop reads a single YAML file passed through `--config`.

| Section | Purpose | Key Fields |
| --- | --- | --- |
| `server` | Admin-side listeners and core runtime behavior | `http.addr`, `grpc_admin.addr`, `grpc_admin.tls.*`, `heartbeat.idle_timeout`, `rpc_timeout`, `limits.*`, `acl.rules` |
| `transport.websocket` | WebSocket listener configuration | `addr`, `path`, `check_origin`, `compression`, `write_timeout`, `tls.*` |
| `transport.grpc` | Client gRPC streaming listener configuration | `addr`, `write_timeout`, `tls.*` |
| `broker` | Messaging backend selection | `type`, `redis.*` |
| `cluster` | Optional distributed control plane | `enabled`, `node_id`, `backend` |
| `proxy` | Backend routing rules for RPC and hooks | `name`, `endpoint`, `timeout`, `http`, `grpc`, `routes` |

### Limits And Built-In ACL

```yaml
server:
  limits:
    max_connections_per_user: 3
    max_subscriptions_per_client: 100
    max_publishes_per_second: 50
  acl:
    rules:
      - channel_pattern: "chat.public.*"
        allow_subscribe: ["*"]
        allow_publish: ["alice", "bob"]
      - channel_pattern: "chat.private.*"
        deny_all: true
```

Behavior notes:

- Built-in ACL rules are evaluated only when no proxy ACL route is configured for the same operation.
- `allow_subscribe` and `allow_publish` are user-ID allow lists.
- `"*"` means any authenticated user.
- If no ACL rule matches a channel, access is allowed by default.

### Proxy Routing

Proxies let the server delegate RPC and lifecycle decisions to backend services. Routes are matched by `channel` and `method` glob patterns, and the first matching route wins.

Example:

```yaml
proxy:
  - name: example
    endpoint: 127.0.0.1:10091
    timeout: 30s
    grpc:
      insecure: true
    routes:
      - channel: "*"
        method: "*"
```

Current proxy hooks include:

- RPC forwarding
- authentication
- subscribe ACL
- publish ACL
- on-connected notification
- on-subscribed notification
- on-unsubscribed notification
- on-disconnected notification

## Protocol Capabilities

### Client Session Operations

The client protocol supports these core flows:

- connect and reconnect with session resumption
- subscribe and unsubscribe
- publish to channels
- RPC request and reply
- ping and pong heartbeats
- subscription refresh
- survey request and reply

### Presence And History

- Presence is tracked per channel through a pluggable presence store.
- Presence join and leave events are published to an internal `/<channel>/__presence`-style companion channel.
- History can be queried from the broker and is exposed through the server-side gRPC admin API.

### Server-Side gRPC Admin API

The admin gRPC listener exposed by `server.grpc_admin.addr` serves `messageloop.server.v1.APIService`, including:

- `Publish`
- `Survey`
- `Disconnect`
- `Subscribe`
- `Unsubscribe`
- `GetPresence`
- `GetHistory`
- `GetChannels`

## SDKs

### Go SDK

Go client SDK lives in [sdks/go](sdks/go) and includes WebSocket and gRPC clients.

```go
package main

import (
    "context"
    "log"

    messageloopgo "github.com/messageloopio/messageloop/sdks/go"
)

func main() {
    client, err := messageloopgo.Dial(
        "ws://localhost:9080/ws",
        messageloopgo.WithClientID("example-client"),
        messageloopgo.WithAutoSubscribe("chat.general"),
    )
    if err != nil {
        log.Fatal(err)
    }
    defer client.Close()

    client.OnMessage(func(msgs []*messageloopgo.Message) {
        for _, msg := range msgs {
            log.Printf("received %s", msg.Type)
        }
    })

    if err := client.Connect(context.Background()); err != nil {
        log.Fatal(err)
    }

    msg := messageloopgo.NewMessageWithData(
        "chat.message",
        messageloopgo.NewTextData("hello"),
    )
    if err := client.Publish("chat.general", msg); err != nil {
        log.Fatal(err)
    }
}
```

Examples:

- `sdks/go/example/basicwebsocket`
- `sdks/go/example/basicgrpc`
- `sdks/go/example/dynamicsub`
- `sdks/go/example/protobuf`
- `sdks/go/example/wsrpc`
- `sdks/go/example/proxyserver`

### TypeScript SDK

TypeScript SDK lives in [sdks/ts](sdks/ts), publishes as `@messageloop/sdk`, and currently targets browser and Node.js WebSocket clients.

```typescript
import {
  MessageLoopClient,
  createJSONMessage,
  setAutoSubscribe,
  setClientId,
} from "@messageloop/sdk";

const client = await MessageLoopClient.dial("ws://localhost:9080/ws", [
  setClientId("web-client"),
  setAutoSubscribe("chat.general"),
]);

client.onMessage((messages) => {
  for (const message of messages) {
    console.log(message.channel, message.message.type, message.message.data);
  }
});

await client.publish(
  "chat.general",
  createJSONMessage("chat.message", { text: "hello" })
);
```

Examples:

- `sdks/ts/examples/node/client.ts`
- `sdks/ts/examples/browser/index.html`

## Development

### Build And Test

```bash
go build ./...
go test ./...
```

Useful targeted test commands:

```bash
go test -v ./pkg/topics/...
go test -v ./pkg/topics/... -run TestCSTrieMatcher
```

### Regenerate Protobuf Code

Install toolchain:

```bash
task init
```

Generate code:

```bash
task generate-protocol
```

### TypeScript SDK

```bash
cd sdks/ts
npm install
npm run build
npm test
```

## Repository Guide

- [config-example.yaml](config-example.yaml): fuller Redis and proxy example
- [CLAUDE.md](CLAUDE.md): architecture and development notes
- [RPC_TIMEOUT.md](RPC_TIMEOUT.md): RPC timeout behavior and rationale
- [sdks/go](sdks/go): Go SDK module and examples
- [sdks/ts](sdks/ts): TypeScript SDK package and examples

## License

Apache-2.0. See [LICENSE](LICENSE).
