import { create } from "@bufbuild/protobuf";
import { PayloadSchema } from "../proto/shared/v1/types_pb";
import type { Payload } from "../proto/shared/v1/types_pb";

/**
 * Data type discriminator
 */
export type DataType = "json" | "binary" | "text";

/**
 * Message data structure
 */
export interface Data {
  /** MIME content type */
  contentType: string;
  /** Data type */
  type: DataType;
  /** JSON data (when type is "json") */
  json?: Record<string, any>;
  /** Binary data (when type is "binary") */
  binary?: Uint8Array;
  /** Text data (when type is "text") */
  text?: string;
}

/**
 * Message structure
 */
export interface Message {
  /** Unique message identifier */
  id: string;
  /** Message type */
  type: string;
  /** Message data */
  data: Data;
  /** Metadata key-value pairs */
  metadata?: Record<string, string>;
}

/**
 * Type guard for JSON data
 */
export function isJSONData(data: Data): data is Data & { json: Record<string, any> } {
  return data.type === "json" && data.json !== undefined;
}

/**
 * Type guard for binary data
 */
export function isBinaryData(data: Data): data is Data & { binary: Uint8Array } {
  return data.type === "binary" && data.binary !== undefined;
}

/**
 * Type guard for text data
 */
export function isTextData(data: Data): data is Data & { text: string } {
  return data.type === "text" && data.text !== undefined;
}

/**
 * Create a new message with auto-generated ID.
 */
export function createMessage(type: string, data: Data): Message {
  return {
    id: crypto.randomUUID(),
    type,
    data,
    metadata: {},
  };
}

/**
 * Create a message with JSON data.
 */
export function createJSONMessage(type: string, json: Record<string, any>, contentType: string = "application/json"): Message {
  return createMessage(type, {
    contentType,
    type: "json",
    json,
  });
}

/**
 * Create a message with binary data.
 */
export function createBinaryMessage(type: string, binary: Uint8Array, contentType: string = "application/octet-stream"): Message {
  return createMessage(type, {
    contentType,
    type: "binary",
    binary,
  });
}

/**
 * Create a message with text data.
 */
export function createTextMessage(type: string, text: string, contentType: string = "text/plain"): Message {
  return createMessage(type, {
    contentType,
    type: "text",
    text,
  });
}

/**
 * Create data from content type and value.
 * Automatically detects the data type based on content type and value type.
 */
export function createData(contentType: string, value: unknown): Data {
  const ct = contentType.toLowerCase();

  if (ct.includes("json") || ct.includes("application/json")) {
    if (typeof value === "object" && value !== null && !ArrayBuffer.isView(value)) {
      return {
        contentType,
        type: "json",
        json: value as Record<string, any>,
      };
    }
    // Try to parse as JSON
    if (typeof value === "string") {
      try {
        return {
          contentType,
          type: "json",
          json: JSON.parse(value),
        };
      } catch {
        // Fall through to text
      }
    }
  }

  if (ct.startsWith("text/")) {
    if (typeof value === "string") {
      return {
        contentType,
        type: "text",
        text: value,
      };
    }
    if (value instanceof Uint8Array) {
      return {
        contentType,
        type: "text",
        text: new TextDecoder().decode(value),
      };
    }
  }

  // Default to binary
  if (value instanceof Uint8Array) {
    return {
      contentType,
      type: "binary",
      binary: value,
    };
  }
  if (typeof value === "string") {
    return {
      contentType,
      type: "binary",
      binary: new TextEncoder().encode(value),
    };
  }

  // Fallback: try to JSON stringify
  return {
    contentType,
    type: "json",
    json: { value },
  };
}

/**
 * Decode message data as a specific type.
 * For JSON data, returns the JSON object.
 * For binary data, tries to decode as JSON first, then as text.
 * For text data, tries to parse as JSON first, then returns the string.
 */
export function dataAs<T = unknown>(msg: Message): T {
  const data = msg.data;

  switch (data.type) {
    case "json":
      return data.json as T;
    case "binary":
      if (data.binary) {
        try {
          const text = new TextDecoder().decode(data.binary);
          return JSON.parse(text) as T;
        } catch {
          return data.binary as unknown as T;
        }
      }
      break;
    case "text":
      if (data.text) {
        try {
          return JSON.parse(data.text) as T;
        } catch {
          return data.text as unknown as T;
        }
      }
      break;
  }

  return undefined as T;
}

/**
 * Convert Message to protobuf Payload.
 */
export function messageToPayload(msg: Message): Payload {
  const data = msg.data;

  switch (data.type) {
    case "json":
      return create(PayloadSchema, {
        contentType: data.contentType,
        data: { case: "json", value: data.json! },
      });
    case "binary":
      return create(PayloadSchema, {
        contentType: data.contentType,
        data: { case: "binary", value: data.binary! },
      });
    case "text":
      return create(PayloadSchema, {
        contentType: data.contentType,
        data: { case: "text", value: data.text! },
      });
    default:
      return create(PayloadSchema, {
        contentType: data.contentType,
        data: { case: "binary", value: new Uint8Array(0) },
      });
  }
}

/**
 * Convert protobuf Payload to Message.
 */
export function payloadToMessage(payload: Payload, id: string, type: string = "messageloop.message"): Message {
  if (!payload) {
    return createMessage(type, { contentType: "", type: "binary" });
  }

  const contentType = payload.contentType || "";

  switch (payload.data.case) {
    case "json":
      return {
        id,
        type,
        data: {
          contentType: contentType || "application/json",
          type: "json",
          json: payload.data.value,
        },
      };
    case "binary":
      return {
        id,
        type,
        data: {
          contentType: contentType || "application/octet-stream",
          type: "binary",
          binary: payload.data.value,
        },
      };
    case "text":
      return {
        id,
        type,
        data: {
          contentType: contentType || "text/plain",
          type: "text",
          text: payload.data.value,
        },
      };
    default:
      return {
        id,
        type,
        data: {
          contentType,
          type: "binary",
          binary: new Uint8Array(0),
        },
      };
  }
}

/**
 * Received message from a subscribed channel.
 */
export interface ReceivedMessage {
  /** Unique message identifier */
  id: string;
  /** Channel the message was published to */
  channel: string;
  /** Message offset in the channel */
  offset: bigint;
  /** The decoded message */
  message: Message;
}

/**
 * Convert protobuf Message to ReceivedMessage.
 */
export function protoToReceivedMessage(msg: {
  id: string;
  channel: string;
  offset: bigint;
  payload?: Payload;
}): ReceivedMessage {
  return {
    id: msg.id,
    channel: msg.channel,
    offset: msg.offset,
    message: payloadToMessage(msg.payload || null as any, msg.id),
  };
}
