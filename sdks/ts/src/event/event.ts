import { CloudEvent as CloudEventSDK, type CloudEvent as CloudEventSDKType } from "cloudevents";
import type { Payload } from "../proto/shared/v1/types_pb";

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
 * Convert a CloudEvent SDK instance to Payload protobuf format.
 * @param event - CloudEvent SDK instance
 * @returns Protobuf Payload
 * @deprecated Use cloudEventToPayload from converters.ts instead
 */
export function toProtoCloudEvent(event: CloudEventSDKType): Payload {
  // Import and use the conversion from converters
  const { cloudEventToPayload } = require("./converters");
  return cloudEventToPayload(event);
}

/**
 * Convert a protobuf Payload to SDK format.
 * @param payload - Protobuf Payload
 * @param source - Source for the CloudEvent
 * @returns CloudEvent SDK instance
 * @deprecated Use payloadToCloudEvent from converters.ts instead
 */
export function fromProtoCloudEvent(
  payload: Payload,
  source: string = "messageloop"
): CloudEventSDKType {
  // Import and use the conversion from converters
  const { payloadToCloudEvent } = require("./converters");
  return payloadToCloudEvent(payload, source);
}

// Re-export Payload type
export type { Payload } from "../proto/shared/v1/types_pb";
