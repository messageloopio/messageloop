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
| `pong` | Pong | Keepalive response |
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

If resumption succeeds, the response includes `resumed: true` and any missed publications since the last known offset.

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

Clients should send periodic pings to prevent idle disconnection:

```json
{"id": "6", "ping": {}}
```

Response:

```json
{"id": "6", "pong": {}}
```

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
| `DISCONNECT_ERROR` | `transport_error` | Connection being terminated |

## Disconnect Codes

When the server closes a connection, it sends a disconnect with a numeric code:

| Code | Name | Reconnect | Description |
| --- | --- | --- | --- |
| 3000 | BadRequest | Yes | Malformed message |
| 3001 | ServerError | Yes | Internal server error |
| 3003 | Stale | Yes | Unauthenticated or idle timeout |
| 3005 | SlowConsumer | Yes | Client not reading fast enough |
| 3008 | ForceNoReconnect | No | Server shutting down |
| 3009 | ConnectionClosed | No | Connection already closed |
| 3010 | InvalidToken | No | Authentication failed |
| 3500 | ConnectionLimit | Yes | Per-user connection limit exceeded |
| 3501 | ChannelLimit | Yes | Per-client subscription limit exceeded |

## Channel Naming

- Channels use `.` as the hierarchy delimiter.
- Wildcard subscriptions use `*` to match a single level (e.g., `chat.*` matches `chat.general` but not `chat.rooms.1`).
- Channel names are case-sensitive.

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
