// Main exports
export { MessageLoopClient } from "./client";
export type { ClientOptions, ClientOption } from "./client";
export type {
  IClient,
  ConnectionState,
  ConnectionStateChangeEvent,
} from "./client/types";

// Transport exports
export type { Transport, TransportFactory } from "./transport/transport";
export { WebSocketTransport } from "./transport/websocket";

// Codec exports
export type { Codec } from "./transport/codec/codec";
export { JSONCodec, jsonCodec, ProtobufCodec, protobufCodec } from "./transport/codec";

// Event exports
export {
  createCloudEvent,
  toProtoCloudEvent,
  fromProtoCloudEvent,
  createConnectMessage,
  createSubscribeMessage,
  createUnsubscribeMessage,
  createPublishMessage,
  createRPCRequestMessage,
  createPingMessage,
  createSubRefreshMessage,
  generateMessageId,
  cloudEventToProto,
  protoToCloudEvent,
  parseOutboundMessage,
  type ReceivedMessage,
} from "./event";

// Option builders
export {
  setEncoding,
  setClientId,
  setClientType,
  setToken,
  setVersion,
  setAutoSubscribe,
  setPingInterval,
  setPingTimeout,
  setConnectTimeout,
  setRPCTimeout,
  setEphemeral,
  setAutoReconnect,
  setReconnectDelay,
  setReconnectMaxAttempts,
} from "./client/options";

// Re-export CloudEvent from cloudevents
export type { CloudEvent } from "cloudevents";
