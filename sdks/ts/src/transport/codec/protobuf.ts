import type { OutboundMessage } from "../../proto/v1/service_pb";
import type { Codec } from "./codec";
import { OutboundMessageSchema } from "../../proto/v1/service_pb";

/**
 * Protobuf codec implementation.
 * Encodes/decodes messages as binary protobuf.
 */
export class ProtobufCodec implements Codec {
  name(): string {
    return "messageloop+proto";
  }

  encode(msg: object): Uint8Array {
    // Use toBinary() if available (protobuf v2 messages have this method)
    if ("toBinary" in msg && typeof msg.toBinary === "function") {
      return (msg as any).toBinary();
    }
    throw new Error("Message does not support binary serialization");
  }

  decode(data: Uint8Array | string): OutboundMessage {
    const bytes = data instanceof Uint8Array ? data : new TextEncoder().encode(data);
    // Use fromBinary() with Schema for protobuf v2
    // Cast to any because TypeScript doesn't recognize the method on GenMessage type
    return (OutboundMessageSchema as any).fromBinary(bytes) as unknown as OutboundMessage;
  }

  useBytes(): boolean {
    return true;
  }
}

/**
 * Singleton instance of ProtobufCodec.
 */
export const protobufCodec = new ProtobufCodec();
