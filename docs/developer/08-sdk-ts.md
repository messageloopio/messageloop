# TypeScript SDK 指南

## 1. 概述

`@messageloop/sdk` 是 MessageLoop 的官方 TypeScript/JavaScript 客户端 SDK，面向浏览器与 Node.js 的 WebSocket 客户端，源码位于 `sdks/ts/`。当前版本为 `1.2.0`（见 `sdks/ts/package.json`）。

功能范围：

- WebSocket 客户端（浏览器原生 `WebSocket` 与 Node.js 双环境）
- 消息构造辅助：JSON、文本（text）、二进制（binary）三种载荷
- 频道订阅（默认等待服务端确认，见[订阅生效契约](#订阅生效契约)）与发布（含瞬时发布与确认发布）
- 订阅级 token、消息恢复（`recover` + 游标）与 Presence（事件、快照、查询）
- 频道级调查：`survey()` 发起、`onSurvey` / `onSurveyRequest` 应答
- GapNotice（历史空洞通知）与订阅重校验（`subRefresh`）
- RPC 请求/回复（`client.rpc`，服务端经 proxy 转发，见[《架构指南》](01-architecture.md)）
- 心跳（Ping/Pong，含服务端 Ping → 客户端 Pong）与断线自动重连、会话恢复（offset + epoch 语义）
- JSON 与 protobuf 两种线上编码

包同时输出 ESM（`dist/esm`）、CommonJS（`dist/cjs`）与类型声明（`dist/types`），并在 `exports` 中按 `import` / `require` 条件分发。运行时依赖为 `@bufbuild/protobuf`（`^2.0.0`）与 `ws`（`^8.0.0`，Node 18–20 无全局 `WebSocket` 时使用）。

与 [Go SDK 指南](07-sdk-go.md) 对应：两者共享同一份 `shared/genproto` 协议定义与线上协议（见[《客户端协议参考》](../protocol.md)）；本 SDK 目前仅实现 WebSocket 传输，不暴露 gRPC / QUIC / KCP 传输（见 Go SDK 的 `DialGRPC` / `DialQUIC` / `DialKCP`）。

## 2. 安装

```bash
npm install @messageloop/sdk
```

要求 Node.js `>=18.0.0`（`package.json` 的 `engines` 字段）。TypeScript 开发环境下需要 `typescript ^5.0.0` 及以上。

注意：`ws` 是运行依赖（`dependencies`）。`WebSocketTransport` 优先使用全局 `WebSocket`（`globalThis.WebSocket`），不存在时才动态 `import("ws")`。Node.js 18–20 默认没有全局 `WebSocket`，若在这些版本运行需保证 `ws` 可用（npm 安装本包时已自动带上）。

## 3. 快速开始

### Node.js

以下代码参考 `sdks/ts/examples/node/client.ts`（运行方式：`npx ts-node examples/node/client.ts`）：

```typescript
import {
  MessageLoopClient,
  createJSONMessage,
  setClientId,
  setAutoSubscribe,
  setToken,
  setEncoding,
} from "@messageloop/sdk";

async function main() {
  const client = await MessageLoopClient.dial("ws://localhost:9080/ws", [
    setClientId("node-client-001"),
    setAutoSubscribe("dev:chat.general", "dev:notifications"),
    setToken("your-auth-token"),
    setEncoding("json"),
  ]);

  console.log(`Connected with session: ${client.getSessionId()}`);

  client.onMessage((messages) => {
    for (const msg of messages) {
      console.log(`[${msg.channel}] ${msg.message.type}:`, msg.message.data);
    }
  });

  client.onError((err) => {
    console.error("Error:", err.message);
  });

  client.onClosed(() => {
    console.log("Connection closed");
  });

  // 订阅默认等待服务端 SubscribeAck
  await client.subscribe("dev:chat.dev", "dev:chat.random");

  const message = createJSONMessage("chat.message", {
    text: "Hello from Node.js SDK!",
    timestamp: new Date().toISOString(),
  });
  await client.publish("dev:chat.general", message);

  try {
    const rpcRequest = createJSONMessage("user.get", { userId: "12345" });
    const response = await client.rpc("dev:user.service", "GetUser", rpcRequest, {
      timeout: 5000,
    });
    console.log("RPC Response:", response.data);
  } catch (err) {
    console.log("RPC not available:", (err as Error).message);
  }

  await new Promise((resolve) => setTimeout(resolve, 5000));
  await client.close();
}

main().catch(console.error);
```

### 浏览器

参考 `sdks/ts/examples/browser/index.html`。该示例以原生 ES module 直接引用构建产物：

```html
<script type="module">
  import {
    MessageLoopClient,
    createJSONMessage,
    setClientId,
    setAutoSubscribe,
  } from '../dist/esm/index.js';

  const client = await MessageLoopClient.dial('ws://localhost:9080/ws', [
    setClientId('browser-' + Math.random().toString(36).slice(2, 11)),
    setAutoSubscribe('dev:chat.general'),
  ]);

  client.onMessage((messages) => {
    for (const msg of messages) {
      // msg.channel, msg.message.type, msg.message.data
    }
  });

  await client.publish(
    'dev:chat.general',
    createJSONMessage('chat.message', { text: 'Hello from browser!' })
  );
</script>
```

示例里的 `../dist/esm/index.js` 是相对于 SDK 仓库的路径，打开前必须先 `npm run build`。自己项目中的浏览器引用方式见第 8 节。

## 4. 客户端

核心类是 `MessageLoopClient`（`src/client/client.ts`），实现了 `IClient` 接口（`src/client/types.ts`）。

### 创建与连接

构造函数是私有的，只能通过工厂方法创建：

```typescript
static async dial(url: string, options?: ClientOption[]): Promise<MessageLoopClient>
```

`dial` 依次完成：建立 WebSocket 连接（超时 `connectTimeout`）→ 启动消息接收循环 → 发送 `Connect` 信封认证（携带 `clientId`、`clientType`、`token`、`version` 与订阅列表）→ 等待服务端 `Connected` 回复。连接失败或认证失败会抛出异常。

连接建立后，`sessionId` 可通过 `getSessionId(): string | null` 获取，状态可用 `getConnectionState()` 查询（`"disconnected" | "connecting" | "connected" | "reconnecting"`）。

### 生命周期方法

| 方法 | 说明 |
| --- | --- |
| `connect(): Promise<void>` | 发送 `Connect` 信封进行认证；重连场景下附带 `sessionId`、每频道游标（epoch + offset）用于会话恢复；`autoSubscribe` 频道随本帧建立订阅 |
| `close(): Promise<void>` | 主动关闭：停止重连与心跳、拒绝所有挂起的 RPC / Presence 查询 / Survey / 订阅 Ack 等待、关闭传输、触发 `onClosed` |
| `subscribe(...channels): Promise<void>` | 订阅一个或多个频道并**等待服务端 `SubscribeAck`**（见[订阅生效契约](#订阅生效契约)）；每个参数是频道名或 `SubscriptionSpec`（`{ channel, token?, recover?, cursor?, fresh? }`，见下文）。`subscribedChannels` 的本地记录在 Ack 到达后写入 |
| `subscribeAsync(...channels): Promise<void>` | 尽力而为订阅：写帧即 resolve，不等待 `SubscribeAck` |
| `unsubscribe(...channels): Promise<void>` | 取消订阅并**等待服务端 `UnsubscribeAck`**；本地记录与每频道恢复游标在 Ack 到达后才清除 |
| `unsubscribeAsync(...channels): Promise<void>` | 尽力而为退订：写帧即 resolve，不等待 `UnsubscribeAck` |
| `publish(channel: string, msg: Message, transient?: boolean): Promise<void>` | 向频道发布一条消息；`transient=true` 时以瞬时方式发布（服务端不写历史） |
| `publishWithAck(channel, msg, options?: { transient?, timeout? }): Promise<{ id, offset }>` | 发布并等待服务端 `PublishAck`，返回消息 id 与 broker 分配的 offset |
| `rpc(channel: string, method: string, request: Message, options?: { timeout?: number }): Promise<Message>` | 发起 RPC 请求，返回服务端回复载荷构造的 `Message`；超时（默认 `rpcTimeout`）或服务端返回错误时 reject |
| `subRefresh(...channels): Promise<void>` | ACL（如 token 轮换）变更后请服务端重新校验订阅；发送型方法，不等待 Ack |
| `presence(channel: string): Promise<PresenceSnapshot>` | 查询精确频道的当前 Presence 快照；失败时 reject（error 带服务端 `code`）；空/通配频道交给服务端拒绝 |
| `survey(channel: string, payload: Message \| null, timeoutMs?: number): Promise<SurveyAnswer[]>` | 发起频道级调查，等待聚合答案；`timeoutMs <= 0` 发 0 由服务端策略决定 |
| `isConnected(): boolean` | 是否已连接（别名 `isConnectedToServer()`） |
| `getSubscribedChannels(): string[]` | 当前订阅的频道列表 |
| `disableAutoReconnect()` / `enableAutoReconnect()` | 运行时开关自动重连 |

### 订阅生效契约

`subscribe()` / `unsubscribe()` 默认等待服务端 Ack（pending 按请求 Id 关联，服务端在 Ack 信封上回显该 Id）：**promise resolve 即服务端已注册 / 已移除对应订阅**，因此 `await` 之后的发布（哪怕来自另一条连接）不会再与在途订阅竞速导致消息静默丢失。规则与 Go SDK 一致：

- 等待受 `rpcTimeout`（`setRPCTimeout`，默认 30000ms）约束；`<= 0` 时完全不设定时器，仅由 Ack / 断连 / `close()` 解除等待。Ack 不到达时以 `Subscribe ack timeout after ${timeout}ms` / `Unsubscribe ack timeout after ${timeout}ms` reject。
- 服务端以回显请求 Id 的顶层 Error 信封拒绝时立即 reject（错误携带 `code`）；断连与 `close()` 会以 `Connection closed` reject 所有 in-flight 订阅/退订等待。
- `subscribeAsync()` / `unsubscribeAsync()` 是显式的尽力而为变体：请求写入传输即 resolve（fire-and-forget 语义），随后立即发布仍可能与订阅注册竞速。
- 重连后的内部重订阅（`resubscribeAllChannels`）发送的是 Subscribe 帧（fire-and-forget，不走 Ack 等待路径）；`autoSubscribe` 随 Connect 帧建立。两者都不存在「等待发生在接收循环里」的自锁。
- **不要在收包回调里同步 `await`** `subscribe()` / `unsubscribe()`（以及其他等待型调用）：等待发生在调用方 Promise 上，接收循环负责填充结果，在回调里同步等待会互相卡死。

### 订阅恢复参数（SubscriptionSpec）

`subscribe` 的对象参数字段（与 Go SDK 的 `WithRecover` / `WithFresh` 对应）：

- `recover?: boolean` —— `true` 时服务端在 `SubscribeAck` 之后以流式回放补发离线消息（`Publication(replay=true)` 逐条 + 每频道一条 `RecoverComplete`），回放走与普通消息相同的 `onMessage` 投递路径。
- `cursor?: { streamEpoch: string, offset?: bigint }` —— 恢复游标；`offset` 缺省表示「无提示」，由服务端按其记录的投递位置决定续读点。
- `fresh?: boolean` —— 显式从频道历史开头恢复。**没有「offset 0 = 从头」的语义**：`0n` 是一个具体的 offset；要从头请用 `fresh: true`。
- `token?: string` —— 订阅级 token。

纯字符串频道参数不触发恢复。订阅带 `recover` 时，服务端的 `RecoverComplete` 会回显每频道已确认游标，SDK 写入本地 offset 表供下次重连继续恢复；空批不会抹掉已知位置。

### 事件回调

| 方法 | 签名 | 说明 |
| --- | --- | --- |
| `onMessage` | `(handler: (messages: ReceivedMessage[]) => void) => void` | 接收一批消息（每次投递一个批次，内含 `id`、`channel`、`offset`、`replay` 与解码后的 `message`） |
| `onError` | `(handler: (error: Error) => void) => void` | 错误回调；连接错误会触发自动重连 |
| `onConnected` | `(handler: (sessionId: string) => void) => void` | 连接（或恢复）成功 |
| `onClosed` | `(handler: () => void) => void` | 连接关闭 |
| `onSurvey` | `(requestId: string, request: Message) => Message \| Promise<Message>` | 应答服务端发来的调查；无 handler 时默认把请求载荷原样 echo 回去 |
| `onSurveyRequest` | `(requestId: string, channel: string, request: Message) => Message \| Promise<Message>` | 同 `onSurvey`，额外携带请求频道；设置后优先于 `onSurvey` |
| `onPresence` | `(event: PresenceEvent) => void` | Presence 事件（join/leave）；未知 action 仍投递 |
| `onPresenceSnapshot` | `(snap: PresenceSnapshot) => void` | Presence 快照：`SubscribeAck.presence`、`presence()` 查询结果与连接后服务端主动推送的快照各触发一次 |
| `onGapNotice` | `(notice: GapNotice) => void` | 频道历史空洞通知：`channel`、`gapReason`、`streamEpoch`、`offset`（最后已知安全位置） |

以上九个是单处理器（重复设置会覆盖）。需要多处理器时使用：

- `addMessageHandler(handler): () => void` —— 追加消息处理器，返回移除该处理器的函数（`removeMessageHandler(handler)` 亦可）
- `addStateChangeHandler(handler: (event: ConnectionStateChangeEvent) => void): () => void` —— 监听连接状态迁移，事件含 `previousState` 与 `newState` 两个字段

## 5. 客户端选项

选项通过选项设置函数（option setter）传给 `dial`，均为 `(options: ClientOptions) => void` 类型的 `ClientOption`。全部设置函数导出自 `src/client/options.ts`：

| 设置函数 | 默认值 | 作用 |
| --- | --- | --- |
| `setEncoding(encoding: "json" \| "proto")` | `"json"` | 线上编码，决定使用的 Codec 与 WebSocket 子协议 |
| `setClientId(clientId: string)` | 自动生成的 UUID | 逻辑客户端标识，随 `Connect` 发送 |
| `setClientType(clientType: string)` | `"sdk"` | 客户端类型元数据（如 `"mobile"`、`"web"`） |
| `setToken(token: string)` | `""` | 认证令牌，随 `Connect` 发送 |
| `setVersion(version: string)` | `"2.0.0"` | 客户端版本元数据 |
| `setAutoSubscribe(...channels: string[])` | `[]` | 连接时自动订阅的频道，随 `Connect` 建立订阅 |
| `setPingInterval(interval: number)` | `30000` | 心跳间隔（毫秒），`0` 表示禁用 |
| `setPingTimeout(timeout: number)` | `10000` | Pong 超时（毫秒），超时视为断连 |
| `setConnectTimeout(timeout: number)` | `30000` | WebSocket 建连与 `Connected` 等待超时（毫秒） |
| `setRPCTimeout(timeout: number)` | `30000` | RPC 默认超时（毫秒），`rpc()` 可逐次覆盖；同时也是 `subscribe()` / `unsubscribe()` 等待 Ack 的默认预算 |
| `setEphemeral(ephemeral: boolean)` | `false` | 订阅是否标记为临时（ephemeral，不登记 presence） |
| `setAutoReconnect(enabled: boolean)` | `true` | 是否自动重连 |
| `setReconnectDelay(initial: number, max: number)` | `1000`, `30000` | 重连退避窗口（毫秒） |
| `setReconnectBackoff(initial: number, max: number, multiplier: number)` | `1000`, `30000`, `2` | 重连退避窗口与指数乘数 |
| `setReconnectMaxAttempts(attempts: number)` | `0` | 最大重连次数，`0` 表示无限 |

示例：

```typescript
const client = await MessageLoopClient.dial("ws://localhost:9080/ws", [
  setEncoding("proto"),
  setClientId("web-001"),
  setClientType("web"),
  setToken(process.env.TOKEN!),
  setAutoSubscribe("dev:chat.general"),
  setPingInterval(15000),
  setPingTimeout(5000),
  setConnectTimeout(15000),
  setRPCTimeout(60000),
  setEphemeral(true),
  setAutoReconnect(true),
  setReconnectBackoff(500, 10000, 2),
  setReconnectMaxAttempts(10),
]);
```

`buildClientOptions(setters)` 也在包内导出，可独立构造完整选项对象。

## 6. 消息 API

### 核心类型（`src/message/message.ts`）

```typescript
interface Data {
  contentType: string;                    // MIME 内容类型
  type: "json" | "binary" | "text";       // 数据种类判别器
  json?: Record<string, any>;
  binary?: Uint8Array;
  text?: string;
}

interface Message {
  id: string;                             // 唯一消息 ID（createMessage 自动生成）
  type: string;                           // 业务消息类型，如 "chat.message"
  data: Data;
  metadata?: Record<string, string>;
}

interface ReceivedMessage {
  id: string;
  channel: string;
  offset: bigint;                         // 频道内单调序号（bigint）
  offsetSet: boolean;                     // offset 是否有效
  replay: boolean;                        // 是否为恢复回放消息
  message: Message;                       // 解码后的载荷
}
```

### 构造辅助

| 函数 | 签名 | 说明 |
| --- | --- | --- |
| `createMessage` | `(type: string, data: Data) => Message` | 最底层构造，自动生成 `id` 并补空 `metadata` |
| `createJSONMessage` | `(type: string, json: Record<string, any>, contentType?: string) => Message` | JSON 载荷，默认 `contentType` 为 `application/json` |
| `createTextMessage` | `(type: string, text: string, contentType?: string) => Message` | 文本载荷，默认 `text/plain` |
| `createBinaryMessage` | `(type: string, binary: Uint8Array, contentType?: string) => Message` | 二进制载荷，默认 `application/octet-stream` |
| `createData` | `(contentType: string, value: unknown) => Data` | 按 content type 与值类型自动探测：JSON 内容优先，`text/*` 走文本，`Uint8Array` 走二进制，兜底 JSON 序列化 |
| `dataAs<T>` | `(msg: Message) => T` | 按数据种类解码：JSON 直接返回对象；binary/text 先尝试 `JSON.parse`，失败则原样返回 |

### 类型守卫

- `isJSONData(data: Data)`、`isBinaryData(data: Data)`、`isTextData(data: Data)` —— 分别收窄到 `json`、`binary`、`text` 分支。

### Payload 互转（`src/message/converters.ts`）

- `messageToPayload(msg: Message): Payload` / `payloadToMessage(payload: Payload, id: string, type?: string): Message` —— 与协议层的 `sharedpb.Payload`（json/binary/text 三态）互转，细节见 [../protocol.md](../protocol.md)。
- `generateMessageId(): string` —— 生成 `{unix纳秒}-{计数器}` 格式的 ID。
- `createConnectMessage`、`createSubscribeMessage`、`createUnsubscribeMessage`、`createPublishMessage`、`createRPCRequestMessage`、`createPingMessage`、`createPongMessage`、`createPresenceQueryMessage`、`createSurveyRequestMessage`、`createSurveyReplyMessage`、`createSubRefreshMessage` —— 信封构造器，返回 `InboundMessage`；`MessageLoopClient` 内部即使用这些构造器，高级场景可直接复用。
- `parseOutboundMessage(msg): { type, data, id }` —— 解析服务端 `OutboundMessage`，`type` 为信封类型判别。
- `extractRpcReply(reply)` —— 从 `RpcReply` 中提取 `requestId`、`payload` 与可选 `error { code, message }`。

### Presence 与 Survey 类型（`src/client/types.ts`）

SDK 不直接把 proto 的 `PresenceEvent` / `SurveyResult` 暴露为公共 API，而是包一层公共类型：

```typescript
interface PresenceInfo { sessionId: string; userId: string; clientId: string; connectedAt: bigint; }
interface PresenceEvent { channel: string; action: string; info: PresenceInfo; }   // action: "join" | "leave"
interface PresenceSnapshot { channel: string; clients: PresenceInfo[]; truncated: boolean; occupancy: number; }
interface SurveyAnswer { sessionId: string; userId: string; payload?: Message; error?: Error; }
interface GapNotice { channel: string; gapReason: string; streamEpoch: string; offset: bigint; offsetSet: boolean; }
```

- `SurveyAnswer.userId` 读自答案的 `metadata.entries["user_id"]`，缺失为 `""`。
- 调查结果整体失败时 `survey()` reject，error 携带服务端 `code`；若 `SurveyResult` 自身带 error，答案挂在被 reject 的 error 的 `answers` 属性上。

## 7. 传输与编码

### Transport 抽象（`src/transport/transport.ts`）

```typescript
interface Transport {
  send(msg: object): Promise<void>;
  recv(): AsyncIterable<OutboundMessage>;
  close(): Promise<void>;
  isConnected(): boolean;
}
```

### WebSocketTransport（`src/transport/websocket.ts`）

唯一内置实现，兼容浏览器原生 `WebSocket` 与 Node.js `ws`。构造器接受已建立的 socket 与一个 `Codec`；`WebSocketTransport.dial(url, codec, options?)` 负责建连，`options` 支持 `subprotocols`、`headers`（仅 Node.js 的 `ws`）与 `timeout`。发送走串行队列，接收通过 `recv()` 异步迭代器消费。

### Codec 与编码选择

`Codec` 接口（`src/transport/codec/codec.ts`）定义 `name()`、`encode()`、`decode()`、`useBytes()`。两个内置实现：

| Codec | `name()` | 帧格式 | 说明 |
| --- | --- | --- | --- |
| `JSONCodec`（`jsonCodec` 单例） | `messageloop+json` | 文本帧 | proto3 JSON 映射：出站用 proto 蛇形字段名（`useProtoFieldName`），入站忽略未知字段（`ignoreUnknownFields`）；`BigInt` 序列化为字符串 |
| `ProtobufCodec`（`protobufCodec` 单例） | `messageloop+proto` | 二进制帧（`useBytes()` 为 `true`） | 基于 `@bufbuild/protobuf` 的 `toBinary()` / `fromBinary()` |

编码通过 `setEncoding("json" | "proto")` 选择，默认 `"json"`。`codec.name()` 会作为 WebSocket 子协议在握手时协商（`Sec-WebSocket-Protocol`），与服务端子协议 `messageloop+json` / `messageloop+proto` 对应，见[《客户端协议参考》](../protocol.md) 的「传输协商」一节。

### 心跳、重连与会话恢复（`src/client/client.ts`）

- **心跳**：连接建立后按 `pingInterval` 发送 `Ping`，等待 `Pong`；超过 `pingTimeout` 未收到时 SDK 通知错误（`Pong timeout`）并触发断连处理（走重连流程）——注意 SDK 不会调用 `close()`，传输由断连路径关闭。
- **服务端 Ping**：收到服务端发来的 Outbound `Ping` 时，SDK 立即回一条携带相同 `id` 的 Inbound `Pong`，并当作存活证据（清掉客户端自己的 ping 超时计时），避免「服务端在探活、客户端却因自己的 pingTimeout 掐掉连接」。开启服务端 `server.heartbeat.ping_interval` 必须使用本版本（1.2.0+）SDK。
- **重连**：断连后按指数退避（`initial * multiplier^attempts`，封顶 `max`；默认 `setReconnectBackoff(1000, 30000, 2)`）自动重连，`reconnectMaxAttempts` 为 `0` 时无限重试。
- **会话恢复**：重连时的 `Connect` 会携带原 `sessionId`、当前 `epoch` 与各频道最后收到的 `offset`。服务端 `Connected` 回复 `resumed` 为 `false` 时，SDK 会对 `subscribedChannels` 全部重新订阅（发送 Subscribe 帧），每条带 `recover: true` + 已记录游标。`RecoverComplete` 会回写每频道已确认游标（空批不抹掉已知位置）。offset/epoch 的语义与恢复边界见[《架构指南》](01-architecture.md)。

## 8. 浏览器使用

- **打包方式**：包发布 ESM 与 CJS 双格式（`exports` 按 `import`/`require` 分发），推荐经 bundler（Vite、webpack、Rollup 等）引入 `@messageloop/sdk`。仓库示例为免构建的用法：以 `<script type="module">` 直接引用构建产物 `dist/esm/index.js`（见 `sdks/ts/examples/browser/index.html`），使用前需先 `npm run build`。
- **WebSocket 实现**：浏览器使用原生 `WebSocket`（`globalThis.WebSocket`），无需 `ws`。
- **连接地址**：示例连到 `ws://localhost:9080/ws`，端口为服务端 `transport.websocket.addr`（见[《配置参考》](02-configuration.md)）。
- **与 Node 的差异**：浏览器环境受限于原生 WebSocket（不支持自定义 header）；`crypto.randomUUID()` 需要安全上下文（HTTPS 或 localhost）；`ReceivedMessage.offset` 为 `bigint`，JSON 编码下会被序列化为字符串。

## 9. 错误处理

SDK 层的错误形态均为原生 `Error`，来源与附加信息如下（`src/client/client.ts`）：

- **服务端错误信封**：`Error` 并附加 `code` 与 `type` 属性（取自协议错误字段），经 `onError` 回调分发。
- **RPC 错误**：服务端 `RpcReply.error` 时 reject，错误对象带 `code`（字符串）；RPC 超时 reject `RPC timeout after ${timeout}ms`。
- **订阅/退订 Ack 错误**：`subscribe()` / `unsubscribe()` 的 Ack 不到达时按 `rpcTimeout` reject（`Subscribe ack timeout after ${timeout}ms` / `Unsubscribe ack timeout after ${timeout}ms`）；服务端拒绝（匹配请求 Id 的 Error 信封）、断连或 `close()` 时立即 reject。
- **Presence / Survey 错误**：`presence()` / `survey()` 被服务端拒绝时 reject，错误对象带服务端 `code`；断连或 `close()` 时挂起的查询/调查/订阅等待以 `Connection closed` reject。未连接时调用两者直接 reject `Not connected`。
- **连接问题**：未连接时调用发送类方法抛 `Not connected`；`dial` 失败直接抛出（超时为 `Connection timeout`，WebSocket 层为 `WebSocket connection failed` 等）。
- **心跳超时**：`Pong timeout`，随后走断连处理。

连接期间发生的错误默认不会终止客户端：`onError` 触发后，若处于 `connected` 状态且开启自动重连，会进入重连流程；应用可调用 `disableAutoReconnect()` 停止重试，或 `close()` 彻底关闭。

**断开码（disconnect code）**：服务端关闭连接时以 WebSocket close 帧携带数字断开码（如 `3000` ConnectionClosed、`3503` ForceNoReconnect、`3500` InvalidToken、`3514` UnsupportedVersion 等，完整表见[《可观测性指南》](05-observability.md) 的断连码参考与[《客户端协议参考》](../protocol.md)）。SDK 不把 close code 映射为类型化错误，也不会依据断开码调整重连策略；若服务端强制断开（如令牌失效、协议版本过旧），需应用层自行通过 `onClosed` / `addStateChangeHandler` 感知并决定是否 `disableAutoReconnect()`。

## 10. 构建与测试

```bash
npm install       # 安装依赖
npm run build     # 依次构建 ESM（dist/esm）、CJS（dist/cjs）与类型声明（dist/types）
npm test          # Jest 测试（ts-jest，测试位于 test/）
```

- 构建由三个 `tsc` 调用完成：`build:esm`、`build:cjs`、`build:types`。
- 测试用 Jest（preset `ts-jest`，roots 为 `test/`），覆盖客户端选项构造（`client.test.ts`）、编解码（`codec.test.ts`）、协议行为（`protocol.test.ts` / `regression.test.ts`）、订阅生效契约（`subscribe_ack.test.ts`）、GapNotice（`gapnotice.test.ts`）与 Presence/Survey 能力（`pr09.test.ts`）。
- `src/proto/` 下的代码由 buf 生成，不要手工编辑；开发流程与 Protobuf 工作流见[《开发指南》](06-development.md) 的「TypeScript SDK 开发」与「Protobuf 工作流」两节。

## 11. 发布

执行 `task release-sdk-ts`：清理 `dist` → `npm run build` → `npm publish --access public`（发布到 `https://registry.npmjs.org/`）。npm 包版本（`package.json` 的 `version`，当前 `1.2.0`）独立于 Go 侧的 git 标签，需要手动递增，详见[《开发指南》](06-development.md) 的「发布流程」一节。
