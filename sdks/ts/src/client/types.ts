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
 * Per-channel subscription spec: a plain channel name or a channel with an
 * optional subscription token (used for subscription-level authorization).
 */
export interface SubscriptionSpec {
  /** Channel name */
  channel: string;
  /** Optional subscription token */
  token?: string;
}

/**
 * A channel argument that accepts either a plain channel name or a
 * SubscriptionSpec carrying an optional per-channel token.
 */
export type ChannelOrSpec = string | SubscriptionSpec;

/**
 * MessageLoop client type definition.
 */
export interface IClient {
  connect(): Promise<void>;
  close(): Promise<void>;
  subscribe(...channels: ChannelOrSpec[]): Promise<void>;
  unsubscribe(...channels: ChannelOrSpec[]): Promise<void>;
  publish(
    channel: string,
    msg: import("../message").Message,
    transient?: boolean
  ): Promise<void>;
  publishWithAck(
    channel: string,
    msg: import("../message").Message,
    options?: { transient?: boolean; timeout?: number }
  ): Promise<{ id: string; offset: bigint }>;
  subRefresh(...channels: string[]): Promise<void>;
  onSurvey(
    handler: (
      requestId: string,
      request: import("../message").Message
    ) => import("../message").Message | Promise<import("../message").Message>
  ): void;
  rpc(
    channel: string,
    method: string,
    request: import("../message").Message,
    options?: { timeout?: number }
  ): Promise<import("../message").Message>;
  /** Single-slot convenience alias; prefer addMessageHandler for new code. */
  onMessage(handler: (messages: import("../message").ReceivedMessage[]) => void): void;
  onError(handler: (error: Error) => void): void;
  onConnected(handler: (sessionId: string) => void): void;
  onClosed(handler: () => void): void;
  getSessionId(): string | null;
  isConnected(): boolean;
  getSubscribedChannels(): string[];

  // Multi-handler support (recommended over the onXxx aliases)
  /** Register a message handler; returns a disposer. */
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
