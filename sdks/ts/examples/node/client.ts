/**
 * Node.js example for MessageLoop SDK
 *
 * Run: npx ts-node examples/node/client.ts
 */

import {
  MessageLoopClient,
  createJSONMessage,
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
  client.onMessage((messages) => {
    for (const msg of messages) {
      console.log(`[${msg.channel}] ${msg.message.type}:`, msg.message.data);
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
  const message = createJSONMessage("chat.message", {
    text: "Hello from Node.js SDK!",
    timestamp: new Date().toISOString(),
  });

  await client.publish("chat.general", message);

  // Make an RPC call (if server supports it)
  try {
    const rpcRequest = createJSONMessage("user.get", {
      userId: "12345",
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
