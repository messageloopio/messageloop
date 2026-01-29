import type { OutboundMessage } from "../proto/v1/service_pb";
import type { Transport } from "./transport";
import type { Codec } from "./codec/codec";

/**
 * WebSocket transport implementation.
 * Compatible with both Node.js (ws) and browser (native WebSocket).
 */
export class WebSocketTransport implements Transport {
  private socket: WebSocket | import("ws").WebSocket;
  private sendQueue: Array<{ msg: object; resolve: () => void; reject: (err: Error) => void }> = [];
  private isSending = false;
  private messageListeners: Array<(msg: OutboundMessage) => void> = [];
  private errorListeners: Array<(err: Error) => void> = [];
  private closeListeners: Array<(code?: number) => void> = [];
  private _connected = false;
  private codec: Codec;

  /**
   * Create a WebSocket transport.
   * @param socket - WebSocket instance (browser native or ws)
   * @param codec - Message codec for encoding/decoding
   */
  constructor(socket: WebSocket | import("ws").WebSocket, codec: Codec) {
    this.socket = socket;
    this.codec = codec;

    // Set up message handler - use any type for cross-environment compatibility
    this.socket.onmessage = (event: any) => {
      try {
        const data = event.data as Uint8Array | string;
        const outboundMsg = this.codec.decode(data);
        this.handleMessage(outboundMsg);
      } catch (err) {
        this.handleError(err instanceof Error ? err : new Error(String(err)));
      }
    };

    // Set up error handler - use any type for cross-environment compatibility
    this.socket.onerror = (event: any) => {
      const err = new Error(`WebSocket error: ${JSON.stringify(event)}`);
      this.handleError(err);
    };

    // Set up close handler - use any type for cross-environment compatibility
    this.socket.onclose = (event: any) => {
      this._connected = false;
      this.closeListeners.forEach((listener) => listener(event?.code));
    };

    // Set up open handler
    this.socket.onopen = () => {
      this._connected = true;
      this.processSendQueue();
    };
  }

  /**
   * Create a WebSocket transport by dialing a URL.
   * @param url - WebSocket URL to connect to
   * @param codec - Message codec for encoding/decoding
   * @param options - Optional WebSocket options
   */
  static async dial(
    url: string,
    codec: Codec,
    options?: {
      subprotocols?: string[];
      headers?: Record<string, string>;
      timeout?: number;
    }
  ): Promise<WebSocketTransport> {
    // Use WebSocket from global scope (browser) or require ws (Node.js)
    let WebSocketClass: new (
      url: string,
      protocols?: string | string[]
    ) => WebSocket | import("ws").WebSocket;

    if (typeof globalThis.WebSocket !== "undefined") {
      WebSocketClass = globalThis.WebSocket as new (
        url: string,
        protocols?: string | string[]
      ) => WebSocket;
    } else {
      const ws = await import("ws");
      WebSocketClass = ws.WebSocket as new (
        url: string,
        protocols?: string | string[]
      ) => import("ws").WebSocket;
    }

    const subprotocol = codec.name();
    const socket = new WebSocketClass(url, subprotocol ? [subprotocol] : undefined);

    if (options?.headers) {
      // @ts-expect-error - ws supports headers in Node.js
      if (options.headers && typeof socket.additionalHeaders !== "undefined") {
        // @ts-expect-error - ws specific
        socket.additionalHeaders = options.headers;
      }
    }

    // Wait for connection with timeout
    return new Promise((resolve, reject) => {
      const timeout = options?.timeout ?? 30000;
      const timeoutId = setTimeout(() => {
        socket.close();
        reject(new Error(`WebSocket connection timeout after ${timeout}ms`));
      }, timeout);

      socket.onopen = () => {
        clearTimeout(timeoutId);
        resolve(new WebSocketTransport(socket, codec));
      };

      socket.onerror = () => {
        clearTimeout(timeoutId);
        reject(new Error("WebSocket connection failed"));
      };
    });
  }

  async send(msg: object): Promise<void> {
    if (!this._connected) {
      throw new Error("WebSocket is not connected");
    }

    return new Promise((resolve, reject) => {
      this.sendQueue.push({
        msg,
        resolve,
        reject,
      });
      this.processSendQueue();
    });
  }

  private processSendQueue(): void {
    if (this.isSending || this.sendQueue.length === 0) {
      return;
    }

    this.isSending = true;
    const item = this.sendQueue.shift()!;

    try {
      const data = this.codec.encode(item.msg);
      this.socket.send(data);
      item.resolve();
    } catch (err) {
      item.reject(err instanceof Error ? err : new Error(String(err)));
    }

    this.isSending = false;
    this.processSendQueue();
  }

  private handleMessage(msg: OutboundMessage): void {
    this.messageListeners.forEach((listener) => listener(msg));
  }

  private handleError(err: Error): void {
    this.errorListeners.forEach((listener) => listener(err));
  }

  async *recv(): AsyncIterable<OutboundMessage> {
    const queue: OutboundMessage[] = [];
    let resolver: ((value: IteratorResult<OutboundMessage>) => void) | null = null;

    const pushMessage = (msg: OutboundMessage) => {
      queue.push(msg);
      if (resolver) {
        resolver({ done: false, value: queue.shift()! });
        resolver = null;
      }
    };

    const errorHandler = (err: Error) => {
      if (resolver) {
        resolver({ done: true, value: null as any as OutboundMessage });
        resolver = null;
      }
    };

    this.messageListeners.push(pushMessage);
    this.errorListeners.push(errorHandler);

    try {
      while (this._connected) {
        if (queue.length > 0) {
          yield queue.shift()!;
        } else {
          yield await new Promise<OutboundMessage>((resolve) => {
            resolver = (result) => resolve(result.value);
          });
        }
      }
    } finally {
      const idx = this.messageListeners.indexOf(pushMessage);
      if (idx >= 0) {
        this.messageListeners.splice(idx, 1);
      }
      const errIdx = this.errorListeners.indexOf(errorHandler);
      if (errIdx >= 0) {
        this.errorListeners.splice(errIdx, 1);
      }
    }
  }

  async close(): Promise<void> {
    this._connected = false;
    return new Promise((resolve) => {
      if (this.socket.readyState === WebSocket.OPEN || this.socket.readyState === WebSocket.CONNECTING) {
        this.socket.close(1000, "Client closing");
      }
      // Wait a bit for close event
      setTimeout(resolve, 100);
    });
  }

  isConnected(): boolean {
    return this._connected;
  }

  /**
   * Add a listener for when the transport closes.
   */
  onClose(listener: (code?: number) => void): void {
    this.closeListeners.push(listener);
  }

  /**
   * Add a listener for messages.
   */
  onMessage(listener: (msg: OutboundMessage) => void): void {
    this.messageListeners.push(listener);
  }

  /**
   * Add a listener for errors.
   */
  onError(listener: (err: Error) => void): void {
    this.errorListeners.push(listener);
  }
}
