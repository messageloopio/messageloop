import {
  MessageLoopClient,
  createJSONMessage,
  createTextMessage,
  setAutoReconnect,
  setAutoSubscribe,
  setClientId,
  setClientType,
  setEncoding,
  setPingInterval,
  setPingTimeout,
  setReconnectBackoff,
  setReconnectMaxAttempts,
  setRPCTimeout,
  setToken,
  setVersion,
  type PresenceEvent,
  type PresenceSnapshot,
  type ReceivedMessage,
} from "@messageloop/sdk";

// ---------------------------------------------------------------------------
// DOM references

const $ = <T extends HTMLElement>(id: string): T =>
  document.getElementById(id) as T;

const userSel = $<HTMLSelectElement>("user");
const roomInput = $<HTMLInputElement>("room");
const connectBtn = $<HTMLButtonElement>("connect");
const disconnectBtn = $<HTMLButtonElement>("disconnect");
const statusEl = $<HTMLSpanElement>("status");
const messagesEl = $<HTMLDivElement>("messages");
const usersEl = $<HTMLUListElement>("users");
const inputEl = $<HTMLInputElement>("input");
const sendBtn = $<HTMLButtonElement>("send");

// ---------------------------------------------------------------------------
// ChatRoom state

interface ChatPayload {
  user?: string;
  text?: string;
  kind?: string;
}

const WS_URL = `ws://${location.hostname}:9080/ws`;
let client: MessageLoopClient | null = null;
let currentRoom = "chat:lobby";

// ---------------------------------------------------------------------------
// UI helpers

function appendMessage(channel: string, offset: bigint, payload: ChatPayload, raw: string): void {
  const div = document.createElement("div");
  div.className = "msg";
  if (payload.kind === "system") div.classList.add("system");
  else if (payload.kind === "whisper") div.classList.add("whisper");
  else if (payload.kind === "poll") div.classList.add("poll");

  const meta = document.createElement("span");
  meta.className = "meta";
  meta.textContent = `[${channel}#${offset}]`;
  div.appendChild(meta);

  const who = document.createElement("span");
  who.className = "who";
  who.textContent = payload.user || "?";
  div.appendChild(who);
  div.appendChild(document.createTextNode(payload.text || raw));

  messagesEl.appendChild(div);
  messagesEl.scrollTop = messagesEl.scrollHeight;
}

function renderUsers(snap: { channel: string; clients: { userId: string; clientId: string }[] }): void {
  usersEl.replaceChildren();
  const names = snap.clients.map((c) => c.userId || c.clientId);
  if (names.length === 0) {
    const li = document.createElement("li");
    li.textContent = "nobody yet";
    usersEl.appendChild(li);
    return;
  }
  for (const name of names) {
    const li = document.createElement("li");
    li.textContent = name;
    usersEl.appendChild(li);
  }
}

function setStatus(text: string, online: boolean): void {
  statusEl.textContent = text;
  statusEl.className = online ? "online" : "offline";
}

// ---------------------------------------------------------------------------
// Connection

async function connect(): Promise<void> {
  const name = userSel.value;
  client = await MessageLoopClient.dial(WS_URL, [
    setClientId(`web-${name}-${Math.random().toString(36).slice(2, 8)}`),
    setClientType("web"),
    setToken(`token-${name}`),
    setVersion("chatroom/1.0.0"),
    setEncoding("json"),
    setAutoSubscribe(currentRoom),
    setAutoReconnect(true),
    setReconnectBackoff(500, 10000, 2),
    setReconnectMaxAttempts(10),
    setRPCTimeout(10000),
    setPingInterval(30000),
    setPingTimeout(10000),
  ]);

  client.onMessage((msgs: ReceivedMessage[]) => {
    for (const msg of msgs) {
      let payload: ChatPayload = {};
      if (msg.message.data.type === "json" && msg.message.data.json) {
        payload = msg.message.data.json as ChatPayload;
      } else if (msg.message.data.type === "text") {
        payload = { text: msg.message.data.text };
      }
      appendMessage(msg.channel, msg.offset, payload, msg.message.data.text ?? "");
    }
  });

  client.onPresence((ev: PresenceEvent) => {
    const action = ev.action === "leave" ? "left" : "joined";
    appendMessage(ev.channel, 0n, { kind: "system", text: `${ev.info.userId || ev.info.clientId} ${action}` }, "");
  });

  client.onPresenceSnapshot((snap: PresenceSnapshot) => {
    if (snap.channel) renderUsers(snap);
  });

  client.onSurveyRequest(async (_requestId, _channel, request) => {
    const question =
      request.data.type === "json" && request.data.json
        ? String(request.data.json.text ?? "?")
        : request.data.text ?? "?";
    const answer = window.prompt(`Survey: ${question}\nYour answer:`) ?? "no answer";
    return createTextMessage("chat.poll.answer", answer);
  });

  client.onConnected((sessionId: string) => {
    setStatus(`connected (${sessionId.slice(0, 8)}…)`, true);
    appendMessage("system", 0n, { kind: "system", user: "system", text: `connected as ${name}` }, "");
  });
  client.onError((err: Error) => {
    setStatus(`error: ${err.message}`, false);
  });
  client.onClosed(() => {
    setStatus("disconnected", false);
  });

  connectBtn.disabled = true;
  disconnectBtn.disabled = false;
  roomInput.disabled = true;
}

async function disconnect(): Promise<void> {
  await client?.close();
  client = null;
  connectBtn.disabled = false;
  disconnectBtn.disabled = true;
  roomInput.disabled = false;
  setStatus("offline", false);
  usersEl.replaceChildren();
}

// ---------------------------------------------------------------------------
// Commands

async function sendLine(line: string): Promise<void> {
  if (!client || !client.isConnected()) {
    appendMessage("system", 0n, { kind: "system", text: "not connected" }, "");
    return;
  }
  const name = userSel.value;
  const [cmd, ...restArr] = line.trim().split(/\s+/);
  const rest = restArr.join(" ");

  try {
    switch (cmd) {
      case "/join": {
        if (!rest) return error("usage: /join <room>");
        await client.subscribe(rest);
        currentRoom = rest;
        roomInput.value = rest;
        return info(`joined ${rest}`);
      }
      case "/leave":
        if (!rest) return error("usage: /leave <room>");
        await client.unsubscribe(rest);
        return info(`left ${rest}`);
      case "/roll":
      case "/stats":
      case "/history":
      case "/kick":
      case "/whoami": {
        const resp = await client.rpc(currentRoom, cmd, createTextMessage("chat.rpc", rest));
        const text = resp.data.type === "text" ? resp.data.text : JSON.stringify(resp.data.json);
        return appendMessage("rpc", 0n, { kind: "whisper", user: "backend", text }, "");
      }
      case "/presence": {
        const snap = await client.presence(currentRoom);
        renderUsers(snap);
        return info(`${snap.clients.length} online in ${snap.channel}`);
      }
      case "/poll": {
        if (!rest) return error("usage: /poll <question>");
        const answers = await client.survey(currentRoom, createJSONMessage("chat.poll", { user: name, kind: "poll", text: rest }), 5000);
        const lines = answers
          .map((a) => `${a.userId || a.sessionId}: ${a.payload?.data.text ?? ""}`)
          .join("\n");
        return appendMessage("survey", 0n, { kind: "poll", user: "poll", text: `${rest}\n${lines}` }, "");
      }
      case "/whisper": {
        if (!rest) return error("usage: /whisper <text>");
        await client.publish(currentRoom, createJSONMessage("chat.message", { user: name, kind: "whisper", text: rest }), true);
        return;
      }
      case "/sys": {
        if (!rest) return error("usage: /sys <text>");
        const ack = await client.publishWithAck(currentRoom, createJSONMessage("chat.message", { user: name, kind: "system", text: rest }));
        return info(`published at offset ${ack.offset}`);
      }
      case "/refresh":
        await client.subRefresh(currentRoom);
        return info("subscriptions re-validated");
      case "/help":
        return info("commands: /join /leave /roll /stats /history /kick /whoami /presence /poll /whisper /sys /refresh");
      default:
        if (cmd.startsWith("/")) return error(`unknown command ${cmd}, try /help`);
        await client.publish(currentRoom, createJSONMessage("chat.message", { user: name, kind: "chat", text: line.trim() }));
    }
  } catch (err) {
    appendMessage("system", 0n, { kind: "system", text: `error: ${(err as Error).message}` }, "");
  }
}

function info(text: string): void {
  appendMessage("system", 0n, { kind: "system", user: "system", text }, "");
}

function error(text: string): void {
  appendMessage("system", 0n, { kind: "system", text }, "");
}

// ---------------------------------------------------------------------------
// Wiring

connectBtn.addEventListener("click", () => {
  currentRoom = roomInput.value.trim() || "chat:lobby";
  connect().catch((err) => setStatus(`connect failed: ${err.message}`, false));
});
disconnectBtn.addEventListener("click", () => void disconnect());
sendBtn.addEventListener("click", () => {
  const line = inputEl.value.trim();
  if (!line) return;
  inputEl.value = "";
  void sendLine(line);
});
inputEl.addEventListener("keydown", (ev) => {
  if (ev.key === "Enter") {
    const line = inputEl.value.trim();
    if (!line) return;
    inputEl.value = "";
    void sendLine(line);
  }
});
