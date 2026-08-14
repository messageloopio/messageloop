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
 * optional subscription token (used for subscription-level authorization)
 * and optional recovery parameters (recover history after the given offset).
 */
export interface SubscriptionSpec {
  /** Channel name */
  channel: string;
  /** Optional subscription token */
  token?: string;
  /**
   * When true, the server replays messages published after `offset`
   * (interpreted in `epoch`) through the SubscribeAck publications, delivered
   * via onMessage/addMessageHandler. offset 0 / empty epoch still sends
   * recover=true (a fresh subscription recovering from the start, subject to
   * server policy). The default is false (no recovery).
   */
  recover?: boolean;
  /** Offset to recover from (bigint to match the proto uint64). */
  offset?: bigint;
  /** Broker epoch the offset is interpreted in. */
  epoch?: string;
}

/**
 * A channel argument that accepts either a plain channel name or a
 * SubscriptionSpec carrying an optional per-channel token and recovery
 * parameters.
 */
export type ChannelOrSpec = string | SubscriptionSpec;

/**
 * Presence entry of one connected client on a channel.
 */
export interface PresenceInfo {
  /** Session ID of the present client. */
  sessionId: string;
  /** Authenticated user ID; empty when the server did not resolve one. */
  userId: string;
  /** Connect client_id (device/app instance), not the session ID. */
  clientId: string;
  /** Unix millisecond timestamp of the client's connect (int64). */
  connectedAt: bigint;
}

/**
 * A join/leave notification for a channel.
 */
export interface PresenceEvent {
  /** Always the exact channel the event belongs to. */
  channel: string;
  /** "join" or "leave"; unknown actions are still delivered. */
  action: string;
  /** Presence entry of the joining/leaving client. */
  info: PresenceInfo;
}

/**
 * Point-in-time presence view of a channel.
 */
export interface PresenceSnapshot {
  /** The exact channel the snapshot belongs to. */
  channel: string;
  /** Present clients, capped by the server policy. */
  clients: PresenceInfo[];
  /** Whether the clients list was capped by the server. */
  truncated: boolean;
  /** Full member count of the channel. */
  occupancy: number;
}

/**
 * One answer of a client-initiated survey.
 */
export interface SurveyAnswer {
  /** Session ID of the answering session. */
  sessionId: string;
  /** Answering user, read from metadata.entries["user_id"]; "" when absent. */
  userId: string;
  /** The answer payload; undefined when the answer carries an error. */
  payload?: import("../message").Message;
  /**
   * Per-answer error (e.g. SURVEY_ANSWER_TOO_LARGE / SURVEY_FAILED);
   * undefined for a healthy answer.
   */
  error?: Error;
}

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
  /**
   * Register the handler for survey requests from the server, additionally
   * receiving the request channel. When set it takes precedence over the
   * handler registered with onSurvey.
   */
  onSurveyRequest(
    handler: (
      requestId: string,
      channel: string,
      request: import("../message").Message
    ) => import("../message").Message | Promise<import("../message").Message>
  ): void;
  /**
   * Initiate a survey on an exact channel and wait for the aggregated
   * answers. The wait happens on the caller's promise; the receive loop
   * fills the pending result, so this must not be awaited synchronously
   * from receive-loop callbacks (onMessage, onPresence*, onSurvey*).
   * A timeoutMs <= 0 sends 0 and lets the server apply its policy cap.
   */
  survey(
    channel: string,
    payload: import("../message").Message | null,
    timeoutMs?: number
  ): Promise<SurveyAnswer[]>;
  /** Register the handler for presence events (join/leave). */
  onPresence(handler: (event: PresenceEvent) => void): void;
  /**
   * Register the handler for presence snapshots delivered with Connected /
   * SubscribeAck, and for the snapshot returned by a presence() query.
   */
  onPresenceSnapshot(handler: (snap: PresenceSnapshot) => void): void;
  /**
   * Query the current presence snapshot of an exact channel. An empty or
   * wildcard channel is handed to the server, which rejects it. The wait
   * happens on the caller's promise: do not await it synchronously from
   * receive-loop callbacks.
   */
  presence(channel: string): Promise<PresenceSnapshot>;
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
