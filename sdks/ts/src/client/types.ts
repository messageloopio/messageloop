/**
 * Connection state for the MessageLoop client.
 */
export type ConnectionState =
  | "connecting"
  | "connected"
  | "disconnected"
  | "reconnecting";

/**
 * Event emitted when connection state changes.
 */
export interface ConnectionStateChangeEvent {
  previousState: ConnectionState;
  newState: ConnectionState;
}

/**
 * MessageLoop client type definition.
 */
export interface IClient {
  connect(): Promise<void>;
  close(): Promise<void>;
  subscribe(...channels: string[]): Promise<void>;
  unsubscribe(...channels: string[]): Promise<void>;
  publish(channel: string, event: import("cloudevents").CloudEvent): Promise<void>;
  rpc(
    channel: string,
    method: string,
    request: import("cloudevents").CloudEvent,
    options?: { timeout?: number }
  ): Promise<import("cloudevents").CloudEvent>;
  onMessage(handler: (events: import("../event/converters").ReceivedMessage[]) => void): void;
  onError(handler: (error: Error) => void): void;
  onConnected(handler: (sessionId: string) => void): void;
  onClosed(handler: () => void): void;
  getSessionId(): string | null;
  isConnected(): boolean;
  getSubscribedChannels(): string[];

  // Multi-handler support
  addMessageHandler(
    handler: (events: import("../event/converters").ReceivedMessage[]) => void
  ): () => void;
  removeMessageHandler(
    handler: (events: import("../event/converters").ReceivedMessage[]) => void
  ): void;
  addStateChangeHandler(
    handler: (event: ConnectionStateChangeEvent) => void
  ): () => void;

  // Connection state
  getConnectionState(): ConnectionState;

  // Reconnect control
  disableAutoReconnect(): void;
  enableAutoReconnect(): void;
}
