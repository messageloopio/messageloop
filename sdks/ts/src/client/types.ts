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
  publish(channel: string, msg: import("../message").Message): Promise<void>;
  rpc(
    channel: string,
    method: string,
    request: import("../message").Message,
    options?: { timeout?: number }
  ): Promise<import("../message").Message>;
  onMessage(handler: (messages: import("../message").ReceivedMessage[]) => void): void;
  onError(handler: (error: Error) => void): void;
  onConnected(handler: (sessionId: string) => void): void;
  onClosed(handler: () => void): void;
  getSessionId(): string | null;
  isConnected(): boolean;
  getSubscribedChannels(): string[];

  // Multi-handler support
  addMessageHandler(
    handler: (messages: import("../message").ReceivedMessage[]) => void
  ): () => void;
  removeMessageHandler(
    handler: (messages: import("../message").ReceivedMessage[]) => void
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
