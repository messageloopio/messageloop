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

## 结论

如果你看到任何 `CloudEvent`、`NewCloudEvent` 或 `cloudevents.Event` 的 Go SDK 示例，请把它们视为历史文档。当前 SDK 的权威参考是：

- `sdks/go/message.go`
- `sdks/go/client.go`
- `sdks/go/example/`