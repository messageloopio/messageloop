import type { OutboundMessage } from "../../proto/v1/service_pb";
import type { Codec } from "./codec";
import { OutboundMessageSchema } from "../../proto/v1/service_pb";

/**
 * JSON codec implementation.
 * Encodes/decodes messages as JSON strings.
 */
export class JSONCodec implements Codec {
  name(): string {
    return "messageloop+json";
  }

  encode(msg: object): string {
    // Use toJson() if available (protobuf v2 messages have this method)
    if ("toJson" in msg && typeof msg.toJson === "function") {
      return JSON.stringify((msg as any).toJson());
    }
    return JSON.stringify(msg);
  }

  decode(data: Uint8Array | string): OutboundMessage {
    const json = typeof data === "string" ? JSON.parse(data) : JSON.parse(new TextDecoder().decode(data));
    // Return as unknown type - actual parsing would be done by the caller
    // using parseOutboundMessage from converters
    return json as unknown as OutboundMessage;
  }

  useBytes(): boolean {
    return false;
  }
}

/**
 * Singleton instance of JSONCodec.
 */
export const jsonCodec = new JSONCodec();
