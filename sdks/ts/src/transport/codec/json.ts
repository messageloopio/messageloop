import type { OutboundMessage } from "../../proto/client/v1/service_pb";
import type { Codec } from "./codec";
import { OutboundMessageSchema } from "../../proto/client/v1/service_pb";

/**
 * Mapping from proto3 JSON field names to bufbuild protobuf case names (for decoding).
 * Proto3 JSON uses snake_case (e.g., "subscribe_ack") while bufbuild uses camelCase (e.g., "subscribeAck").
 */
const JSON_FIELD_TO_CASE: Record<string, string> = {
  error: "error",
  connected: "connected",
  subscribe_ack: "subscribeAck",
  unsubscribe_ack: "unsubscribeAck",
  rpc_reply: "rpcReply",
  pong: "pong",
  publish_ack: "publishAck",
  publication: "publication",
  sub_refresh_ack: "subRefreshAck",
  survey_request: "surveyRequest",
  survey_response: "surveyResponse",
};

/**
 * Mapping from bufbuild case names to proto3 JSON field names (for encoding).
 */
const CASE_TO_JSON_FIELD: Record<string, string> = {
  connect: "connect",
  subscribe: "subscribe",
  unsubscribe: "unsubscribe",
  publish: "publish",
  rpc_request: "rpcRequest",
  sub_refresh: "subRefresh",
  ping: "ping",
};

/**
 * Transform proto3 JSON format to bufbuild protobuf format (for decoding).
 * Proto3 JSON: {"id": "xxx", "pong": {}}
 * Bufbuild: {"id": "xxx", "envelope": {"case": "pong", "value": {}}}
 */
function transformOutboundMessage(json: Record<string, unknown>): OutboundMessage {
  const result: any = {
    id: json.id,
    metadata: json.metadata,
    time: json.time,
    envelope: { case: undefined, value: undefined },
  };

  // Find which oneof field is present
  for (const [fieldName, caseName] of Object.entries(JSON_FIELD_TO_CASE)) {
    if (fieldName in json && json[fieldName] !== undefined) {
      result.envelope = {
        case: caseName,
        value: json[fieldName],
      };
      break;
    }
  }

  return result as OutboundMessage;
}

/**
 * Transform bufbuild protobuf format to proto3 JSON format (for encoding).
 * Bufbuild: {"id": "xxx", "envelope": {"case": "connect", "value": {...}}}
 * Proto3 JSON: {"id": "xxx", "connect": {...}}
 */
function transformInboundMessage(msg: Record<string, unknown>): Record<string, unknown> {
  const result: Record<string, unknown> = {};

  // Copy standard fields
  if (msg.id !== undefined) result.id = msg.id;
  if (msg.channel !== undefined) result.channel = msg.channel;
  if (msg.method !== undefined) result.method = msg.method;
  if (msg.metadata !== undefined) result.metadata = msg.metadata;
  if (msg.time !== undefined) result.time = msg.time;

  // Transform envelope to proto3 JSON format
  const envelope = msg.envelope as { case?: string; value?: unknown } | undefined;
  if (envelope && envelope.case && envelope.value !== undefined) {
    const jsonField = CASE_TO_JSON_FIELD[envelope.case] || envelope.case;
    // Recursively transform nested objects (remove $typeName, transform nested envelopes)
    result[jsonField] = deepTransform(envelope.value);
  }

  return result;
}

/**
 * Deep transform an object: remove $typeName fields and transform nested envelopes.
 */
function deepTransform(obj: unknown): unknown {
  if (obj === null || obj === undefined) {
    return obj;
  }

  if (Array.isArray(obj)) {
    return obj.map(deepTransform);
  }

  if (typeof obj !== "object") {
    return obj;
  }

  const result: Record<string, unknown> = {};

  for (const [key, value] of Object.entries(obj as Record<string, unknown>)) {
    // Skip $typeName fields
    if (key === "$typeName") {
      continue;
    }

    // Transform envelope fields
    if (key === "envelope" && value && typeof value === "object") {
      const envelope = value as { case?: string; value?: unknown };
      if (envelope.case && envelope.value !== undefined) {
        const jsonField = CASE_TO_JSON_FIELD[envelope.case] || envelope.case;
        result[jsonField] = deepTransform(envelope.value);
      }
      continue;
    }

    // Recursively transform nested objects
    result[key] = deepTransform(value);
  }

  return result;
}

/**
 * JSON codec implementation.
 * Encodes/decodes messages as JSON strings.
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
    // Convert to JSON-serializable object first
    let jsonObj: Record<string, unknown>;

    // Use toJson() if available (protobuf v2 messages have this method)
    if ("toJson" in msg && typeof msg.toJson === "function") {
      jsonObj = (msg as any).toJson();
    } else {
      jsonObj = msg as Record<string, unknown>;
    }

    // Transform from bufbuild format to proto3 JSON format
    const proto3Json = transformInboundMessage(jsonObj);

    return JSON.stringify(proto3Json, JSONCodec.bigIntReplacer);
  }

  decode(data: Uint8Array | string): OutboundMessage {
    const json = typeof data === "string" ? JSON.parse(data) : JSON.parse(new TextDecoder().decode(data));
    return transformOutboundMessage(json);
  }

  useBytes(): boolean {
    return false;
  }
}

/**
 * Singleton instance of JSONCodec.
 */
export const jsonCodec = new JSONCodec();
