import type { CloudEvent as CloudEventProto } from "../proto/includes/cloudevents/cloudevents_pb";
import { CloudEvent as CloudEventSDK, type CloudEvent as CloudEventSDKType } from "cloudevents";

/**
 * Create a CloudEvent with the given properties.
 * @param options - CloudEvent options
 * @returns CloudEvent SDK instance
 */
export function createCloudEvent(options: {
  id?: string;
  source: string;
  type: string;
  data?: any;
  datacontenttype?: string;
  subject?: string;
  time?: string | Date;
  extensions?: Record<string, string | number | boolean>;
}): CloudEventSDKType {
  // Convert Date to ISO string if provided
  const timeValue = options.time instanceof Date
    ? options.time.toISOString()
    : options.time;

  // Build the object with only defined values
  const eventOptions: Record<string, any> = {
    id: options.id ?? crypto.randomUUID(),
    source: options.source,
    type: options.type,
    data: options.data,
  };

  // Only add optional fields if they have values
  if (options.datacontenttype) {
    eventOptions.datacontenttype = options.datacontenttype;
  }
  if (options.subject) {
    eventOptions.subject = options.subject;
  }
  if (timeValue) {
    eventOptions.time = timeValue;
  }
  if (options.extensions) {
    const extensions: Record<string, string | number | boolean> = {};
    for (const [key, value] of Object.entries(options.extensions)) {
      if (value !== undefined) {
        extensions[key] = value;
      }
    }
    if (Object.keys(extensions).length > 0) {
      eventOptions.extensions = extensions;
    }
  }

  return new CloudEventSDK(eventOptions);
}

/**
 * Convert a CloudEvent SDK instance to protobuf format.
 * @param event - CloudEvent SDK instance
 * @returns Protobuf CloudEvent
 * @deprecated Use cloudEventToProto from converters.ts instead
 */
export function toProtoCloudEvent(event: CloudEventSDKType): CloudEventProto {
  // Import and use the conversion from converters
  const { cloudEventToProto } = require("./converters");
  return cloudEventToProto(event);
}

/**
 * Convert a protobuf CloudEvent to SDK format.
 * @param event - Protobuf CloudEvent
 * @returns CloudEvent SDK instance
 * @deprecated Use protoToCloudEvent from converters.ts instead
 */
export function fromProtoCloudEvent(
  event: CloudEventProto
): CloudEventSDKType {
  // Import and use the conversion from converters
  const { protoToCloudEvent } = require("./converters");
  return protoToCloudEvent(event);
}

// Re-export protobuf CloudEvent type
export type { CloudEvent as CloudEvent } from "../proto/includes/cloudevents/cloudevents_pb";
export type { CloudEventProto };
