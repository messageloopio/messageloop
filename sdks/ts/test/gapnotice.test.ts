// C6 TS SDK: catch-up GapNotice is a first-class envelope — the converter
// maps it, the client dispatches it to onGapNotice, and it never reaches the
// "Unknown message type" error path nor the channel cursor.

import { create } from "@bufbuild/protobuf";
import { OutboundMessageSchema } from "../src/proto/client/v2/service_pb";
import { GapReason } from "../src/proto/shared/v2/types_pb";
import { MessageLoopClient } from "../src/client/client";
import { buildClientOptions } from "../src/client/options";
import {
  gapNoticeFromPB,
  parseOutboundMessage,
} from "../src/message/converters";

function makeClient(): MessageLoopClient {
  return new (MessageLoopClient as any)(
    buildClientOptions([])
  ) as MessageLoopClient;
}

function connectedClient(): MessageLoopClient {
  const client = makeClient();
  (client as any).transport = {
    send: jest.fn(async (_msg: any) => {}),
  };
  (client as any).isConnectedFlag = true;
  return client;
}

describe("C6: gap notice", () => {
  it("parseOutboundMessage maps gapNotice instead of the unknown-type error", () => {
    const msg = create(OutboundMessageSchema, {
      envelope: {
        case: "gapNotice",
        value: {
          channel: "ch1",
          gapReason: GapReason.MIDDLE,
          position: { streamEpoch: "ep", offset: 41n },
        },
      },
    });

    const parsed = parseOutboundMessage(msg);
    expect(parsed.type).toBe("gapNotice");
    expect(parsed.data.channel).toBe("ch1");
  });

  it("gapNoticeFromPB maps the notice; an unset offset stays undefined", () => {
    const msg = create(OutboundMessageSchema, {
      envelope: {
        case: "gapNotice",
        value: {
          channel: "ch1",
          gapReason: GapReason.REPLAY_TRUNCATED,
          position: { streamEpoch: "ep" },
        },
      },
    });
    const parsed = parseOutboundMessage(msg);
    const notice = gapNoticeFromPB(parsed.data);
    expect(notice.channel).toBe("ch1");
    expect(notice.gapReason).toBe(GapReason.REPLAY_TRUNCATED);
    expect(notice.streamEpoch).toBe("ep");
    expect(notice.offset).toBeUndefined();
  });

  it("gap notice reaches onGapNotice, never onError, and never touches the cursor", () => {
    const client = connectedClient();
    const handler = jest.fn();
    const errorHandler = jest.fn();
    client.onGapNotice(handler);
    client.onError(errorHandler);

    // Seed a cursor the notice must not touch.
    (client as any).channelOffsets.set("ch1", 42n);

    const msg = create(OutboundMessageSchema, {
      envelope: {
        case: "gapNotice",
        value: {
          channel: "ch1",
          gapReason: GapReason.MIDDLE,
          position: { streamEpoch: "ep", offset: 41n },
        },
      },
    });
    (client as any).handleMessage(msg);

    expect(handler).toHaveBeenCalledTimes(1);
    const notice = handler.mock.calls[0][0];
    expect(notice.channel).toBe("ch1");
    expect(notice.gapReason).toBe(GapReason.MIDDLE);
    expect(notice.streamEpoch).toBe("ep");
    expect(notice.offset).toBe(41n);
    expect(errorHandler).not.toHaveBeenCalled();
    expect((client as any).channelOffsets.get("ch1")).toBe(42n);
  });

  it("gap notice without a handler is silently ignored (no error callback)", () => {
    const client = connectedClient();
    const errorHandler = jest.fn();
    client.onError(errorHandler);

    const msg = create(OutboundMessageSchema, {
      envelope: {
        case: "gapNotice",
        value: { channel: "ch1", gapReason: GapReason.REPLAY_TRUNCATED },
      },
    });
    (client as any).handleMessage(msg);

    expect(errorHandler).not.toHaveBeenCalled();
  });
});
