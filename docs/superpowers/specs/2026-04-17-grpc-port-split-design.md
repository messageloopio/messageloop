# Split gRPC Streaming And Admin API Ports

## Status

This document is the design proposal for separating client-facing gRPC streaming traffic from server-facing gRPC admin traffic.

Current runtime behavior before implementation:

- `transport.grpc.addr` is the only gRPC listener.
- That single listener serves both `messageloop.client.v1.MessageLoopService` and `messageloop.server.v1.APIService`.
- Admin HTTP endpoints (`/health`, `/metrics`) already live on a separate HTTP listener.

Current source-of-truth files:

- `cmd/server/main.go`
- `pkg/grpcstream/server.go`
- `pkg/grpcstream/handler.go`
- `pkg/grpcstream/api_handler.go`
- `README.md`
- `config-example.yaml`

## Problem

The current gRPC listener mixes two different network surfaces:

- Client data-plane traffic: long-lived bidirectional streaming sessions.
- Server control-plane traffic: short-lived admin and query RPCs.

This creates avoidable coupling in four areas:

1. Exposure boundary: client traffic is more likely to be internet-facing, while admin traffic should usually stay private.
2. Security policy: admin traffic often needs different TLS, identity, and authorization requirements.
3. Resource isolation: long-lived streaming connections and short admin RPCs compete on the same listener.
4. Configuration semantics: `transport.grpc` currently hides both a client transport and a server control surface.

## Goals

- Separate client gRPC streaming and admin gRPC API onto distinct ports.
- Keep a single MessageLoop process and a single shared `Node` runtime.
- Make configuration semantics explicit about data plane versus control plane.
- Allow independent TLS and listener configuration for each gRPC surface.
- Make test coverage prove that each port exposes only the intended service.

## Non-Goals

- Backward compatibility with the old single-listener configuration.
- Extracting Admin API into a separate process or service.
- Changing protobuf service definitions or RPC payload shapes.
- Introducing internal RPC between the two gRPC servers.

## Selected Approach

Use a single binary with one shared `Node`, but run two independent gRPC server components:

- Client gRPC server
  - Owns `messageloop.client.v1.MessageLoopService`.
  - Lives under `transport.grpc`.
- Admin gRPC server
  - Owns `messageloop.server.v1.APIService`.
  - Lives under `server.grpc_admin`.

This keeps deployment simple while making the network and configuration boundary explicit.

## Configuration Design

### New Configuration Shape

```yaml
server:
  http:
    addr: "127.0.0.1:8080"
  grpc_admin:
    addr: "127.0.0.1:9091"
    tls:
      cert_file: ""
      key_file: ""

transport:
  websocket:
    addr: ":9080"
    path: "/ws"
  grpc:
    addr: ":9090"
    write_timeout: "10s"
    tls:
      cert_file: ""
      key_file: ""
```

### Semantics

- `transport.grpc` means client traffic only.
- `server.grpc_admin` means server-side operational and query traffic only.
- The old behavior where Admin API rides on the client gRPC port is removed.
- Default documentation should describe two distinct gRPC addresses.
- Admin gRPC documentation and examples should use loopback or private-network addresses by default rather than wildcard binding.
- In the new config contract, both `transport.grpc.addr` and `server.grpc_admin.addr` are required and must be explicitly set.

### Validation Rules

- Startup validation fails if either `transport.grpc.addr` or `server.grpc_admin.addr` is empty.
- TLS configuration is independent for each listener.
- For each listener, `cert_file` and `key_file` must either both be set or both be empty.
- A partial TLS configuration is a startup error; the process must not silently fall back to plaintext.
- Actual socket binding is the source of truth for conflicts between the client gRPC and admin gRPC listeners.
- An optional raw string equality check may be used only to improve the error message for obvious duplicates, but it must not be treated as complete collision detection.
- Collision handling for WebSocket and admin HTTP remains unchanged in this design and is out of scope.

## Runtime Architecture

### Shared Runtime

Both gRPC servers share the same in-process `Node` instance.

This means:

- There is no new internal transport between client and admin paths.
- Existing handler logic continues to call `Node` methods directly.
- Session ownership, cluster state, broker access, and hub state remain centralized.

### New Server Components

Replace the current combined `pkg/grpcstream/server.go` shape with clearer composition:

- `pkg/grpcstream/client_server.go`
  - Creates a gRPC server that registers only `MessageLoopService`.
- `pkg/grpcstream/admin_server.go`
  - Creates a gRPC server that registers only `APIService`.
- `pkg/grpcstream/server_common.go`
  - Holds shared listener, TLS, codec, and lifecycle wiring helpers.

Server constructors should not bind sockets.

- `NewClientServer(...)` and `NewAdminServer(...)` build component state only.
- Client/admin gRPC listeners follow a two-phase lifecycle:
  - An explicit bootstrap preflight phase validates configuration and pre-binds both gRPC listeners before `node.Run(...)`.
  - `Start` only begins `Serve` on already prepared listeners.
- This creates a startup barrier for the two gRPC surfaces so that bind failures are detected before either surface begins serving and before `Node` side effects begin.
- This avoids constructor-time partial startup while still preventing one gRPC surface from serving before the other has finished binding.
- This preflight step is an intentional divergence from the current WebSocket component lifecycle and applies only to the new split gRPC components.

The existing handler files remain focused:

- `pkg/grpcstream/handler.go`
  - Client streaming handler only.
- `pkg/grpcstream/api_handler.go`
  - Admin API handler only.

### Component Lifecycle

`cmd/server/main.go` should wire three independent network components:

1. WebSocket server
2. Client gRPC server
3. Admin gRPC server

All three remain part of the same process lifecycle managed by `lynx`.

Lifecycle expectations:

- `cmd/server/main.go` must run an explicit gRPC preflight step before `node.Run(...)` is called.
- That preflight step validates config, prepares TLS state, and pre-binds both gRPC listeners.
- This ensures gRPC bind-time failures do not occur after broker, cluster, or other `Node`-managed side effects have already started.
- If either gRPC listener fails during preflight, process startup returns an error before either gRPC surface begins serving.
- If either gRPC listener fails after `Serve` begins, the process follows normal component shutdown behavior.
- The split-port change guarantees no half-started state caused by gRPC bind-time failure.
- This change does not require moving the existing admin HTTP listener into a `lynx` component.
- The two gRPC surfaces still use the `lynx` component model, but their preflight/`Start` split intentionally differs from the current WebSocket server so they can form a pre-serve startup barrier.
- Existing admin HTTP endpoints remain process-level liveness and metrics endpoints rather than per-listener readiness signals.
- Temporary reachability of admin HTTP during a failing process startup is acceptable; readiness semantics for listener-specific availability are out of scope for this change.
- If one gRPC component has already pre-bound a listener and a later gRPC preflight step fails, the already-bound listener must be closed before startup returns an error.
- If gRPC preflight succeeds but any later bootstrap step aborts process startup before normal serving is established, both pre-bound gRPC listeners must still be closed before returning the startup error.

## Detailed File-Level Changes

### Configuration Layer

- `config/config.go`
  - Add `Server.GRPCAdmin` struct.
  - Keep `Transport.GRPC` for client traffic only.
- `config-example.yaml`
  - Show separate client and admin gRPC ports.
- `README.md`
  - Update endpoint table, architecture notes, and Admin API documentation.

### Bootstrap Layer

- `cmd/server/main.go`
  - Build distinct option structs for client gRPC and admin gRPC.
  - Instantiate both components.
  - Run an explicit client/admin gRPC preflight bind step before calling `node.Run(...)`.
  - Reject invalid configuration such as missing addresses or partial TLS configuration.
  - Treat actual bind failures as definitive listener-collision detection.

### gRPC Package Layer

- Replace the current combined `NewServer` constructor with explicit constructors:
  - `NewClientServer(...)`
  - `NewAdminServer(...)`
- Keep handler constructors unchanged where possible:
  - `NewGRPCHandler(...)`
  - `NewAPIServiceHandler(...)`
- Move shared listener creation and TLS setup into a private helper to avoid duplication.
- Move RawCodec registration behind a process-global one-time guard such as `sync.Once`.
- RawCodec registration must not occur independently in both server constructors.

### Tests

- Update or replace any tests that assume both services are on one gRPC port.
- Add focused tests for each server constructor and service surface.

## Runtime Behavior

### Client gRPC Port

The client port exposes only `MessageLoopService`.

Expected behavior:

- Streaming clients can connect and exchange `InboundMessage` and `OutboundMessage` traffic.
- Admin RPCs against this port fail because `APIService` is not registered.

### Admin gRPC Port

The admin port exposes only `APIService`.

Expected behavior:

- Server-side publish, survey, disconnect, subscribe, unsubscribe, history, presence, and channel queries continue to work.
- Streaming client RPCs against this port fail because `MessageLoopService` is not registered.

### Failure Handling

- Process startup fails immediately if either configured listener cannot bind.
- Process startup fails immediately if configuration makes any listener collide at bind time.
- One gRPC surface must not silently inherit settings from the other.
- One gRPC surface must not silently downgrade from TLS to plaintext because of incomplete certificate configuration.
- Logging should name the surfaces distinctly, for example `grpc-client-server` and `grpc-admin-server`.

## Security And Operations

This split is primarily an operational boundary, not a domain-model change.

Operational benefits:

- Admin gRPC can be placed behind private networking, mTLS, or service-mesh policy without affecting clients.
- Client gRPC can keep connection-oriented tuning for streaming without constraining admin requests.
- Logs and future listener-scoped metrics can distinguish client-plane and control-plane traffic.
- Admin HTTP should be documented as a private operational surface for the same reason.

This design does not add authorization by itself, but it makes later authorization work much cleaner because the entry points are no longer shared.

## Testing Strategy

### Unit Coverage

- Validate client server registers only `MessageLoopService`.
- Validate admin server registers only `APIService`.
- Validate empty-address configuration is rejected.
- Validate partial TLS configuration is rejected.

### Integration Coverage

- Client gRPC connection succeeds on the client port.
- Admin API calls succeed on the admin port.
- Admin API call against the client port fails with unimplemented service.
- Streaming call against the admin port fails with unimplemented service.
- Startup fails cleanly when one gRPC listener cannot bind.
- Startup failure does not leave one gRPC surface serving while the other failed.
- Startup failure during later gRPC initialization does not leave an earlier pre-bound listener holding its port.
- At least one collision test must use addresses that differ textually but still resolve to the same bind target, proving collision handling relies on real bind behavior rather than string equality.
- Successful startup must be proven through a real bootstrap path that verifies the client port exposes only `MessageLoopService` and the admin port exposes only `APIService`.
- At least one integration path must exercise the real bootstrap wiring rather than only handler-level or bufconn-only assembly.
- At least one bootstrap-level test must prove that a gRPC preflight bind failure prevents `node.Run(...)` side effects from starting.

### Regression Coverage

- Existing admin behavior remains unchanged once routed through the admin port.
- Existing gRPC client behavior remains unchanged once routed through the client port.
- WebSocket and HTTP admin surfaces are unaffected.

## Migration Impact

This is an intentionally breaking configuration change.

Operator-facing impact:

- Existing configs that only define `transport.grpc.addr` must be updated.
- Admin automation and internal service clients must move from the old shared gRPC port to the new admin port.
- Existing deployments must explicitly choose a private or protected address for `server.grpc_admin.addr`.
- Existing deployments should treat `server.http.addr` as a private operational surface as well.
- Documentation must clearly call out the port split and new config fields.

## Alternatives Considered

### Keep One Port And Use Interceptors

Rejected because it preserves a soft boundary only.

- Security, exposure, and connection-isolation concerns remain.
- Configuration semantics stay muddled.

### Run Admin API As A Separate Process

Rejected for now because it adds unnecessary complexity.

- Both current handlers already operate against the same in-process `Node`.
- Extracting a new process would require new internal communication and state routing.

## Resolved Decisions

- Empty addresses do not disable a gRPC surface in this design; they are invalid configuration.
- Listener collision detection relies on actual bind behavior rather than full address canonicalization.
- Admin gRPC examples and default docs should use loopback or private-network addresses by default.