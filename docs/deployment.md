# Deployment Guide

This guide covers running MessageLoop in production environments.

## Single Binary Deployment

MessageLoop compiles to a single static binary with no runtime dependencies (unless you use the Redis broker).

```bash
go build -o messageloop cmd/server/main.go
./messageloop --config ./config.yaml
```

## Listener Model

A single MessageLoop process exposes four network listeners:

| Listener | Config Key | Purpose | Default |
| --- | --- | --- | --- |
| WebSocket | `transport.websocket.addr` | Client pub/sub traffic | `:9080` |
| gRPC streaming | `transport.grpc.addr` | Client pub/sub traffic | `:9090` |
| gRPC admin | `server.grpc_admin.addr` | Server-side admin API | `127.0.0.1:9091` |
| HTTP admin | `server.http.addr` | Health checks and Prometheus metrics | `127.0.0.1:8080` |

Bind client-facing listeners to public interfaces and admin listeners to private/loopback interfaces.

## TLS Configuration

Each listener supports optional TLS:

```yaml
transport:
  websocket:
    addr: ":9080"
    tls:
      cert_file: "./certs/server.crt"
      key_file: "./certs/server.key"
  grpc:
    addr: ":9090"
    tls:
      cert_file: "./certs/server.crt"
      key_file: "./certs/server.key"

server:
  grpc_admin:
    addr: "127.0.0.1:9091"
    tls:
      cert_file: "./certs/admin.crt"
      key_file: "./certs/admin.key"
```

For WebSocket, TLS turns the listener into a `wss://` endpoint. For gRPC, standard gRPC TLS applies.

## Admin API Authentication

Protect the admin gRPC API with a bearer token in production:

```yaml
server:
  grpc_admin:
    auth_token: "your-secret-token"
```

Clients must include the token as a `authorization: Bearer <token>` gRPC metadata header.

## Health And Metrics

- **Health**: `GET http://<server.http.addr>/health` returns `200 OK` when the server is ready.
- **Prometheus metrics**: `GET http://<server.http.addr>/metrics` returns standard Prometheus-format metrics.

Key metrics:

| Metric | Type | Description |
| --- | --- | --- |
| `messageloop_connections_total` | Gauge | Current active connections |
| `messageloop_subscriptions_total` | Gauge | Current active subscriptions |
| `messageloop_messages_published_total` | Counter | Total messages published |
| `messageloop_messages_delivered_total` | Counter | Total messages delivered |
| `messageloop_delivery_failures_total` | Counter | Failed delivery attempts |

## Broker Selection

### Memory Broker

Default. No external dependencies. Suitable for single-node deployments.

```yaml
broker:
  type: memory
```

- History is kept in a per-channel ring buffer (default 256 entries).
- All data is lost on restart.

### Redis Broker

Required for multi-node deployments. Uses Redis Streams for history and Redis Pub/Sub for cross-node delivery.

```yaml
broker:
  type: redis
  redis:
    addr: 127.0.0.1:6379
    password: "secret"
    db: 10
    pool_size: 10
    min_idle_conns: 5
    max_retries: 3
    stream_max_length: 10000
    history_ttl: "24h"
```

Redis requirements:

- Redis 7.0+ recommended.
- Use a dedicated database number (`db`) to avoid key collisions.
- Ensure sufficient memory for stream storage.

## Multi-Node Cluster

Enable the Redis-backed control plane for distributed session management:

```yaml
cluster:
  enabled: true
  node_id: node-a    # Must be unique per node
  backend: redis
```

Cluster capabilities:

- **Session ownership**: Each session is owned by exactly one node.
- **Remote session resume**: Clients reconnecting to a different node can resume their session.
- **Cluster-wide survey**: Survey requests fan out to all nodes.
- **Command deduplication**: Cluster commands are deduplicated across nodes.
- **Projection repair**: Automatic reconciliation of stale session state.

All nodes in a cluster must:

1. Share the same Redis instance and database.
2. Use `broker.type: redis` with identical broker settings.
3. Have unique `cluster.node_id` values.

## Resource Limits

Protect the server from resource exhaustion:

```yaml
server:
  limits:
    max_connections_per_user: 3         # Limit concurrent sessions per user ID
    max_subscriptions_per_client: 100   # Limit channels per client session
    max_publishes_per_second: 50        # Rate limit publishes per client
    max_message_size: 65536             # Max inbound message size in bytes
```

All limits default to `0` (unlimited) when not set.

## Idle Connection Management

```yaml
server:
  heartbeat:
    idle_timeout: "300s"
```

Clients that send no messages (including pings) within the idle timeout are disconnected with a `DisconnectStale` error code. Clients should send periodic `Ping` messages to keep connections alive.

## WebSocket Options

```yaml
transport:
  websocket:
    addr: ":9080"
    path: "/ws"
    compression: true          # Enable permessage-deflate
    write_timeout: "10s"       # Timeout for writing to client
    allow_all_origins: true    # Development only; set to false in production
    allowed_origins:           # Explicit origin allowlist
      - "https://app.example.com"
```

## Proxy Backend Integration

Route RPC requests and lifecycle hooks to backend services:

```yaml
proxy:
  - name: main-backend
    endpoint: "backend:10091"
    timeout: "30s"
    grpc:
      insecure: true
    routes:
      - channel: "*"
        method: "authenticate"
      - channel: "api.*"
        method: "rpc"
```

Proxy hooks (in order of client lifecycle):

1. `authenticate` — Validate connection token, return user info.
2. `subscribe_acl` — Allow or deny channel subscription.
3. `publish_acl` — Allow or deny channel publish.
4. `on_connected` — Notification after successful connect.
5. `on_subscribed` — Notification after successful subscribe.
6. `on_unsubscribed` — Notification after unsubscribe.
7. `on_disconnected` — Notification after disconnect.

## Graceful Shutdown

The server handles `SIGINT` and `SIGTERM` signals:

1. Stops accepting new connections.
2. Drains existing connections (sends disconnect with `ForceNoReconnect` code).
3. Shuts down the cluster control plane (if enabled).
4. Exits after a configurable timeout (default 10 seconds).

## Docker

```dockerfile
FROM golang:1.25 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /messageloop cmd/server/main.go

FROM gcr.io/distroless/static-debian12
COPY --from=builder /messageloop /messageloop
COPY config.yaml /config.yaml
EXPOSE 9080 9090 9091 8080
ENTRYPOINT ["/messageloop", "--config", "/config.yaml"]
```

## Production Checklist

- [ ] Bind admin listeners (`server.http.addr`, `server.grpc_admin.addr`) to loopback or private interfaces.
- [ ] Set `server.grpc_admin.auth_token` to a strong secret.
- [ ] Disable `allow_all_origins` on the WebSocket transport; use `allowed_origins` instead.
- [ ] Configure TLS on client-facing listeners or terminate TLS at a load balancer.
- [ ] Set appropriate resource limits (`max_connections_per_user`, `max_publishes_per_second`).
- [ ] Enable `compression: true` on WebSocket for bandwidth savings.
- [ ] For multi-node: ensure all nodes share the same Redis and have unique `node_id`.
- [ ] Monitor `/health` and `/metrics` endpoints.
- [ ] Set `require_auth: true` if all connections must present a token.
