# Go SDK 迁移说明

这份说明用于纠正仓库里早期残留的旧示例。

当前 MessageLoop Go SDK 不是 CloudEvents API。现在的 SDK 以 `Message` 和 `Data` 为核心抽象，发布、订阅回调和 RPC 都围绕这套类型工作。

## 当前模型

- 发布消息使用 `*Message`
- RPC 请求和响应使用 `*Message`
- 收到的消息回调是 `func([]*Message)`
- 消息体通过 `NewJSONData`、`NewTextData`、`NewBinaryData` 创建

## 常见旧写法到新写法

| 旧概念 | 当前写法 |
| --- | --- |
| `Publish(channel, event)` | `Publish(channel, msg)` |
| `RPC(..., reqEvent, respEvent)` | `RPC(..., reqMsg, respMsg)` |
| `OnMessage(func([]*cloudevents.Event))` | `OnMessage(func([]*Message))` |
| `NewCloudEvent(...)` | `NewMessageWithData(type, data)` |

## 发布消息

```go
msg := messageloopgo.NewMessageWithData(
    "chat.message",
    messageloopgo.NewJSONData(map[string]any{
        "text": "hello",
    }),
)

if err := client.Publish("chat.general", msg); err != nil {
    return err
}
```

## RPC

```go
req := messageloopgo.NewMessageWithData(
    "user.get",
    messageloopgo.NewJSONData(map[string]any{"userId": "123"}),
)
resp := messageloopgo.NewMessage("")

if err := client.RPC(ctx, "user.service", "GetUser", req, resp); err != nil {
    return err
}
```

## 收消息

```go
client.OnMessage(func(messages []*messageloopgo.Message) {
    for _, msg := range messages {
        fmt.Println(msg.Type, msg.Data.ContentType())
    }
})
```

## 数据类型辅助函数

- `NewJSONData(map[string]any)`
- `NewTextData(string)`
- `NewBinaryData([]byte)`
- `(*Message).DataAs(&target)`

## 新增能力（非破坏性）

以下能力为增量 API，不影响既有调用方；详细用法见 `README.md`。

### v1.0：Recover / Presence / Survey / 服务端 Ping

- `SubscribeWith(channel, WithRecover(cursor *Position))`：按 cursor 恢复订阅。`cursor` 用 `Position(epoch, offset)` 构造；`nil` 表示无提示（服务端从自身记录继续，无记录则 skip）。恢复消息走流式 `Replay=true` `Publication`（与 live 相同 `OnMessage` 路径），随后 `RecoverComplete` 回显权威游标。从头恢复请用 `WithFresh()`（`fresh=true`）；没有「offset 0 = 从头」。
- `OnPresence(fn func(PresenceEvent))` / `OnPresenceSnapshot(fn func(PresenceSnapshot))` / `Presence(ctx, channel) (*PresenceSnapshot, error)`。
- `Survey(ctx, channel, payload, timeout) ([]SurveyAnswer, error)`：发起频道级调查；`OnSurveyRequest(fn func(requestID, channel string, req *Message) (*Message, error))` 带频道的应答 handler。
- **`OnSurvey` 签名不变**，旧 handler 与默认 echo 行为照旧；`OnSurveyRequest` 设置时优先于 `OnSurvey`。
- 服务端 Outbound Ping 现在会被自动应答（同 id Inbound Pong）并计为存活证据——开启 `server.heartbeat.ping_interval` 必须使用本版本 SDK。
- 注意：`RPC` / `Survey` / `Presence` 都阻塞等待接收循环，禁止在 `OnMessage` / `OnPresence` / `OnPresenceSnapshot` / `OnSurvey*` 里同步调用。

### 订阅级 / 发布级 token

- `SubscribeWith(channel, WithSubscriptionToken("..."))`：订阅时携带订阅级鉴权 token，服务端会将其传给订阅 ACL proxy。重连恢复订阅时 token 随订阅状态一并保存与恢复。
  - 命名说明：因 `WithToken` 已被客户端级连接鉴权 token（`options.go`）占用，本选项命名为 `WithSubscriptionToken`。
- `PublishWith(channel, msg, WithPublishToken("..."))`：发布时携带发布级 token，服务端会将其传给发布 ACL proxy。`Publish` 原有签名与行为不变。

### PublishWithAck：等待发布确认

`PublishWithAck(ctx, channel, msg, opts...) (offset uint64, err error)`：等待服务端 `PublishAck` 并返回 broker 分配的 offset。ctx 超时/取消、连接断开或 `Close()` 时，pending 的发布会被 reject 并清理，调用方可据此重试。`Publish`（fire-and-forget）行为保持不变。

### 带数值码的 typed Disconnect

- 新增 `DisconnectError{Code, Reason}`：gRPC 路径解析 `DISCONNECT_ERROR` 信封 `metadata.disconnect_code` 得到数值断连码（3500-3513），与 WebSocket close frame 路径统一为同一类型，可通过 `errors.As(err, &de)` 取出数值码。
- 行为变化：WebSocket 收到 close frame 时，`Recv` 不再返回笼统的 `"connection closed"`，而是带数值码的 `*DisconnectError`（reason 取自 close frame 文本，为空时为 `"disconnected (code: <n>)"`，不再保证包含 `connection closed` 字样）；gRPC 收到带 `disconnect_code` metadata 的错误信封时，错误处理器收到 `*DisconnectError` 而非 `"server error: ..."`。缺失/畸形 metadata 时保持原有行为。

### Client 接口新增方法（对自定义实现是 breaking）

- `PublishWith(channel string, msg *Message, opts ...PublishOption) error`
- `PublishWithAck(ctx context.Context, channel string, msg *Message, opts ...PublishOption) (uint64, error)`

## 破坏性变更（Breaking Changes）

### LifecycleHandler 回调签名

`LifecycleHandler.OnSubscribed/OnUnsubscribed` 的签名变更为 `(ctx, sessionID, channel, username)`：

```go
OnSubscribed(ctx context.Context, sessionID, channel, username string) error
OnUnsubscribed(ctx context.Context, sessionID, channel, username string) error
```

相比旧签名 `(ctx, sessionID, username)` 新增了 `channel` 参数，实现该接口的代码需要同步更新。

### Client 接口新增方法

`Client` 接口新增以下方法，自定义实现（如 mock）需要补全：

- `SubscribeWith(channel string, opts ...SubscribeOption) error`：按订阅选项订阅单个频道（如 `WithEphemeral`、`WithRecover`）
- `SubRefresh(ctx context.Context, channels ...string) error`：请求服务端重新校验订阅（如后端 ACL 变更后）
- `SendSurveyReply(ctx context.Context, requestID string, reply *Message, replyErr error) error`：回复服务端下发的 survey 请求
- `OnSurvey(fn func(requestID string, req *Message) (*Message, error))`：设置 survey 处理器；未设置时默认把请求 payload 原样回显给服务端
- `OnSurveyRequest(fn func(requestID, channel string, req *Message) (*Message, error))`：带频道的 survey 处理器，优先于 `OnSurvey`
- `Survey(ctx context.Context, channel string, payload *Message, timeout time.Duration) ([]SurveyAnswer, error)`：发起频道级调查
- `OnPresence(fn func(PresenceEvent))` / `OnPresenceSnapshot(fn func(PresenceSnapshot))`：presence 事件/快照回调
- `Presence(ctx context.Context, channel string) (*PresenceSnapshot, error)`：主动查询频道 presence 快照

### RPC 默认超时

`RPC` 现在有默认 30 秒超时：当调用方传入的 context 未携带 deadline 时，SDK 自动应用 `RPCTimeout`（默认 30s），避免死连接挂死调用。可通过 `WithRPCTimeout` 调整，传 `0` 可禁用该默认超时。

### PingTimeout 语义

`PingTimeout` 现在会实际关闭 transport 触发重连：pong 超时后连接被视为半开，SDK 会关闭当前 transport，接收循环观察到失败后（若启用了 `WithAutoReconnect`）进入重连流程，而不是只记录超时。

### Connect() 每次推进 generation

`Connect()` 每次调用（包括失败后的重试）都会推进连接 generation 并换上全新的 transport，旧连接的产物（如过期的 Connected 响应、残留的接收循环）会被识别并丢弃。若你的代码缓存了 `Connect` 返回后的 session 状态，请注意重连后 session ID 可能变化。

### 默认 Version 升至 "2.0.0"

`Options.Version` 默认值从 `"1.0.0"` 改为 `"2.0.0"`（服务端握手版本门只接受 generation 2；`WithVersion` 仍可显式覆盖，但非 2 世代会被服务端以 `VERSION_UNSUPPORTED` Error + 断开码 3514 拒绝）。

## 结论

如果你看到任何 `CloudEvent`、`NewCloudEvent` 或 `cloudevents.Event` 的 Go SDK 示例，请把它们视为历史文档。当前 SDK 的权威参考是：

- `sdks/go/message.go`
- `sdks/go/client.go`
- `sdks/go/example/`