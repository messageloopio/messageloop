import type { CloudEvent } from "../proto/includes/cloudevents/cloudevents_pb";
import { CloudEventSchema } from "../proto/includes/cloudevents/cloudevents_pb";
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
} from "../proto/v1/service_pb";

import type { CloudEventAttributeValue } from "../proto/includes/cloudevents/cloudevents_pb";
import { CloudEventAttributeValueSchema } from "../proto/includes/cloudevents/cloudevents_pb";

import type { InboundMessage, OutboundMessage, Message, Publication } from "../proto/v1/service_pb";

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
 * Create a CloudEventAttributeValue with string value.
 */
function stringAttr(value: string): CloudEventAttributeValue {
  return create(CloudEventAttributeValueSchema, {
    attr: { case: "ceString", value },
  });
}

/**
 * Create a CloudEventAttributeValue with boolean value.
 */
function booleanAttr(value: boolean): CloudEventAttributeValue {
  return create(CloudEventAttributeValueSchema, {
    attr: { case: "ceBoolean", value },
  });
}

/**
 * Create a CloudEventAttributeValue with integer value.
 */
function integerAttr(value: number): CloudEventAttributeValue {
  return create(CloudEventAttributeValueSchema, {
    attr: { case: "ceInteger", value },
  });
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
  const protoEvent = cloudEventToProto(event);
  return create(InboundMessageSchema, {
    id: generateMessageId(),
    channel,
    envelope: { case: "publish", value: protoEvent },
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
  const protoEvent = cloudEventToProto(event);
  return create(InboundMessageSchema, {
    id: generateMessageId(),
    channel,
    method,
    envelope: { case: "rpcRequest", value: protoEvent },
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
 * Convert CloudEvent SDK to protobuf format.
 */
export function cloudEventToProto(event: CloudEventSDK): CloudEvent {
  const attributes: Record<string, CloudEventAttributeValue> = {};

  const eventId = event.id || "";
  const eventSource = event.source || "";
  const eventSpecVersion: string = (event.specVersion as string) || "";
  const eventType: string = (event.type as string) || "";
  const eventSubject: string = (event.subject as string) || "";
  const eventDataContentType: string = (event.datacontenttype as string) || "";

  if (event.id && typeof event.id === "string") attributes["id"] = stringAttr(event.id);
  if (event.source && typeof event.source === "string") attributes["source"] = stringAttr(event.source);
  if (event.specVersion && typeof event.specVersion === "string") attributes["specversion"] = stringAttr(event.specVersion);
  if (event.type && typeof event.type === "string") attributes["type"] = stringAttr(event.type);
  if (event.subject && typeof event.subject === "string") attributes["subject"] = stringAttr(event.subject);
  if (event.datacontenttype && typeof event.datacontenttype === "string") attributes["datacontenttype"] = stringAttr(event.datacontenttype);

  for (const [key, value] of Object.entries(event.extensions || {})) {
    if (typeof value === "string") {
      attributes[key] = stringAttr(value);
    } else if (typeof value === "number") {
      attributes[key] = integerAttr(Math.round(value));
    } else if (typeof value === "boolean") {
      attributes[key] = booleanAttr(value);
    }
  }

  let data: { case: "binaryData"; value: Uint8Array } | { case: "textData"; value: string } | { case: undefined };

  // Check if data is Uint8Array using type guard
  const dataValue = event.data;
  if (isUint8Array(dataValue)) {
    data = { case: "binaryData", value: dataValue };
  } else if (typeof dataValue === "string") {
    data = { case: "textData", value: dataValue };
  } else if (dataValue) {
    data = { case: "textData", value: JSON.stringify(dataValue) };
  } else {
    data = { case: undefined };
  }

  return create(CloudEventSchema, {
    id: eventId || crypto.randomUUID(),
    source: eventSource,
    specVersion: eventSpecVersion,
    type: eventType,
    attributes,
    data,
  });
}

/**
 * Type guard for Uint8Array.
 */
function isUint8Array(value: unknown): value is Uint8Array {
  return value instanceof Uint8Array;
}

/**
 * Convert protobuf CloudEvent to SDK format.
 */
export function protoToCloudEvent(event: CloudEvent): CloudEventSDKClass {
  const extensions: Record<string, string | number | boolean> = {};

  for (const [key, value] of Object.entries(event.attributes || {})) {
    // Skip reserved attributes that should be on the top-level
    if (key === "specversion" || key === "type" || key === "source" ||
        key === "id" || key === "subject" || key === "datacontenttype") {
      continue;
    }
    if (value.attr.case === "ceString") {
      extensions[key] = value.attr.value;
    } else if (value.attr.case === "ceInteger") {
      extensions[key] = value.attr.value;
    } else if (value.attr.case === "ceBoolean") {
      extensions[key] = value.attr.value;
    }
  }

  let data: any;
  if (event.data.case === "binaryData") {
    data = event.data.value;
  } else if (event.data.case === "textData") {
    const text = event.data.value;
    try {
      data = JSON.parse(text);
    } catch {
      data = text;
    }
  } else if (event.data.case === "protoData") {
    data = event.data.value;
  }

  return new CloudEventSDKClass({
    id: event.id,
    source: event.source,
    specversion: event.specVersion, // Note: CloudEvents SDK uses lowercase 'specversion'
    type: event.type,
    data,
    extensions: Object.keys(extensions).length > 0 ? extensions : undefined,
  });
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
  return {
    id: msg.id,
    channel: msg.channel,
    offset: msg.offset,
    event: protoToCloudEvent(msg.payload!),
  };
}

/**
 * Parse an outbound message and extract relevant data.
 */
export function parseOutboundMessage(
  msg: OutboundMessage
): {
  type: "connected" | "error" | "subscribeAck" | "unsubscribeAck" | "publishAck" | "publication" | "rpcReply" | "pong" | "subRefreshAck";
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
  }

  return { type: "error", data: new Error("Unknown message type"), id };
}

// Re-export types that might be needed
export type { CloudEvent };
export { create };
export type { InboundMessage, OutboundMessage, Message, Publication };
