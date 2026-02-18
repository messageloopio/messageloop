/**
 * Node.js example for MessageLoop SDK
 *
 * Run: npx ts-node examples/node/client.ts
 */

import {
  MessageLoopClient,
  createCloudEvent,
  setClientId,
  setAutoSubscribe,
  setToken,
  setEncoding,
} from "../src/index";

async function main() {
  // Create and connect client
  const client = await MessageLoopClient.dial("ws://localhost:9080/ws", [
    setClientId("node-client-001"),
    setAutoSubscribe("chat.general", "notifications"),
    setToken("your-auth-token"),
    setEncoding("json"),
  ]);

  console.log(`Connected with session: ${client.getSessionId()}`);

  // Set up handlers
  client.onConnected((sessionId) => {
    console.log(`Connected! Session ID: ${sessionId}`);
  });

  client.onMessage((events) => {
    for (const msg of events) {
      console.log(`[${msg.channel}] ${msg.event.type}:`, msg.event.data);
    }
  });

  client.onError((err) => {
    console.error("Error:", err.message);
  });

  client.onClosed(() => {
    console.log("Connection closed");
  });

  // Subscribe to additional channels
  await client.subscribe("chat.dev", "chat.random");

  // Publish a message
  const messageEvent = createCloudEvent({
    source: "/node-client",
    type: "chat.message",
    data: {
      text: "Hello from Node.js SDK!",
      timestamp: new Date().toISOString(),
    },
  });

  await client.publish("chat.general", messageEvent);

  // Make an RPC call (if server supports it)
  try {
    const rpcRequest = createCloudEvent({
      source: "/node-client",
      type: "user.get",
      data: { userId: "12345" },
    });

    const response = await client.rpc("user.service", "GetUser", rpcRequest, {
      timeout: 5000,
    });

    console.log("RPC Response:", response.data);
  } catch (err) {
    console.log("RPC not available:", (err as Error).message);
  }

  // Wait a bit for messages
  await new Promise((resolve) => setTimeout(resolve, 5000));

  // Clean up
  await client.close();
  console.log("Client closed");
}

main().catch(console.error);
