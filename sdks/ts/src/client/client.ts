import type { CloudEvent } from "cloudevents";
import type { OutboundMessage } from "../proto/v1/service_pb";
import type { ReceivedMessage } from "../event/converters";
import type { Transport } from "../transport/transport";
import type { Codec } from "../transport/codec/codec";
import type { ClientOptions, ClientOption } from "./options";
import type {
  ConnectionState,
  ConnectionStateChangeEvent,
} from "./types";

import { WebSocketTransport } from "../transport/websocket";
import { jsonCodec, protobufCodec } from "../transport/codec";
import {
  createConnectMessage,
  createSubscribeMessage,
  createUnsubscribeMessage,
  createPublishMessage,
  createRPCRequestMessage,
  createPingMessage,
  parseOutboundMessage,
  protoToCloudEvent,
} from "../event/converters";

/**
 * MessageLoop client for connecting to the messaging server.
 */
export class MessageLoopClient {
  private transport: Transport | null = null;
  private codec: Codec;
  private options: ClientOptions;
  private sessionId: string | null = null;
  private isConnectedFlag = false;
  private isClosedFlag = false;
  private url: string = "";

  // Connection state
  private connectionState: ConnectionState = "disconnected";
  private autoReconnectEnabled = true;

  // Multi-handler support using Sets
  private messageHandlers: Set<(events: ReceivedMessage[]) => void> =
    new Set();
  private stateChangeHandlers: Set<(event: ConnectionStateChangeEvent) => void> =
    new Set();

  // Legacy handlers (for backward compatibility)
  private messageHandler: ((events: ReceivedMessage[]) => void) | null = null;
  private errorHandler: ((error: Error) => void) | null = null;
  private connectedHandler: ((sessionId: string) => void) | null = null;
  private closedHandler: (() => void) | null = null;

  // Subscriptions
  private subscribedChannels: Set<string> = new Set();

  // RPC pending requests
  private pendingRPC: Map<
    string,
    { resolve: (event: CloudEvent) => void; reject: (err: Error) => void }
  > = new Map();

  // Ping/Pong
  private pingTimer: ReturnType<typeof setInterval> | null = null;
  private pingTimeoutTimer: ReturnType<typeof setTimeout> | null = null;

  // Reconnection
  private reconnectAttempts = 0;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private isReconnecting = false;

  /**
   * Create a new MessageLoop client.
   * @param options - Client options
   */
  private constructor(options: ClientOptions) {
    this.options = options;
    this.codec = options.encoding === "proto" ? protobufCodec : jsonCodec;
    this.autoReconnectEnabled = options.autoReconnect;

    // Add auto-subscribe channels to subscribed set
    for (const channel of options.autoSubscribe) {
      this.subscribedChannels.add(channel);
    }
  }

  /**
   * Create a client and connect via WebSocket.
   * @param url - WebSocket URL
   * @param options - Client option setters
   * @returns Connected client instance
   */
  static async dial(
    url: string,
    options: ClientOption[] = []
  ): Promise<MessageLoopClient> {
    const opts = buildClientOptions(options);
    const codec = opts.encoding === "proto" ? protobufCodec : jsonCodec;

    const client = new MessageLoopClient(opts);
    client.url = url;
    client.setConnectionState("connecting");

    try {
      const transport = await WebSocketTransport.dial(url, codec, {
        timeout: opts.connectTimeout,
      });

      client.transport = transport;

      // Start receiving messages
      client.startMessageLoop();

      // Wait for connection
      await client.waitForConnection();

      return client;
    } catch (err) {
      client.setConnectionState("disconnected");
      // Attempt reconnection if enabled
      if (client.autoReconnectEnabled && !client.isClosedFlag) {
        client.attemptReconnect();
      }
      throw err;
    }
  }

  /**
   * Start the message receiving loop.
   */
  private startMessageLoop(): void {
    if (!this.transport) return;

    const receive = async () => {
      if (!this.transport || this.isClosedFlag) return;

      try {
        for await (const msg of this.transport.recv()) {
          this.handleMessage(msg);
        }
      } catch (err) {
        if (!this.isClosedFlag) {
          this.handleError(err instanceof Error ? err : new Error(String(err)));
        }
      }
    };

    receive();
  }

  /**
   * Wait for connection to be established.
   */
  private async waitForConnection(): Promise<void> {
    return new Promise((resolve, reject) => {
      const timeout = setTimeout(() => {
        reject(new Error("Connection timeout"));
      }, this.options.connectTimeout);

      const checkConnection = () => {
        if (this.isConnectedFlag) {
          clearTimeout(timeout);
          resolve();
        } else if (this.isClosedFlag) {
          clearTimeout(timeout);
          reject(new Error("Connection closed"));
        } else {
          setTimeout(checkConnection, 100);
        }
      };

      checkConnection();
    });
  }

  /**
   * Handle an incoming message from the transport.
   */
  private handleMessage(msg: OutboundMessage): void {
    const parsed = parseOutboundMessage(msg);

    switch (parsed.type) {
      case "connected": {
        this.sessionId = parsed.data.sessionId || null;
        this.isConnectedFlag = true;
        this.reconnectAttempts = 0;
        this.isReconnecting = false;

        // Update connection state
        const wasReconnecting = this.connectionState === "reconnecting";
        this.setConnectionState("connected");

        // Resubscribe to channels after reconnection
        if (wasReconnecting && this.subscribedChannels.size > 0) {
          this.resubscribeAllChannels();
        }

        // Start ping loop
        this.startPingLoop();

        // Notify connected handler
        if (this.connectedHandler && this.sessionId) {
          this.connectedHandler(this.sessionId);
        }
        break;
      }

      case "publication": {
        // Convert messages to ReceivedMessage format
        const messages: ReceivedMessage[] = [];
        const envelopes = parsed.data.envelopes || [];
        for (const env of envelopes) {
          messages.push({
            id: env.id,
            channel: env.channel,
            offset: env.offset,
            event: protoToCloudEvent(env.payload!),
          });
        }

        if (messages.length > 0) {
          // Notify legacy handler
          if (this.messageHandler) {
            this.messageHandler(messages);
          }
          // Notify all registered handlers
          for (const handler of this.messageHandlers) {
            try {
              handler(messages);
            } catch {
              // Ignore handler errors
            }
          }
        }
        break;
      }

      case "rpcReply": {
        const id = parsed.id;
        const pending = this.pendingRPC.get(id);
        if (pending) {
          this.pendingRPC.delete(id);
          const event = protoToCloudEvent(parsed.data.payload!);
          pending.resolve(event);
        }
        break;
      }

      case "pong": {
        if (this.pingTimeoutTimer) {
          clearTimeout(this.pingTimeoutTimer);
          this.pingTimeoutTimer = null;
        }
        break;
      }

      case "error": {
        const error = new Error(parsed.data.message || "Server error");
        (error as any).code = parsed.data.code;
        (error as any).type = parsed.data.type;
        this.handleError(error);
        break;
      }
    }
  }

  /**
   * Handle an error.
   */
  private handleError(err: Error): void {
    if (this.errorHandler) {
      this.errorHandler(err);
    }

    // Trigger reconnection for connection errors
    if (
      this.autoReconnectEnabled &&
      !this.isClosedFlag &&
      !this.isReconnecting &&
      this.connectionState === "connected"
    ) {
      this.handleDisconnect();
    }
  }

  /**
   * Set the connection state and notify handlers.
   */
  private setConnectionState(newState: ConnectionState): void {
    const previousState = this.connectionState;
    if (previousState === newState) return;

    this.connectionState = newState;

    // Notify state change handlers
    const event: ConnectionStateChangeEvent = {
      previousState,
      newState,
    };
    for (const handler of this.stateChangeHandlers) {
      try {
        handler(event);
      } catch {
        // Ignore handler errors
      }
    }
  }

  /**
   * Handle unexpected disconnect.
   */
  private handleDisconnect(): void {
    this.isConnectedFlag = false;
    this.stopPingLoop();
    this.setConnectionState("disconnected");

    // Attempt reconnection if enabled
    if (this.autoReconnectEnabled && !this.isClosedFlag) {
      this.attemptReconnect();
    }
  }

  /**
   * Attempt to reconnect with exponential backoff.
   */
  private attemptReconnect(): void {
    if (this.isReconnecting || this.isClosedFlag) return;

    // Check max attempts
    if (
      this.options.reconnectMaxAttempts > 0 &&
      this.reconnectAttempts >= this.options.reconnectMaxAttempts
    ) {
      this.setConnectionState("disconnected");
      return;
    }

    this.isReconnecting = true;
    this.setConnectionState("reconnecting");

    // Calculate delay with exponential backoff
    const delay = Math.min(
      this.options.reconnectInitialDelay *
        Math.pow(this.options.reconnectBackoffMultiplier, this.reconnectAttempts),
      this.options.reconnectMaxDelay
    );

    this.reconnectAttempts++;

    this.reconnectTimer = setTimeout(() => {
      this.reconnect().catch(() => {
        // Reconnect failed, will retry
      });
    }, delay);
  }

  /**
   * Perform reconnection.
   */
  private async reconnect(): Promise<void> {
    if (this.isClosedFlag || !this.url) {
      this.isReconnecting = false;
      return;
    }

    try {
      // Clean up old transport
      if (this.transport) {
        try {
          await this.transport.close();
        } catch {
          // Ignore cleanup errors
        }
        this.transport = null;
      }

      this.setConnectionState("connecting");

      // Create new transport
      const transport = await WebSocketTransport.dial(this.url, this.codec, {
        timeout: this.options.connectTimeout,
      });

      this.transport = transport;
      this.startMessageLoop();

      // Send connect message
      await this.connect();
    } catch {
      this.isReconnecting = false;
      // Schedule next attempt
      if (this.autoReconnectEnabled && !this.isClosedFlag) {
        this.attemptReconnect();
      }
    }
  }

  /**
   * Resubscribe to all channels after reconnection.
   */
  private async resubscribeAllChannels(): Promise<void> {
    if (this.subscribedChannels.size === 0) return;

    const channels = Array.from(this.subscribedChannels);
    try {
      const msg = createSubscribeMessage(channels, this.options.ephemeral);
      await this.send(msg);
    } catch {
      // Ignore resubscribe errors
    }
  }

  /**
   * Start the ping loop.
   */
  private startPingLoop(): void {
    if (this.options.pingInterval === 0) return;

    this.pingTimer = setInterval(() => {
      this.sendPing();
    }, this.options.pingInterval);
  }

  /**
   * Stop the ping loop.
   */
  private stopPingLoop(): void {
    if (this.pingTimer) {
      clearInterval(this.pingTimer);
      this.pingTimer = null;
    }
    if (this.pingTimeoutTimer) {
      clearTimeout(this.pingTimeoutTimer);
      this.pingTimeoutTimer = null;
    }
  }

  /**
   * Send a ping message.
   */
  private async sendPing(): Promise<void> {
    if (!this.transport || !this.isConnectedFlag) return;

    try {
      const pingMsg = createPingMessage();
      await this.transport.send(pingMsg as any);

      // Set up pong timeout
      this.pingTimeoutTimer = setTimeout(() => {
        this.handleError(new Error("Pong timeout"));
        this.close();
      }, this.options.pingTimeout);
    } catch (err) {
      this.handleError(err instanceof Error ? err : new Error(String(err)));
    }
  }

  /**
   * Send a message through the transport.
   */
  private async send(msg: { id: string; channel?: string; method?: string } & Record<string, any>): Promise<void> {
    if (!this.transport || !this.isConnectedFlag) {
      throw new Error("Not connected");
    }
    await this.transport.send(msg as any);
  }

  /**
   * Connect to the server and authenticate.
   */
  async connect(): Promise<void> {
    if (!this.transport) {
      throw new Error("Transport not initialized");
    }

    const connectMsg = createConnectMessage(
      this.options.clientId,
      this.options.clientType,
      this.options.token,
      this.options.version,
      Array.from(this.subscribedChannels).map((ch) => ({
        channel: ch,
        ephemeral: this.options.ephemeral,
        token: "",
      }))
    );

    await this.transport.send(connectMsg as any);
  }

  /**
   * Close the client connection.
   */
  async close(): Promise<void> {
    if (this.isClosedFlag) return;
    this.isClosedFlag = true;

    // Stop reconnection
    this.isReconnecting = false;
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }

    // Stop ping loop
    this.stopPingLoop();

    // Reject pending RPC requests
    for (const [_, pending] of this.pendingRPC) {
      pending.reject(new Error("Connection closed"));
    }
    this.pendingRPC.clear();

    // Close transport
    if (this.transport) {
      await this.transport.close();
      this.transport = null;
    }

    this.isConnectedFlag = false;
    this.sessionId = null;
    this.setConnectionState("disconnected");

    // Notify closed handler
    if (this.closedHandler) {
      this.closedHandler();
    }
  }

  /**
   * Subscribe to one or more channels.
   */
  async subscribe(...channels: string[]): Promise<void> {
    const msg = createSubscribeMessage(channels, this.options.ephemeral);
    await this.send(msg);

    // Add to subscribed channels
    for (const channel of channels) {
      this.subscribedChannels.add(channel);
    }
  }

  /**
   * Unsubscribe from one or more channels.
   */
  async unsubscribe(...channels: string[]): Promise<void> {
    const msg = createUnsubscribeMessage(channels);
    await this.send(msg);

    // Remove from subscribed channels
    for (const channel of channels) {
      this.subscribedChannels.delete(channel);
    }
  }

  /**
   * Publish an event to a channel.
   */
  async publish(channel: string, event: CloudEvent): Promise<void> {
    const msg = createPublishMessage(channel, event);
    await this.send(msg);
  }

  /**
   * Make an RPC request to a channel.
   */
  async rpc(
    channel: string,
    method: string,
    request: CloudEvent,
    options?: { timeout?: number }
  ): Promise<CloudEvent> {
    const msg = createRPCRequestMessage(channel, method, request);
    const id = msg.id;

    return new Promise((resolve, reject) => {
      // Set up timeout
      const timeout = options?.timeout ?? this.options.rpcTimeout;
      const timeoutId = setTimeout(() => {
        this.pendingRPC.delete(id);
        reject(new Error(`RPC timeout after ${timeout}ms`));
      }, timeout);

      // Store pending request
      this.pendingRPC.set(id, {
        resolve: (event: CloudEvent) => {
          clearTimeout(timeoutId);
          resolve(event);
        },
        reject: (err: Error) => {
          clearTimeout(timeoutId);
          reject(err);
        },
      });

      // Send request
      this.send(msg).catch((err) => {
        clearTimeout(timeoutId);
        this.pendingRPC.delete(id);
        reject(err);
      });
    });
  }

  /**
   * Set the message handler.
   */
  onMessage(handler: (events: ReceivedMessage[]) => void): void {
    this.messageHandler = handler;
  }

  /**
   * Set the error handler.
   */
  onError(handler: (error: Error) => void): void {
    this.errorHandler = handler;
  }

  /**
   * Set the connected handler.
   */
  onConnected(handler: (sessionId: string) => void): void {
    this.connectedHandler = handler;
  }

  /**
   * Set the closed handler.
   */
  onClosed(handler: () => void): void {
    this.closedHandler = handler;
  }

  /**
   * Get the current session ID.
   */
  getSessionId(): string | null {
    return this.sessionId;
  }

  /**
   * Check if the client is connected.
   */
  isConnectedToServer(): boolean {
    return this.isConnectedFlag;
  }

  /**
   * Get subscribed channels.
   */
  getSubscribedChannels(): string[] {
    return Array.from(this.subscribedChannels);
  }

  // ========== Multi-handler API ==========

  /**
   * Add a message handler. Returns a function to remove the handler.
   */
  addMessageHandler(
    handler: (events: ReceivedMessage[]) => void
  ): () => void {
    this.messageHandlers.add(handler);
    return () => this.removeMessageHandler(handler);
  }

  /**
   * Remove a message handler.
   */
  removeMessageHandler(handler: (events: ReceivedMessage[]) => void): void {
    this.messageHandlers.delete(handler);
  }

  /**
   * Add a state change handler. Returns a function to remove the handler.
   */
  addStateChangeHandler(
    handler: (event: ConnectionStateChangeEvent) => void
  ): () => void {
    this.stateChangeHandlers.add(handler);
    return () => this.stateChangeHandlers.delete(handler);
  }

  /**
   * Get the current connection state.
   */
  getConnectionState(): ConnectionState {
    return this.connectionState;
  }

  /**
   * Disable automatic reconnection.
   */
  disableAutoReconnect(): void {
    this.autoReconnectEnabled = false;
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    this.isReconnecting = false;
  }

  /**
   * Enable automatic reconnection.
   */
  enableAutoReconnect(): void {
    this.autoReconnectEnabled = true;
  }

  /**
   * Check if connected (alias for backward compatibility).
   */
  isConnected(): boolean {
    return this.isConnectedFlag;
  }
}

/**
 * Build client options from option setters.
 */
function buildClientOptions(setters: ClientOption[]): ClientOptions {
  const defaults: ClientOptions = {
    encoding: "json",
    clientId: crypto.randomUUID(),
    clientType: "sdk",
    token: "",
    version: "1.0.0",
    autoSubscribe: [],
    pingInterval: 30000,
    pingTimeout: 10000,
    connectTimeout: 30000,
    rpcTimeout: 30000,
    ephemeral: false,
    autoReconnect: true,
    reconnectInitialDelay: 1000,
    reconnectMaxDelay: 30000,
    reconnectMaxAttempts: 0,
    reconnectBackoffMultiplier: 2,
  };

  for (const setter of setters) {
    setter(defaults);
  }

  return defaults;
}
