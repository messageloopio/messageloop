# MessageLoop TypeScript SDK

A TypeScript SDK for MessageLoop messaging platform, supporting both Node.js and browsers.

## Features

- WebSocket transport with JSON and Protobuf encoding
- Pub/sub messaging with wildcard channel support
- RPC-style request/response
- Automatic reconnection support (coming soon)
- Heartbeat/ping-pong keepalive

## Installation

```bash
npm install @messageloop/sdk
```

## Quick Start

```typescript
import { dial, createMessage } from "@messageloop/sdk";

// Create and connect client
const client = await dial("ws://localhost:9080/ws", [
  setClientId("my-client"),
  setAutoSubscribe("chat.messages"),
]);

// Set up handlers
client.onConnected((sessionId) => console.log("Connected:", sessionId));
client.onMessage((events) => {
  events.forEach((msg) => console.log("Message:", msg.payload));
});
client.onError((err) => console.error("Error:", err));

// Publish a message
const msg = createMessage({
  channel: "chat.messages",
  payload: { text: "Hello!" },
});
await client.publish(msg);

// Make an RPC call
const response = await client.rpc("user.service", "GetUser", requestMsg);

// Clean up
await client.close();
```

## Configuration Options

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `encoding` | `"json"` \| `"proto"` | `"json"` | Message encoding |
| `clientId` | `string` | Auto-generated | Client identifier |
| `clientType` | `string` | `"sdk"` | Client type |
| `token` | `string` | `""` | Authentication token |
| `version` | `string` | `"1.0.0"` | Client version |
| `autoSubscribe` | `string[]` | `[]` | Channels to auto-subscribe |
| `pingInterval` | `number` | `30000` | Heartbeat interval (ms) |
| `pingTimeout` | `number` | `10000` | Pong timeout (ms) |
| `connectTimeout` | `number` | `30000` | Connection timeout (ms) |
| `rpcTimeout` | `number` | `30000` | RPC timeout (ms) |

## API Reference

### Client Methods

- `connect()` - Connect to the server
- `close()` - Close the connection
- `subscribe(...channels)` - Subscribe to channels
- `unsubscribe(...channels)` - Unsubscribe from channels
- `publish(channel, message)` - Publish a message to a channel
- `rpc(channel, method, request, options?)` - Make an RPC call
- `getSessionId()` - Get current session ID
- `isConnected()` - Check connection status
- `getSubscribedChannels()` - Get subscribed channels

### Event Handlers

- `onMessage(handler)` - Handle incoming messages
- `onError(handler)` - Handle errors
- `onConnected(handler)` - Handle connection established
- `onClosed(handler)` - Handle connection closed

## Building

```bash
npm install
npm run build
```

## Testing

```bash
npm test
```
