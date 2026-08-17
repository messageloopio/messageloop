# Client Protocol Reference

This document describes the client protocol of the current standalone version (KD-K31); envelope definitions live in `protocol/client/v2`. The server-side admin gRPC API remains `server.v1` (an explicitly accepted decision, PR-KA-B3).

MessageLoop uses a bidirectional message protocol over WebSocket, gRPC streaming, or QUIC. All messages are wrapped in `InboundMessage` (client → server) and `OutboundMessage` (server → client) envelopes.

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

### QUIC

The optional QUIC listener (`transport.quic.addr`) carries the same envelopes over one bidirectional QUIC stream. Each frame is a 4-byte big-endian length prefix followed by the payload. Encoding is negotiated via TLS ALPN:

| ALPN | Encoding |
| --- | --- |
| `messageloop+json` | JSON (protobuf-compatible JSON mapping) |
| `messageloop` | JSON (alias) |
| `messageloop+proto` | Protobuf binary |

QUIC requires TLS 1.3. Disconnect reasons are delivered both as a `DISCONNECT_ERROR` envelope (same metadata as gRPC) and as a QUIC application error code matching the numeric disconnect code.

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
| `survey_request` | SurveyRequest | Initiate a survey on a channel (client-initiated, PR-07) |
| `survey_reply` | SurveyReply | Reply to a survey the session was asked |
| `sub_refresh` | SubRefresh | Refresh subscription tokens |

### OutboundMessage (Server → Client)

| Field | Type | Description |
| --- | --- | --- |
| `connected` | Connected | Session established |
| `subscribe_ack` | SubscribeAck | Subscriptions confirmed |
| `unsubscribe_ack` | UnsubscribeAck | Unsubscriptions confirmed |
| `publish_ack` | PublishAck | Publish confirmed with position (offset set, or omitted for transient / no-history) |
| `publication` | Publication | Messages delivered to subscriber (replay=true marks a recovery replay) |
| `recover_complete` | RecoverComplete | Per-channel end of a streamed recovery (position / truncated / gap / error) |
| `rpc_reply` | RpcReply | RPC response from backend |
| `sub_refresh_ack` | SubRefreshAck | SubRefresh acknowledged (subscriptions re-validated) |
| `pong` | Pong | Keepalive response |
| `ping` | Ping | Server-initiated keepalive ping (when `server.heartbeat.ping_interval` is enabled) |
| `error` | Error | Error notification |
| `survey_request` | SurveyRequest | Survey forwarded to a channel subscriber (server-generated `request_id`; the subscriber replies with `survey_reply`) |
| `survey_reply` | SurveyReply | Survey response |
| `survey_result` | SurveyResult | Aggregated survey answers (async reply to a client-initiated survey) |
| `gap_notice` | GapNotice | Catch-up gap notification (`channel`, `position`, `gap_reason`); `gap_reason` is `GAP_REASON_MIDDLE` or `GAP_REASON_REPLAY_TRUNCATED`; `position` is the last known safe position (offset omitted when unknown); at most one per channel per catch-up |

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
    "stream_epoch": "broker-epoch-id",
    "subscriptions": [
      {"channel": "chat.general"}
    ]
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
channels from the resumed session. Every recovered channel streams its
replay publications and ends with exactly one `recover_complete` (see
[Recovery](#recovery) below).

Only two conditions replay from the beginning of history: `fresh=true`, or
a resume whose snapshot epoch differs from the broker epoch (both known).
An offset of 0 — or an absent cursor — never means "from the start".
During session resume the server trusts its own recorded per-channel
offset; a channel without one is skipped (`RECOVER_SKIPPED`) and history
is never replayed from scratch.

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
| `recover` | `false` | Request streamed recovery of missed messages |
| `cursor` | — | Resume hint: `{ stream_epoch, offset? }` (see Recovery) |
| `fresh` | `false` | Explicit from-the-start replay (see Recovery) |

### Recovery

When `recover=true`, the server **streams** the channel's history instead
of embedding it in the ack:

```
SubscribeAck                      // bare ack: no publications, no recover_results
Publication { messages[].replay=true, position }   // 0..N frames, one message each
RecoverComplete { channel, position, truncated, gap, gap_reason, error? }
```

Every recover=true channel ends with exactly one `RecoverComplete`:

- `position` echoes the authoritative cursor: the last delivered
  publication's position on success, or the requested cursor for skipped /
  failed / empty batches (offset omitted when deliberately unset).
- `truncated=true` — the batch hit the request-level cap
  (`MaxRecoveredPublications`, shared across all channels of the request or
  the policy `recover_limit`); the client may issue another `recover` from
  `position`.
- `gap=true` + `gap_reason` — History reported a gap
  (`HEAD_TRIMMED` / `EMPTY_EXPIRED` / `EPOCH_RESET`): the client cannot
  prove it is caught up, so the recovery is reported truncated, never OK.
- `error.code=RECOVER_FAILED` — the broker history read failed. The
  subscription **stays active**: a history hiccup never disconnects the
  client or revokes the subscription.
- `error.code=RECOVER_SKIPPED` — the client asked for recovery
  (`recover=true`) but History was not called: a wildcard channel, channel
  policy denies history / recovery (`history=false` or `transient_only`),
  or no cursor and no server-recorded delivered position.

`SubscribeAck.recover` summarizes the batch: `NONE` (no recover requested),
`PENDING` (at least one channel will stream), or `SKIPPED` (every recover
request was skippable before History).

Recovery start point:

- `fresh=true` — replay from the beginning, ignoring any cursor.
- resume with a snapshot epoch that differs from the broker epoch (both
  known) — offsets were invalidated, replay from the beginning.
- non-resume with `cursor.offset` set — replay from `offset+1`.
- non-resume with `recover=true` and no cursor — "no hint": resume from the
  server-recorded delivered position (if any), otherwise skip. **Never**
  replay the full history because the cursor was absent or 0.

Without `recover`, the bare ack succeeds with no recovery stream. Old
clients that ignore the new fields keep working: the subscription succeeds
either way.

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
    "position": {
      "stream_epoch": "broker-epoch-id",
      "offset": 42
    }
  }
}
```

Transient / no-history publishes ack with an unset position (no `offset`
field), never `0`-means-offset.

### Publication (Server → Client)

When a message is published to a channel a client is subscribed to — or
replayed as part of a streamed recovery:

```json
{
  "publication": {
    "messages": [
      {
        "id": "msg-uuid",
        "channel": "chat.general",
        "position": {
          "stream_epoch": "broker-epoch-id",
          "offset": 42
        },
        "replay": false,
        "payload": {
          "text": "hello world"
        }
      }
    ]
  }
}
```

`replay: true` marks a recovery replay; `position` is the authoritative
message position in both cases.

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
in real time, replayed during streamed recovery, and returned by the
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

## Client Survey

A client can ask every subscriber of a channel to answer, and collects the
answers asynchronously. This is a separate, client-initiated flow from the
admin `Survey` RPC.

```json
{
  "id": "7",
  "survey_request": {
    "request_id": "my-survey-1",
    "channel": "game.room.1",
    "payload": {"text": "ready?"},
    "timeout_ms": 2000
  }
}
```

Rules (PR-07):

- The channel must be an **exact** channel: an empty channel or a wildcard
  pattern is rejected with `BAD_REQUEST` (`request_error`). A wildcard
  **subscription** covers exact channels for survey initiation.
- The session must cover the channel (exact subscription or a matching
  wildcard pattern): otherwise `PERMISSION_DENIED` (`acl_error`).
- Survey is **off by default** (channel policy `survey: false`): the request
  fails with `SURVEY_DISABLED` (`policy_error`). Enable it per channel
  prefix via `server.channels` and grant users via ACL `allow_survey`.
- Each subscriber receives an outbound `survey_request` whose `request_id`
  is the **server-generated** survey id (not the client's); subscribers
  answer with an inbound `survey_reply` carrying that id. Answers from
  sessions that were not asked are dropped.
- The initiator is not blocked: the server validates synchronously, then a
  worker collects answers. The initiator's own `survey_reply` is read by its
  (free) read loop, and the aggregate arrives later as an outbound
  `survey_result` echoing the client's `request_id` (a missing one is
  server-generated and echoed back).
- `timeout_ms` is clamped to `[100ms, min(policy.max_survey_timeout || 5s,
  10s)]`; `timeout_ms <= 0` uses the policy cap (5s default).
- One survey per session at a time, rate-limited to 1/s (burst 1): further
  requests fail with `RATE_LIMITED` (`rate_limit`).
- When the channel's subscriber count (local fast path, then a cluster-wide
  count preflight) exceeds `max_survey_subscribers` (default 256), the
  survey fails with `SURVEY_TOO_MANY_SUBSCRIBERS` (`survey_error`) and
  **zero** `survey_request` frames are delivered. The admin `Survey` RPC is
  not subject to this cap.
- A single answer payload is capped at 4096 bytes; larger answers become a
  `SURVEY_ANSWER_TOO_LARGE` error with an empty payload. The whole encoded
  result is capped at 256 KiB; answers beyond the cap are truncated the same
  way. `user_id` is carried in answer `metadata.entries["user_id"]`.
- Failures never disconnect the client and never revoke subscriptions.

## Heartbeat

Heartbeat is **bidirectional**:

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
| `SURVEY_DISABLED` | `policy_error` | Client survey refused by channel policy (off by default) |
| `SURVEY_TOO_MANY_SUBSCRIBERS` | `survey_error` | Client survey refused: subscriber count above the cap |
| `SURVEY_ANSWER_TOO_LARGE` | `survey_error` | A survey answer (or result) exceeded the size cap and was truncated |

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

The gRPC admin API (`messageloop.server.v1.APIService`) provides server-side management. The admin surface remains `server.v1` by explicit decision (PR-KA-B3), not by omission:

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
