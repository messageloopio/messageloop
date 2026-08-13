# MessageLoop TypeScript SDK

A TypeScript SDK for MessageLoop, supporting Node.js and browsers over WebSocket.

## Features

- WebSocket client for Node.js and browsers
- JSON and protobuf encoding
- Channel pub/sub and RPC
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
- `subscribe(...channels)` - Subscribe to channels
- `unsubscribe(...channels)` - Unsubscribe from channels
- `publish(channel, message)` - Publish a message to a channel
- `rpc(channel, method, request, options?)` - Make an RPC call
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
- Default values intentionally differ from the Go SDK in two ways (explicit
  decisions, not drift): `autoReconnect` defaults to `true` here (Go: `false`),
  and `connectTimeout` defaults to `30000ms` here (Go `DialTimeout`: `10000ms`).
- The current TypeScript SDK is WebSocket-based; it does not expose a gRPC transport.
- Run `npm run build` before opening the browser example because it imports from `dist/`.
