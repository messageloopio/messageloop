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
  PongSchema,
  PublishSchema,
  RpcRequestSchema,
  SurveyReplySchema,
  SurveyRequestSchema,
  PresenceQuerySchema,
} from "../proto/client/v2/service_pb";

import { PayloadSchema, MetadataSchema } from "../proto/shared/v2/types_pb";
import { ErrorSchema } from "../proto/shared/v2/errors_pb";

import type {
  InboundMessage,
  OutboundMessage,
  Message as ProtoMessage,
  Publication,
  Publish,
  RpcRequest,
  RpcReply,
  PresenceEvent as PresenceEventPB,
  PresenceSnapshot as PresenceSnapshotPB,
  PresenceInfo as PresenceInfoPB,
  SurveyAnswer as SurveyAnswerPB,
  GapNotice as GapNoticePB,
  Subscription,
} from "../proto/client/v2/service_pb";
import type { Payload, Metadata } from "../proto/shared/v2/types_pb";

import type {
  SubscriptionSpec,
  ChannelOrSpec,
  PresenceEvent,
  PresenceSnapshot,
  PresenceInfo,
  SurveyAnswer,
  GapNotice,
} from "../client/types";

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
 * Wire-level recovery hint for a subscription: a shared.v2 Position. The
 * offset is optional; an unset offset is a deliberate "no hint", never
 * "0 means from the start".
 */
export interface WireCursor {
  /** Broker stream epoch the offset is interpreted in. */
  streamEpoch: string;
  /** Optional offset to resume after. */
  offset?: bigint;
}

/**
 * Input shape for building a wire Subscription: channel plus optional token,
 * ephemeral, recover flag, recovery cursor and explicit fresh flag.
 */
export interface WireSubscriptionSpec {
  channel: string;
  ephemeral: boolean;
  token: string;
  recover?: boolean;
  /** Recovery resume hint. Omit for a no-hint recover. */
  cursor?: WireCursor;
  /** Explicit from-the-start replay. */
  fresh?: boolean;
}

/**
 * Create an InboundMessage with Connect envelope.
 */
export function createConnectMessage(
  clientId: string,
  clientType: string,
  token: string,
  version: string,
  autoSubscribe: WireSubscriptionSpec[],
  sessionId?: string
): InboundMessage {
  const connect = create(ConnectSchema, {
    clientId,
    clientType,
    token,
    version,
    sessionId: sessionId || "",
    subscriptions: autoSubscribe.map((sub) =>
      buildWireSubscription(sub)
    ),
  });

  return create(InboundMessageSchema, {
    id: generateMessageId(),
    envelope: { case: "connect", value: connect },
  });
}

/**
 * Build one client-v2 wire Subscription from a spec.
 */
function buildWireSubscription(spec: WireSubscriptionSpec): Subscription {
  return create(SubscriptionSchema, {
    channel: spec.channel,
    ephemeral: spec.ephemeral,
    token: spec.token || "",
    recover: spec.recover === true,
    cursor: spec.cursor
      ? {
          streamEpoch: spec.cursor.streamEpoch || "",
          offset: spec.cursor.offset,
        }
      : undefined,
    fresh: spec.fresh === true,
  });
}

/**
 * Re-export the subscription spec types from the client package so the
 * wire-level builders and the client-facing API share one definition and
 * cannot drift apart.
 */
export type { SubscriptionSpec, ChannelOrSpec } from "../client/types";
export type {
  PresenceEvent,
  PresenceSnapshot,
  PresenceInfo,
  SurveyAnswer,
} from "../client/types";

/**
 * Create an InboundMessage with Subscribe envelope.
 * @param channels - Channel names or SubscriptionSpec objects with optional
 * per-channel tokens and recovery parameters.
 * @param ephemeral - When true, the subscriptions are ephemeral.
 */
export function createSubscribeMessage(
  channels: ChannelOrSpec[],
  ephemeral: boolean = false
): InboundMessage {
  const subscribe = create(SubscribeSchema, {
    subscriptions: channels.map((ch) => {
      const spec = typeof ch === "string" ? { channel: ch } : ch;
      return buildWireSubscription({
        channel: spec.channel,
        ephemeral,
        token: spec.token || "",
        recover: spec.recover === true,
        cursor: spec.cursor
          ? {
              streamEpoch: spec.cursor.streamEpoch || "",
              offset: spec.cursor.offset,
            }
          : undefined,
        fresh: spec.fresh === true,
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
 * Create an InboundMessage with Pong envelope answering a server-issued
 * Ping. The id mirrors the outbound Ping's id (an empty id still produces
 * a Pong).
 */
export function createPongMessage(id: string): InboundMessage {
  return create(InboundMessageSchema, {
    id,
    envelope: { case: "pong", value: create(PongSchema, {}) },
  });
}

/**
 * Create an InboundMessage with PresenceQuery envelope for an exact channel.
 * An empty channel is handed to the server, which rejects it.
 */
export function createPresenceQueryMessage(channel: string): InboundMessage {
  const presenceQuery = create(PresenceQuerySchema, { channel });
  return create(InboundMessageSchema, {
    id: generateMessageId(),
    envelope: { case: "presenceQuery", value: presenceQuery },
  });
}

/**
 * Create an InboundMessage with SurveyRequest envelope initiating a
 * channel-scoped survey. A timeoutMs <= 0 sends 0 and lets the server apply
 * its policy cap.
 */
export function createSurveyRequestMessage(
  requestId: string,
  channel: string,
  payload: Message | null,
  timeoutMs?: number
): InboundMessage {
  const surveyRequest = create(SurveyRequestSchema, {
    requestId,
    channel,
    payload: payload ? messageToPayload(payload) : undefined,
    timeoutMs: timeoutMs && timeoutMs > 0 ? timeoutMs : 0,
  });

  return create(InboundMessageSchema, {
    id: generateMessageId(),
    envelope: { case: "surveyRequest", value: surveyRequest },
  });
}

/**
 * Create an InboundMessage with SubRefresh envelope.
 */
export function createSubRefreshMessage(
  channels: string[]
): InboundMessage {
  const subRefresh = create(SubRefreshSchema, {
    channels,
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
    offset: msg.position?.offset ?? 0n,
    offsetSet: msg.position?.offset !== undefined,
    replay: msg.replay || false,
    message: payloadToMessage(msg.payload ?? emptyPayload, msg.id),
  };
}

/**
 * Parse an outbound message and extract relevant data.
 */
export function parseOutboundMessage(
  msg: OutboundMessage
): {
  type: "connected" | "error" | "subscribeAck" | "unsubscribeAck" | "publishAck" | "publication" | "rpcReply" | "pong" | "subRefreshAck" | "surveyRequest" | "surveyReply" | "presence" | "presenceEvent" | "ping" | "surveyResult" | "recoverComplete" | "gapNotice";
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
  } else if (envelope.case === "presence") {
    return { type: "presence", data: envelope.value, id };
  } else if (envelope.case === "presenceEvent") {
    return { type: "presenceEvent", data: envelope.value, id };
  } else if (envelope.case === "ping") {
    return { type: "ping", data: envelope.value, id };
  } else if (envelope.case === "surveyResult") {
    return { type: "surveyResult", data: envelope.value, id };
  } else if (envelope.case === "recoverComplete") {
    return { type: "recoverComplete", data: envelope.value, id };
  } else if (envelope.case === "gapNotice") {
    return { type: "gapNotice", data: envelope.value, id };
  }

  return { type: "error", data: new Error("Unknown message type"), id };
}

/**
 * Convert a protocol PresenceInfo to the SDK type.
 */
export function presenceInfoFromPB(info?: PresenceInfoPB): PresenceInfo {
  if (!info) {
    return { sessionId: "", userId: "", clientId: "", connectedAt: BigInt(0) };
  }
  return {
    sessionId: info.sessionId,
    userId: info.userId,
    clientId: info.clientId,
    connectedAt: info.connectedAt,
  };
}

/**
 * Convert a protocol PresenceEvent to the SDK type. Unknown actions are
 * still delivered.
 */
export function presenceEventFromPB(ev: PresenceEventPB): PresenceEvent {
  return {
    channel: ev.channel,
    action: ev.action,
    info: presenceInfoFromPB(ev.info),
  };
}

/**
 * Convert a protocol PresenceSnapshot to the SDK type.
 */
export function presenceSnapshotFromPB(snap: PresenceSnapshotPB): PresenceSnapshot {
  return {
    channel: snap.channel,
    clients: (snap.clients || []).map(presenceInfoFromPB),
    truncated: snap.truncated,
    occupancy: snap.occupancy,
  };
}

/**
 * Convert a protocol GapNotice to the SDK type (C6). The notice is
 * informational only: it never enters the message stream and never advances
 * the channel cursor. An unset position offset stays undefined (never 0).
 */
export function gapNoticeFromPB(notice: GapNoticePB): GapNotice {
  return {
    channel: notice.channel,
    gapReason: notice.gapReason,
    streamEpoch: notice.position?.streamEpoch ?? "",
    offset: notice.position?.offset,
  };
}

/**
 * Convert a protocol SurveyAnswer to the SDK type. The user id is read from
 * metadata.entries["user_id"] (the proto has no user_id field on answers).
 */
export function surveyAnswerFromPB(answer: SurveyAnswerPB): SurveyAnswer {
  const out: SurveyAnswer = {
    sessionId: answer.sessionId,
    userId: answer.metadata?.entries?.["user_id"] || "",
  };
  if (answer.payload) {
    out.payload = payloadToMessage(answer.payload, "");
  }
  if (answer.error) {
    out.error = new Error(`${answer.error.code}: ${answer.error.message}`);
  }
  return out;
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
export type { InboundMessage, OutboundMessage, Message as ProtoMessage, Publication, Publish, RpcRequest, RpcReply, Subscription } from "../proto/client/v2/service_pb";
export type { Payload, Metadata } from "../proto/shared/v2/types_pb";
