import type { OutboundMessage } from "../proto/client/v2/service_pb";
import type { Transport } from "./transport";
import type { Codec } from "./codec/codec";

// WebSocket.readyState constants, defined locally so no global WebSocket is
// required (Node < 21 has no global WebSocket).
const WS_CONNECTING = 0;
const WS_OPEN = 1;
const WS_CLOSING = 2;
const WS_CLOSED = 3;

// Maximum number of queued send operations. When the queue is full, send()
// rejects immediately so callers see backpressure instead of unbounded growth.
const MAX_SEND_QUEUE = 1024;

type WSSocket = WebSocket | import("ws").WebSocket;

type WSConstructor = new (
  url: string,
  protocols?: string | string[],
  options?: { headers?: Record<string, string> }
) => WSSocket;

/**
 * WebSocket transport implementation.
 * Compatible with both Node.js (ws) and browser (native WebSocket).
 */
export class WebSocketTransport implements Transport {
  private socket: WSSocket;
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
  constructor(socket: WSSocket, codec: Codec) {
    this.socket = socket;
    this.codec = codec;

    // In browsers, binary frames arrive as Blob unless binaryType is set.
    // ws (Node.js) accepts "arraybuffer" as well, so setting it uniformly is
    // safe; decode() also handles Blob for sockets created outside dial().
    socket.binaryType = "arraybuffer";

    // Check if socket is already open (happens when created via dial())
    // WebSocket.OPEN = 1 (both browser and ws library use the same value)
    if (socket.readyState === WS_OPEN) {
      this._connected = true;
    }

    // Set up message handler - use any type for cross-environment compatibility
    this.socket.onmessage = async (event: any) => {
      try {
        // binaryType "arraybuffer" delivers raw ArrayBuffer frames; normalize
        // them to Uint8Array before handing them to the codec.
        const raw = event.data as Uint8Array | string | Blob | ArrayBuffer;
        const data = raw instanceof ArrayBuffer ? new Uint8Array(raw) : raw;
        const outboundMsg = await this.codec.decode(data);
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
      // Reject any send operations still waiting in the queue: without this a
      // caller awaiting publish() would hang forever after an unexpected close.
      const pending = this.sendQueue.splice(0);
      this.isSending = false;
      for (const item of pending) {
        item.reject(new Error("WebSocket is not connected"));
      }
      this.closeListeners.forEach((listener) => listener(event?.code));
    };

    // Set up open handler (for sockets that are still connecting)
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
    let WebSocketClass: WSConstructor;

    if (typeof globalThis.WebSocket !== "undefined") {
      WebSocketClass = globalThis.WebSocket as unknown as WSConstructor;
    } else {
      const ws = await import("ws");
      WebSocketClass = ws.WebSocket as unknown as WSConstructor;
    }

    const subprotocol = codec.name();
    // Headers are honored by ws (Node.js); the browser WebSocket constructor
    // ignores the third argument, so headers are a Node-only feature.
    const socket = new WebSocketClass(
      url,
      subprotocol ? [subprotocol] : undefined,
      options?.headers ? { headers: options.headers } : undefined
    );

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
    if (this.sendQueue.length >= MAX_SEND_QUEUE) {
      throw new Error("WebSocket send queue is full");
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
    try {
      while (this.sendQueue.length > 0) {
        const item = this.sendQueue.shift()!;
        try {
          const data = this.codec.encode(item.msg);
          this.socket.send(data);
          item.resolve();
        } catch (err) {
          item.reject(err instanceof Error ? err : new Error(String(err)));
        }
      }
    } finally {
      this.isSending = false;
    }
  }

  private handleMessage(msg: OutboundMessage): void {
    this.messageListeners.forEach((listener) => listener(msg));
  }

  private handleError(err: Error): void {
    this.errorListeners.forEach((listener) => listener(err));
  }

  async *recv(): AsyncIterable<OutboundMessage> {
    const queue: OutboundMessage[] = [];
    let resolver: ((result: IteratorResult<OutboundMessage>) => void) | null = null;
    let error: Error | null = null;
    let closed = false;

    const pushMessage = (msg: OutboundMessage) => {
      if (resolver) {
        const r = resolver;
        resolver = null;
        r({ done: false, value: msg });
      } else {
        queue.push(msg);
      }
    };

    const errorHandler = (err: Error) => {
      error = err;
      if (resolver) {
        const r = resolver;
        resolver = null;
        r({ done: true, value: undefined });
      }
    };

    const closeHandler = () => {
      closed = true;
      if (resolver) {
        const r = resolver;
        resolver = null;
        r({ done: true, value: undefined });
      }
    };

    this.messageListeners.push(pushMessage);
    this.errorListeners.push(errorHandler);
    this.closeListeners.push(closeHandler);

    try {
      while (true) {
        // Deliver buffered messages before surfacing close/error so no
        // received data is lost.
        if (queue.length > 0) {
          yield queue.shift()!;
          continue;
        }
        if (error) {
          throw error;
        }
        if (closed || !this._connected) {
          throw new Error("Connection closed");
        }
        const result = await new Promise<IteratorResult<OutboundMessage>>((resolve) => {
          resolver = resolve;
        });
        if (result.done) {
          throw error ?? new Error("Connection closed");
        }
        yield result.value;
      }
    } finally {
      const msgIdx = this.messageListeners.indexOf(pushMessage);
      if (msgIdx >= 0) {
        this.messageListeners.splice(msgIdx, 1);
      }
      const errIdx = this.errorListeners.indexOf(errorHandler);
      if (errIdx >= 0) {
        this.errorListeners.splice(errIdx, 1);
      }
      const closeIdx = this.closeListeners.indexOf(closeHandler);
      if (closeIdx >= 0) {
        this.closeListeners.splice(closeIdx, 1);
      }
    }
  }

  async close(): Promise<void> {
    this._connected = false;

    // Reject any send operations still waiting in the queue so callers do not
    // hang forever on publish()/subscribe() after close.
    const pending = this.sendQueue.splice(0);
    this.isSending = false;
    for (const item of pending) {
      item.reject(new Error("WebSocket is not connected"));
    }

    const state = this.socket.readyState;
    if (state === WS_CLOSED) {
      return;
    }

    if (state === WS_OPEN || state === WS_CONNECTING) {
      this.socket.close(1000, "Client closing");
    }

    // Resolve on the real close event with a timeout fallback so close() can
    // never hang even if the peer never acknowledges the close handshake.
    return new Promise((resolve) => {
      let settled = false;
      const finish = () => {
        if (settled) return;
        settled = true;
        clearTimeout(timeoutId);
        const idx = this.closeListeners.indexOf(onClose);
        if (idx >= 0) {
          this.closeListeners.splice(idx, 1);
        }
        resolve();
      };
      const onClose = () => finish();
      const timeoutId = setTimeout(finish, 1000);
      this.closeListeners.push(onClose);
    });
  }

  isConnected(): boolean {
    return this._connected;
  }

  /**
   * Add a listener for when the transport closes. Returns a disposer.
   */
  onClose(listener: (code?: number) => void): () => void {
    this.closeListeners.push(listener);
    return () => {
      const idx = this.closeListeners.indexOf(listener);
      if (idx >= 0) {
        this.closeListeners.splice(idx, 1);
      }
    };
  }

  /**
   * Add a listener for messages. Returns a disposer.
   */
  onMessage(listener: (msg: OutboundMessage) => void): () => void {
    this.messageListeners.push(listener);
    return () => {
      const idx = this.messageListeners.indexOf(listener);
      if (idx >= 0) {
        this.messageListeners.splice(idx, 1);
      }
    };
  }

  /**
   * Add a listener for errors. Returns a disposer.
   */
  onError(listener: (err: Error) => void): () => void {
    this.errorListeners.push(listener);
    return () => {
      const idx = this.errorListeners.indexOf(listener);
      if (idx >= 0) {
        this.errorListeners.splice(idx, 1);
      }
    };
  }
}
