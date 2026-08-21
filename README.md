# MessageLoop

MessageLoop is a real-time messaging server written in Go. It provides channel-based pub/sub, request/response RPC, presence, history, and session-aware connection management over WebSocket, gRPC, and optional QUIC.

The project supports a simple single-node setup with in-memory components and can be extended with Redis for distributed delivery, durable history, and an optional Redis-backed control plane for multi-node deployments.

## Highlights

- WebSocket, gRPC, and optional QUIC client transports
- JSON and protobuf wire encodings
- Channel pub/sub with wildcard topic matching
- Request/response RPC routed to HTTP or gRPC backends
- Presence tracking plus join/leave presence events
- Message history via in-memory ring buffers or Redis Streams
- Session resumption support for reconnecting clients
- Built-in authorizer rules (`server.authorizer`) plus proxy-backed auth and ACL checks
- Per-user and per-client limits
- Prometheus metrics and health checks
- Optional Redis-backed cluster control plane for multi-node operation

## Architecture At A Glance

| Component | Responsibility |
| --- | --- |
| `Node` | Central coordinator for transports, broker, proxy, presence, surveys, and the cluster control plane |
| `Hub` | Sharded in-memory registry for sessions and subscriptions |
| `Broker` | Pub/sub backend, implemented by in-memory broker or Redis broker |
| `Transport` | Connection abstraction used by WebSocket, gRPC streaming, and QUIC servers |
| `Proxy` | RPC/auth/lifecycle delegation to HTTP or gRPC backends |
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
    auth_token: "change-me"   # Required (or set allow_insecure: true for dev only)

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

Note: when `server.grpc_admin.addr` is set, configuration validation requires `auth_token` (or `allow_insecure: true`, which serves the admin API without authentication and is intended for development environments only).

Start the server:

```bash
go run ./cmd/server --config ./config.yaml
```

Default endpoints:

- WebSocket: `ws://localhost:9080/ws`
- gRPC streaming: `localhost:9090`
- QUIC (optional): enable `transport.quic.addr` (e.g. `:4433`) and dial with `DialQUIC`
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
  hmac_key_file: /path/to/cluster-hmac.key   # or inline hmac_key; see below
```

Operational requirements:

- Every process in the same cluster must share the same Redis namespace and broker settings.
- `cluster.node_id` must be unique per logical node.
- All nodes must share the same command-bus HMAC key: at least 32 bytes, configured via exactly one of `cluster.hmac_key` or `cluster.hmac_key_file` — startup is refused otherwise.
- Session-targeted admin operations and cluster-wide survey only become cluster-aware when `cluster.enabled: true`.

## Configuration Overview

MessageLoop reads a single YAML file passed through `--config`.

| Section | Purpose | Key Fields |
| --- | --- | --- |
| `server` | Admin-side listeners and core runtime behavior | `http.addr`, `grpc_admin.addr`, `grpc_admin.tls.*`, `heartbeat.idle_timeout`, `rpc_timeout`, `limits.*`, `authorizer.rules` |
| `transport.websocket` | WebSocket listener configuration | `addr`, `path`, `check_origin`, `compression`, `write_timeout`, `tls.*` |
| `transport.grpc` | Client gRPC streaming listener configuration | `addr`, `write_timeout`, `tls.*` |
| `broker` | Messaging backend selection | `type`, `redis.*` |
| `cluster` | Optional distributed control plane | `enabled`, `node_id`, `backend` |
| `proxy` | Backend routing rules for RPC and hooks | `name`, `endpoint`, `timeout`, `http`, `grpc`, `routes` |

### Limits And Authorizer Rules

```yaml
server:
  limits:
    max_connections_per_user: 3
    max_subscriptions_per_client: 100
    max_publishes_per_second: 50
  authorizer:
    rules:
      - pattern: "chat.public.*"
        allow_subscribe: ["*"]
        allow_publish: ["alice", "bob"]
      - pattern: "chat.private.*"
        deny_all: true
```

Behavior notes:

- `server.authorizer` is the single authorization table: a `default` fallback ChannelPolicySpec plus `rules[]`, each rule carrying `pattern`, `deny_all`, `allow_subscribe` / `allow_publish` / `allow_survey`, and inline channel-policy fields.
- Allow lists are user-ID lists: unset means the action is not constrained, an explicit empty list denies it, and `"*"` allows any authenticated user.
- When no rule matches a channel, the default policy applies (subscribe and publish are allowed, survey is off).
- Rules are evaluated in configuration order and later rules override earlier ones (not first-match); a deny cannot be punched through by a more specific allow.
- The old `server.acl` block was removed: configuration that still contains it fails validation.

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
- Subscribers receive join/leave as first-class `presence_event` envelopes on the channel they subscribed to (snapshots ride on `connected.presence` / `subscribe_ack.presence` and `PresenceQuery`). A `/<channel>/__presence` companion channel is only written when the channel policy sets `legacy_presence_channel: true`; occupancy events always cross nodes over the live bus (no extra configuration, `server.presence.cluster_emit` was removed).
- History can be queried from the broker and is exposed through the server-side gRPC admin API.

### Server-Side gRPC Admin API

The admin gRPC listener exposed by `server.grpc_admin.addr` serves `messageloop.server.v2.APIService`, including:

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

- [ROADMAP.md](ROADMAP.md): product roadmap (v0.2 preview → v1.0 → v1.x) and schedule
- [docs/design](docs/design/README.md): approved design for v1.0 platform gaps
- [docs/developer](docs/developer/README.md): developer documentation suite (Chinese) — architecture, configuration reference, admin API, distributed cluster, observability, development workflow, and SDK guides
- [config-example.yaml](config-example.yaml): fuller Redis and proxy example
- [docs/deployment.md](docs/deployment.md): production deployment guide, TLS, Docker, multi-node
- [docs/protocol.md](docs/protocol.md): client protocol reference with message formats
- [CLAUDE.md](CLAUDE.md): architecture and development notes
- [RPC_TIMEOUT.md](docs/archive/RPC_TIMEOUT.md): RPC timeout behavior and rationale (archived historical record)
- [sdks/go](sdks/go): Go SDK module and examples
- [sdks/ts](sdks/ts): TypeScript SDK package and examples

## License

Apache-2.0. See [LICENSE](LICENSE).
