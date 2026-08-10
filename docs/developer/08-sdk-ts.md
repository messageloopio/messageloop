# TypeScript SDK 指南

## 1. 概述

`@messageloop/sdk` 是 MessageLoop 的官方 TypeScript/JavaScript 客户端 SDK，面向浏览器与 Node.js 的 WebSocket 客户端，源码位于 `sdks/ts/`。当前版本为 `1.0.5`（见 `sdks/ts/package.json`）。

功能范围：

- WebSocket 客户端（浏览器原生 `WebSocket` 与 Node.js 双环境）
- 消息构造辅助：JSON、文本（text）、二进制（binary）三种载荷
- 频道订阅、取消订阅与发布
- RPC 请求/回复（`client.rpc`，服务端经 proxy 转发，见 [architecture.md](architecture.md)）
- 心跳（Ping/Pong）与断线自动重连、会话恢复（offset + epoch 语义，见 [architecture.md](architecture.md) 第 3.4 节）
- JSON 与 protobuf 两种线上编码

**不支持 Survey**：`parseOutboundMessage` 虽能识别 `surveyRequest` / `surveyReply` 信封，但 `MessageLoopClient` 不处理这两种消息，SDK 也未暴露 survey 相关 API。

包同时输出 ESM（`dist/esm`）、CommonJS（`dist/cjs`）与类型声明（`dist/types`），并在 `exports` 中按 `import` / `require` 条件分发。运行时依赖仅 `@bufbuild/protobuf`（`^2.0.0`）。

与 [Go SDK 指南](sdk-go.md) 对应：两者共享同一份 `shared/genproto` 协议定义与线上协议（见 [../protocol.md](../protocol.md)）；本 SDK 目前仅实现 WebSocket 传输，不暴露 gRPC 传输。

## 2. 安装

```bash
npm install @messageloop/sdk
```

要求 Node.js `>=18.0.0`（`package.json` 的 `engines` 字段）。TypeScript 开发环境下需要 `typescript ^5.0.0` 及以上。

注意两点：

- `@grpc/grpc-js`（`^1.9.0`）声明为 peer 依赖，但当前 SDK 不含 gRPC 传输实现，普通 WebSocket 使用无需安装。
- `ws` 包位于 `devDependencies` 而非运行依赖。`WebSocketTransport` 优先使用全局 `WebSocket`（`globalThis.WebSocket`），不存在时才动态 `import("ws")`。Node.js 18–20 默认没有全局 `WebSocket`，若在这些版本运行需自行安装 `ws`。

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
    setAutoSubscribe("chat.general", "notifications"),
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

  await client.subscribe("chat.dev", "chat.random");

  const message = createJSONMessage("chat.message", {
    text: "Hello from Node.js SDK!",
    timestamp: new Date().toISOString(),
  });
  await client.publish("chat.general", message);

  try {
    const rpcRequest = createJSONMessage("user.get", { userId: "12345" });
    const response = await client.rpc("user.service", "GetUser", rpcRequest, {
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
    setClientId('browser-' + Math.random().toString(36).substr(2, 9)),
    setAutoSubscribe('chat.general'),
  ]);

  client.onMessage((messages) => {
    for (const msg of messages) {
      // msg.channel, msg.message.type, msg.message.data
    }
  });

  await client.publish(
    'chat.general',
    createJSONMessage('chat.message', { text: 'Hello from browser!' })
  );
</script>
```

示例里的 `../dist/esm/index.js` 是相对于 SDK 仓库的路径，**打开前必须先 `npm run build`**。自己项目中的浏览器引用方式见第 8 节。

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
| `connect(): Promise<void>` | 发送 `Connect` 信封进行认证；重连场景下附带 `sessionId`、每频道 `offset` 与 `epoch` 用于会话恢复 |
| `close(): Promise<void>` | 主动关闭：停止重连与心跳、拒绝所有挂起的 RPC、关闭传输、触发 `onClosed` |
| `subscribe(...channels): Promise<void>` | 订阅一个或多个频道，并记录进 `subscribedChannels`（重连后自动恢复） |
| `unsubscribe(...channels): Promise<void>` | 取消订阅 |
| `publish(channel: string, msg: Message): Promise<void>` | 向频道发布一条消息 |
| `rpc(channel: string, method: string, request: Message, options?: { timeout?: number }): Promise<Message>` | 发起 RPC 请求，返回服务端回复载荷构造的 `Message`；超时（默认 `rpcTimeout`）或服务端返回错误时 reject |
| `isConnected(): boolean` | 是否已连接（别名 `isConnectedToServer()`） |
| `getSubscribedChannels(): string[]` | 当前订阅的频道列表 |
| `disableAutoReconnect()` / `enableAutoReconnect()` | 运行时开关自动重连 |

### 事件回调

| 方法 | 签名 | 说明 |
| --- | --- | --- |
| `onMessage` | `(handler: (messages: ReceivedMessage[]) => void) => void` | 接收一批消息（每次投递一个批次，内含 `id`、`channel`、`offset` 与解码后的 `message`） |
| `onError` | `(handler: (error: Error) => void) => void` | 错误回调；连接错误会触发自动重连 |
| `onConnected` | `(handler: (sessionId: string) => void) => void` | 连接（或恢复）成功 |
| `onClosed` | `(handler: () => void) => void` | 连接关闭 |

以上四个是单处理器（重复设置会覆盖）。需要多处理器时使用：

- `addMessageHandler(handler): () => void` —— 追加消息处理器，返回移除该处理器的函数（`removeMessageHandler(handler)` 亦可）
- `addStateChangeHandler(handler: (event: ConnectionStateChangeEvent) => void): () => void` —— 监听连接状态迁移，事件含 `previousState` 与 `newState` 两个字段

## 5. 客户端选项

选项通过**选项设置函数**（option setter）传给 `dial`，均为 `(options: ClientOptions) => void` 类型的 `ClientOption`。全部设置函数导出自 `src/client/options.ts`：

| 设置函数 | 默认值 | 作用 |
| --- | --- | --- |
| `setEncoding(encoding: "json" \| "proto")` | `"json"` | 线上编码，决定使用的 Codec 与 WebSocket 子协议 |
| `setClientId(clientId: string)` | 自动生成的 UUID | 逻辑客户端标识，随 `Connect` 发送 |
| `setClientType(clientType: string)` | `"sdk"` | 客户端类型元数据（如 `"mobile"`、`"web"`） |
| `setToken(token: string)` | `""` | 认证令牌，随 `Connect` 发送 |
| `setVersion(version: string)` | `"1.0.0"` | 客户端版本元数据 |
| `setAutoSubscribe(...channels: string[])` | `[]` | 连接时自动订阅的频道，随 `Connect` 建立订阅 |
| `setPingInterval(interval: number)` | `30000` | 心跳间隔（毫秒），`0` 表示禁用 |
| `setPingTimeout(timeout: number)` | `10000` | Pong 超时（毫秒），超时视为断连 |
| `setConnectTimeout(timeout: number)` | `30000` | WebSocket 建连与 `Connected` 等待超时（毫秒） |
| `setRPCTimeout(timeout: number)` | `30000` | RPC 默认超时（毫秒），`rpc()` 可逐次覆盖 |
| `setEphemeral(ephemeral: boolean)` | `false` | 订阅是否标记为临时（ephemeral） |
| `setAutoReconnect(enabled: boolean)` | `true` | 是否自动重连 |
| `setReconnectDelay(initial: number, max: number)` | `1000`, `30000` | 重连退避窗口（毫秒） |
| `setReconnectMaxAttempts(attempts: number)` | `0` | 最大重连次数，`0` 表示无限 |

示例：

```typescript
const client = await MessageLoopClient.dial("ws://localhost:9080/ws", [
  setEncoding("proto"),
  setClientId("web-001"),
  setClientType("web"),
  setToken(process.env.TOKEN!),
  setAutoSubscribe("chat.general"),
  setPingInterval(15000),
  setPingTimeout(5000),
  setConnectTimeout(15000),
  setRPCTimeout(60000),
  setEphemeral(true),
  setAutoReconnect(true),
  setReconnectDelay(500, 10000),
  setReconnectMaxAttempts(10),
]);
```

重连退避为指数退避，乘数为 2（`reconnectBackoffMultiplier`），该值当前没有对应的 setter，只能在 `ClientOptions` 里定制（通过 `buildClientOptions` 组合）。`buildClientOptions(setters)` 也在包内导出，可独立构造完整选项对象。

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
  id: string;                             // 唯一消息 ID（createMessage 自动生成 UUID）
  type: string;                           // 业务消息类型，如 "chat.message"
  data: Data;
  metadata?: Record<string, string>;
}

interface ReceivedMessage {
  id: string;
  channel: string;
  offset: bigint;                         // 频道内单调序号（bigint）
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
- `createConnectMessage`、`createSubscribeMessage`、`createUnsubscribeMessage`、`createPublishMessage`、`createRPCRequestMessage`、`createPingMessage`、`createSubRefreshMessage` —— 信封构造器，返回 `InboundMessage`；`MessageLoopClient` 内部即使用这些构造器，高级场景可直接复用。
- `parseOutboundMessage(msg): { type, data, id }` —— 解析服务端 `OutboundMessage`，`type` 为信封类型判别。
- `extractRpcReply(reply)` —— 从 `RpcReply` 中提取 `requestId`、`payload` 与可选 `error { code, message }`。

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
| `JSONCodec`（`jsonCodec` 单例） | `messageloop+json` | 文本帧 | proto3 JSON 映射：入站用蛇形字段名（如 `subscribe_ack`），出站做 `envelope.case` ↔ 字段名转换；`BigInt` 序列化为字符串 |
| `ProtobufCodec`（`protobufCodec` 单例） | `messageloop+proto` | 二进制帧（`useBytes()` 为 `true`） | 基于 `@bufbuild/protobuf` 的 `toBinary()` / `fromBinary()` |

编码通过 `setEncoding("json" | "proto")` 选择，默认 `"json"`。`codec.name()` 会作为 WebSocket 子协议在握手时协商（`Sec-WebSocket-Protocol`），与服务端子协议 `messageloop+json` / `messageloop+proto` 对应，见 [../protocol.md](../protocol.md) 的「传输协商」一节。

### 心跳与会话恢复（`src/client/client.ts`）

- **心跳**：连接建立后按 `pingInterval` 发送 `Ping`，等待 `Pong`；超过 `pingTimeout` 未收到即触发错误并关闭连接（进而走重连）。
- **重连**：断连后按指数退避（`reconnectInitialDelay * 2^attempts`，封顶 `reconnectMaxDelay`）自动重连，`reconnectMaxAttempts` 为 `0` 时无限重试。
- **会话恢复**：重连时的 `Connect` 会携带原 `sessionId`、当前 `epoch` 与各频道最后收到的 `offset`（`recover: true`）。服务端 `Connected` 回复 `resumed` 为 `false` 时，SDK 会对 `subscribedChannels` 全部重新订阅。offset/epoch 的语义与恢复边界见 [architecture.md](architecture.md) 第 3.4 节。

## 8. 浏览器使用

- **打包方式**：包发布 ESM 与 CJS 双格式（`exports` 按 `import`/`require` 分发），推荐经 bundler（Vite、webpack、Rollup 等）引入 `@messageloop/sdk`。仓库示例为免构建的用法：以 `<script type="module">` 直接引用构建产物 `dist/esm/index.js`（见 `sdks/ts/examples/browser/index.html`），使用前需先 `npm run build`。
- **WebSocket 实现**：浏览器使用原生 `WebSocket`（`globalThis.WebSocket`），无需安装 `ws`。
- **连接地址**：示例连到 `ws://localhost:9080/ws`，端口为服务端 `transport.websocket.addr`（见 [configuration.md](configuration.md)）。
- **与 Node 的差异**：浏览器环境受限于原生 WebSocket（不支持自定义 header）；`crypto.randomUUID()` 需要安全上下文（HTTPS 或 localhost）；`ReceivedMessage.offset` 为 `bigint`，JSON 编码下会被序列化为字符串。

## 9. 错误处理

SDK 层的错误形态均为原生 `Error`，来源与附加信息如下（`src/client/client.ts`）：

- **服务端错误信封**：`Error` 并附加 `code` 与 `type` 属性（取自协议错误字段），经 `onError` 回调分发。
- **RPC 错误**：服务端 `RpcReply.error` 时 reject，错误对象带 `code`（字符串）；RPC 超时 reject `RPC timeout after ${timeout}ms`。
- **连接问题**：未连接时调用发送类方法抛 `Not connected`；`dial` 失败直接抛出（超时为 `Connection timeout`，WebSocket 层为 `WebSocket connection failed` 等）。
- **心跳超时**：`Pong timeout`，随后关闭连接。

连接期间发生的错误默认不会终止客户端：`onError` 触发后，若处于 `connected` 状态且开启自动重连，会进入重连流程；应用可调用 `disableAutoReconnect()` 停止重试，或 `close()` 彻底关闭。

**断开码（disconnect code）**：服务端关闭连接时以 WebSocket close 帧携带数字断开码（如 `3000` ConnectionClosed、`3503` ForceNoReconnect、`3500` InvalidToken 等，完整表见 [../protocol.md](../protocol.md) 的「Disconnect Codes」一节）。SDK 不把 close code 映射为类型化错误，也不会依据断开码调整重连策略；若服务端强制断开（如令牌失效），需应用层自行通过 `onClosed` / `addStateChangeHandler` 感知并决定是否 `disableAutoReconnect()`。

## 10. 构建与测试

```bash
npm install       # 安装依赖
npm run build     # 依次构建 ESM（dist/esm）、CJS（dist/cjs）与类型声明（dist/types）
npm test          # Jest 测试（ts-jest，测试位于 test/）
npm run lint      # ESLint 检查 src/
```

- 构建由三个 `tsc` 调用完成：`build:esm`、`build:cjs`、`build:types`。
- 测试用 Jest（preset `ts-jest`，roots 为 `test/`），目前覆盖客户端选项构造（`test/client.test.ts`）与编解码（`test/codec.test.ts`）两组用例。
- `src/proto/` 下的代码由 buf 生成，不要手工编辑；开发流程与 Protobuf 工作流见 [development.md](development.md) 的「TypeScript SDK 开发」与「Protobuf 工作流」两节。

## 11. 发布

执行 `task release-sdk-ts`：清理 `dist` → `npm run build` → `npm publish --access public`（发布到 `https://registry.npmjs.org/`）。npm 包版本（`package.json` 的 `version`）独立于 Go 侧的 git 标签，需要手动递增，详见 [development.md](development.md) 的「发布流程」一节。
