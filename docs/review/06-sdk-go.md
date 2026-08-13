# 评审任务 06：Go SDK

## 背景

你要评审的项目是 **MessageLoop**（工作目录：`D:/Codes/qiulin/messageloop`），一个用 Go 编写的实时消息平台服务端，通过 WebSocket 和 gRPC 双向流提供 pub/sub 消息能力。先读根目录 `AGENTS.md` 了解整体架构。

你是独立评审 agent，没有任何先前上下文。**只做只读评审，不修改任何代码。**

## 评审范围（Go SDK）

- `sdks/go/`：`client.go`（终端实时客户端，WebSocket/gRPC 双传输）、`websocket.go`、`grpc.go`、`mux.go`（RPC 路由）、`options.go`、`proxy.go`（后端 Proxy 服务器骨架：Auth/ACL/RPC/生命周期钩子）、`message.go`（`Message/Data/ReceivedMessage` 模型）及全部测试
- `sdks/go/MIGRATION_GUIDE.md`、`sdks/go/example/`
- 协议契约来源：`protocol/client/v1/service.proto`、`protocol/proxy/v1/proxy.proto`（生成代码在 `shared/genproto/`）
- 参考文档：`docs/developer/07-sdk-go.md`

## 模块职责与关键契约（供定位，需你自行通读验证）

- `Client` 接口（`sdks/go/client.go`）：Connect/Subscribe/Unsubscribe/Publish（含 transient）/RPC；传输抽象 `transport`，实现 `wsTransport`、`grpcTransport`。
- 协议流：发 `InboundMessage`（Connect/Subscribe/Publish/RpcRequest/Ping），收 `OutboundMessage`（Connected/SubscribeAck/PublishAck/RpcReply/Pong/Error）。
- 并发模型：`client.mu` 保护 transport/session/epoch；`receiveLoop` 每 transport 一个，按 generation 过滤过期连接消息；`pingLoop`、指数退避 `reconnectLoop`；pendingRPC 用 channel + `sync.Once`。
- Proxy 骨架：`HandlerImpl` 实现 `ProxyServiceServer`，供服务端回调。
- 自动重连默认关闭，`MaxAttempts=0` 表示无限。

## 评审维度

1. **并发正确性**：锁覆盖、transport 热切换竞态、channel 关闭时机、goroutine 泄漏。
2. **协议契约对等性**：SDK 发送/解析的消息与服务端（根包 `client.go`）处理逻辑是否完全对齐；哪些协议特性 SDK 未实现。
3. **重连与会话恢复**：offset/epoch 的保存与重建、resumed 会话的本地状态一致性。
4. **错误处理**：错误吞没、超时语义、错误到调用方的传播。
5. **API 易用性与一致性**：与 TS SDK 的语义差异、文档准确性。
6. **测试缺口**：真实 transport 集成测试、重连成功路径、会话恢复。

## 已知线索（需你独立核实真伪，确认或推翻，并给出证据）

1. `handleConnected` 中若 `resumed=true`，疑不把 `Connected.Subscriptions` 写回本地 `subscriptions` 映射（`sdks/go/client.go` 约 311 行），导致恢复后的会话下次重连无法重建订阅/offset。服务端始终下发订阅列表（根 `client.go` 约 691 行）——对照核实。
2. `Subscribe` 疑永远设置 `Ephemeral=false`，完全不支持 ephemeral，而协议与服务端支持。
3. `BuildPublishMessage` 与 `BuildRPCMessage` 疑用 `_` 忽略 `ToPayload` 错误。
4. 有 `PingInterval` 但无 `PingTimeout`，`handlePong` 疑为空实现，无法发现半开连接（对比 TS SDK 有完整 pong timeout）。
5. Go SDK 未实现 `SubRefresh`、`SurveyRequest/SurveyReply`。
6. `LifecycleHandler.OnSubscribed/OnUnsubscribed` 疑只有 `ctx` 参数，未透传 `proxy.proto` 请求中的 `session_id/channel/username`。
7. 测试缺口：无真实 WebSocket/gRPC transport 集成测试；无重连成功、会话恢复 offset/epoch、ping/pong、Subscribe/Unsubscribe 完整流程、PublishAck 处理测试。

## 工作流程

1. 先跑 `cd sdks/go && go build ./... && go test ./...` 确认基线。
2. 通读范围内代码，并对照根包服务端 `client.go` 的消息处理逻辑验证协议对等性。
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
