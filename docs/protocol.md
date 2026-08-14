# Client Protocol Reference

MessageLoop uses a bidirectional message protocol over WebSocket or gRPC streaming. All messages are wrapped in `InboundMessage` (client → server) and `OutboundMessage` (server → client) envelopes.

## Transport Negotiation

### WebSocket

Connect to the WebSocket endpoint and request a subprotocol:

| Subprotocol | Encoding |
| --- | --- |
| `messageloop+json` | JSON (protobuf-compatible JSON mapping) |
| `messageloop+proto` | Protobuf binary |
| `messageloop` | Protobuf binary (default) |

Example:

```
GET /ws HTTP/1.1
Upgrade: websocket
Sec-WebSocket-Protocol: messageloop+json
```

### gRPC

The gRPC transport uses the `MessageLoopService.MessageLoop` bidirectional streaming RPC. Messages are always protobuf-encoded.

```protobuf
service MessageLoopService {
  rpc MessageLoop(stream InboundMessage) returns (stream OutboundMessage);
}
```

## Message Envelope

### InboundMessage (Client → Server)

```json
{
  "id": "unique-request-id",
  "envelope-type": { ... }
}
```

The `id` field is echoed back in the corresponding response, allowing clients to correlate request/response pairs.

Envelope types:

| Field | Type | Description |
| --- | --- | --- |
| `connect` | Connect | Establish or resume a session |
| `subscribe` | Subscribe | Subscribe to channels |
| `unsubscribe` | Unsubscribe | Unsubscribe from channels |
| `publish` | Publish | Publish a message to a channel |
| `rpc_request` | RpcRequest | Send an RPC request to a backend |
| `ping` | Ping | Keepalive ping |
| `pong` | Pong | Keepalive response to a server ping |
| `survey_request` | SurveyRequest | Initiate a survey |
| `survey_reply` | SurveyReply | Reply to a survey |
| `sub_refresh` | SubRefresh | Refresh subscription tokens |

### OutboundMessage (Server → Client)

| Field | Type | Description |
| --- | --- | --- |
| `connected` | Connected | Session established |
| `subscribe_ack` | SubscribeAck | Subscriptions confirmed |
| `unsubscribe_ack` | UnsubscribeAck | Unsubscriptions confirmed |
| `publish_ack` | PublishAck | Publish confirmed with offset |
| `publication` | Publication | Messages delivered to subscriber |
| `rpc_reply` | RpcReply | RPC response from backend |
| `sub_refresh_ack` | SubRefreshAck | SubRefresh acknowledged (subscriptions re-validated) |
| `pong` | Pong | Keepalive response |
| `ping` | Ping | Server-initiated keepalive ping (when `server.heartbeat.ping_interval` is enabled) |
| `error` | Error | Error notification |
| `survey_request` | SurveyRequest | Incoming survey from another client |
| `survey_reply` | SurveyReply | Survey response |

## Connection Lifecycle

### Connect

```json
{
  "id": "1",
  "connect": {
    "client_id": "my-client",
    "token": "auth-token",
    "subscriptions": [
      {"channel": "chat.general"}
    ]
  }
}
```

Response:

```json
{
  "id": "1",
  "connected": {
    "session_id": "abc-123",
    "epoch": "broker-epoch-id"
  }
}
```

Fields:

| Field | Required | Description |
| --- | --- | --- |
| `client_id` | Yes | Unique identifier for this client instance |
| `token` | No | Authentication token (passed to proxy if configured) |
| `subscriptions` | No | Channels to subscribe immediately on connect |
| `session_id` | No | Previous session ID to attempt resumption |

### Session Resumption

To resume a previous session, include `session_id` in the Connect message:

```json
{
  "id": "1",
  "connect": {
    "client_id": "my-client",
    "session_id": "previous-session-id"
  }
}
```

If resumption succeeds, the response includes `resumed: true`. Connect
uses the same recovery helper as Subscribe: the recovery set is the
ordered union of ACL-passed `connect.subscriptions` plus any snapshot-only
channels from the resumed session. Each channel appears in
`connected.recover_results`.

On a **fresh** (non-resumed) Connect, `recover=true` + `offset=0` replays
from the beginning of history. During session resume the server trusts its
own recorded per-channel offset; a channel without one is skipped
(`RECOVER_SKIPPED`) and history is never replayed from scratch. Subscribe
is never a session resume — those offset rules apply only here.

## Pub/Sub

### Subscribe

```json
{
  "id": "2",
  "subscribe": {
    "subscriptions": [
      {"channel": "chat.general"},
      {"channel": "notifications", "ephemeral": true}
    ]
  }
}
```

Response:

```json
{
  "id": "2",
  "subscribe_ack": {
    "subscriptions": [
      {"channel": "chat.general"},
      {"channel": "notifications", "ephemeral": true}
    ]
  }
}
```

Subscription options:

| Field | Default | Description |
| --- | --- | --- |
| `channel` | — | Channel name to subscribe to |
| `ephemeral` | `false` | If true, subscription is not tracked for presence |
| `token` | — | Per-channel auth token (passed to proxy ACL) |
| `offset` | `0` | Resume from this offset (0 = latest) |
| `recover` | `false` | Request missed messages since offset |
| `epoch` | — | Broker epoch for offset validation |

Recovery (PR-03): when `recover=true`, the `subscribe_ack` additionally
carries `publications` (the missed messages, with the same stable
`channel-offset` IDs as realtime delivery) and `recover_results`, one entry
per subscribed channel:

- `recovered=true` — History was read successfully (an empty batch means the
  client is caught up; `offset` echoes the requested cursor).
- `truncated=true` — the batch hit the request-level cap
  (`MaxRecoveredPublications`, shared across all channels of the request or
  the policy `recover_limit`); `offset` is the last delivered message and the
  client may issue another `recover` from there.
- `error.code=RECOVER_FAILED` — the broker history read failed. The
  subscription **stays active**: a history hiccup never disconnects the
  client or revokes the subscription.
- `error.code=RECOVER_SKIPPED` — the client asked for recovery
  (`recover=true`) but History was not called: a wildcard channel, or
  channel policy denies history / recovery (`history=false` or
  `transient_only`).

Without `recover`, the ack still carries a `recover_results` entry per
channel (`recovered=false`, **no** error). Old clients that ignore the new
fields keep working: the subscription succeeds either way.

`offset=0` with `recover=true` on Subscribe replays from the beginning of
history (Subscribe is never a session resume). Resume offset rules — server
recorded offset wins, missing offset is skipped — apply only to Connect;
see [Session Resumption](#session-resumption).

### Unsubscribe

```json
{
  "id": "3",
  "unsubscribe": {
    "subscriptions": [
      {"channel": "chat.general"}
    ]
  }
}
```

### Publish

```json
{
  "id": "4",
  "publish": {
    "channel": "chat.general",
    "payload": {
      "text": "hello world"
    }
  }
}
```

Response:

```json
{
  "id": "4",
  "publish_ack": {
    "id": "4",
    "offset": 42
  }
}
```

### Publication (Server → Client)

When a message is published to a channel a client is subscribed to:

```json
{
  "publication": {
    "messages": [
      {
        "id": "msg-uuid",
        "channel": "chat.general",
        "offset": 42,
        "payload": {
          "text": "hello world"
        }
      }
    ]
  }
}
```

## Payload Types

The `Payload` message supports three data formats:

```json
{"text": "plain text string"}
```

```json
{"binary": "base64-encoded-bytes"}
```

```json
{"json": {"key": "value"}}
```

Optional `content_type` field can specify the MIME type (e.g., `application/json`).

### Payload type preservation

The server preserves the original `Payload` oneof variant end to end: a
message published as `json` (or `text`/`binary`) is delivered to subscribers
in real time, replayed during connect-time recovery, and returned by the
admin `GetHistory` API in the same variant. Before this guarantee existed,
`json` payloads were collapsed to `text` on the wire.

Known limitation: JSON numbers larger than 2^53 lose precision at the
ingress boundary (structpb conversion); the payload type is preserved, the
exact numeric values are not.

## RPC

### Request

```json
{
  "id": "5",
  "rpc_request": {
    "channel": "api.users",
    "method": "getProfile",
    "payload": {
      "json": {"user_id": "123"}
    }
  }
}
```

### Reply

```json
{
  "id": "5",
  "rpc_reply": {
    "request_id": "5",
    "payload": {
      "json": {"name": "Alice", "email": "alice@example.com"}
    }
  }
}
```

RPC requests are forwarded to a proxy backend matching the channel and method patterns. The server applies a timeout (default 30s, configurable via `server.rpc_timeout`).

## Heartbeat

Heartbeat is **bidirectional** in v1.0:

- **Client → Server**: clients send periodic `Ping` frames; the server replies with `Pong` (same `id`).
- **Server → Client**: when `server.heartbeat.ping_interval > 0`, the server sends `Ping` frames on its own and expects any inbound frame within `ping_timeout` (a `Pong` with the same `id` is the conventional answer).

```json
{"id": "6", "ping": {}}
```

Response:

```json
{"id": "6", "pong": {}}
```

Rules:

- Either side that receives a `Ping` replies with a `Pong` carrying the same `id`.
- **Any inbound frame** (Ping/Pong/publish/subscribe/business traffic) counts as liveness: it refreshes the server-side activity timestamp and disarms the outstanding server ping deadline. A pong is not the only valid answer.
- Inbound `Pong` refreshes presence and the cluster session lease, throttled like `Ping` (at most once per 10s); a client that only answers server pings stays visible to the cluster.
- The server disconnects with `DisconnectIdleTimeout` (3511) when either the idle timeout elapses with no activity, or a server ping goes unanswered for `ping_timeout` (strategy B — it does not wait for the idle check).
- `ping_interval` defaults to off (0s): the server never pings, and old clients that only send their own pings are unaffected. Enabling `ping_interval` without upgrading clients to answer pings disconnects them after `ping_timeout`.

## Error Codes

Errors are returned as `Error` messages:

```json
{
  "error": {
    "code": "ACL_DENIED",
    "type": "acl_error",
    "message": "publish denied by ACL rule"
  }
}
```

Common error codes:

| Code | Type | Description |
| --- | --- | --- |
| `ACL_DENIED` | `acl_error` | Operation blocked by ACL rule |
| `ACL_ERROR` | `acl_error` | ACL proxy check failed |
| `RATE_LIMITED` | `rate_limit` | Publish rate limit exceeded |
| `RPC_TIMEOUT` | `timeout` | RPC forwarded to a proxy timed out |
| `PROXY_ERROR` | `proxy_error` | Proxy call failed |
| `AUTH_REQUIRED` | `auth_error` | Authentication required but no token (or no auth proxy) |
| `INTERNAL_ERROR` | `server_error` | Internal server error while handling the request |
| `BAD_REQUEST` | `client_error` | Frame could not be decoded |
| `DISCONNECT_ERROR` | `transport_error` | Connection being terminated |

## Disconnect Codes

When the server closes a connection, it sends a disconnect with a numeric code. Codes are defined in `disconnect.go`; the client should treat them as advisory (reconnect unless told not to):

| Code | Name | Reconnect | Description |
| --- | --- | --- | --- |
| 3000 | ConnectionClosed | Yes | Clean disconnect or network loss; the server cannot distinguish the two |
| 3500 | InvalidToken | Yes | Invalid token, or missing token when `require_auth` is enabled |
| 3501 | BadRequest | Yes | Malformed protocol frame (e.g. second Connect on an authenticated connection) |
| 3502 | Stale | Yes | Cluster session resume failed (remote session lease CAS claim failed, or resume/takeover rollback) |
| 3503 | ForceNoReconnect | No | Server requires the client not to reconnect (e.g. shutdown drain) |
| 3504 | ConnectionLimit | Yes | Per-user connection limit exceeded |
| 3505 | ChannelLimit | Yes | Per-client subscription limit exceeded |
| 3506 | InappropriateProtocol | Yes | Transport cannot carry the data (e.g. binary data to a JSON client). Reserved definition; no current trigger point |
| 3507 | PermissionDenied | Yes | Not enough permissions. Reserved definition; no current trigger point |
| 3508 | NotAvailable | Yes | Server cannot process the message type. Reserved definition; no current trigger point |
| 3509 | TooManyErrors | Yes | Client produced too many errors. Reserved definition; no current trigger point |
| 3511 | IdleTimeout | Yes | No activity within the heartbeat idle timeout, or an unanswered server ping within `ping_timeout` |
| 3512 | SlowConsumer | Yes | Client cannot consume messages fast enough |
| 3513 | Internal | Yes | Connect path internal error (e.g. cluster state sync failed), connection forced closed (`disconnectOnConnectError`, `client.go`) |

## Channel Naming

- Channels use `.` as the hierarchy delimiter.
- Channel names are non-empty dot-separated lists of non-empty segments: `a.` and `..b` are invalid and rejected at subscription and publish time.
- Wildcard subscriptions use `*` to match a single level (e.g., `chat.*` matches `chat.general` but not `chat.rooms.1`).
- A trailing `**` matches zero or more levels (MQTT-style suffix wildcard): `chat.**` matches `chat`, `chat.general` and `chat.rooms.1`, and a bare `**` matches every channel. `**` is only valid as the final segment — patterns like `a.**.b` or `a**b` are rejected.
- Channel names are case-sensitive.

## Presence

Presence is tracked per subscribed channel. A subscription is tracked only when all three hold: `ephemeral: false`, the channel is exact (wildcard patterns like `chat.**` are never tracked), and the channel policy enables `presence`. Tracked members appear in the presence store and their join/leave becomes a first-class `presence_event`:

- Subscribing to `C` delivers `C`'s join/leave events as `presence_event` envelopes — there is no need to also subscribe to a `C/__presence` companion channel.
- The joining client does **not** receive its own join; the leaving client does **not** receive its own leave.
- Events always carry the exact channel in `event.channel`; a wildcard subscriber receives the events of every exact channel its pattern covers.
- Snapshots ride on `connected.presence` / `subscribe_ack.presence` (and are available on demand via `PresenceQuery`). A snapshot carries `occupancy` (all members), the capped `clients` list and `truncated`; the cap is `presence_snapshot_limit` (default 256).
- `PresenceQuery` requires the session to cover the channel (exact subscription or matching wildcard pattern) plus the channel policy plus the built-in ACL; failures return `PERMISSION_DENIED` / `POLICY_DENIED` top-level errors and never disconnect.
- Wildcard patterns, ephemeral subscriptions and `presence=false` channels produce no store entry, no snapshot entry and no events.
- Presence failures never disconnect the client and never revoke a subscription.
- By default no companion channel is written. With `legacy_presence_channel: true` (exact channels only), join/leave is additionally published transiently on `<channel>/__presence` in the legacy JSON format.
- Phase 1 delivers presence events locally only (no cross-node emit). With `server.presence.cluster_emit: true` (default `false`; enable only after every node runs PR-04a+) join/leave events are published through the broker on the exact channel and rewritten by every node, so members on other nodes receive them too; the joiner/leaver still never receives its own event.

Presence state is also served via the admin `GetPresence` API (backed by the presence store).

## Server-Side Admin API

The gRPC admin API (`messageloop.server.v1.APIService`) provides server-side management:

| RPC | Description |
| --- | --- |
| `Publish` | Publish messages to channels from the server side |
| `Disconnect` | Force-disconnect client sessions |
| `Subscribe` | Subscribe a session to channels |
| `Unsubscribe` | Unsubscribe a session from channels |
| `GetChannels` | List active channels with subscriber counts |
| `GetPresence` | Get presence info for a channel |
| `GetHistory` | Retrieve message history for a channel |
| `Survey` | Send a survey to all connected clients |
