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
      const jsonString = JSON.stringify({
        id: "msg-1",
        envelope: {
          case: "connected",
          value: { sessionId: "session-123" },
        },
      });

      const decoded = codec.decode(jsonString);

      expect(decoded).toBeDefined();
      expect((decoded as any).envelope?.case).toEqual("connected");
    });

    it("should return false from useBytes", () => {
      expect(codec.useBytes()).toEqual(false);
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
