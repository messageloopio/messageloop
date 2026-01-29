import {
  generateMessageId,
  createConnectMessage,
  createSubscribeMessage,
  createUnsubscribeMessage,
  createPublishMessage,
  createRPCRequestMessage,
  createPingMessage,
  createSubRefreshMessage,
  cloudEventToProto,
} from "../src/event/converters";
import { createCloudEvent } from "../src/event/event";
import { CloudEvent } from "cloudevents";

describe("Event Converters", () => {
  describe("generateMessageId", () => {
    it("should generate unique IDs", () => {
      const id1 = generateMessageId();
      const id2 = generateMessageId();
      expect(id1).not.toEqual(id2);
    });

    it("should include timestamp and counter", () => {
      const id = generateMessageId();
      const [timestamp, counter] = id.split("-");
      expect(timestamp).toBeDefined();
      expect(counter).toBeDefined();
    });
  });

  describe("createConnectMessage", () => {
    it("should create a connect message", () => {
      const msg = createConnectMessage("client-1", "sdk", "token", "1.0.0", []);

      // Protobuf v2: access envelope via case/value
      expect(msg.envelope.case).toEqual("connect");
      expect(msg.envelope.value.clientId).toEqual("client-1");
      expect(msg.envelope.value.clientType).toEqual("sdk");
      expect(msg.envelope.value.token).toEqual("token");
      expect(msg.envelope.value.version).toEqual("1.0.0");
      expect(msg.id).toBeDefined();
    });

    it("should include auto-subscribe channels", () => {
      const msg = createConnectMessage("client-1", "sdk", "token", "1.0.0", [
        { channel: "chat.general", ephemeral: false, token: "" },
      ]);

      const subscriptions = msg.envelope.value.subscriptions;
      expect(subscriptions).toHaveLength(1);
      expect(subscriptions[0].channel).toEqual("chat.general");
    });
  });

  describe("createSubscribeMessage", () => {
    it("should create a subscribe message", () => {
      const msg = createSubscribeMessage(["chat.general", "chat.dev"]);

      expect(msg.envelope.case).toEqual("subscribe");
      expect(msg.envelope.value.subscriptions.map((s: any) => s.channel)).toContain("chat.general");
      expect(msg.envelope.value.subscriptions.map((s: any) => s.channel)).toContain("chat.dev");
      expect(msg.id).toBeDefined();
    });

    it("should support ephemeral subscriptions", () => {
      const msg = createSubscribeMessage(["temp.channel"], true);
      expect(msg.envelope.value.subscriptions[0].ephemeral).toBe(true);
    });
  });

  describe("createUnsubscribeMessage", () => {
    it("should create an unsubscribe message", () => {
      const msg = createUnsubscribeMessage(["chat.general"]);

      expect(msg.envelope.case).toEqual("unsubscribe");
      expect(msg.envelope.value.subscriptions.map((s: any) => s.channel)).toContain("chat.general");
      expect(msg.id).toBeDefined();
    });
  });

  describe("createPingMessage", () => {
    it("should create a ping message", () => {
      const msg = createPingMessage();
      expect(msg.envelope.case).toEqual("ping");
      expect(msg.id).toBeDefined();
    });
  });

  describe("createSubRefreshMessage", () => {
    it("should create a sub refresh message", () => {
      const msg = createSubRefreshMessage([
        { channel: "ch1", token: "t1" },
      ]);
      expect(msg.envelope.case).toEqual("subRefresh");
      expect(msg.envelope.value.channels).toContain("ch1");
    });
  });
});

describe("CloudEvent Creation", () => {
  it("should create a CloudEvent with required fields", () => {
    const event = createCloudEvent({
      source: "/test",
      type: "test.event",
      data: { key: "value" },
    });

    expect(event.source).toEqual("/test");
    expect(event.type).toEqual("test.event");
    expect(event.data).toEqual({ key: "value" });
  });

  it("should create a CloudEvent with all options", () => {
    const event = createCloudEvent({
      id: "custom-id",
      source: "/test",
      type: "test.event",
      data: { key: "value" },
      datacontenttype: "application/json",
      subject: "test-subject",
      time: new Date("2024-01-01T00:00:00Z"),
      extensions: { ext1: "value1" },
    });

    expect(event.id).toEqual("custom-id");
    expect(event.source).toEqual("/test");
    expect(event.type).toEqual("test.event");
    expect(event.datacontenttype).toEqual("application/json");
    expect(event.subject).toEqual("test-subject");
    expect(event.extensions).toEqual({ ext1: "value1" });
  });
});

describe("CloudEvent Conversion", () => {
  it("should convert CloudEvent SDK to protobuf", () => {
    const event = new CloudEvent({
      id: "event-1",
      source: "/test",
      type: "test.event",
      data: { key: "value" },
    });

    const proto = cloudEventToProto(event);

    expect(proto.id).toEqual("event-1");
    expect(proto.source).toEqual("/test");
    expect(proto.type).toEqual("test.event");
    expect(proto.data.case).toEqual("textData");
  });

  it("should convert binary data CloudEvent", () => {
    const binaryData = new Uint8Array([1, 2, 3, 4]);
    const event = new CloudEvent({
      id: "event-2",
      source: "/test",
      type: "binary.event",
      data: binaryData,
    });

    const proto = cloudEventToProto(event);

    expect(proto.data.case).toEqual("binaryData");
    expect(proto.data.value).toEqual(binaryData);
  });
});
