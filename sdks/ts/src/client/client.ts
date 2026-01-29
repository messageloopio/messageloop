import type { CloudEvent } from "cloudevents";
import type { OutboundMessage } from "../proto/v1/service_pb";
import type { ReceivedMessage } from "../event/converters";
import type { Transport } from "../transport/transport";
import type { Codec } from "../transport/codec/codec";
import type { ClientOptions, ClientOption } from "./options";

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

  // Handlers
  private messageHandler: ((events: ReceivedMessage[]) => void) | null = null;
  private errorHandler: ((error: Error) => void) | null = null;
  private connectedHandler: ((sessionId: string) => void) | null = null;
  private closedHandler: (() => void) | null = null;

  // Subscriptions
  private subscribedChannels: Set<string> = new Set();

  // RPC pending requests
  private pendingRPC: Map<string, { resolve: (event: CloudEvent) => void; reject: (err: Error) => void }> = new Map();

  // Ping/Pong
  private pingTimer: ReturnType<typeof setInterval> | null = null;
  private pingTimeoutTimer: ReturnType<typeof setTimeout> | null = null;

  /**
   * Create a new MessageLoop client.
   * @param options - Client options
   */
  private constructor(options: ClientOptions) {
    this.options = options;
    this.codec = options.encoding === "proto" ? protobufCodec : jsonCodec;

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
  static async dial(url: string, options: ClientOption[] = []): Promise<MessageLoopClient> {
    const opts = buildClientOptions(options);
    const codec = opts.encoding === "proto" ? protobufCodec : jsonCodec;

    const transport = await WebSocketTransport.dial(url, codec, {
      timeout: opts.connectTimeout,
    });

    const client = new MessageLoopClient(opts);
    client.transport = transport;

    // Start receiving messages
    client.startMessageLoop();

    // Wait for connection
    await client.waitForConnection();

    return client;
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

        if (this.messageHandler && messages.length > 0) {
          this.messageHandler(messages);
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
  };

  for (const setter of setters) {
    setter(defaults);
  }

  return defaults;
}
