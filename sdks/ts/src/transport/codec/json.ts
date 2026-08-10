import { fromJson, toJson } from "@bufbuild/protobuf";
import { InboundMessage, InboundMessageSchema, OutboundMessage, OutboundMessageSchema } from "../../proto/client/v1/service_pb";
import type { Codec } from "./codec";

/**
 * JSON codec implementation.
 * Encodes/decodes messages using the canonical proto3 JSON mapping from
 * @bufbuild/protobuf, which matches the server-side wire format
 * (protojson with UseProtoNames: true, snake_case field names).
 */
export class JSONCodec implements Codec {
  name(): string {
    return "messageloop+json";
  }

  /**
   * JSON replacer function that handles BigInt serialization.
   * BigInt values are converted to strings to avoid JSON.stringify errors.
   */
  private static bigIntReplacer(_key: string, value: unknown): unknown {
    if (typeof value === "bigint") {
      return value.toString();
    }
    return value;
  }

  encode(msg: object): string {
    // Bufbuild messages carry $typeName; plain objects are serialized as-is
    // (legacy convenience) while messages use the canonical proto3 JSON
    // mapping so the output matches the server wire format.
    const isMessage = typeof (msg as { $typeName?: unknown }).$typeName === "string";
    const json = isMessage ? toJson(InboundMessageSchema, msg as InboundMessage) : msg;
    return JSON.stringify(json, JSONCodec.bigIntReplacer);
  }

  decode(data: Uint8Array | string): OutboundMessage {
    const text = typeof data === "string" ? data : new TextDecoder().decode(data);
    // ignoreUnknownFields mirrors the server's protojson DiscardUnknown
    // behavior, so forward-incompatible server payloads still decode.
    return fromJson(OutboundMessageSchema, JSON.parse(text), { ignoreUnknownFields: true });
  }

  useBytes(): boolean {
    return false;
  }
}

/**
 * Singleton instance of JSONCodec.
 */
export const jsonCodec: Codec = new JSONCodec();
