# MessageLoop Go SDK

MessageLoop 的官方 Go 客户端 SDK，独立 Go module（`github.com/messageloopio/messageloop/sdks/go`），支持 WebSocket 与 gRPC 双传输、代理（proxy）后端支持。

- 详细使用指南见 `docs/developer/07-sdk-go.md`（仓库文档）。
- 旧 CloudEvents 写法纠正见 `MIGRATION_GUIDE.md`。

## 安装

```bash
go get github.com/messageloopio/messageloop/sdks/go
```

## 快速开始

```go
import messageloopgo "github.com/messageloopio/messageloop/sdks/go"

client, err := messageloopgo.Dial("ws://localhost:8001/ws", messageloopgo.WithClientID("app-1"))
if err != nil {
    panic(err)
}
defer client.Close()

if err := client.Connect(context.Background()); err != nil {
    panic(err)
}

// 订阅
if err := client.Subscribe("chat.general"); err != nil {
    panic(err)
}
client.OnMessage(func(messages []*messageloopgo.Message) {
    for _, msg := range messages {
        fmt.Println(msg.Data.AsText())
    }
})

// 发布（fire-and-forget）
msg := messageloopgo.NewMessageWithData("chat.message", messageloopgo.NewTextData("hello"))
if err := client.Publish("chat.general", msg); err != nil {
    panic(err)
}

// RPC
resp := messageloopgo.NewMessage("")
if err := client.RPC(ctx, "user.service", "GetUser", req, resp); err != nil {
    panic(err)
}
```

## 订阅级 / 发布级鉴权 token

- `SubscribeWith(channel, messageloopgo.WithSubscriptionToken("..."))`：订阅时携带订阅级 token，服务端会将其传给订阅 ACL proxy。重连自动重订阅时 token 随订阅状态保存与恢复。
  - 命名说明：`WithToken` 已被客户端级连接鉴权 token 占用（`Dial(..., WithToken(...))`），故订阅级选项命名为 `WithSubscriptionToken`。
- `PublishWith(channel, msg, messageloopgo.WithPublishToken("..."))`：发布时携带发布级 token，服务端会将其传给发布 ACL proxy。
- `WithEphemeral(true)`：订阅标记为 ephemeral（不注册 presence、重连不持久化）。

```go
if err := client.SubscribeWith("secure.ch",
    messageloopgo.WithSubscriptionToken("sub-token"),
    messageloopgo.WithEphemeral(false),
); err != nil {
    panic(err)
}

if err := client.PublishWith("secure.ch", msg, messageloopgo.WithPublishToken("pub-token")); err != nil {
    panic(err)
}
```

## 等待发布确认：PublishWithAck

`PublishWithAck(ctx, channel, msg, opts...)` 等待服务端 `PublishAck` 并返回 broker 分配的 offset；ctx 超时/取消、连接断开或 `Close()` 时 pending 发布会被 reject 并清理，调用方可据此重试。`Publish`（fire-and-forget）行为保持不变。

```go
ctx := context.Background()
offset, err := client.PublishWithAck(ctx, "chat.general", msg)
if err != nil {
    // 超时/断连/服务端错误：可安全重试
    return err
}
fmt.Println("committed at offset", offset)
```

## 带数值码的断连错误

服务端主动断连时会下发带数值断连码（3000、3500-3513）的通知：WebSocket 路径走 close frame，gRPC 路径走 `DISCONNECT_ERROR` 信封的 `metadata.disconnect_code`。SDK 把两条路径统一为 `*DisconnectError`，可用 `errors.As` 取出：

```go
client.OnError(func(err error) {
    var de *messageloopgo.DisconnectError
    if errors.As(err, &de) {
        fmt.Printf("disconnected: code=%d reason=%s\n", de.Code, de.Reason)
    }
})
```

## 测试

```bash
cd sdks/go
go build ./...
go test -race ./...
go vet ./...
```
