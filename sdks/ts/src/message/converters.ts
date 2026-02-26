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

import type { InboundMessage, OutboundMessage, Message as ProtoMessage, Publication, Publish, RpcRequest, RpcReply } from "../proto/v1/service_pb";
import type { Payload, Metadata } from "../proto/shared/v1/types_pb";

import {
  Message,
  Data,
  createMessage,
  createJSONMessage,
  createBinaryMessage,
  createTextMessage,
  createData,
  messageToPayload,
  payloadToMessage,
  ReceivedMessage,
} from "./message";

// Re-export message types
export type {
  Message,
  Data,
  createMessage,
  createJSONMessage,
  createBinaryMessage,
  createTextMessage,
  createData,
  messageToPayload,
  payloadToMessage,
  ReceivedMessage,
};

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
  msg: Message
): InboundMessage {
  const payload = messageToPayload(msg);
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
  msg: Message
): InboundMessage {
  const payload = messageToPayload(msg);
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
 * Extract messages from a Publication.
 */
export function extractMessages(publication: Publication): ProtoMessage[] {
  return publication.messages || [];
}

/**
 * Convert a proto Message to ReceivedMessage.
 */
export function messageToReceived(msg: ProtoMessage): ReceivedMessage {
  const emptyPayload = create(PayloadSchema, {
    contentType: "",
    data: { case: "binary", value: new Uint8Array(0) },
  });
  return {
    id: msg.id,
    channel: msg.channel,
    offset: msg.offset,
    message: payloadToMessage(msg.payload ?? emptyPayload, msg.id),
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
export type { InboundMessage, OutboundMessage, Message as ProtoMessage, Publication, Publish, RpcRequest, RpcReply } from "../proto/v1/service_pb";
export type { Payload, Metadata } from "../proto/shared/v1/types_pb";
