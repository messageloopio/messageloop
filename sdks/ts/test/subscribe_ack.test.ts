// Subscription contract: subscribe()/unsubscribe() resolve only after the
// server's SubscribeAck / UnsubscribeAck (echoing the request id) arrives;
// subscribeAsync()/unsubscribeAsync() keep the best-effort fire-and-forget
// semantics. Mirrors the Go SDK tests in sdks/go/subscribe_ack_test.go.

import { create } from "@bufbuild/protobuf";
import { OutboundMessageSchema } from "../src/proto/client/v2/service_pb";
import { MessageLoopClient } from "../src/client/client";
import { buildClientOptions, setRPCTimeout } from "../src/client/options";

function makeClient(options: any[] = []): MessageLoopClient {
  return new (MessageLoopClient as any)(
    buildClientOptions(options)
  ) as MessageLoopClient;
}

function connectedClient(options: any[] = []): {
  client: MessageLoopClient;
  send: jest.Mock;
} {
  const client = makeClient(options);
  const send = jest.fn().mockResolvedValue(undefined);
  (client as any).transport = { send };
  (client as any).isConnectedFlag = true;
  return { client, send };
}

async function flush(): Promise<void> {
  for (let i = 0; i < 6; i++) {
    await Promise.resolve();
  }
}

function subscribeAck(id: string, channels: string[]): any {
  return create(OutboundMessageSchema, {
    id,
    envelope: {
      case: "subscribeAck",
      value: { subscriptions: channels.map((channel) => ({ channel })) },
    },
  });
}

function unsubscribeAck(id: string, channels: string[]): any {
  return create(OutboundMessageSchema, {
    id,
    envelope: {
      case: "unsubscribeAck",
      value: { subscriptions: channels.map((channel) => ({ channel })) },
    },
  });
}

describe("Subscription contract: subscribe waits for SubscribeAck", () => {
  it("does not resolve when merely sent; resolves after the matching ack", async () => {
    const { client, send } = connectedClient();

    let settled = false;
    const promise = client.subscribe("chat.room1").then(() => {
      settled = true;
    });

    // The Subscribe message went out, but the caller must still be waiting.
    expect(send).toHaveBeenCalledTimes(1);
    const sentId = send.mock.calls[0][0].id;
    expect(sentId).not.toBe("");
    await flush();
    expect(settled).toBe(false);
    expect(client.getSubscribedChannels()).toEqual([]);

    // The ack echoing the request id completes the call, and the local
    // subscription bookkeeping is written from the ack (Go parity).
    (client as any).handleMessage(
      subscribeAck(sentId, ["chat.room1"])
    );
    await promise;
    expect(settled).toBe(true);
    expect(client.getSubscribedChannels()).toEqual(["chat.room1"]);
  });

  it("rejects with the ack timeout when the ack never arrives", async () => {
    const { client } = connectedClient([setRPCTimeout(30)]);

    await expect(client.subscribe("chat.room1")).rejects.toThrow(
      "Subscribe ack timeout after 30ms"
    );
  });

  it("fails fast when the server rejects with a matching error envelope", async () => {
    const { client, send } = connectedClient();

    const promise = client.subscribe("chat.room1");
    await flush();
    const sentId = send.mock.calls[0][0].id;

    (client as any).handleMessage(
      create(OutboundMessageSchema, {
        id: sentId,
        envelope: {
          case: "error",
          value: { code: "PERMISSION_DENIED", type: "acl_error", message: "nope" },
        },
      })
    );

    await expect(promise).rejects.toMatchObject({
      message: "nope",
      code: "PERMISSION_DENIED",
    });
  });

  it("rejects in-flight subscribes on disconnect", async () => {
    const { client, send } = connectedClient();
    client.disableAutoReconnect();

    const promise = client.subscribe("chat.room1");
    await flush();
    expect(send).toHaveBeenCalledTimes(1);

    (client as any).isConnectedFlag = false;
    (client as any).connectionState = "connected";
    (client as any).handleDisconnect();

    await expect(promise).rejects.toThrow("Connection closed");
    expect((client as any).pendingSubAck.size).toBe(0);
  });
});

describe("Subscription contract: subscribeAsync is best-effort", () => {
  it("resolves immediately without any ack and records the channels", async () => {
    const { client, send } = connectedClient();

    let settled = false;
    const promise = client.subscribeAsync("chat.room1").then(() => {
      settled = true;
    });
    await flush();

    expect(settled).toBe(true);
    expect(send).toHaveBeenCalledTimes(1);
    expect((client as any).pendingSubAck.size).toBe(0);
    expect(client.getSubscribedChannels()).toEqual(["chat.room1"]);
    await promise;
  });
});

describe("Subscription contract: unsubscribe waits for UnsubscribeAck", () => {
  it("does not resolve when merely sent; ack clears bookkeeping", async () => {
    const { client, send } = connectedClient();
    (client as any).subscribedChannels = new Map([["chat.room1", ""]]);
    (client as any).channelOffsets.set("chat.room1", 100n);

    let settled = false;
    const promise = client.unsubscribe("chat.room1").then(() => {
      settled = true;
    });

    expect(send).toHaveBeenCalledTimes(1);
    const sentId = send.mock.calls[0][0].id;
    await flush();
    expect(settled).toBe(false);
    // Bookkeeping is only dropped once the server confirmed the removal.
    expect(client.getSubscribedChannels()).toEqual(["chat.room1"]);

    (client as any).handleMessage(unsubscribeAck(sentId, ["chat.room1"]));
    await promise;
    expect(settled).toBe(true);
    expect(client.getSubscribedChannels()).toEqual([]);
    expect((client as any).channelOffsets.has("chat.room1")).toBe(false);
  });

  it("rejects with the ack timeout when the ack never arrives", async () => {
    const { client } = connectedClient([setRPCTimeout(30)]);

    await expect(client.unsubscribe("chat.room1")).rejects.toThrow(
      "Unsubscribe ack timeout after 30ms"
    );
  });
});

describe("Subscription contract: unsubscribeAsync is best-effort", () => {
  it("resolves immediately without any ack and clears bookkeeping", async () => {
    const { client, send } = connectedClient();
    (client as any).subscribedChannels = new Map([["chat.room1", ""]]);
    (client as any).channelOffsets.set("chat.room1", 100n);

    let settled = false;
    const promise = client.unsubscribeAsync("chat.room1").then(() => {
      settled = true;
    });
    await flush();

    expect(settled).toBe(true);
    expect(send).toHaveBeenCalledTimes(1);
    expect((client as any).pendingSubAck.size).toBe(0);
    expect(client.getSubscribedChannels()).toEqual([]);
    expect((client as any).channelOffsets.has("chat.room1")).toBe(false);
    await promise;
  });
});
