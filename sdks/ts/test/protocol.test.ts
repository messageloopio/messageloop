import { create } from "@bufbuild/protobuf";
import { OutboundMessageSchema } from "../src/proto/client/v1/service_pb";
import { MessageLoopClient } from "../src/client/client";
import { buildClientOptions } from "../src/client/options";
import { createTextMessage } from "../src/message";

function makeClient(): MessageLoopClient {
  return new (MessageLoopClient as any)(buildClientOptions([])) as MessageLoopClient;
}

async function flush(): Promise<void> {
  for (let i = 0; i < 6; i++) {
    await Promise.resolve();
  }
}

describe("B3-TS: subscription-level token", () => {
  it("subscribe sends per-channel tokens and records them", async () => {
    const client = makeClient();
    const sent: any[] = [];
    (client as any).transport = {
      send: jest.fn(async (msg: any) => {
        sent.push(msg);
      }),
    };
    (client as any).isConnectedFlag = true;

    await client.subscribe("plain", { channel: "tokened", token: "t1" });

    const sub = sent[sent.length - 1];
    expect(sub.envelope.case).toBe("subscribe");
    const subs = sub.envelope.value.subscriptions;
    expect(subs).toHaveLength(2);
    expect(subs[0]).toMatchObject({ channel: "plain", token: "" });
    expect(subs[1]).toMatchObject({ channel: "tokened", token: "t1" });
    expect(client.getSubscribedChannels()).toEqual(["plain", "tokened"]);
  });

  it("unsubscribe accepts channel specs and clears bookkeeping", async () => {
    const client = makeClient();
    const sent: any[] = [];
    (client as any).transport = {
      send: jest.fn(async (msg: any) => {
        sent.push(msg);
      }),
    };
    (client as any).isConnectedFlag = true;

    await client.subscribe({ channel: "ch1", token: "t1" });
    await client.unsubscribe({ channel: "ch1" });

    const unsub = sent[sent.length - 1];
    expect(unsub.envelope.case).toBe("unsubscribe");
    expect(unsub.envelope.value.subscriptions).toEqual([
      expect.objectContaining({ channel: "ch1" }),
    ]);
    expect(client.getSubscribedChannels()).toEqual([]);
  });

  it("connect() carries per-channel tokens", async () => {
    const client = makeClient();
    const sent: any[] = [];
    (client as any).transport = {
      send: jest.fn(async (msg: any) => {
        sent.push(msg);
      }),
    };
    (client as any).isConnectedFlag = true;
    (client as any).subscribedChannels = new Map([
      ["ch1", "tok1"],
      ["ch2", ""],
    ]);

    await (client as any).connect();

    const connect = sent[sent.length - 1];
    const subs = connect.envelope.value.subscriptions;
    expect(subs).toHaveLength(2);
    expect(subs[0]).toMatchObject({ channel: "ch1", token: "tok1" });
    expect(subs[1]).toMatchObject({ channel: "ch2", token: "" });
  });

  it("resubscribe after reconnect preserves tokens", async () => {
    const client = makeClient();
    const sent: any[] = [];
    (client as any).transport = {
      send: jest.fn(async (msg: any) => {
        sent.push(msg);
      }),
    };
    (client as any).isConnectedFlag = true;
    (client as any).connectionState = "reconnecting";
    (client as any).subscribedChannels = new Map([["ch1", "tok1"]]);

    const connected = create(OutboundMessageSchema, {
      envelope: {
        case: "connected",
        value: {
          sessionId: "s",
          epoch: "e",
          resumed: false,
          subscriptions: [{ channel: "ch1" }],
        },
      },
    });
    (client as any).handleMessage(connected);
    (client as any).stopPingLoop();

    const subscribeMsg = sent.find((m) => m.envelope?.case === "subscribe");
    expect(subscribeMsg).toBeDefined();
    expect(subscribeMsg.envelope.value.subscriptions).toEqual([
      expect.objectContaining({ channel: "ch1", token: "tok1" }),
    ]);
  });
});

describe("B3-TS: publishWithAck", () => {
  it("resolves with id and offset on publishAck", async () => {
    const client = makeClient();
    let sentId = "";
    (client as any).transport = {
      send: jest.fn(async (msg: any) => {
        sentId = msg.id;
      }),
    };
    (client as any).isConnectedFlag = true;

    const p = client.publishWithAck("ch1", createTextMessage("t", "hi"));
    await flush();
    expect(sentId).not.toBe("");

    const ack = create(OutboundMessageSchema, {
      id: sentId,
      envelope: { case: "publishAck", value: { id: sentId, offset: 42n } },
    });
    (client as any).handleMessage(ack);

    await expect(p).resolves.toEqual({ id: sentId, offset: 42n });
    expect((client as any).pendingPublish.size).toBe(0);
  });

  it("passes transient flag to the wire message", async () => {
    const client = makeClient();
    const sent: any[] = [];
    (client as any).transport = {
      send: jest.fn(async (msg: any) => {
        sent.push(msg);
      }),
    };
    (client as any).isConnectedFlag = true;

    const p = client.publishWithAck("ch1", createTextMessage("t", "hi"), {
      transient: true,
      timeout: 10000,
    });
    await flush();

    expect(sent[0].envelope.value.transient).toBe(true);
    const ack = create(OutboundMessageSchema, {
      id: sent[0].id,
      envelope: { case: "publishAck", value: { id: sent[0].id, offset: 1n } },
    });
    (client as any).handleMessage(ack);
    await expect(p).resolves.toEqual({ id: sent[0].id, offset: 1n });
  });

  it("rejects on timeout and cleans up the pending entry", async () => {
    jest.useFakeTimers();
    const client = makeClient();
    (client as any).transport = {
      send: jest.fn().mockResolvedValue(undefined),
    };
    (client as any).isConnectedFlag = true;

    const p = client.publishWithAck("ch1", createTextMessage("t", "hi"), {
      timeout: 100,
    });
    await flush();
    jest.advanceTimersByTime(100);

    await expect(p).rejects.toThrow(/ack timeout/i);
    expect((client as any).pendingPublish.size).toBe(0);

    jest.useRealTimers();
  });

  it("rejects pending publishes on close()", async () => {
    const client = makeClient();
    (client as any).transport = {
      send: jest.fn().mockResolvedValue(undefined),
      close: jest.fn().mockResolvedValue(undefined),
    };
    (client as any).isConnectedFlag = true;

    const p = client.publishWithAck("ch1", createTextMessage("t", "hi"), {
      timeout: 60000,
    });
    await flush();

    await client.close();
    await expect(p).rejects.toThrow("Connection closed");
    expect((client as any).pendingPublish.size).toBe(0);
  });

  it("rejects pending publishes on unexpected disconnect", async () => {
    const client = makeClient();
    (client as any).transport = {
      send: jest.fn().mockResolvedValue(undefined),
    };
    (client as any).isConnectedFlag = true;
    (client as any).autoReconnectEnabled = false;

    const p = client.publishWithAck("ch1", createTextMessage("t", "hi"), {
      timeout: 60000,
    });
    await flush();

    (client as any).handleDisconnect();
    await expect(p).rejects.toThrow("Connection closed");
    expect((client as any).pendingPublish.size).toBe(0);
  });
});

describe("B3-TS: survey", () => {
  it("echoes the request payload when no handler is set", async () => {
    const client = makeClient();
    const sent: any[] = [];
    (client as any).transport = {
      send: jest.fn(async (msg: any) => {
        sent.push(msg);
      }),
    };
    (client as any).isConnectedFlag = true;

    const req = create(OutboundMessageSchema, {
      envelope: {
        case: "surveyRequest",
        value: {
          requestId: "req-1",
          payload: {
            contentType: "text/plain",
            data: { case: "text", value: "ping?" },
          },
        },
      },
    });
    (client as any).handleMessage(req);
    await flush();

    const reply = sent.find((m) => m.envelope?.case === "surveyReply");
    expect(reply).toBeDefined();
    expect(reply.envelope.value.requestId).toBe("req-1");
    expect(reply.envelope.value.payload).toMatchObject({
      contentType: "text/plain",
      data: { case: "text", value: "ping?" },
    });
  });

  it("sends the custom handler reply with request_id", async () => {
    const client = makeClient();
    const sent: any[] = [];
    (client as any).transport = {
      send: jest.fn(async (msg: any) => {
        sent.push(msg);
      }),
    };
    (client as any).isConnectedFlag = true;

    let seenRequestId = "";
    client.onSurvey(async (requestId, request) => {
      seenRequestId = requestId;
      return createTextMessage("survey.answer", `answered: ${(request.data as any).text}`);
    });

    const req = create(OutboundMessageSchema, {
      envelope: {
        case: "surveyRequest",
        value: {
          requestId: "req-1",
          payload: {
            contentType: "text/plain",
            data: { case: "text", value: "ping?" },
          },
        },
      },
    });
    (client as any).handleMessage(req);
    await flush();

    expect(seenRequestId).toBe("req-1");
    const reply = sent.find((m) => m.envelope?.case === "surveyReply");
    expect(reply).toBeDefined();
    expect(reply.envelope.value.requestId).toBe("req-1");
    expect(reply.envelope.value.payload).toMatchObject({
      contentType: "text/plain",
      data: { case: "text", value: "answered: ping?" },
    });
    expect(reply.envelope.value.error).toBeUndefined();
  });

  it("sends an error reply when the handler throws", async () => {
    const client = makeClient();
    const sent: any[] = [];
    (client as any).transport = {
      send: jest.fn(async (msg: any) => {
        sent.push(msg);
      }),
    };
    (client as any).isConnectedFlag = true;

    client.onSurvey(() => {
      throw new Error("boom");
    });

    const req = create(OutboundMessageSchema, {
      envelope: {
        case: "surveyRequest",
        value: { requestId: "req-1" },
      },
    });
    (client as any).handleMessage(req);
    await flush();

    const reply = sent.find((m) => m.envelope?.case === "surveyReply");
    expect(reply).toBeDefined();
    expect(reply.envelope.value.requestId).toBe("req-1");
    expect(reply.envelope.value.payload).toBeUndefined();
    expect(reply.envelope.value.error).toMatchObject({
      code: "SURVEY_REPLY_ERROR",
      type: "survey_error",
      message: "boom",
    });
  });
});

describe("B3-TS: subRefresh", () => {
  it("sends a subRefresh envelope with the channels", async () => {
    const client = makeClient();
    const sent: any[] = [];
    (client as any).transport = {
      send: jest.fn(async (msg: any) => {
        sent.push(msg);
      }),
    };
    (client as any).isConnectedFlag = true;

    await client.subRefresh("ch1", "ch2");

    expect(sent).toHaveLength(1);
    expect(sent[0].envelope.case).toBe("subRefresh");
    expect(sent[0].envelope.value.channels).toEqual(["ch1", "ch2"]);
  });
});
