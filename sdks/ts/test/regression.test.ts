import { create, toBinary } from "@bufbuild/protobuf";
import { OutboundMessageSchema } from "../src/proto/client/v1/service_pb";
import { MessageLoopClient } from "../src/client/client";
import { buildClientOptions } from "../src/client/options";
import { WebSocketTransport } from "../src/transport/websocket";
import { jsonCodec, protobufCodec } from "../src/transport/codec";
import { createTextMessage } from "../src/message";

/**
 * Minimal WebSocket stand-in for transport-level tests.
 */
class FakeSocket {
  readyState = 1; // OPEN
  binaryType = "nodebuffer";
  onopen: any = null;
  onmessage: any = null;
  onerror: any = null;
  onclose: any = null;
  sent: unknown[] = [];

  send(data: unknown): void {
    this.sent.push(data);
  }

  close(_code?: number, _reason?: string): void {
    this.readyState = 3; // CLOSED
    if (this.onclose) {
      this.onclose({ code: 1000, reason: "closed" });
    }
  }
}

function makeClient(): MessageLoopClient {
  return new (MessageLoopClient as any)(buildClientOptions([])) as MessageLoopClient;
}

describe("P0-1: pong timeout triggers reconnect, not permanent close", () => {
  it("does not set closed flag and schedules reconnection", async () => {
    jest.useFakeTimers();

    const client = makeClient();
    (client as any).transport = {
      send: jest.fn().mockResolvedValue(undefined),
    };
    (client as any).isConnectedFlag = true;
    (client as any).autoReconnectEnabled = true;
    (client as any).connectionState = "connected";

    const errorHandler = jest.fn();
    client.onError(errorHandler);

    await (client as any).sendPing();
    jest.advanceTimersByTime((client as any).options.pingTimeout);

    expect(errorHandler).toHaveBeenCalledWith(expect.objectContaining({ message: "Pong timeout" }));
    expect((client as any).isClosedFlag).toBe(false);
    expect((client as any).isReconnecting).toBe(true);
    expect((client as any).connectionState).toBe("reconnecting");
    expect((client as any).reconnectTimer).not.toBeNull();

    jest.useRealTimers();
  });
});

describe("P0-2: recv() propagates close/error to the iterator", () => {
  it("throws 'Connection closed' on server close instead of hanging", async () => {
    const socket = new FakeSocket();
    const transport = new WebSocketTransport(socket as any, jsonCodec);

    const iter = transport.recv();
    const pending = iter.next();
    socket.onmessage({ data: JSON.stringify({ pong: {} }) });
    await expect(pending).resolves.toEqual({ done: false, value: expect.anything() });

    const afterClose = iter.next();
    socket.onclose({ code: 1000 });
    await expect(afterClose).rejects.toThrow("Connection closed");

    expect((transport as any).messageListeners).toHaveLength(0);
    expect((transport as any).errorListeners).toHaveLength(0);
    expect((transport as any).closeListeners).toHaveLength(0);
  });

  it("propagates the real error from onerror", async () => {
    const socket = new FakeSocket();
    const transport = new WebSocketTransport(socket as any, jsonCodec);

    const iter = transport.recv();
    const pending = iter.next();
    socket.onerror({ message: "boom" });
    await expect(pending).rejects.toThrow(/WebSocket error/);
  });

  it("delivers buffered messages before surfacing close", async () => {
    const socket = new FakeSocket();
    const transport = new WebSocketTransport(socket as any, jsonCodec);

    const iter = transport.recv();
    const p1 = iter.next();
    socket.onmessage({ data: JSON.stringify({ pong: {} }) });
    await expect(p1).resolves.toEqual({ done: false, value: expect.anything() });

    // Generator is now suspended at the yield: a message arriving here is
    // buffered, and close must not drop it before the iterator throws.
    socket.onmessage({ data: JSON.stringify({ pong: {} }) });
    await Promise.resolve();
    socket.onclose({ code: 1000 });
    const p2 = iter.next();
    await expect(p2).resolves.toEqual({ done: false, value: expect.anything() });
    await expect(iter.next()).rejects.toThrow("Connection closed");
  });
});

describe("P0-3: error envelope routes to pending RPC without reconnecting", () => {
  it("rejects the pending RPC fast and keeps the connection", async () => {
    const client = makeClient();
    let sentId = "";
    (client as any).transport = {
      send: jest.fn(async (msg: any) => {
        sentId = msg.id;
      }),
    };
    (client as any).isConnectedFlag = true;
    (client as any).connectionState = "connected";

    const errorHandler = jest.fn();
    client.onError(errorHandler);

    const rpcPromise = client.rpc("ch1", "method", createTextMessage("t", "x"), {
      timeout: 10000,
    });
    await Promise.resolve();
    expect(sentId).not.toBe("");

    const errorEnvelope = create(OutboundMessageSchema, {
      id: sentId,
      envelope: {
        case: "error",
        value: { code: "RPC_FAILED", type: "rpc_error", message: "boom" },
      },
    });
    (client as any).handleMessage(errorEnvelope);

    let caught: any;
    try {
      await rpcPromise;
    } catch (err) {
      caught = err;
    }
    expect(caught).toBeDefined();
    expect(caught.message).toBe("boom");
    expect(caught.code).toBe("RPC_FAILED");
    expect(caught.type).toBe("rpc_error");

    expect(errorHandler).not.toHaveBeenCalled();
    expect((client as any).connectionState).toBe("connected");
    expect((client as any).isClosedFlag).toBe(false);
    expect((client as any).isReconnecting).toBe(false);
    expect((client as any).reconnectTimer).toBeNull();
  });
});

describe("P0-4: Connected.publications recovery messages are delivered", () => {
  it("delivers recovery messages and updates channel offsets", () => {
    const client = makeClient();
    const handler = jest.fn();
    client.onMessage(handler);

    const connected = create(OutboundMessageSchema, {
      envelope: {
        case: "connected",
        value: {
          sessionId: "s1",
          epoch: "e1",
          resumed: true,
          subscriptions: [{ channel: "ch1" }],
          publications: [
            {
              messages: [
                {
                  id: "m1",
                  channel: "ch1",
                  offset: 42n,
                  payload: {
                    contentType: "text/plain",
                    data: { case: "text", value: "hello" },
                  },
                },
              ],
            },
          ],
        },
      },
    });

    (client as any).handleMessage(connected);
    (client as any).stopPingLoop();

    expect(handler).toHaveBeenCalledTimes(1);
    expect(handler.mock.calls[0][0]).toEqual([
      expect.objectContaining({ id: "m1", channel: "ch1", offset: 42n }),
    ]);
    expect((client as any).channelOffsets.get("ch1")).toBe(42n);
    expect(client.getSubscribedChannels()).toEqual(["ch1"]);
    expect(client.getSessionId()).toBe("s1");
    expect((client as any).epoch).toBe("e1");
  });
});

describe("P0-5: Blob input decodes without throwing (browser)", () => {
  it("protobuf codec decodes a Blob", async () => {
    const msg = create(OutboundMessageSchema, {
      id: "x",
      envelope: { case: "pong", value: {} },
    });
    const bytes = toBinary(OutboundMessageSchema, msg);
    const blob = new Blob([bytes as BlobPart]);

    const out = await protobufCodec.decode(blob);
    expect(out.envelope.case).toBe("pong");
  });

  it("json codec decodes a Blob", async () => {
    const blob = new Blob([JSON.stringify({ pong: {} })]);
    const out = await jsonCodec.decode(blob);
    expect(out.envelope.case).toBe("pong");
  });

  it("transport receives a Blob frame from the socket", async () => {
    const socket = new FakeSocket();
    const transport = new WebSocketTransport(socket as any, jsonCodec);

    const iter = transport.recv();
    const pending = iter.next();
    socket.onmessage({ data: new Blob([JSON.stringify({ pong: {} })]) });
    await expect(pending).resolves.toEqual({ done: false, value: expect.anything() });
    socket.close();
    await expect(iter.next()).rejects.toThrow("Connection closed");
  });
});

describe("P1-9: unsubscribe clears channelOffsets", () => {
  it("removes the offset entry when unsubscribing", async () => {
    const client = makeClient();
    (client as any).transport = {
      send: jest.fn().mockResolvedValue(undefined),
    };
    (client as any).isConnectedFlag = true;
    (client as any).channelOffsets.set("ch1", 100n);
    (client as any).subscribedChannels.add("ch1");

    await client.unsubscribe("ch1");

    expect((client as any).channelOffsets.has("ch1")).toBe(false);
    expect(client.getSubscribedChannels()).toEqual([]);
  });
});

describe("P1-G3: error envelope during connecting rejects waitForConnection", () => {
  it("fails the connect fast with code/type instead of a 30s timeout", async () => {
    const client = makeClient();
    (client as any).connectionState = "connecting";
    (client as any).transport = {
      send: jest.fn().mockResolvedValue(undefined),
    };

    const waitPromise = (client as any).waitForConnection();
    const errorEnvelope = create(OutboundMessageSchema, {
      envelope: {
        case: "error",
        value: { code: "AUTH_REQUIRED", type: "auth_error", message: "invalid token" },
      },
    });
    (client as any).handleMessage(errorEnvelope);

    let caught: any;
    try {
      await waitPromise;
    } catch (err) {
      caught = err;
    }
    expect(caught).toBeDefined();
    expect(caught.message).toBe("invalid token");
    expect(caught.code).toBe("AUTH_REQUIRED");
    expect(caught.type).toBe("auth_error");
  });
});

describe("P2-1: Connected.subscriptions is authoritative on reconnect", () => {
  it("drops local channels missing from the server list before resubscribing", async () => {
    const client = makeClient();
    const sent: any[] = [];
    (client as any).transport = {
      send: jest.fn(async (msg: any) => {
        sent.push(msg);
      }),
    };
    (client as any).isConnectedFlag = true;
    (client as any).connectionState = "reconnecting";
    (client as any).subscribedChannels = new Set(["keep", "gone"]);

    const connected = create(OutboundMessageSchema, {
      envelope: {
        case: "connected",
        value: {
          sessionId: "s2",
          epoch: "e2",
          resumed: false,
          subscriptions: [{ channel: "keep" }],
        },
      },
    });

    (client as any).handleMessage(connected);
    (client as any).stopPingLoop();

    expect(client.getSubscribedChannels()).toEqual(["keep"]);

    const subscribeMsg = sent.find((m) => m.envelope?.case === "subscribe");
    expect(subscribeMsg).toBeDefined();
    const channels = subscribeMsg.envelope.value.subscriptions.map(
      (s: any) => s.channel
    );
    expect(channels).toEqual(["keep"]);
  });

  it("clears the local set when the server returns an empty list", async () => {
    const client = makeClient();
    const send = jest.fn().mockResolvedValue(undefined);
    (client as any).transport = { send };
    (client as any).isConnectedFlag = true;
    (client as any).connectionState = "reconnecting";
    (client as any).subscribedChannels = new Set(["stale"]);

    const connected = create(OutboundMessageSchema, {
      envelope: {
        case: "connected",
        value: {
          sessionId: "s3",
          epoch: "e3",
          resumed: false,
          subscriptions: [],
        },
      },
    });

    (client as any).handleMessage(connected);
    (client as any).stopPingLoop();

    expect(client.getSubscribedChannels()).toEqual([]);
    expect(send).not.toHaveBeenCalled();
  });
});
