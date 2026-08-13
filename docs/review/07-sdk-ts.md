# 评审任务 07：TypeScript SDK

## 背景

你要评审的项目是 **MessageLoop**（工作目录：`D:/Codes/qiulin/messageloop`），一个用 Go 编写的实时消息平台服务端，通过 WebSocket 和 gRPC 双向流提供 pub/sub 消息能力。先读根目录 `AGENTS.md` 了解整体架构。

你是独立评审 agent，没有任何先前上下文。**只做只读评审，不修改任何代码。**

## 评审范围（TypeScript SDK）

- `sdks/ts/src/`：`client/client.ts`（`MessageLoopClient`）、`client/options.ts`、`transport/transport.ts`、`transport/websocket.ts`、`transport/codec/`（JSON/Protobuf codec）、`message/message.ts`、`types.ts`
- `sdks/ts/test/`、`sdks/ts/examples/`、`sdks/ts/package.json`、`tsconfig*.json`、`jest.config.js`
- 协议契约来源：`protocol/client/v1/service.proto`（TS 生成代码在 `sdks/ts/sdks/` 或 buf 输出目录，自行确认）
- 参考文档：`docs/developer/08-sdk-ts.md`、`sdks/ts/README.md`

## 模块职责与关键契约（供定位，需你自行通读验证）

- TS SDK 只提供 WebSocket 客户端，目标覆盖浏览器与 Node.js。
- 协议流：发 `InboundMessage`（Connect/Subscribe/Publish/RpcRequest/Ping），收 `OutboundMessage`。
- 单线程事件循环：`WebSocketTransport` 用 `sendQueue` + `isSending` 串行发送；`recv()` 是 async generator 靠 Promise resolver 推送。
- `connectTimeout`/`rpcTimeout`/`pingTimeout` 可配；pong timeout 触发 `handleError` + `close()`；自动重连默认开启。

## 评审维度

1. **协议契约对等性**：与服务端（根包 `client.go`）及 Go SDK（`sdks/go/`）的行为差异；哪些协议特性未实现。
2. **异步正确性**：Promise/async generator 的边界、sendQueue 背压、close 时序、事件监听泄漏。
3. **重连与会话恢复**：offset/epoch 状态管理、重连退避参数一致性。
4. **类型安全**：`any` 绕过、接口与实现不一致、公开 API 类型完整性。
5. **打包与依赖**：package.json 依赖声明与实际使用、浏览器/Node 双环境兼容、构建产物。
6. **测试缺口**：真实 WebSocket 集成、重连、RPC、error 路由、protobuf 端到端。

## 已知线索（需你独立核实真伪，确认或推翻，并给出证据）

1. `package.json` 疑声明 `@grpc/grpc-js` peer dependency，但无 gRPC transport 实现。
2. `IClient` 接口的 `publish` 签名疑缺少 `transient` 参数（`types.ts`），与类实现不一致。
3. `setReconnectDelay` 疑只设 initial/max，没有 multiplier setter，与 `reconnectBackoffMultiplier` 字段不对应。
4. `WebSocketTransport.close()` 疑固定 `setTimeout(resolve, 100)` 等待关闭，较脆弱。
5. `ProtobufCodec.decode` 疑用 `(OutboundMessageSchema as any).fromBinary` 绕过类型。
6. 未实现 `SubRefresh`、`SurveyRequest/SurveyReply`（Go SDK 同样未实现——确认是否是协议整体未对齐）。
7. 测试缺口：无真实 WebSocket 集成测试；无 reconnect、RPC、error 路由、state change、multi-handler、protobuf 端到端测试。

## 工作流程

1. 先跑 `cd sdks/ts && npm test`（如依赖未装先 `npm ci` 或 `pnpm install`，注意不修改 lockfile）确认基线；`npm run build`（如存在）验证编译。
2. 通读范围内代码，并对照根包服务端 `client.go` 与 `protocol/client/v1/service.proto` 验证协议对等性。
3. 逐条核实"已知线索"：确认（给出决定性证据）或推翻。
4. 补充你自己发现的新问题。

## 输出格式

用中文输出。先给基线测试结果与总体评价（3-5 句），然后逐条 findings：

```
[级别] Critical / Important / Minor
[位置] path:line
[问题] ...
[证据] 关键代码摘录或推理
[修复建议] ...
[置信度] high / medium / low
```

最后单独一节列出"建议补充的测试"。不要贴大段代码，每条 finding 引用不超过 10 行。
