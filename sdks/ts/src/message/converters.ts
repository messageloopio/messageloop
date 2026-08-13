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
  SurveyReplySchema,
} from "../proto/client/v1/service_pb";

import { PayloadSchema, MetadataSchema } from "../proto/shared/v1/types_pb";
import { ErrorSchema } from "../proto/shared/v1/errors_pb";

import type { InboundMessage, OutboundMessage, Message as ProtoMessage, Publication, Publish, RpcRequest, RpcReply } from "../proto/client/v1/service_pb";
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
  autoSubscribe: { channel: string; ephemeral: boolean; token: string; recover?: boolean; offset?: bigint; epoch?: string }[],
  sessionId?: string
): InboundMessage {
  const connect = create(ConnectSchema, {
    clientId,
    clientType,
    token,
    version,
    sessionId: sessionId || "",
    subscriptions: autoSubscribe.map((sub) =>
      create(SubscriptionSchema, {
        channel: sub.channel,
        ephemeral: sub.ephemeral,
        token: sub.token,
        recover: sub.recover || false,
        offset: sub.offset || BigInt(0),
        epoch: sub.epoch || "",
      })
    ),
  });

  return create(InboundMessageSchema, {
    id: generateMessageId(),
    envelope: { case: "connect", value: connect },
  });
}

/**
 * Per-channel subscription spec: a plain channel name or a channel with an
 * optional subscription token (used for subscription-level authorization).
 */
export interface SubscriptionSpec {
  /** Channel name */
  channel: string;
  /** Optional subscription token */
  token?: string;
}

/**
 * A channel argument that accepts either a plain channel name or a
 * SubscriptionSpec carrying an optional per-channel token.
 */
export type ChannelOrSpec = string | SubscriptionSpec;

/**
 * Create an InboundMessage with Subscribe envelope.
 * @param channels - Channel names or SubscriptionSpec objects with optional per-channel tokens.
 * @param ephemeral - When true, the subscriptions are ephemeral.
 */
export function createSubscribeMessage(
  channels: ChannelOrSpec[],
  ephemeral: boolean = false
): InboundMessage {
  const subscribe = create(SubscribeSchema, {
    subscriptions: channels.map((ch) => {
      const spec = typeof ch === "string" ? { channel: ch } : ch;
      return create(SubscriptionSchema, {
        channel: spec.channel,
        ephemeral,
        token: spec.token || "",
      });
    }),
  });

  return create(InboundMessageSchema, {
    id: generateMessageId(),
    envelope: { case: "subscribe", value: subscribe },
  });
}

/**
 * Create an InboundMessage with Unsubscribe envelope.
 * @param channels - Channel names or SubscriptionSpec objects with optional per-channel tokens.
 */
export function createUnsubscribeMessage(channels: ChannelOrSpec[]): InboundMessage {
  const unsubscribe = create(UnsubscribeSchema, {
    subscriptions: channels.map((ch) => {
      const spec = typeof ch === "string" ? { channel: ch } : ch;
      return create(SubscriptionSchema, {
        channel: spec.channel,
        ephemeral: false,
        token: spec.token || "",
      });
    }),
  });

  return create(InboundMessageSchema, {
    id: generateMessageId(),
    envelope: { case: "unsubscribe", value: unsubscribe },
  });
}

/**
 * Create an InboundMessage with Publish envelope.
 * @param transient - When true, skip persistence and only deliver to currently connected subscribers.
 */
export function createPublishMessage(
  channel: string,
  msg: Message,
  transient: boolean = false
): InboundMessage {
  const payload = messageToPayload(msg);
  const publish = create(PublishSchema, {
    channel,
    payload,
    transient,
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
 * Create an InboundMessage with SurveyReply envelope.
 * @param requestId - ID of the survey request being answered.
 * @param reply - Reply message payload, or null when the reply carries an error.
 * @param err - Optional error carried in the reply instead of the payload.
 */
export function createSurveyReplyMessage(
  requestId: string,
  reply: Message | null,
  err?: { code: string; type: string; message: string }
): InboundMessage {
  const surveyReply = create(SurveyReplySchema, {
    requestId,
    payload: reply ? messageToPayload(reply) : undefined,
    error: err
      ? create(ErrorSchema, {
          code: err.code,
          type: err.type,
          message: err.message,
        })
      : undefined,
  });

  return create(InboundMessageSchema, {
    id: generateMessageId(),
    envelope: { case: "surveyReply", value: surveyReply },
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
export type { InboundMessage, OutboundMessage, Message as ProtoMessage, Publication, Publish, RpcRequest, RpcReply } from "../proto/client/v1/service_pb";
export type { Payload, Metadata } from "../proto/shared/v1/types_pb";
