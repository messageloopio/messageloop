# ChatRoom — MessageLoop E2E Demo

A full-stack chat room built on MessageLoop that exercises every core
capability of the platform: pub/sub, presence, surveys, RPC, ACL, session
resumption, the admin gRPC API, multiple transports and multiple encodings.

```
┌──────────────┐  WebSocket (JSON)   ┌─────────────────┐   gRPC proxy    ┌──────────────┐
│ web client   │ ──────────────────► │ MessageLoop     │ ──────────────► │ backend      │
│ (Vite + TS)  │                     │ server          │                 │ auth / RPC / │
└──────────────┘                     │ (cmd/server)    │                 │ ACL / hooks  │
┌──────────────┐  client gRPC        │                 │ ◄────────────── └──────────────┘
│ go client    │ ──────────────────► │                 │   admin gRPC API
│ (terminal)   │                     └─────────────────┘
└──────────────┘
```

## Layout

| Path | Purpose |
| --- | --- |
| `config.yaml` | Server config for the demo (memory broker, no Redis needed) |
| `cmd/backend` | gRPC backend: token auth, RPC commands, private-channel ACL, lifecycle hooks; calls the server's admin API |
| `cmd/goclient` | Interactive terminal client (Go SDK; grpc transport, `-transport ws` switches to WebSocket) |
| `cmd/e2e` | Automated end-to-end scenario with assertions (exit 0 = all pass) |
| `web/` | Browser chat UI built with the official TypeScript SDK and Vite |
| `internal/chatroom` | Shared demo constants and the admin gRPC client wrapper |

## Prerequisites

- Go 1.26+
- Node.js 18+ (web client only)
- No Redis required — the demo config uses the in-memory broker.

## Run the stack

Three terminals:

```bash
# 1. backend (auth / RPC / ACL / lifecycle)
cd _examples/chatroom && go run ./cmd/backend

# 2. MessageLoop server
go run ./cmd/server --config ./_examples/chatroom/config.yaml

# 3. e2e verification (optional)
cd _examples/chatroom && go run ./cmd/e2e
```

Terminal chat client (any transport):

```bash
cd _examples/chatroom
go run ./cmd/goclient -name alice          # client gRPC (default)
go run ./cmd/goclient -name bob -transport ws
```

Browser chat UI:

```bash
cd _examples/chatroom/web
npm install
npm run dev          # http://localhost:5173
```

The TS SDK publishes prebuilt artifacts under `sdks/ts/dist`; if you change
the SDK source, rebuild it first with `cd sdks/ts && npm install && npm run build`.

## Demo accounts

| user | token | role |
| --- | --- | --- |
| alice | `token-alice` | owner |
| bob / carol / dave / eve | `token-<name>` | member |

Every demo client derives its token from its user name, and the backend
resolves it into a user identity during connect (auth proxy). Private
channels (`private:*`) additionally require a per-subscription token —
see the ACL scenario in `cmd/e2e`.

## What the demo covers

| Feature | Where |
| --- | --- |
| Token auth via backend proxy | every connect |
| Subscribe / unsubscribe / publish / receive | chat messages (JSON payloads) |
| Publish-with-ack (broker offsets) | `/sys`, e2e phase 2 |
| Transient publish (no persistence) | `/whisper`, e2e phase 6 |
| RPC through the backend | `/roll` `/stats` `/history` `/kick` `/whoami` |
| Presence (events + snapshot queries) | online user list, `/presence` |
| Survey (channel poll, aggregated answers) | `/poll` |
| Auto-reconnect + session resume, no message loss | e2e phase 10 (admin kick) |
| History recovery (`WithRecover` / `SubscriptionSpec.recover`) | e2e phase 7 |
| Admin gRPC API (publish / disconnect / channels / presence / history) | backend RPCs, e2e phase 8 |
| ACL on private channels | e2e phase 9 |
| Lifecycle hooks | join/leave announcements |
| Transports: WebSocket + client gRPC (+ optional QUIC) | goclient `-transport`, e2e |
| Encodings: JSON (web) + protobuf-capable SDKs | `setEncoding` / `WithEncoding` |

## goclient commands

```
<text>             publish a chat message to the current room
/join <room>       subscribe to a room
/leave <room>      unsubscribe from a room
/roll              dice RPC via the backend
/stats             room stats RPC via the backend (admin API)
/history [n]       last n history entries RPC
/kick <name>       force-disconnect a user RPC (admin API)
/whoami            echo RPC metadata
/presence          query the presence snapshot of the current room
/poll <question>   start a survey in the current room
/whisper <text>    transient publish (not persisted)
/sys <text>        publish with ack, prints the broker offset
/refresh           re-validate subscriptions (SubRefresh)
/help              show this help
/quit              disconnect and exit
```

The web UI supports the same commands in its input box.

## Notes

- `_examples/chatroom` is its own Go module (like `sdks/go`); run `go run` /
  `go build` from inside this directory. It depends on the SDK via a
  `replace` directive pointing at `../../sdks/go` and `../../shared`.
- QUIC transport is optional: add `transport.quic.addr` (e.g. `:9443`) plus
  `transport.quic.insecure: true` to `config.yaml`, then run
  `go run ./cmd/goclient -transport quic`.
- The demo config opens the WebSocket endpoint to all origins because the Go
  SDK clients send no `Origin` header while the browser sends one; an origin
  whitelist would reject the former.
