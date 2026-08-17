import type { OutboundMessage } from "../proto/client/v2/service_pb";
import type { SurveyRequest } from "../proto/client/v2/service_pb";
import type { ReceivedMessage, Message } from "../message";
import type { ChannelOrSpec } from "../message";
import type { Transport } from "../transport/transport";
import type { Codec } from "../transport/codec/codec";
import type { ClientOptions, ClientOption } from "./options";
import type {
  ConnectionState,
  ConnectionStateChangeEvent,
  IClient,
  PresenceEvent,
  PresenceSnapshot,
  SurveyAnswer,
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
  createPongMessage,
  createPresenceQueryMessage,
  createSurveyRequestMessage,
  createSubRefreshMessage,
  createSurveyReplyMessage,
  parseOutboundMessage,
  payloadToMessage,
  createMessage,
  generateMessageId,
  presenceEventFromPB,
  presenceSnapshotFromPB,
  surveyAnswerFromPB,
} from "../message";

/**
 * Top-level error codes the server may use to reject a client survey without
 * echoing the request id (asynchronous worker failures).
 */
const surveyRejectCodes = new Set([
  "SURVEY_DISABLED",
  "SURVEY_TOO_MANY_SUBSCRIBERS",
  "BAD_REQUEST",
  "PERMISSION_DENIED",
  "RATE_LIMITED",
  "INTERNAL_ERROR",
]);

/**
 * MessageLoop client for connecting to the messaging server.
 */
export class MessageLoopClient implements IClient {
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

  // Connect waiters (waitForConnection), settled by Connected or error envelope
  private connectWaitReject: ((err: Error) => void) | null = null;
  private connectError: Error | null = null;

  // Multi-handler support using Sets
  private messageHandlers: Set<(messages: ReceivedMessage[]) => void> =
    new Set();
  private stateChangeHandlers: Set<(event: ConnectionStateChangeEvent) => void> =
    new Set();

  // Legacy handlers (for backward compatibility)
  private messageHandler: ((messages: ReceivedMessage[]) => void) | null = null;
  private errorHandler: ((error: Error) => void) | null = null;
  private connectedHandler: ((sessionId: string) => void) | null = null;
  private closedHandler: (() => void) | null = null;

  // Subscriptions: channel -> optional subscription token
  private subscribedChannels: Map<string, string> = new Map();

  // RPC pending requests
  private pendingRPC: Map<
    string,
    { resolve: (msg: Message) => void; reject: (err: Error) => void }
  > = new Map();

  // Publish pending acks (publishWithAck)
  private pendingPublish: Map<
    string,
    {
      timer: ReturnType<typeof setTimeout>;
      resolve: (ack: { id: string; offset: bigint }) => void;
      reject: (err: Error) => void;
    }
  > = new Map();

  // Survey request handler; when unset survey requests are echoed back
  private surveyHandler: ((
    requestId: string,
    request: Message
  ) => Message | Promise<Message>) | null = null;

  // Survey request handler with the request channel; when set it takes
  // precedence over surveyHandler.
  private surveyRequestHandler: ((
    requestId: string,
    channel: string,
    request: Message
  ) => Message | Promise<Message>) | null = null;

  // Presence handlers
  private presenceHandler: ((event: PresenceEvent) => void) | null = null;
  private presenceSnapshotHandler: ((snap: PresenceSnapshot) => void) | null =
    null;

  // Presence query pending replies, keyed by the inbound message id
  private pendingPresence: Map<
    string,
    { resolve: (snap: PresenceSnapshot) => void; reject: (err: Error) => void }
  > = new Map();

  // Client-initiated surveys, keyed by the inbound message id; resolved by a
  // SurveyResult carrying the generated request id or by a top-level error.
  private pendingSurvey: Map<
    string,
    {
      requestId: string;
      resolve: (answers: SurveyAnswer[]) => void;
      reject: (err: Error) => void;
    }
  > = new Map();

  // Ping/Pong
  private pingTimer: ReturnType<typeof setInterval> | null = null;
  private pingTimeoutTimer: ReturnType<typeof setTimeout> | null = null;

  // Reconnection
  private reconnectAttempts = 0;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private isReconnecting = false;

  // Session resumption state
  private epoch: string = "";
  private channelOffsets: Map<string, bigint> = new Map();

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
      this.subscribedChannels.set(channel, "");
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

      // Send Connect message to authenticate
      await client.connect();

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
   * Rejected by the Connected state, an error envelope received while
   * connecting, the client being closed, or the connect timeout.
   */
  private waitForConnection(): Promise<void> {
    return new Promise((resolve, reject) => {
      let settled = false;
      const settle = (fn: (...args: any[]) => void) => (...args: any[]) => {
        if (settled) return;
        settled = true;
        clearTimeout(timeout);
        this.connectWaitReject = null;
        fn(...args);
      };

      const timeout = setTimeout(
        settle(() => reject(new Error("Connection timeout"))),
        this.options.connectTimeout
      );

      this.connectWaitReject = settle((err: Error) => reject(err));

      const checkConnection = () => {
        if (settled) return;
        if (this.connectError) {
          const err = this.connectError;
          this.connectError = null;
          settle(() => reject(err))();
        } else if (this.isConnectedFlag) {
          settle(() => resolve())();
        } else if (this.isClosedFlag) {
          settle(() => reject(new Error("Connection closed")))();
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
        this.epoch = parsed.data.streamEpoch || "";
        const resumed = parsed.data.resumed || false;
        this.isConnectedFlag = true;
        this.reconnectAttempts = 0;
        this.isReconnecting = false;
        this.connectWaitReject = null;
        this.connectError = null;

        // The server's Connected.subscriptions is authoritative: replace the
        // local map unconditionally (even with an empty list) so channels
        // dropped server-side are forgotten before resubscribeAllChannels.
        // When the server omits the subscription token (e.g. an older
        // broker), fall back to the locally known token so resubscribes keep
        // it across reconnects.
        const serverSubs: { channel: string; token: string }[] = (
          parsed.data.subscriptions || []
        )
          .filter(
            (sub: any) =>
              sub && typeof sub.channel === "string" && sub.channel.length > 0
          )
          .map((sub: any) => ({
            channel: sub.channel,
            token:
              typeof sub.token === "string" && sub.token
                ? sub.token
                : this.subscribedChannels.get(sub.channel) || "",
          }));
        this.subscribedChannels = new Map(
          serverSubs.map((sub) => [sub.channel, sub.token])
        );

        // v2 delivers no recovery batches on Connected: any recovery replay
        // arrives as streamed Publication envelopes (replay=true) followed by
        // one RecoverComplete per channel after the bare Connected frame.
        // Presence snapshots ride separate Presence envelopes.

        // Update connection state
        const wasReconnecting = this.connectionState === "reconnecting";
        this.setConnectionState("connected");

        // Resubscribe to channels after reconnection only if session was not resumed
        if (wasReconnecting && !resumed && this.subscribedChannels.size > 0) {
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

      case "subscribeAck": {
        // Keep/update the local subscription set. When the server omits the
        // subscription token, fall back to the locally known token (same
        // rule as the Connected handler).
        const ackSubs: { channel: string; token: string }[] = (
          parsed.data.subscriptions || []
        )
          .filter(
            (sub: any) =>
              sub && typeof sub.channel === "string" && sub.channel.length > 0
          )
          .map((sub: any) => ({
            channel: sub.channel,
            token:
              typeof sub.token === "string" && sub.token
                ? sub.token
                : this.subscribedChannels.get(sub.channel) || "",
          }));
        for (const sub of ackSubs) {
          this.subscribedChannels.set(sub.channel, sub.token);
        }

        // v2 delivers no recovery batches on SubscribeAck: the per-channel
        // replay stream (Publication replay=true + RecoverComplete) follows
        // the bare ack. Presence snapshots still ride the ack; dispatch them
        // after the subscription write-back above.
        for (const snap of parsed.data.presence || []) {
          this.dispatchPresenceSnapshot(presenceSnapshotFromPB(snap));
        }
        break;
      }

      case "publication": {
        this.deliverMessages(parsed.data.messages || []);
        break;
      }

      case "rpcReply": {
        const id = parsed.id;
        const pending = this.pendingRPC.get(id);
        if (pending) {
          this.pendingRPC.delete(id);
          const reply = parsed.data;
          if (reply.error) {
            const err = new Error(reply.error.message || "RPC error");
            (err as any).code = reply.error.code;
            pending.reject(err);
          } else {
            const respMsg = reply.payload ? payloadToMessage(reply.payload, id) : createMessage("rpc.reply", { contentType: "", type: "binary" });
            pending.resolve(respMsg);
          }
        }
        break;
      }

      case "publishAck": {
        // The server echoes the publish message id on the envelope and in
        // PublishAck.id: resolve the matching pending publishWithAck. Only a
        // set position offset is authoritative; an unset position (transient
        // / no-history) yields 0n.
        const pending = this.pendingPublish.get(parsed.id);
        if (pending) {
          this.pendingPublish.delete(parsed.id);
          clearTimeout(pending.timer);
          pending.resolve({
            id: parsed.data.id,
            offset: parsed.data.position?.offset ?? 0n,
          });
        }
        break;
      }

      case "recoverComplete": {
        // RecoverComplete echoes the authoritative position for exactly one
        // recovered channel: write it back so the next reconnect resumes from
        // the server-confirmed cursor. An unset position never creates or
        // wipes a cursor ("0 means from the start" is forbidden).
        const rc = parsed.data;
        if (rc && rc.channel && rc.position && rc.position.offset !== undefined) {
          this.channelOffsets.set(rc.channel, rc.position.offset);
        }
        break;
      }

      case "presence": {
        // A same-id snapshot resolves a pending PresenceQuery; snapshot
        // replies without a pending match (pushed after Connected) are simply
        // dispatched to onPresenceSnapshot.
        const pending = this.pendingPresence.get(parsed.id);
        if (pending) {
          this.pendingPresence.delete(parsed.id);
          const snap = presenceSnapshotFromPB(parsed.data);
          this.dispatchPresenceSnapshot(snap);
          pending.resolve(snap);
        } else if (parsed.data) {
          this.dispatchPresenceSnapshot(presenceSnapshotFromPB(parsed.data));
        }
        break;
      }

      case "presenceEvent": {
        // Unknown actions are still delivered.
        if (this.presenceHandler) {
          this.presenceHandler(presenceEventFromPB(parsed.data));
        }
        break;
      }

      case "ping": {
        // The server probes us: answer with a Pong carrying the same id
        // (an empty id still gets a Pong) and treat the exchange as
        // liveness: clear the client's own pong deadline so a server that
        // is actively probing is not killed by our pingTimeout.
        const pong = createPongMessage(parsed.id);
        this.send(pong).catch(() => {
          // Ignore send failures on the pong path
        });
        if (this.pingTimeoutTimer) {
          clearTimeout(this.pingTimeoutTimer);
          this.pingTimeoutTimer = null;
        }
        break;
      }

      case "surveyResult": {
        this.handleSurveyResult(parsed.data);
        break;
      }

      case "surveyRequest": {
        this.handleSurveyRequest(parsed.data);
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

        // The server echoes the request id on error envelopes: route to the
        // pending RPC so the caller fails fast instead of waiting for the
        // rpcTimeout.
        if (parsed.id) {
          const pending = this.pendingRPC.get(parsed.id);
          if (pending) {
            this.pendingRPC.delete(parsed.id);
            pending.reject(error);
            break;
          }
        }

        // Fail the pending presence query with the matching id, if any.
        if (parsed.id && this.pendingPresence.has(parsed.id)) {
          const pending = this.pendingPresence.get(parsed.id)!;
          this.pendingPresence.delete(parsed.id);
          pending.reject(error);
          break;
        }

        // Fail the pending survey with the matching id, if any. When the
        // error has no id and the code is a survey rejection code, deliver
        // it to the single in-flight survey (the server allows one in-flight
        // survey per session).
        if (parsed.id) {
          const pending = this.pendingSurvey.get(parsed.id);
          if (pending) {
            this.pendingSurvey.delete(parsed.id);
            pending.reject(error);
            break;
          }
        } else if (
          this.pendingSurvey.size === 1 &&
          surveyRejectCodes.has(parsed.data.code)
        ) {
          for (const [id, pending] of this.pendingSurvey) {
            this.pendingSurvey.delete(id);
            pending.reject(error);
          }
          break;
        }

        if (this.connectionState === "connecting") {
          // Fail the pending connect with the real reason (e.g. invalid token)
          // instead of a generic 30s "Connection timeout".
          this.notifyError(error);
          if (this.connectWaitReject) {
            this.connectWaitReject(error);
          } else {
            this.connectError = error;
          }
          break;
        }

        // Connected state: this is an application-level error (ACL denial,
        // server-side RPC failure, ...). Notify the handler but do not tear
        // down the connection.
        this.notifyError(error);
        break;
      }
    }
  }

  /**
   * Convert messages to ReceivedMessage format, track per-channel offsets for
   * session resumption (live messages only), and deliver them to all
   * registered handlers. Replay and live messages share this single consumer
   * path (§5); a replayed run waits for the RecoverComplete position instead
   * of advancing the cursor mid-replay.
   */
  private deliverMessages(msgs: Array<{
    id: string;
    channel: string;
    position?: { streamEpoch: string; offset?: bigint };
    replay?: boolean;
    payload?: any;
  }>): void {
    const messages: ReceivedMessage[] = [];
    for (const m of msgs) {
      // Live (non-replay) messages advance the per-channel cursor; replayed
      // messages let RecoverComplete set it.
      if (!m.replay && m.position?.offset !== undefined && m.channel) {
        this.channelOffsets.set(m.channel, m.position.offset);
      }
      messages.push({
        id: m.id,
        channel: m.channel,
        offset: m.position?.offset ?? 0n,
        offsetSet: m.position?.offset !== undefined,
        replay: m.replay || false,
        message: m.payload ? payloadToMessage(m.payload, m.id) : createMessage("messageloop.message", { contentType: "", type: "binary" }),
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
  }

  /**
   * Dispatch one already-converted presence snapshot to the handler, if any.
   */
  private dispatchPresenceSnapshot(snap: PresenceSnapshot): void {
    if (!this.presenceSnapshotHandler) return;
    this.presenceSnapshotHandler(snap);
  }

  /**
   * Route a SurveyResult envelope to the pending survey with the matching
   * request id, if any, and remove the entry. Results that arrive after the
   * pending survey was cleaned up (close, disconnect) are dropped. When the
   * SurveyResult itself carries an error, the answers (if any) are attached
   * to the rejected error's `answers` property.
   */
  private handleSurveyResult(result: any): void {
    if (!result || !result.requestId) return;

    for (const [id, pending] of this.pendingSurvey) {
      if (pending.requestId === result.requestId) {
        this.pendingSurvey.delete(id);
        const answers = (result.answers || []).map(surveyAnswerFromPB);
        if (result.error) {
          const err = new Error(result.error.message || "Survey error");
          (err as any).code = result.error.code;
          (err as any).answers = answers;
          pending.reject(err);
        } else {
          pending.resolve(answers);
        }
        return;
      }
    }
  }

  /**
   * Reject all pending presence queries and surveys: the connection is gone,
   * no reply will arrive.
   */
  private rejectPendingPresenceAndSurveys(): void {
    for (const [_, pending] of this.pendingPresence) {
      pending.reject(new Error("Connection closed"));
    }
    this.pendingPresence.clear();
    for (const [_, pending] of this.pendingSurvey) {
      pending.reject(new Error("Connection closed"));
    }
    this.pendingSurvey.clear();
  }

  /**
   * Notify the registered error handler only.
   */
  private notifyError(err: Error): void {
    if (this.errorHandler) {
      this.errorHandler(err);
    }
  }

  /**
   * Handle a survey request from the server: dispatch to the onSurveyRequest
   * handler (with the request channel), fall back to the onSurvey handler
   * (without channel), or echo the request payload back when no handler is
   * set (the SDK answering side).
   */
  private handleSurveyRequest(req: SurveyRequest): void {
    const requestId = req.requestId;
    const channel = req.channel || "";
    const reqMsg = req.payload
      ? payloadToMessage(req.payload, "")
      : createMessage("messageloop.message", { contentType: "", type: "binary" });

    if (this.surveyRequestHandler) {
      Promise.resolve()
        .then(() => this.surveyRequestHandler!(requestId, channel, reqMsg))
        .then((reply) => this.sendSurveyReply(requestId, reply, null))
        .catch((err) =>
          this.sendSurveyReply(requestId, null, err instanceof Error ? err : new Error(String(err)))
        );
      return;
    }

    if (!this.surveyHandler) {
      this.sendSurveyReply(requestId, reqMsg, null).catch(() => {
        // Ignore send failures on the default echo path
      });
      return;
    }

    Promise.resolve()
      .then(() => this.surveyHandler!(requestId, reqMsg))
      .then((reply) => this.sendSurveyReply(requestId, reply, null))
      .catch((err) =>
        this.sendSurveyReply(requestId, null, err instanceof Error ? err : new Error(String(err)))
      );
  }

  /**
   * Send a SurveyReply for the given request id.
   * @param reply - Reply message, or null when the reply carries an error.
   * @param replyErr - Optional error carried in the reply's error field.
   */
  private async sendSurveyReply(
    requestId: string,
    reply: Message | null,
    replyErr: Error | null
  ): Promise<void> {
    const msg = createSurveyReplyMessage(
      requestId,
      reply,
      replyErr
        ? {
            code: "SURVEY_REPLY_ERROR",
            type: "survey_error",
            message: replyErr.message,
          }
        : undefined
    );
    await this.send(msg);
  }

  /**
   * Handle an error.
   */
  private handleError(err: Error): void {
    this.notifyError(err);

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

    // Reject pending publish acks: the connection is gone, no ack will arrive.
    for (const [_, pending] of this.pendingPublish) {
      clearTimeout(pending.timer);
      pending.reject(new Error("Connection closed"));
    }
    this.pendingPublish.clear();

    // Reject pending presence queries and surveys: the connection is gone,
    // no reply will arrive.
    this.rejectPendingPresenceAndSurveys();

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

      // The client may have been closed while dialing: do not resurrect it.
      if (this.isClosedFlag) {
        await transport.close();
        this.isReconnecting = false;
        return;
      }

      this.transport = transport;
      this.startMessageLoop();

      // Send connect message and wait for Connected (with timeout); a missing
      // reply falls through to the next retry instead of hanging forever.
      await this.connect();
      await this.waitForConnection();
    } catch {
      this.isReconnecting = false;
      // Schedule next attempt
      if (this.autoReconnectEnabled && !this.isClosedFlag) {
        this.attemptReconnect();
      }
    }
  }

  /**
   * Resubscribe to all channels after reconnection. The session was not
   * resumed, so every channel is re-subscribed with recover=true and the
   * recorded per-channel cursor so the server replays messages missed while
   * disconnected (Go SDK resumeSubscriptions parity).
   */
  private async resubscribeAllChannels(): Promise<void> {
    if (this.subscribedChannels.size === 0) return;

    const channels: ChannelOrSpec[] = Array.from(
      this.subscribedChannels,
      ([channel, token]) => {
        const offset = this.channelOffsets.get(channel);
        return {
          channel,
          token,
          recover: true,
          cursor:
            offset !== undefined
              ? { streamEpoch: this.epoch, offset }
              : undefined,
        };
      }
    );
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

      // Set up pong timeout: a missed pong means the connection is dead, but
      // the client must survive via reconnection — never close() it (that
      // would set the closed flag and kill the reconnect machinery).
      this.pingTimeoutTimer = setTimeout(() => {
        this.pingTimeoutTimer = null;
        this.notifyError(new Error("Pong timeout"));
        this.handleDisconnect();
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

    // Build subscription list with recovery info when reconnecting. recover
    // matches the Go SDK: always true when reconnecting. A recorded
    // per-channel cursor resumes from that point; without one the server
    // falls back to its own recorded delivered position (or skips). No
    // "offset 0 means from the start".
    const subs = Array.from(this.subscribedChannels).map(([channel, token]) => {
      const offset = this.channelOffsets.get(channel);
      return {
        channel,
        ephemeral: this.options.ephemeral,
        token,
        recover: this.isReconnecting,
        cursor:
          this.isReconnecting && offset !== undefined
            ? { streamEpoch: this.epoch, offset }
            : undefined,
      };
    });

    const connectMsg = createConnectMessage(
      this.options.clientId,
      this.options.clientType,
      this.options.token,
      this.options.version,
      subs,
      this.isReconnecting ? (this.sessionId || undefined) : undefined
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

    // Reject any pending connect waiters
    this.connectWaitReject = null;
    this.connectError = null;

    // Reject pending RPC requests
    for (const [_, pending] of this.pendingRPC) {
      pending.reject(new Error("Connection closed"));
    }
    this.pendingRPC.clear();

    // Reject pending publish acks
    for (const [_, pending] of this.pendingPublish) {
      clearTimeout(pending.timer);
      pending.reject(new Error("Connection closed"));
    }
    this.pendingPublish.clear();

    // Reject pending presence queries and surveys
    this.rejectPendingPresenceAndSurveys();

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
   * @param channels - Channel names, or SubscriptionSpec objects carrying an
   * optional per-channel token and recovery fields (e.g.
   * `{ channel: "ch1", token: "t1", recover: true, offset: 7n, epoch: "ep" }`).
   */
  async subscribe(...channels: ChannelOrSpec[]): Promise<void> {
    const msg = createSubscribeMessage(channels, this.options.ephemeral);
    await this.send(msg);

    // Add to subscribed channels
    for (const channel of channels) {
      const spec = typeof channel === "string" ? { channel } : channel;
      this.subscribedChannels.set(spec.channel, spec.token || "");
    }
  }

  /**
   * Unsubscribe from one or more channels.
   * @param channels - Channel names, or SubscriptionSpec objects.
   */
  async unsubscribe(...channels: ChannelOrSpec[]): Promise<void> {
    const msg = createUnsubscribeMessage(channels);
    await this.send(msg);

    // Remove from subscribed channels and clear per-channel offsets so a
    // later resubscribe + reconnect does not replay stale history.
    for (const channel of channels) {
      const name = typeof channel === "string" ? channel : channel.channel;
      this.subscribedChannels.delete(name);
      this.channelOffsets.delete(name);
    }
  }

  /**
   * Publish a message to a channel.
   * @param transient - When true, skip persistence and only deliver to currently connected subscribers.
   */
  async publish(
    channel: string,
    msg: Message,
    transient: boolean = false
  ): Promise<void> {
    const pbMsg = createPublishMessage(channel, msg, transient);
    await this.send(pbMsg);
  }

  /**
   * Publish a message and await the server's PublishAck.
   * @param options.transient - When true, skip persistence and only deliver to currently connected subscribers.
   * @param options.timeout - Ack timeout in milliseconds (defaults to the RPC timeout).
   * @returns The publish message id and the channel offset at which it was stored.
   */
  async publishWithAck(
    channel: string,
    msg: Message,
    options?: { transient?: boolean; timeout?: number }
  ): Promise<{ id: string; offset: bigint }> {
    const pbMsg = createPublishMessage(channel, msg, options?.transient ?? false);
    const id = pbMsg.id;

    return new Promise((resolve, reject) => {
      // Set up timeout
      const timeout = options?.timeout ?? this.options.rpcTimeout;
      const timeoutId = setTimeout(() => {
        this.pendingPublish.delete(id);
        reject(new Error(`Publish ack timeout after ${timeout}ms`));
      }, timeout);

      // Store pending request
      this.pendingPublish.set(id, {
        timer: timeoutId,
        resolve: (ack: { id: string; offset: bigint }) => {
          clearTimeout(timeoutId);
          resolve(ack);
        },
        reject: (err: Error) => {
          clearTimeout(timeoutId);
          reject(err);
        },
      });

      // Send request
      this.send(pbMsg).catch((err) => {
        clearTimeout(timeoutId);
        this.pendingPublish.delete(id);
        reject(err);
      });
    });
  }

  /**
   * Make an RPC request to a channel.
   */
  async rpc(
    channel: string,
    method: string,
    request: Message,
    options?: { timeout?: number }
  ): Promise<Message> {
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
        resolve: (respMsg: Message) => {
          clearTimeout(timeoutId);
          resolve(respMsg);
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
   * Ask the server to re-validate the subscriptions for the given channels
   * (e.g. after an ACL change on the backend). subRefreshAck needs no
   * special handling, mirroring the Go SDK.
   */
  async subRefresh(...channels: string[]): Promise<void> {
    const msg = createSubRefreshMessage(
      channels.map((channel) => ({ channel, token: "" }))
    );
    await this.send(msg);
  }

  /**
   * Query the current presence snapshot of an exact channel. The server
   * replies with a single snapshot matched by this query's id, which is
   * returned and also dispatched to the onPresenceSnapshot handler. An empty
   * or wildcard channel is handed to the server, which rejects it
   * (BAD_REQUEST); failures surface as an error carrying the server
   * code/message. The wait happens on the caller's promise, so it must not
   * be awaited synchronously from receive-loop callbacks (onMessage,
   * onPresence, onSurvey*, onPresenceSnapshot). Close and disconnect reject
   * the pending query.
   */
  presence(channel: string): Promise<PresenceSnapshot> {
    if (!this.transport || !this.isConnectedFlag) {
      return Promise.reject(new Error("Not connected"));
    }

    const msg = createPresenceQueryMessage(channel);
    const id = msg.id;

    return new Promise((resolve, reject) => {
      // Register the pending query before sending so the reply can never be
      // missed between the send and the registration.
      this.pendingPresence.set(id, { resolve, reject });

      this.send(msg).catch((err) => {
        this.pendingPresence.delete(id);
        reject(err);
      });
    });
  }

  /**
   * Initiate a survey on an exact channel and wait for the aggregated
   * answers. A timeoutMs <= 0 sends 0 and lets the server apply its policy
   * cap. The wait happens on the caller's promise: the receive loop fills
   * the pending result, so this must not be awaited synchronously from
   * receive-loop callbacks (onMessage, onPresence*, onSurvey*).
   *
   * Completion conditions, first match wins:
   * - a SurveyResult carrying the generated request id;
   * - a top-level error whose id equals this request's inbound id
   *   (synchronous rejections such as SURVEY_DISABLED);
   * - a top-level error without a matchable id whose code is a survey
   *   rejection code, when exactly one survey() is in flight (server worker
   *   failures may not echo the request id; the server allows one in-flight
   *   survey per session).
   *
   * When the SurveyResult itself carries an error, the answers (if any) are
   * attached to the rejected error's `answers` property. Close and
   * disconnect reject the pending survey.
   */
  survey(
    channel: string,
    payload: Message | null,
    timeoutMs?: number
  ): Promise<SurveyAnswer[]> {
    if (!this.transport || !this.isConnectedFlag) {
      return Promise.reject(new Error("Not connected"));
    }

    const requestId = generateMessageId();
    const msg = createSurveyRequestMessage(requestId, channel, payload, timeoutMs);
    const id = msg.id;

    return new Promise((resolve, reject) => {
      // Register the pending survey before sending so the result can never
      // be missed between the send and the registration.
      this.pendingSurvey.set(id, { requestId, resolve, reject });

      this.send(msg).catch((err) => {
        this.pendingSurvey.delete(id);
        reject(err);
      });
    });
  }

  /**
   * Register the handler for survey requests from the server. The handler
   * receives the request id and the decoded request message and returns the
   * reply message; the reply is sent back with the request id. When the
   * handler throws (or rejects), an error reply is sent instead. When no
   * handler is registered, the request payload is echoed back unchanged.
   */
  onSurvey(
    handler: (
      requestId: string,
      request: Message
    ) => Message | Promise<Message>
  ): void {
    this.surveyHandler = handler;
  }

  /**
   * Register the handler for survey requests from the server, additionally
   * receiving the request channel. When set it takes precedence over the
   * handler registered with onSurvey. When no handler at all is registered,
   * the request payload is echoed back unchanged.
   */
  onSurveyRequest(
    handler: (
      requestId: string,
      channel: string,
      request: Message
    ) => Message | Promise<Message>
  ): void {
    this.surveyRequestHandler = handler;
  }

  /**
   * Register the handler for presence events (join/leave). Unknown actions
   * are still delivered.
   */
  onPresence(handler: (event: PresenceEvent) => void): void {
    this.presenceHandler = handler;
  }

  /**
   * Register the handler for presence snapshots delivered with Connected /
   * SubscribeAck, and for the snapshot returned by a presence() query.
   */
  onPresenceSnapshot(handler: (snap: PresenceSnapshot) => void): void {
    this.presenceSnapshotHandler = handler;
  }

  /**
   * Set the message handler.
   * Convenience alias for a single message slot: prefer addMessageHandler,
   * and do not mix both on the same client (messages would be delivered to
   * each, duplicating delivery).
   */
  onMessage(handler: (messages: ReceivedMessage[]) => void): void {
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
    return Array.from(this.subscribedChannels.keys());
  }

  // ========== Multi-handler API ==========

  /**
   * Add a message handler. Returns a function to remove the handler.
   * Recommended over onMessage: supports multiple handlers and disposers.
   */
  addMessageHandler(
    handler: (messages: ReceivedMessage[]) => void
  ): () => void {
    this.messageHandlers.add(handler);
    return () => this.removeMessageHandler(handler);
  }

  /**
   * Remove a message handler.
   */
  removeMessageHandler(handler: (messages: ReceivedMessage[]) => void): void {
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
