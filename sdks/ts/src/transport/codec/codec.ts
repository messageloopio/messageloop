import type { OutboundMessage } from "../../proto/v1/service_pb";

/**
 * Codec interface for encoding and decoding messages.
 * Implementations include JSON and Protobuf codecs.
 */
export interface Codec {
  /**
   * Get the codec name for subprotocol negotiation.
   */
  name(): string;

  /**
   * Encode an inbound message to bytes.
   */
  encode(msg: object): Uint8Array | string;

  /**
   * Decode bytes to an outbound message.
   */
  decode(data: Uint8Array | string): OutboundMessage;

  /**
   * Whether this codec uses binary data (true for protobuf).
   */
  useBytes(): boolean;
}
