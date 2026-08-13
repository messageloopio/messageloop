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

- `SubscribeWith(channel string, opts ...SubscribeOption) error`：按订阅选项订阅单个频道（如 `WithEphemeral`）
- `SubRefresh(ctx context.Context, channels ...string) error`：请求服务端重新校验订阅（如后端 ACL 变更后）
- `SendSurveyReply(ctx context.Context, requestID string, reply *Message, replyErr error) error`：回复服务端下发的 survey 请求
- `OnSurvey(fn func(requestID string, req *Message) (*Message, error))`：设置 survey 处理器；未设置时默认把请求 payload 原样回显给服务端

### RPC 默认超时

`RPC` 现在有默认 30 秒超时：当调用方传入的 context 未携带 deadline 时，SDK 自动应用 `RPCTimeout`（默认 30s），避免死连接挂死调用。可通过 `WithRPCTimeout` 调整，传 `0` 可禁用该默认超时。

### PingTimeout 语义

`PingTimeout` 现在会实际关闭 transport 触发重连：pong 超时后连接被视为半开，SDK 会关闭当前 transport，接收循环观察到失败后（若启用了 `WithAutoReconnect`）进入重连流程，而不是只记录超时。

### Connect() 每次推进 generation

`Connect()` 每次调用（包括失败后的重试）都会推进连接 generation 并换上全新的 transport，旧连接的产物（如过期的 Connected 响应、残留的接收循环）会被识别并丢弃。若你的代码缓存了 `Connect` 返回后的 session 状态，请注意重连后 session ID 可能变化。

## 结论

如果你看到任何 `CloudEvent`、`NewCloudEvent` 或 `cloudevents.Event` 的 Go SDK 示例，请把它们视为历史文档。当前 SDK 的权威参考是：

- `sdks/go/message.go`
- `sdks/go/client.go`
- `sdks/go/example/`