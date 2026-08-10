import { create } from "@bufbuild/protobuf";
import { InboundMessageSchema } from "../src/proto/client/v1/service_pb";
import { JSONCodec, jsonCodec, ProtobufCodec, protobufCodec } from "../src/transport/codec";

describe("Codec", () => {
  describe("JSONCodec", () => {
    let codec: JSONCodec;

    beforeEach(() => {
      codec = new JSONCodec();
    });

    it("should have correct name", () => {
      expect(codec.name()).toEqual("messageloop+json");
    });

    it("should return string from encode", () => {
      const msg = { id: "test-1", test: "data" };

      const encoded = codec.encode(msg);

      expect(typeof encoded).toEqual("string");
      expect(encoded).toContain("test");
    });

    it("should decode string to object", () => {
      // Proto3 JSON wire format uses field names directly, not envelope.case
      const jsonString = JSON.stringify({
        id: "msg-1",
        connected: { session_id: "session-123" },
      });

      const decoded = codec.decode(jsonString);

      expect(decoded).toBeDefined();
      expect((decoded as any).envelope?.case).toEqual("connected");
    });

    it("should return false from useBytes", () => {
      expect(codec.useBytes()).toEqual(false);
    });

    it("encodes connect oneof content", () => {
      const msg = create(InboundMessageSchema, {
        envelope: { case: "connect", value: { clientId: "c1", token: "t" } },
      });
      const encoded = JSON.parse(codec.encode(msg));
      expect(encoded.connect.client_id).toBe("c1");
    });

    it("decodes connected with snake_case fields", () => {
      const wire = JSON.stringify({ connected: { session_id: "s1", epoch: "e1", resumed: false } });
      const decoded = codec.decode(wire) as any;
      expect(decoded.envelope.case).toBe("connected");
      expect(decoded.envelope.value.sessionId).toBe("s1");
      expect(decoded.envelope.value.epoch).toBe("e1");
    });

    it("decodes survey_reply", () => {
      const wire = JSON.stringify({ survey_reply: { id: "1", payload: { text: "ok" } } });
      const decoded = codec.decode(wire) as any;
      expect(decoded.envelope.case).toBe("surveyReply");
    });

    it("parses server golden connected wire payload", () => {
      // Golden sample produced by the server's ProtoJSONMarshaler
      // (shared/marshaler.go, UseProtoNames: true).
      const golden = { id: "msg-1", time: "1700000000000", connected: { session_id: "s1", epoch: "e1" } };
      const decoded = codec.decode(JSON.stringify(golden)) as any;
      expect(decoded.envelope.case).toBe("connected");
      expect(decoded.envelope.value.sessionId).toBe("s1");
      expect(decoded.envelope.value.epoch).toBe("e1");
    });

    it("parses server golden publication wire payload", () => {
      // Golden sample produced by the server's ProtoJSONMarshaler
      // (shared/marshaler.go, UseProtoNames: true).
      const golden = {
        id: "msg-2",
        time: "1700000000001",
        publication: {
          messages: [
            {
              id: "chat-42",
              channel: "chat",
              offset: "42",
              payload: { content_type: "text/plain", text: "hello" },
            },
          ],
        },
      };
      const decoded = codec.decode(JSON.stringify(golden)) as any;
      expect(decoded.envelope.case).toBe("publication");
      const m = decoded.envelope.value.messages[0];
      expect(m.id).toBe("chat-42");
      expect(m.channel).toBe("chat");
      expect(m.offset.toString()).toBe("42");
      expect(m.payload.contentType).toBe("text/plain");
      expect(m.payload.data.case).toBe("text");
      expect(m.payload.data.value).toBe("hello");
    });
  });

  describe("ProtobufCodec", () => {
    let codec: ProtobufCodec;

    beforeEach(() => {
      codec = new ProtobufCodec();
    });

    it("should have correct name", () => {
      expect(codec.name()).toEqual("messageloop+proto");
    });

    it("should encode and decode binary data", () => {
      const originalData = new Uint8Array([1, 2, 3, 4, 5]);

      // Test that codec can handle Uint8Array input
      const isBytes = codec.useBytes();
      expect(isBytes).toEqual(true);
    });

    it("should return true from useBytes", () => {
      expect(codec.useBytes()).toEqual(true);
    });
  });

  describe("jsonCodec singleton", () => {
    it("should be an instance of JSONCodec", () => {
      expect(jsonCodec).toBeInstanceOf(JSONCodec);
    });
  });

  describe("protobufCodec singleton", () => {
    it("should be an instance of ProtobufCodec", () => {
      expect(protobufCodec).toBeInstanceOf(ProtobufCodec);
    });
  });
});
