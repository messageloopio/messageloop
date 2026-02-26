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

// Message exports
export {
  createMessage,
  createJSONMessage,
  createBinaryMessage,
  createTextMessage,
  createData,
  messageToPayload,
  payloadToMessage,
  dataAs,
  isJSONData,
  isBinaryData,
  isTextData,
  type Message,
  type Data,
  type DataType,
  type ReceivedMessage,
} from "./message";

// Converter exports
export {
  createConnectMessage,
  createSubscribeMessage,
  createUnsubscribeMessage,
  createPublishMessage,
  createRPCRequestMessage,
  createPingMessage,
  createSubRefreshMessage,
  generateMessageId,
  parseOutboundMessage,
  extractRpcReply,
  type Payload,
  type Metadata,
} from "./message";

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
