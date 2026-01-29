import type { OutboundMessage } from "../proto/v1/service_pb";

/**
 * Transport interface for sending and receiving messages.
 * Implementations include WebSocket and gRPC transports.
 */
export interface Transport {
  /**
   * Send an inbound message to the server.
   */
  send(msg: object): Promise<void>;

  /**
   * Receive outbound messages from the server.
   * Returns an async iterable for streaming messages.
   */
  recv(): AsyncIterable<OutboundMessage>;

  /**
   * Close the transport connection.
   */
  close(): Promise<void>;

  /**
   * Check if the transport is currently connected.
   */
  isConnected(): boolean;
}

/**
 * Transport factory for creating client transports.
 */
export interface TransportFactory {
  /**
   * Create a transport and establish connection.
   */
  create(): Promise<Transport>;
}
