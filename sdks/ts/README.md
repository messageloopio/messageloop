# MessageLoop TypeScript SDK

A TypeScript SDK for MessageLoop, supporting Node.js and browsers over WebSocket.

## Features

- WebSocket client for Node.js and browsers
- JSON and protobuf encoding
- Channel pub/sub and RPC
- Per-channel subscription tokens and streamed message recovery (`recover` + `cursor`/`fresh`)
- Presence: `onPresence` events, `onPresenceSnapshot`, and `presence(channel)` queries
- Client-initiated surveys (`survey`) and survey answering (`onSurvey` / `onSurveyRequest`)
- Server-initiated Ping answered with a same-id Pong
- Ack-aware publishing (`publishWithAck`)
- Subscription refresh (`subRefresh`)
- Automatic reconnection with session resumption
- Heartbeat and pong timeout handling
- Message helpers for JSON, text, and binary payloads

## Installation

```bash
npm install @messageloop/sdk
```

## Quick Start

```typescript
import {
  MessageLoopClient,
  createJSONMessage,
  setAutoSubscribe,
  setClientId,
  setEncoding,
} from "@messageloop/sdk";

const client = await MessageLoopClient.dial("ws://localhost:9080/ws", [
  setClientId("my-client"),
  setAutoSubscribe("chat.messages"),
  setEncoding("json"),
]);

client.onConnected((sessionId) => console.log("Connected:", sessionId));
client.addMessageHandler((messages) => {
  for (const msg of messages) {
    console.log(msg.channel, msg.message.type, msg.message.data);
  }
});
client.onError((err) => console.error("Error:", err));

await client.publish(
  "chat.messages",
  createJSONMessage("chat.message", { text: "Hello!" })
);

const response = await client.rpc(
  "user.service",
  "GetUser",
  createJSONMessage("user.get", { userId: "123" }),
  { timeout: 5000 }
);
console.log("RPC:", response.data);

await client.close();
```

## Option Builders

| Builder | Default | Description |
|--------|---------|-------------|
| `setEncoding("json" \| "proto")` | `"json"` | Select wire encoding |
| `setClientId(string)` | auto-generated UUID | Set logical client ID |
| `setClientType(string)` | `"sdk"` | Set client type metadata |
| `setToken(string)` | `""` | Authentication token passed in `Connect` |
| `setVersion(string)` | `"1.0.0"` | Client version metadata |
| `setAutoSubscribe(...channels)` | `[]` | Subscribe automatically on connect |
| `setPingInterval(number)` | `30000` | Ping interval in milliseconds |
| `setPingTimeout(number)` | `10000` | Pong timeout in milliseconds |
| `setConnectTimeout(number)` | `30000` | Initial WebSocket connect timeout |
| `setRPCTimeout(number)` | `30000` | Default RPC timeout |
| `setEphemeral(boolean)` | `false` | Mark subscriptions as ephemeral |
| `setAutoReconnect(boolean)` | `true` | Enable or disable reconnect logic |
| `setReconnectDelay(initial, max)` | `1000`, `30000` | Configure reconnect backoff window |
| `setReconnectBackoff(initial, max, multiplier)` | `1000`, `30000`, `2` | Configure reconnect backoff window and multiplier |
| `setReconnectMaxAttempts(number)` | `0` | Maximum reconnect attempts, `0` = unlimited |

## API Reference

### Create And Connect

- `MessageLoopClient.dial(url, options?)` - Connect and return a ready client

### Client Methods

- `close()` - Close the connection
- `subscribe(...channels)` - Subscribe to channels; each argument is a channel
  name or `{ channel, token?, recover?, cursor?, fresh? }`. With `recover:
  true` the server streams the channel history as `Publication` envelopes with
  `replay=true` (delivered via the same `onMessage` / `addMessageHandler` path
  as live messages), followed by a `recover_complete` echoing the
  authoritative position. `cursor: { streamEpoch, offset }` is the resume
  hint; omitting it is a no-hint recover (the server resumes from its own
  record or skips). There is no "offset 0 means from the start": use
  `fresh: true` for an explicit from-the-start replay (Go SDK `WithRecover` /
  `WithFresh` parity).
- `unsubscribe(...channels)` - Unsubscribe from channels (same argument form)
- `publish(channel, message)` - Publish a message to a channel (fire-and-forget)
- `publishWithAck(channel, message, options?)` - Publish and await the server
  ack; resolves with `{ id, offset }`, rejects on timeout or disconnect
  (`options.transient`, `options.timeout`)
- `subRefresh(...channels)` - Ask the server to re-validate subscriptions
- `rpc(channel, method, request, options?)` - Make an RPC call
- `presence(channel)` - Query the presence snapshot of an exact channel;
  resolves with `{ channel, clients, truncated, occupancy }`, rejects with a
  server-coded error
- `survey(channel, payload, timeoutMs?)` - Initiate a channel-scoped survey
  and await the aggregated `SurveyAnswer[]` (`sessionId`, `userId` read from
  `metadata.entries["user_id"]`, `payload`, `error`)
- `getSessionId()` - Get current session ID
- `getConnectionState()` - Get `disconnected`, `connecting`, `connected`, or `reconnecting`
- `isConnected()` - Check connection status
- `getSubscribedChannels()` - Get subscribed channels
- `disableAutoReconnect()` - Stop reconnect attempts
- `enableAutoReconnect()` - Re-enable reconnect attempts

### Event Handlers

Use the `add` series for new code: handlers can be registered in multiples and
each call returns a disposer for removal.

- `addMessageHandler(handler)` - Register a message handler; returns a function that removes it
- `addStateChangeHandler(handler)` - Observe connection state transitions; returns a function that removes it
- `removeMessageHandler(handler)` - Remove a message handler

The `onXxx` methods are single-slot convenience aliases (the last registration
wins). They are kept for backward compatibility; do not mix them with the `add`
series on the same event, or handlers may both fire and duplicate delivery.

- `onMessage(handler)` - Convenience alias for the single-slot message handler
- `onError(handler)` - Set the error handler
- `onConnected(handler)` - Set the connected handler
- `onClosed(handler)` - Set the closed handler
- `onSurvey(handler)` - Handle survey requests from the server. The handler
  receives `(requestId, request)` and returns the reply `Message` (sync or
  async); a thrown error is sent back as an error reply. With no handler
  registered, the request payload is echoed back unchanged (Go SDK parity).
- `onSurveyRequest(handler)` - Handle survey requests with the request
  channel: `(requestId, channel, request)`. Takes precedence over `onSurvey`.
- `onPresence(handler)` - Handle presence events (`{ channel, action,
  info }`, join/leave; unknown actions still delivered)
- `onPresenceSnapshot(handler)` - Handle presence snapshots delivered with
  `Connected` / `SubscribeAck`, and the snapshot returned by `presence()`

Note: do not `await rpc() / survey() / presence()` synchronously inside
receive-loop callbacks (`onMessage`, `onPresence*`, `onSurvey*`) — those
calls wait on the caller's promise while the receive loop fills the pending
result, so a synchronous wait deadlocks.

### Message Helpers

- `createJSONMessage(type, json)`
- `createTextMessage(type, text)`
- `createBinaryMessage(type, binary)`
- `createMessage(type, data)`
- `createData(contentType, value)`
- `dataAs(message)`

## Examples

- `examples/node/client.ts` - Node.js WebSocket client example
- `examples/browser/index.html` - Browser example using the built SDK bundle

## Building

```bash
npm install
npm run build
```

## Testing

```bash
npm test
```

## Notes

- Node.js `>=18` is required. On Node.js `<21` the SDK uses the `ws` package
  (a runtime dependency); Node 21+ and browsers use the built-in WebSocket.
- Custom `headers` on `dial()` are honored in Node.js only (via `ws`); the
  browser WebSocket constructor does not support request headers.
- The SDK answers server-initiated Pings with a same-id Pong and treats them
  as liveness evidence: enable `server.heartbeat.ping_interval` only with SDK
  version 1.1.0+.
- Default values intentionally differ from the Go SDK in two ways (explicit
  decisions, not drift): `autoReconnect` defaults to `true` here (Go: `false`),
  and `connectTimeout` defaults to `30000ms` here (Go `DialTimeout`: `10000ms`).
- The current TypeScript SDK is WebSocket-based; it does not expose a gRPC transport.
- Run `npm run build` before opening the browser example because it imports from `dist/`.
