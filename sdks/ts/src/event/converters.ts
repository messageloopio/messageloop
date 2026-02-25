import type { CloudEvent as CloudEventSDK } from "cloudevents";
import { CloudEvent as CloudEventSDKClass } from "cloudevents";
import { create } from "@bufbuild/protobuf";

// Import Schema constants for create function
import {
  InboundMessageSchema,
  ConnectSchema,
  SubscribeSchema,
  UnsubscribeSchema,
  SubscriptionSchema,
  SubRefreshSchema,
  PingSchema,
  PublishSchema,
  RpcRequestSchema,
} from "../proto/v1/service_pb";

import { PayloadSchema, MetadataSchema } from "../proto/shared/v1/types_pb";

import type { InboundMessage, OutboundMessage, Message, Publication, Publish, RpcRequest, RpcReply } from "../proto/v1/service_pb";
import type { Payload, Metadata } from "../proto/shared/v1/types_pb";

/**
 * Generate a unique message ID.
 * Format: "{unix_nanoseconds}-{counter}"
 */
let counter = 0;
export function generateMessageId(): string {
  const now = Date.now() * 1_000_000;
  counter = (counter + 1) % 10000;
  return `${now}-${counter}`;
}

/**
 * Create an InboundMessage with Connect envelope.
 */
export function createConnectMessage(
  clientId: string,
  clientType: string,
  token: string,
  version: string,
  autoSubscribe: { channel: string; ephemeral: boolean; token: string }[]
): InboundMessage {
  const connect = create(ConnectSchema, {
    clientId,
    clientType,
    token,
    version,
    subscriptions: autoSubscribe.map((sub) =>
      create(SubscriptionSchema, {
        channel: sub.channel,
        ephemeral: sub.ephemeral,
        token: sub.token,
      })
    ),
  });

  return create(InboundMessageSchema, {
    id: generateMessageId(),
    envelope: { case: "connect", value: connect },
  });
}

/**
 * Create an InboundMessage with Subscribe envelope.
 */
export function createSubscribeMessage(
  channels: string[],
  ephemeral: boolean = false
): InboundMessage {
  const subscribe = create(SubscribeSchema, {
    subscriptions: channels.map((ch) =>
      create(SubscriptionSchema, {
        channel: ch,
        ephemeral,
        token: "",
      })
    ),
  });

  return create(InboundMessageSchema, {
    id: generateMessageId(),
    envelope: { case: "subscribe", value: subscribe },
  });
}

/**
 * Create an InboundMessage with Unsubscribe envelope.
 */
export function createUnsubscribeMessage(channels: string[]): InboundMessage {
  const unsubscribe = create(UnsubscribeSchema, {
    subscriptions: channels.map((ch) =>
      create(SubscriptionSchema, {
        channel: ch,
        ephemeral: false,
        token: "",
      })
    ),
  });

  return create(InboundMessageSchema, {
    id: generateMessageId(),
    envelope: { case: "unsubscribe", value: unsubscribe },
  });
}

/**
 * Create an InboundMessage with Publish envelope.
 */
export function createPublishMessage(
  channel: string,
  event: CloudEventSDK
): InboundMessage {
  const payload = cloudEventToPayload(event);
  const publish = create(PublishSchema, {
    channel,
    payload,
  });
  return create(InboundMessageSchema, {
    id: generateMessageId(),
    envelope: { case: "publish", value: publish },
  });
}

/**
 * Create an InboundMessage with RPC Request envelope.
 */
export function createRPCRequestMessage(
  channel: string,
  method: string,
  event: CloudEventSDK
): InboundMessage {
  const payload = cloudEventToPayload(event);
  const rpcRequest = create(RpcRequestSchema, {
    channel,
    method,
    payload,
  });
  return create(InboundMessageSchema, {
    id: generateMessageId(),
    envelope: { case: "rpcRequest", value: rpcRequest },
  });
}

/**
 * Create an InboundMessage with Ping envelope.
 */
export function createPingMessage(): InboundMessage {
  return create(InboundMessageSchema, {
    id: generateMessageId(),
    envelope: { case: "ping", value: create(PingSchema, {}) },
  });
}

/**
 * Create an InboundMessage with SubRefresh envelope.
 */
export function createSubRefreshMessage(
  subscriptions: { channel: string; token: string }[]
): InboundMessage {
  const subRefresh = create(SubRefreshSchema, {
    channels: subscriptions.map((sub) => sub.channel),
  });

  return create(InboundMessageSchema, {
    id: generateMessageId(),
    envelope: { case: "subRefresh", value: subRefresh },
  });
}

/**
 * Convert CloudEvent SDK to Payload protobuf format.
 */
export function cloudEventToPayload(event: CloudEventSDK): Payload {
  const dataValue = event.data;
  const contentType = event.datacontenttype || "";

  // Check if data is Uint8Array using type guard
  if (isUint8Array(dataValue)) {
    return create(PayloadSchema, {
      contentType,
      data: { case: "binary", value: dataValue },
    });
  } else if (typeof dataValue === "string") {
    // Check if content type indicates text
    if (contentType.startsWith("text/")) {
      return create(PayloadSchema, {
        contentType,
        data: { case: "text", value: dataValue },
      });
    }
    // Try to parse as JSON first
    try {
      const jsonData = JSON.parse(dataValue);
      return create(PayloadSchema, {
        contentType,
        data: { case: "json", value: jsonData },
      });
    } catch {
      // If not valid JSON, store as text
      return create(PayloadSchema, {
        contentType,
        data: { case: "text", value: dataValue },
      });
    }
  } else if (dataValue && typeof dataValue === "object") {
    // JSON object
    return create(PayloadSchema, {
      contentType,
      data: { case: "json", value: dataValue as Record<string, any> },
    });
  }

  // Default: empty binary
  return create(PayloadSchema, {
    contentType,
    data: { case: "binary", value: new Uint8Array(0) },
  });
}

/**
 * Type guard for Uint8Array.
 */
function isUint8Array(value: unknown): value is Uint8Array {
  return value instanceof Uint8Array;
}

/**
 * Convert Payload protobuf to CloudEvent SDK format.
 */
export function payloadToCloudEvent(payload: Payload, source: string = "messageloop"): CloudEventSDKClass {
  let data: unknown;
  let dataContentType: string | undefined;

  if (payload.data.case === "json") {
    data = payload.data.value;
    dataContentType = payload.contentType || "application/json";
  } else if (payload.data.case === "text") {
    data = payload.data.value;
    dataContentType = payload.contentType || "text/plain";
  } else if (payload.data.case === "binary") {
    data = payload.data.value;
    dataContentType = payload.contentType || "application/octet-stream";
  } else {
    data = undefined;
  }

  const eventOptions = {
    id: crypto.randomUUID(),
    source,
    specversion: "1.0",
    type: "messageloop.message",
    data,
    datacontenttype: dataContentType,
  };

  return new CloudEventSDKClass(eventOptions) as CloudEventSDKClass;
}

/**
 * Extract messages from a Publication.
 */
export function extractMessages(publication: Publication): Message[] {
  return publication.messages || [];
}

/**
 * Received message type
 */
export interface ReceivedMessage {
  id: string;
  channel: string;
  offset: bigint;
  event: CloudEventSDKClass;
}

/**
 * Convert a Message proto to ReceivedMessage.
 */
export function messageToReceived(msg: Message): ReceivedMessage {
  const payload = msg.payload;
  return {
    id: msg.id,
    channel: msg.channel,
    offset: msg.offset,
    event: payload ? payloadToCloudEvent(payload, msg.channel) : new CloudEventSDKClass({ id: msg.id, source: msg.channel, type: "messageloop.message" }),
  };
}

/**
 * Parse an outbound message and extract relevant data.
 */
export function parseOutboundMessage(
  msg: OutboundMessage
): {
  type: "connected" | "error" | "subscribeAck" | "unsubscribeAck" | "publishAck" | "publication" | "rpcReply" | "pong" | "subRefreshAck" | "surveyRequest" | "surveyReply";
  data: any;
  id: string;
} {
  const envelope = msg.envelope;
  const id = msg.id;

  if (envelope.case === "connected") {
    return { type: "connected", data: envelope.value, id };
  } else if (envelope.case === "error") {
    return { type: "error", data: envelope.value, id };
  } else if (envelope.case === "subscribeAck") {
    return { type: "subscribeAck", data: envelope.value, id };
  } else if (envelope.case === "unsubscribeAck") {
    return { type: "unsubscribeAck", data: envelope.value, id };
  } else if (envelope.case === "publishAck") {
    return { type: "publishAck", data: envelope.value, id };
  } else if (envelope.case === "publication") {
    return { type: "publication", data: envelope.value, id };
  } else if (envelope.case === "rpcReply") {
    return { type: "rpcReply", data: envelope.value, id };
  } else if (envelope.case === "pong") {
    return { type: "pong", data: envelope.value, id };
  } else if (envelope.case === "subRefreshAck") {
    return { type: "subRefreshAck", data: envelope.value, id };
  } else if (envelope.case === "surveyRequest") {
    return { type: "surveyRequest", data: envelope.value, id };
  } else if (envelope.case === "surveyReply") {
    return { type: "surveyReply", data: envelope.value, id };
  }

  return { type: "error", data: new Error("Unknown message type"), id };
}

/**
 * Extract payload and error from RpcReply.
 */
export function extractRpcReply(reply: RpcReply): {
  requestId: string;
  payload: Payload | undefined;
  error: { code: string; message: string } | undefined;
} {
  return {
    requestId: reply.requestId,
    payload: reply.payload,
    error: reply.error ? { code: reply.error.code, message: reply.error.message } : undefined,
  };
}

// Re-export types that might be needed
export { create };
export type { InboundMessage, OutboundMessage, Message, Publication, Publish, RpcRequest, RpcReply };
export type { Payload, Metadata };
