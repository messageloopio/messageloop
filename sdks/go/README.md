# MessageLoop Go SDK

MessageLoop 的官方 Go 客户端 SDK，独立 Go module（`github.com/messageloopio/messageloop/sdks/go`），支持 WebSocket、gRPC 与 QUIC 三种传输、代理（proxy）后端支持。

- 详细使用指南见 `docs/developer/07-sdk-go.md`（仓库文档）。
- 旧 CloudEvents 写法纠正见 `MIGRATION_GUIDE.md`。

## 安装

```bash
go get github.com/messageloopio/messageloop/sdks/go
```

## 快速开始

```go
import messageloopgo "github.com/messageloopio/messageloop/sdks/go"

client, err := messageloopgo.Dial("ws://localhost:9080/ws", messageloopgo.WithClientID("app-1"))
// 或 messageloopgo.DialGRPC("localhost:9090", ...)
// 或 messageloopgo.DialQUIC("localhost:4433", messageloopgo.WithInsecureSkipVerify(), ...)
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

服务端主动断连时会下发带数值断连码（3000、3500-3513）的通知：WebSocket 路径走 close frame，gRPC / QUIC 路径走 `DISCONNECT_ERROR` 信封的 `metadata.disconnect_code`（QUIC 同时用 application error code 携带该数值）。SDK 把这些路径统一为 `*DisconnectError`，可用 `errors.As` 取出：

```go
client.OnError(func(err error) {
    var de *messageloopgo.DisconnectError
    if errors.As(err, &de) {
        fmt.Printf("disconnected: code=%d reason=%s\n", de.Code, de.Reason)
    }
})
```

## 恢复订阅

恢复走流式：Connect / Subscribe 先收到 Ack，再按频道收到 `replay=true` 的 `Publication`（与 live 消息走同一条 `OnMessage` 路径），最后收到 `RecoverComplete`。游标（用于下次重连）只从 `RecoverComplete.position` 与 live `Message.position` 更新，恢复放心用 `Position(epoch, lastOffset)` 构造：

```go
// 从已知 offset 之后继续（cursor 是 resume hint）
if err := client.SubscribeWith("chat.recover", messageloopgo.WithRecover(messageloopgo.Position("ep", 42))); err != nil {
    panic(err)
}

// 无提示恢复：recover=true，不带 cursor，服务端从自身记录的 delivered 位置继续（无则 skip）
if err := client.SubscribeWith("chat.nohint", messageloopgo.WithRecover(nil)); err != nil {
    panic(err)
}

// 显式从头：fresh=true，重放整个历史
if err := client.SubscribeWith("chat.fresh", messageloopgo.WithFresh()); err != nil {
    panic(err)
}
```

没有「offset 0 = 从头」：需要从头就用 `WithFresh()`。

## Presence

```go
client.OnPresence(func(ev messageloopgo.PresenceEvent) { ... })                       // join/leave
client.OnPresenceSnapshot(func(snap messageloopgo.PresenceSnapshot) { ... })          // Connected/Ack 快照
snap, err := client.Presence(ctx, "room.x")                                            // 主动查询
```

Connect 后的 presence 快照是独立的 `Presence` 信封；`SubscribeAck.presence` 快照在状态写回后触发 `OnPresenceSnapshot`；`Presence(ctx, channel)` 返回同 id 快照并再触发一次该回调。

## Survey

```go
answers, err := client.Survey(ctx, "chat.x", reqMsg, 2*time.Second)
// a.SessionID / a.UserID（metadata.entries["user_id"]）/ a.Payload / a.Error

client.OnSurvey(func(requestID string, req *Message) (*Message, error) { ... })                    // 旧签名
client.OnSurveyRequest(func(requestID, channel string, req *Message) (*Message, error) { ... })    // 新签名（带频道）
```

`Survey` 按 `request_id` 收回 `SurveyResult`；同步拒绝（如 `SURVEY_DISABLED`）与异步 worker 失败以顶层 Error 返回。`timeout<=0` 由服务端策略决定。旧 `OnSurvey` 签名不变；无 handler 时默认 echo 请求 payload。

## 服务端 Ping

SDK 收到服务端 Outbound `Ping` 会立即以同 id 的 Inbound `Pong` 应答并计为存活证据。开启 `server.heartbeat.ping_interval` 必须使用本版本 SDK。

## 收包回调限制

`RPC` / `Survey` / `Presence` 会阻塞等待接收循环，**不要**在 `OnMessage` / `OnPresence` / `OnPresenceSnapshot` / `OnSurvey` / `OnSurveyRequest` 里同步调用它们；需要时另起 goroutine。

## 测试

```bash
cd sdks/go
go build ./...
go test -race ./...
go vet ./...
```
