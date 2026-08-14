// PR-09 TS SDK: recover, presence, client survey, and pong for server ping.
//
// Go/TS comparison table (PR-09 spec §3):
// | 能力           | Go（PR-08）                              | TS（本 PR）                                            |
// | 恢复订阅       | SubscribeWith(ch, WithRecover(off, epoch)) | subscribe({ channel, recover: true, offset, epoch }) |
// | 恢复投递       | SubscribeAck.publications → OnMessage      | 同上 → onMessage / addMessageHandler                  |
// | Presence 事件  | OnPresence                                | onPresence                                            |
// | Presence 快照  | OnPresenceSnapshot                        | onPresenceSnapshot                                    |
// | Presence 查询  | Presence(ctx, ch)                         | presence(ch): Promise<PresenceSnapshot>               |
// | 发起 Survey    | Survey(ctx, ch, payload, timeout)         | survey(ch, payload, timeoutMs?): Promise<SurveyAnswer[]> |
// | 应答 Survey    | OnSurvey / OnSurveyRequest                | onSurvey / onSurveyRequest                            |
// | 服务端 Ping    | 同 id Inbound Pong + lastPong             | 同 id Pong + 清 pingTimeoutTimer                      |
// | user_id        | metadata.entries["user_id"]               | 同                                                    |

import { create } from "@bufbuild/protobuf";
import { OutboundMessageSchema } from "../src/proto/client/v1/service_pb";
import { MessageLoopClient } from "../src/client/client";
import { buildClientOptions } from "../src/client/options";
import { createTextMessage } from "../src/message";

function makeClient(): MessageLoopClient {
  return new (MessageLoopClient as any)(
    buildClientOptions([])
  ) as MessageLoopClient;
}

function connectedClient(): MessageLoopClient {
  const client = makeClient();
  const sent: any[] = [];
  (client as any).transport = {
    send: jest.fn(async (msg: any) => {
      sent.push(msg);
    }),
  };
  (client as any).isConnectedFlag = true;
  return client;
}

async function flush(): Promise<void> {
  for (let i = 0; i < 6; i++) {
    await Promise.resolve();
  }
}

describe("PR-09: recover", () => {
  it("subscribe with recover sends recover=true, offset, epoch", async () => {
    const client = connectedClient();
    const sent = (client as any).transport.send;

    await client.subscribe({
      channel: "ch1",
      recover: true,
      offset: 7n,
      epoch: "ep",
    });

    const sub = sent.mock.calls[0][0].envelope.value.subscriptions[0];
    expect(sub).toMatchObject({
      channel: "ch1",
      recover: true,
      offset: 7n,
      epoch: "ep",
    });
  });

  it("recover with 0n offset and empty epoch still sends recover=true", async () => {
    const client = connectedClient();
    const sent = (client as any).transport.send;

    await client.subscribe({ channel: "ch2", recover: true });

    const sub = sent.mock.calls[0][0].envelope.value.subscriptions[0];
    expect(sub).toMatchObject({
      channel: "ch2",
      recover: true,
      offset: 0n,
      epoch: "",
    });
  });

  it("plain string channels do not recover", async () => {
    const client = connectedClient();
    const sent = (client as any).transport.send;

    await client.subscribe("plain");

    const sub = sent.mock.calls[0][0].envelope.value.subscriptions[0];
    expect(sub).toMatchObject({ channel: "plain" });
    expect(sub.recover).toBe(false);
  });

  it("subscribeAck publications reach onMessage and track offsets", async () => {
    const client = connectedClient();
    const handler = jest.fn();
    client.onMessage(handler);

    const ack = create(OutboundMessageSchema, {
      envelope: {
        case: "subscribeAck",
        value: {
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
                    data: { case: "text", value: "recovered" },
                  },
                },
              ],
            },
          ],
          // Empty batch (offset 0) must not wipe a known position.
          recoverResults: [{ channel: "ch1", recovered: true, truncated: false, offset: 0n, epoch: "e" }],
        },
      },
    });
    (client as any).handleMessage(ack);

    expect(handler).toHaveBeenCalledTimes(1);
    expect(handler.mock.calls[0][0]).toEqual([
      expect.objectContaining({ id: "m1", channel: "ch1", offset: 42n }),
    ]);
    expect((client as any).channelOffsets.get("ch1")).toBe(42n);
    expect(client.getSubscribedChannels()).toEqual(["ch1"]);
  });
});

describe("PR-09: presence", () => {
  it("presence event reaches onPresence", () => {
    const client = connectedClient();
    const handler = jest.fn();
    client.onPresence(handler);

    const ev = create(OutboundMessageSchema, {
      envelope: {
        case: "presenceEvent",
        value: {
          channel: "ch1",
          action: "join",
          info: {
            sessionId: "s1",
            userId: "u1",
            clientId: "c1",
            connectedAt: 1234567890n,
          },
        },
      },
    });
    (client as any).handleMessage(ev);

    expect(handler).toHaveBeenCalledTimes(1);
    const event = handler.mock.calls[0][0];
    expect(event.channel).toBe("ch1");
    expect(event.action).toBe("join");
    expect(event.info.sessionId).toBe("s1");
    expect(event.info.userId).toBe("u1");
    expect(event.info.clientId).toBe("c1");
    expect(event.info.connectedAt).toBe(1234567890n);
  });

  it("presence snapshot on connected dispatches one onPresenceSnapshot", () => {
    const client = connectedClient();
    const snapHandler = jest.fn();
    client.onPresenceSnapshot(snapHandler);

    const connected = create(OutboundMessageSchema, {
      envelope: {
        case: "connected",
        value: {
          sessionId: "s1",
          epoch: "e1",
          resumed: true,
          subscriptions: [{ channel: "ch1" }],
          recoverResults: [
            { channel: "ch1", recovered: true, truncated: false, offset: 9n, epoch: "e1" },
          ],
          presence: [
            {
              channel: "ch1",
              clients: [{ sessionId: "s1", userId: "u1", clientId: "c1", connectedAt: 1n }],
              truncated: false,
              occupancy: 1,
            },
          ],
        },
      },
    });
    (client as any).handleMessage(connected);
    (client as any).stopPingLoop();

    expect(snapHandler).toHaveBeenCalledTimes(1);
    expect(snapHandler.mock.calls[0][0]).toMatchObject({
      channel: "ch1",
      occupancy: 1,
      truncated: false,
    });
    // Connected.recoverResults write back the cursor (Go parity).
    expect((client as any).channelOffsets.get("ch1")).toBe(9n);
  });

  it("presence snapshot on subscribeAck dispatches one onPresenceSnapshot", () => {
    const client = connectedClient();
    const snapHandler = jest.fn();
    client.onPresenceSnapshot(snapHandler);

    const ack = create(OutboundMessageSchema, {
      envelope: {
        case: "subscribeAck",
        value: {
          subscriptions: [{ channel: "ch1" }],
          presence: [
            {
              channel: "ch1",
              clients: [{ sessionId: "s1", userId: "u1", clientId: "c1", connectedAt: 1n }],
              truncated: false,
              occupancy: 1,
            },
          ],
        },
      },
    });
    (client as any).handleMessage(ack);

    expect(snapHandler).toHaveBeenCalledTimes(1);
    expect(snapHandler.mock.calls[0][0].channel).toBe("ch1");
  });

  it("presence query sends PresenceQuery and resolves the matching snapshot", async () => {
    const client = connectedClient();
    const snapHandler = jest.fn();
    client.onPresenceSnapshot(snapHandler);
    const sent = (client as any).transport.send;

    const p = client.presence("ch1");
    await flush();

    const query = sent.mock.calls[0][0];
    expect(query.envelope.case).toBe("presenceQuery");
    expect(query.envelope.value.channel).toBe("ch1");

    const reply = create(OutboundMessageSchema, {
      id: query.id,
      envelope: {
        case: "presence",
        value: {
          channel: "ch1",
          clients: [{ sessionId: "s1", userId: "u1", clientId: "c1", connectedAt: 1n }],
          truncated: false,
          occupancy: 1,
        },
      },
    });
    (client as any).handleMessage(reply);

    const snap = await p;
    expect(snap.channel).toBe("ch1");
    expect(snap.occupancy).toBe(1);
    expect(snap.clients[0]).toMatchObject({
      sessionId: "s1",
      userId: "u1",
      clientId: "c1",
    });
    // The query reply also fires onPresenceSnapshot.
    expect(snapHandler).toHaveBeenCalledTimes(1);
    expect(snapHandler.mock.calls[0][0].channel).toBe("ch1");
  });

  it("presence query denied rejects with the server error code", async () => {
    const client = connectedClient();
    const sent = (client as any).transport.send;

    const p = client.presence("ch1");
    await flush();

    const query = sent.mock.calls[0][0];
    const errorEnvelope = create(OutboundMessageSchema, {
      id: query.id,
      envelope: {
        case: "error",
        value: {
          code: "PERMISSION_DENIED",
          type: "presence_error",
          message: "no access",
        },
      },
    });
    (client as any).handleMessage(errorEnvelope);

    let caught: any;
    try {
      await p;
    } catch (err) {
      caught = err;
    }
    expect(caught).toBeDefined();
    expect(caught.message).toBe("no access");
    expect(caught.code).toBe("PERMISSION_DENIED");
    expect((client as any).pendingPresence.size).toBe(0);
  });

  it("presence rejects when not connected", async () => {
    const client = makeClient();
    await expect(client.presence("ch1")).rejects.toThrow("Not connected");
  });

  it("close rejects a pending presence query", async () => {
    const client = connectedClient();
    (client as any).transport.close = jest.fn().mockResolvedValue(undefined);

    const p = client.presence("ch1");
    await flush();
    await client.close();

    await expect(p).rejects.toThrow("Connection closed");
    expect((client as any).pendingPresence.size).toBe(0);
  });
});

describe("PR-09: survey", () => {
  it("survey round trip resolves SurveyAnswer with user_id from metadata", async () => {
    const client = connectedClient();
    const sent = (client as any).transport.send;

    const p = client.survey(
      "ch1",
      createTextMessage("survey.q", "who is there?"),
      5000
    );
    await flush();

    const req = sent.mock.calls[0][0];
    expect(req.envelope.case).toBe("surveyRequest");
    expect(req.envelope.value.channel).toBe("ch1");
    expect(req.envelope.value.requestId).toBeTruthy();
    expect(req.envelope.value.timeoutMs).toBe(5000);

    const result = create(OutboundMessageSchema, {
      envelope: {
        case: "surveyResult",
        value: {
          requestId: req.envelope.value.requestId,
          channel: "ch1",
          answers: [
            {
              sessionId: "s-1",
              payload: {
                contentType: "text/plain",
                data: { case: "text", value: "me" },
              },
              metadata: { entries: { user_id: "u-42" } },
            },
          ],
        },
      },
    });
    (client as any).handleMessage(result);

    const answers = await p;
    expect(answers).toHaveLength(1);
    expect(answers[0]).toMatchObject({ sessionId: "s-1", userId: "u-42" });
    expect((answers[0].payload as any).data).toMatchObject({
      type: "text",
      text: "me",
    });
    expect((client as any).pendingSurvey.size).toBe(0);
  });

  it("survey top-level error with matching id rejects fast", async () => {
    const client = connectedClient();
    const sent = (client as any).transport.send;

    const p = client.survey("ch1", createTextMessage("survey.q", "q"));
    await flush();

    const req = sent.mock.calls[0][0];
    const errorEnvelope = create(OutboundMessageSchema, {
      id: req.id,
      envelope: {
        case: "error",
        value: {
          code: "SURVEY_DISABLED",
          type: "survey_error",
          message: "surveys disabled",
        },
      },
    });
    (client as any).handleMessage(errorEnvelope);

    let caught: any;
    try {
      await p;
    } catch (err) {
      caught = err;
    }
    expect(caught).toBeDefined();
    expect(caught.message).toBe("surveys disabled");
    expect(caught.code).toBe("SURVEY_DISABLED");
    expect((client as any).pendingSurvey.size).toBe(0);
  });

  it("survey no-id reject code with exactly one in-flight rejects that survey", async () => {
    const client = connectedClient();

    const p = client.survey("ch1", createTextMessage("survey.q", "q"));
    await flush();
    expect((client as any).pendingSurvey.size).toBe(1);

    const errorEnvelope = create(OutboundMessageSchema, {
      // Worker failures may omit the inbound id.
      envelope: {
        case: "error",
        value: {
          code: "SURVEY_TOO_MANY_SUBSCRIBERS",
          type: "survey_error",
          message: "too many subscribers",
        },
      },
    });
    (client as any).handleMessage(errorEnvelope);

    let caught: any;
    try {
      await p;
    } catch (err) {
      caught = err;
    }
    expect(caught).toBeDefined();
    expect(caught.code).toBe("SURVEY_TOO_MANY_SUBSCRIBERS");
    expect((client as any).pendingSurvey.size).toBe(0);
  });

  it("survey rejects when not connected", async () => {
    const client = makeClient();
    await expect(client.survey("ch1", null)).rejects.toThrow("Not connected");
  });

  it("close rejects a pending survey and drops late results", async () => {
    const client = connectedClient();
    (client as any).transport.close = jest.fn().mockResolvedValue(undefined);

    const p = client.survey("ch1", createTextMessage("survey.q", "q"));
    await flush();
    await client.close();

    await expect(p).rejects.toThrow("Connection closed");
    expect((client as any).pendingSurvey.size).toBe(0);

    // A SurveyResult arriving after close is dropped without error.
    const late = create(OutboundMessageSchema, {
      envelope: {
        case: "surveyResult",
        value: { requestId: "gone", channel: "ch1", answers: [] },
      },
    });
    expect(() => (client as any).handleMessage(late)).not.toThrow();
  });

  it("onSurvey compat: only the old handler still answers with a SurveyReply", async () => {
    const client = connectedClient();
    const sent = (client as any).transport.send;

    client.onSurvey(async (requestId, request) => {
      expect(requestId).toBe("req-1");
      return createTextMessage("survey.answer", `answered: ${(request.data as any).text}`);
    });

    const req = create(OutboundMessageSchema, {
      envelope: {
        case: "surveyRequest",
        value: {
          requestId: "req-1",
          channel: "chX",
          payload: {
            contentType: "text/plain",
            data: { case: "text", value: "ping?" },
          },
        },
      },
    });
    (client as any).handleMessage(req);
    await flush();

    const reply = sent.mock.calls.find(
      (c: any[]) => c[0].envelope?.case === "surveyReply"
    );
    expect(reply).toBeDefined();
    expect(reply[0].envelope.value.requestId).toBe("req-1");
    expect(reply[0].envelope.value.payload).toMatchObject({
      contentType: "text/plain",
      data: { case: "text", value: "answered: ping?" },
    });
  });

  it("onSurveyRequest receives the outbound channel and replies with the right id", async () => {
    const client = connectedClient();
    const sent = (client as any).transport.send;

    let seenChannel = "";
    client.onSurveyRequest(async (requestId, channel, request) => {
      seenChannel = channel;
      expect(requestId).toBe("req-2");
      return createTextMessage("survey.answer", (request.data as any).text);
    });

    const req = create(OutboundMessageSchema, {
      envelope: {
        case: "surveyRequest",
        value: {
          requestId: "req-2",
          channel: "chX",
          payload: {
            contentType: "text/plain",
            data: { case: "text", value: "hello" },
          },
        },
      },
    });
    (client as any).handleMessage(req);
    await flush();

    expect(seenChannel).toBe("chX");
    const reply = sent.mock.calls.find(
      (c: any[]) => c[0].envelope?.case === "surveyReply"
    );
    expect(reply).toBeDefined();
    expect(reply[0].envelope.value.requestId).toBe("req-2");
    expect(reply[0].envelope.value.payload).toMatchObject({
      contentType: "text/plain",
      data: { case: "text", value: "hello" },
    });
  });
});

describe("PR-09: server ping", () => {
  it("outbound Ping is answered with an Inbound Pong carrying the same id", async () => {
    const client = connectedClient();
    const sent = (client as any).transport.send;

    const ping = create(OutboundMessageSchema, {
      id: "server-ping-1",
      envelope: { case: "ping", value: {} },
    });
    (client as any).handleMessage(ping);
    await flush();

    const pong = sent.mock.calls.find((c: any[]) => c[0].envelope?.case === "pong");
    expect(pong).toBeDefined();
    expect(pong[0].envelope.case).toBe("pong");
    expect(pong[0].id).toBe("server-ping-1");
  });

  it("outbound Ping clears the client's own pong timeout", () => {
    const client = connectedClient();
    const sent = (client as any).transport.send;
    (client as any).pingTimeoutTimer = setTimeout(() => {}, 10000);

    const ping = create(OutboundMessageSchema, {
      id: "server-ping-2",
      envelope: { case: "ping", value: {} },
    });
    (client as any).handleMessage(ping);

    expect((client as any).pingTimeoutTimer).toBeNull();
    expect(
      sent.mock.calls.some((c: any[]) => c[0].envelope?.case === "pong")
    ).toBe(true);
  });
});

describe("PR-09: resubscribe after non-resumed reconnect", () => {
  it("resubscribeAllChannels sends recover=true with stored offset and epoch", async () => {
    const client = connectedClient();
    const sent = (client as any).transport.send;
    (client as any).connectionState = "reconnecting";
    (client as any).subscribedChannels = new Map([["ch1", "tok1"]]);
    (client as any).channelOffsets.set("ch1", 42n);

    const connected = create(OutboundMessageSchema, {
      envelope: {
        case: "connected",
        value: {
          sessionId: "s",
          epoch: "e1",
          resumed: false,
          subscriptions: [{ channel: "ch1" }],
        },
      },
    });
    (client as any).handleMessage(connected);
    (client as any).stopPingLoop();

    const subscribeMsg = sent.mock.calls.find(
      (c: any[]) => c[0].envelope?.case === "subscribe"
    );
    expect(subscribeMsg).toBeDefined();
    expect(subscribeMsg[0].envelope.value.subscriptions).toEqual([
      expect.objectContaining({
        channel: "ch1",
        token: "tok1",
        recover: true,
        offset: 42n,
        epoch: "e1",
      }),
    ]);
  });
});
