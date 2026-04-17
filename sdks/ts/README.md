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
client.onMessage((messages) => {
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

- `onMessage(handler)` - Handle incoming messages
- `onError(handler)` - Handle errors
- `onConnected(handler)` - Handle connection established
- `onClosed(handler)` - Handle connection closed
- `addMessageHandler(handler)` - Register an additional message handler and get a disposer
- `addStateChangeHandler(handler)` - Observe connection state transitions and get a disposer

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

- Node.js `>=18` is required.
- The current TypeScript SDK is WebSocket-based; it does not expose a gRPC transport.
- Run `npm run build` before opening the browser example because it imports from `dist/`.
