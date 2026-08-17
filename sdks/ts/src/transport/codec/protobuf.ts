import { fromBinary, toBinary } from "@bufbuild/protobuf";
import { InboundMessage, InboundMessageSchema, OutboundMessage, OutboundMessageSchema } from "../../proto/client/v2/service_pb";
import type { Codec } from "./codec";

const INBOUND_TYPE = "messageloop.client.v2.InboundMessage";

/**
 * Protobuf codec implementation.
 * Encodes/decodes messages as binary protobuf.
 */
export class ProtobufCodec implements Codec {
  name(): string {
    return "messageloop+proto";
  }

  encode(msg: object): Uint8Array {
    if ((msg as { $typeName?: unknown }).$typeName !== INBOUND_TYPE) {
      throw new Error("Message does not support binary serialization");
    }
    return toBinary(InboundMessageSchema, msg as InboundMessage);
  }

  decode(data: Uint8Array | string | Blob): OutboundMessage | Promise<OutboundMessage> {
    if (data instanceof Blob) {
      return data
        .arrayBuffer()
        .then((buf) => this.decode(new Uint8Array(buf)));
    }
    const bytes = data instanceof Uint8Array ? data : new TextEncoder().encode(data);
    return fromBinary(OutboundMessageSchema, bytes) as OutboundMessage;
  }

  useBytes(): boolean {
    return true;
  }
}

/**
 * Singleton instance of ProtobufCodec.
 */
export const protobufCodec = new ProtobufCodec();
