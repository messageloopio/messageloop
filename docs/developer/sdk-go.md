# Go SDK 指南

本文介绍官方 Go 客户端 SDK 的安装与使用。SDK 位于仓库 `sdks/go/` 目录，是一个独立的 Go module，包含客户端（WebSocket / gRPC 两种传输）、消息类型以及代理（proxy）后端支持。协议层面的细节（子协议、消息信封、断连码）请参阅《客户端协议参考》（[../protocol.md](../protocol.md)），本文不再重复。

## 概述

- **模块路径**：`github.com/messageloopio/messageloop/sdks/go`（见 `sdks/go/go.mod`）。
- **Go 版本要求**：`go 1.25.5`。
- **功能范围**：
  - WebSocket 客户端与 gRPC 客户端，共享同一套 `Client` 接口与消息模型；
  - 订阅/退订、发布、请求-响应式 RPC；
  - 自动重连与会话恢复（session resumption，携带 epoch 与逐频道 offset）；
  - 心跳（ping/pong）；
  - 代理后端支持：在业务服务中以 gRPC 实现 RPC 处理、认证、ACL 与生命周期钩子，供服务端回调。
- **依赖**：`gorilla/websocket`、`google.golang.org/grpc`、`google.golang.org/protobuf`、`github.com/google/uuid`，以及同仓库的 `github.com/messageloopio/messageloop/shared`（生成代码与序列化器）。仓库内通过 `replace github.com/messageloopio/messageloop/shared => ./../../shared` 指向本地目录。
- **与其他 SDK 的关系**：TypeScript SDK 提供等价能力，API 设计与本文描述的概念一一对应，参见《TypeScript SDK 指南》（[sdk-ts.md](./sdk-ts.md)）。

## 安装

SDK 是独立 module，在项目中引入依赖即可：

```bash
go get github.com/messageloopio/messageloop/sdks/go
```

引入后以别名导入使用：

```go
import messageloopgo "github.com/messageloopio/messageloop/sdks/go"
```

发布版本遵循 `sdks/go/vX.Y.Z` 形式的模块标签（见《开发指南》[development.md](./development.md) 的发布流程一节）。仓库内开发时，SDK 通过 `replace` 指令直接引用本地的 `shared` 模块。

## 快速开始

以下完整示例参考 `example/basicwebsocket`，演示 WebSocket 连接的完整生命周期：Dial、连接、订阅、发布与收消息。

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	messageloopgo "github.com/messageloopio/messageloop/sdks/go"
)

func main() {
	client, err := messageloopgo.Dial(
		"ws://localhost:9080/ws",
		messageloopgo.WithEncoding(messageloopgo.EncodingJSON),
		messageloopgo.WithClientID("example-client"),
		messageloopgo.WithAutoSubscribe("chat.messages"),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	client.OnConnected(func(sessionID string) {
		log.Printf("Connected! Session ID: %s", sessionID)
	})

	client.OnMessage(func(msgs []*messageloopgo.Message) {
		for _, msg := range msgs {
			log.Printf("Received message - ID: %s, Type: %s, ContentType: %s",
				msg.ID, msg.Type, msg.Data.ContentType())
		}
	})

	client.OnError(func(err error) {
		log.Printf("Error: %v", err)
	})

	// Connect 会阻塞，直到连接建立或失败
	ctx := context.Background()
	if err := client.Connect(ctx); err != nil {
		log.Fatal(err)
	}

	// 连接建立后再订阅更多频道
	if err := client.Subscribe("chat.presence", "chat.typing"); err != nil {
		log.Fatal(err)
	}

	// 发布消息
	msg := messageloopgo.NewMessageWithData("chat.message", messageloopgo.NewTextData("Hello, MessageLoop!"))
	if err := client.Publish("chat.messages", msg); err != nil {
		log.Fatal(err)
	}

	// 保持运行
	select {
	case <-ctx.Done():
	case <-time.After(30 * time.Second):
	}
}
```

要点：

- `Dial(url, opts...)` 只建立底层连接并创建客户端，**不执行协议握手**；`Connect(ctx)` 才会发送 Connect 消息并**阻塞等待服务端的 Connected 响应**，成功返回 nil，失败返回错误。连接建立超时默认 30 秒。
- 建议在 `Connect` 之前注册 `OnConnected` / `OnMessage` / `OnError` 等回调，避免错过连接成功事件。
- `Subscribe` / `Publish` / `Unsubscribe` / `RPC` 在未连接时返回错误（`not connected`）。

gRPC 客户端的用法与 WebSocket 完全一致，只是把 `Dial` 换成 `DialGRPC(addr, opts...)`（见 [example/basicgrpc](../../sdks/go/example/basicgrpc)）。

## 客户端选项

所有选项均为函数式选项（`Option func(*Options)`），通过 `Dial` / `DialGRPC` 的变参传入。完整列表见 `options.go`：

| 选项函数 | 参数 | 作用 | 默认值 |
| --- | --- | --- | --- |
| `WithEncoding` | `EncodingType` | 消息编码：`EncodingJSON`（protojson）或 `EncodingProtobuf`（二进制） | `EncodingJSON` |
| `WithDialTimeout` | `time.Duration` | WebSocket 握手超时 | `10s` |
| `WithClientID` | `string` | 客户端标识（client ID） | 空 |
| `WithClientType` | `string` | 客户端类型，如 `"mobile"`、`"web"`、`"server"` | `"sdk"` |
| `WithToken` | `string` | 认证令牌 | 空 |
| `WithVersion` | `string` | 客户端版本号 | `"1.0.0"` |
| `WithAutoSubscribe` | `...string` | 连接建立时自动订阅的频道列表 | 无 |
| `WithPingInterval` | `time.Duration` | 心跳 Ping 的发送间隔；`<= 0` 时禁用心跳 | `30s` |
| `WithPingTimeout` | `time.Duration` | 等待 Pong 的超时。**当前版本仅保存该配置，未强制实施超时判定** | `10s` |
| `WithAutoReconnect` | `bool` | 断线后自动重连并尝试会话恢复 | `false` |
| `WithReconnectBackoff` | `initial, max time.Duration, factor float64` | 重连退避：初始延迟、最大延迟、指数因子 | `1s` / `30s` / `2.0` |
| `WithReconnectMaxAttempts` | `int` | 最大重连次数，`0` 表示不限次 | `0` |

示例：

```go
client, err := messageloopgo.Dial(
	"ws://localhost:9080/ws",
	messageloopgo.WithClientID("app-1"),
	messageloopgo.WithToken(os.Getenv("MESSAGELOOP_TOKEN")),
	messageloopgo.WithEncoding(messageloopgo.EncodingProtobuf),
	messageloopgo.WithAutoSubscribe("orders", "notifications"),
	messageloopgo.WithAutoReconnect(true),
	messageloopgo.WithReconnectBackoff(1*time.Second, 15*time.Second, 2.0),
	messageloopgo.WithReconnectMaxAttempts(10),
)
```

## 消息 API

SDK 以 `Message` 与 `Data` 为核心抽象（`message.go`），发布、订阅回调和 RPC 请求/响应都围绕这两类类型工作。

### Data

`Data` 携带消息体与 MIME 类型（content type），通过三个构造函数创建：

- `NewJSONData(data map[string]any) Data`——content type 为 `application/json`；
- `NewTextData(text string) Data`——content type 为 `text/plain`；
- `NewBinaryData(data []byte) Data`——content type 为 `application/octet-stream`；
- `NewData(contentType string, data any) (Data, error)`——根据 content type 与值类型自动归类：JSON 内容会尝试序列化为 map，文本内容接受 `string`/`[]byte`，其余按二进制处理。

读取侧方法：

- `(*Data) ContentType() string`——返回 MIME 类型；
- `(*Data) AsJSON() map[string]any`——仅当数据为 JSON 时返回 map，否则返回 nil；
- `(*Data) AsBinary() []byte`——仅当数据为二进制时返回字节，否则返回 nil；
- `(*Data) AsText() string`——仅当数据为文本时返回字符串，否则返回空串；
- `(*Data) As(out any) error`——解码到目标指针。JSON 数据直接 `json.Unmarshal`；二进制/文本数据先尝试按 JSON 解码，失败时若目标是 `*[]byte`/`*string` 则返回原始值。

### Message

```go
type Message struct {
	ID       string
	Type     string
	Data     Data
	Metadata map[string]string
}
```

构造与操作方法：

- `NewMessage(msgType string) *Message`——生成带 UUID ID 的空消息（`Metadata` 已初始化）；
- `NewMessageWithData(msgType string, data Data) *Message`——带数据的消息；
- `(*Message) SetData(contentType string, data any) error`——等价于 `NewData` 后赋值；
- `(*Message) SetMetadata(key, value string)` / `(*Message) GetMetadata(key string) string`——元数据读写；
- `(*Message) DataAs(out any) error`——`m.Data.As(out)` 的便捷方法；
- `(*Message) ToPayload() (*sharedpb.Payload, error)`——转换为协议 Payload；
- `PayloadToMessage(payload *sharedpb.Payload, id string) *Message`——协议 Payload 转回 `Message`；
- `(*Message) String() string`——按数据类型的字符串表示（调试用）。

### 接收消息

`OnMessage` 回调收到的是 `[]*Message`。每条消息的 `Type` 为 `"messageloop.message"`，频道与 offset 存放在元数据中：

```go
client.OnMessage(func(msgs []*messageloopgo.Message) {
	for _, msg := range msgs {
		channel := msg.GetMetadata("channel")   // 消息来自哪个频道
		offset := msg.GetMetadata("offset")     // 该频道内的消息序号
		log.Printf("channel=%s offset=%s: %s", channel, offset, msg.String())
	}
})
```

代码中另有一个 `ReceivedMessage` 结构体（`ID`、`Channel`、`Offset`、`Message` 字段），但目前回调路径使用上面的 `[]*Message` + 元数据形式。

## 传输

### WebSocket 客户端

`Dial(url string, opts ...Option) (Client, error)` 创建 WebSocket 客户端，URL 形如 `ws://localhost:9080/ws`。

编码通过 WebSocket 子协议（subprotocol）协商：`WithEncoding(EncodingJSON)` 对应子协议 `messageloop+json`（文本帧），`WithEncoding(EncodingProtobuf)` 对应 `messageloop+proto`（二进制帧），与协议规范（[../protocol.md](../protocol.md)）中的子协议一一对应。客户端在握手时通过 `Sec-WebSocket-Protocol` 头声明子协议；`WithDialTimeout` 同时作为握手超时。握手中的 Ping/Pong 控制帧由传输层自动应答。

### gRPC 客户端

`DialGRPC(addr string, opts ...Option) (Client, error)` 创建 gRPC 客户端，地址为 `host:port` 形式（如 `localhost:9090`），使用 `MessageLoopService/MessageLoop` 双向流传输协议消息。gRPC 传输固定使用 protobuf，无编码协商；连接使用 insecure 凭据，通过 `ForceCodec` 按连接注入名为 `messageloop-proto` 的原始编解码器，避免覆盖进程级全局 proto codec。

### 重连与会话恢复

重连默认关闭，通过 `WithAutoReconnect(true)` 开启（`client.go` 的 `reconnectLoop` / `reconnect`）：

1. 接收循环报错且客户端未显式关闭时，触发重连流程；
2. 重连期间停止心跳循环，按指数退避重试：初始延迟 `ReconnectInitialDelay`（默认 1s），每次失败乘以 `ReconnectBackoffFactor`（默认 2.0），上限 `ReconnectMaxDelay`（默认 30s）；`ReconnectMaxAttempts` 限制总次数，`0` 为不限；
3. 每次尝试重新拨号（WebSocket 复用原 URL 与编码，gRPC 复用原地址），发送携带原 `SessionId` 的 Connect 消息，并对每个已订阅频道携带 `Recover: true`、`Epoch` 与最近一次收到的 offset——即**会话恢复**；服务端恢复成功时会在 Connected 响应中标记 `resumed`，并保留原有订阅；
4. 重连期间通过连接代际（generation）计数丢弃旧连接的过期 Connected 响应，避免污染重连状态。

重连相关回调：

- `OnReconnecting(fn func(attempt int))`——每次重连尝试之前调用；
- `OnReconnected(fn func(sessionID string))`——重连成功之后调用。

### 心跳

开启后（`PingInterval > 0`，默认 30s），客户端按固定间隔发送 Ping 协议消息；收到 Pong 仅作存活确认（`handlePong` 目前为空实现）。

## 发布/订阅与动态订阅

- `Publish(channel string, msg *Message) error`——向频道发布消息，内部将 `Message` 转换为协议 Payload 后发送；未连接时返回错误。
- `Subscribe(channels ...string) error` / `Unsubscribe(channels ...string) error`——订阅/退订一个或多个频道，可随时调用（不要求频道此前出现在 Connect 中）。
- 订阅集合可在连接时一次性声明：`WithAutoSubscribe(...)` 会把频道随 Connect 消息一起提交，服务端确认后即生效。

动态订阅示例（参考 `example/dynamicsub`）：

```go
// 连接建立后按需订阅
for _, ch := range []string{"channel.1", "channel.2", "channel.3"} {
	if err := client.Subscribe(ch); err != nil {
		log.Printf("failed to subscribe %s: %v", ch, err)
	}
}

// 不再需要时退订
if err := client.Unsubscribe("channel.1", "channel.2"); err != nil {
	log.Printf("failed to unsubscribe: %v", err)
}
```

注意：`Subscribe` / `Unsubscribe` 只负责发送请求，确认以服务端 SubscribeAck / UnsubscribeAck 为准；`Subscribe` 与 `Unsubscribe` 返回的仅是发送错误。会话恢复时，已订阅频道会带 recovery offset 重新声明，见上文「重连与会话恢复」。

## RPC 与代理

### 客户端发起 RPC

```go
req := messageloopgo.NewMessageWithData("getUser", messageloopgo.NewJSONData(map[string]any{
	"userId": "123",
}))
resp := messageloopgo.NewMessage("")

// 阻塞直到收到响应、出错或 ctx 超时
err := client.RPC(ctx, "user.service", "GetUser", req, resp)
if err != nil {
	log.Fatal(err)
}
log.Printf("RPC response: %s", resp.String())
```

`RPC(ctx context.Context, channel, method string, req, resp *Message) error` 的行为：

- 请求带自增消息 ID，按 ID 与响应匹配（`pendingRPC` 表）；响应写入调用方传入的 `resp` 指针；
- 若服务端返回错误信封（Error 或 RpcReply 内嵌错误），RPC 立即以 `rpc error: <message> (code: <code>)` 形式失败，而非挂起至超时；
- 遵循 `ctx` 取消/超时；客户端 `Close()` 时所有挂起 RPC 也会被清理并返回错误。

### 代理（proxy）后端

RPC 的业务实现位于代理后端：服务端把客户端 RPC 请求转发到后端 gRPC 服务（`proxy/v1` 的 `ProxyService`），后端处理后返回。SDK 的 `proxy.go` 提供整套后端实现骨架：

- `RPCHandler` 接口：`HandleRPC(ctx context.Context, req *RPCRequest) (*RPCResponse, error)`，其中 `RPCRequest{ID, Channel, Method, Payload *Message}`、`RPCResponse{Payload *Message, Error *sharedpb.Error}`；
- `AuthHandler`：`Authenticate(ctx, *AuthenticateRequest) (*AuthenticateResponse, error)`，配合 `UserInfo` 返回用户信息；
- `ACLHandler`：`CheckSubscribeACL(ctx, channel, token string) error` 与 `CheckPublishACL(ctx, channel, token string) error`；
- `LifecycleHandler`：`OnConnected` / `OnDisconnected`（携带 sessionID 与 username）/ `OnSubscribed` / `OnUnsubscribed` 生命周期钩子；
- 默认实现 `RPCHandlerImpl` / `AuthHandlerImpl` / `ACLHandlerImpl` / `LifecycleHandlerImpl`（未实现时返回 `UNIMPLEMENTED` 或放行）；
- `HandlerImpl`：嵌入上述四个默认实现并实现 `ProxyServiceServer`，可整体作为 gRPC handler 注册；其 `RPCHandler` / `AuthHandler` / `ACLHandler` / `LifecycleHandler` 四个字段非空时优先于对应内嵌默认实现（覆盖模式）。

启动代理服务：

```go
handler := &messageloopgo.HandlerImpl{
	RPCHandler: myRPCHandler,   // 自定义 RPC 处理
	AuthHandler: &myAuthHandler{},
	ACLHandler: &myACLHandler{},
	LifecycleHandler: &myLifecycleHandler{},
}
proxy, err := messageloopgo.NewProxyServer(
	messageloopgo.ProxyServerOptions{Addr: ":9001", Insecure: true},
	handler,
)
// proxy 实现 lynx.Service 生命周期接口：Start(ctx) / Stop(ctx)
```

`NewProxyServer(opts ProxyServerOptions, handler proxypb.ProxyServiceServer) (*ProxyServer, error)` 创建 gRPC 代理服务；`Insecure` 为 true 时使用明文凭据（开发默认）。服务端侧的代理集成（路由、超时）见《架构文档》[architecture.md](./architecture.md) 与配置文档 [configuration.md](./configuration.md)。

### RPCMux：RPC 路由与中间件

`mux.go` 提供 `RPCMux` 多路复用器，实现 `RPCHandler` 接口，可直接作为 `HandlerImpl.RPCHandler` 使用：

- `NewRPCMux() *RPCMux`；
- `(*RPCMux) Handle(method string, handler RPCHandlerFunc)`——按方法名注册处理器，重复注册会覆盖；
- `(*RPCMux) Use(middleware RPCMiddleware)`——注册中间件，按注册顺序包裹：先注册者最外层（`m1 -> m2 -> handler`）；
- 未注册的方法返回 `UNKNOWN_METHOD` 错误（`rpc_error` 类型）。

其中 `RPCHandlerFunc` 即 `func(ctx context.Context, req *RPCRequest) (*RPCResponse, error)`，`RPCMiddleware` 即 `func(next RPCHandlerFunc) RPCHandlerFunc`。`example/proxyserver` 同时演示了 switch 分发与 RPCMux + 中间件（日志、panic 恢复）两种后端写法。

## 错误处理

- 运行时错误通过 `OnError(fn func(error))` 回调下发。服务端返回的错误信封（非 RPC 场景）转为 `server error: <message> (code: <code>)` 形式。
- 挂起 RPC 的错误信封按 ID 路由到对应 `RPC` 调用，使其快速失败（见上文）。
- 服务端主动断开连接时使用**数字断连码**（disconnect code），各码值的语义与客户端应如何解读见《客户端协议参考》[../protocol.md](../protocol.md) 的 Disconnect Codes 一节（例如 3503 `DisconnectForceNoReconnect` 表示服务端要求不要重连）。断连码全集与定义见《架构文档》[architecture.md](./architecture.md)。
- 重连策略：默认关闭；开启后按「重连与会话恢复」一节所述退避重试，达到 `ReconnectMaxAttempts` 后调用 `OnError` 上报最终失败。服务端要求不重连（force no-reconnect）时，客户端行为与协议约定保持一致即可——SDK 目前不依据断连码区分重连决策。

## 示例清单

`example/` 下的可运行示例：

| 目录 | 说明 |
| --- | --- |
| `basicwebsocket` | WebSocket 客户端最小闭环：连接、订阅、发布、收消息（JSON 编码） |
| `basicgrpc` | gRPC 客户端：连接、订阅、发布，并在同一连接上发起 RPC |
| `dynamicsub` | 动态订阅管理：连接后逐个订阅、定时退订 |
| `protobuf` | 使用 `EncodingProtobuf` 的 WebSocket 客户端与发布 |
| `wsrpc` | 在 WebSocket 连接上发起 RPC 调用并读取响应 |
| `proxyserver` | 代理后端 gRPC 服务：RPC 处理（switch 与 RPCMux 两种写法）、认证、ACL、生命周期钩子 |

## 迁移

仓库内 `sdks/go/MIGRATION_GUIDE.md` 纠正早期示例残留的 CloudEvents 用法：当前 SDK **不是** CloudEvents API，发布、RPC 与订阅回调统一使用 `Message` / `Data`（如 `Publish(channel, event)` → `Publish(channel, msg)`、`NewCloudEvent(...)` → `NewMessageWithData(type, data)`、`OnMessage(func([]*cloudevents.Event))` → `OnMessage(func([]*Message))`）；凡是出现 `CloudEvent`、`NewCloudEvent` 或 `cloudevents.Event` 的 Go SDK 示例均视为历史文档，以 `message.go`、`client.go` 与 `example/` 为准。

## 构建与测试

SDK 是独立 Go module，必须在 `sdks/go/` 目录内构建与测试（根目录的 `go build ./...` 不会覆盖它）：

```bash
cd sdks/go
go build ./...
go test ./...
```

测试套件包含 `client_test.go`、`message_test.go`、`proxy_test.go` 等（重连、RPC 竞态、处理器覆盖等场景）。环境要求、仓库模块划分与发布流程（`sdks/go/vX.Y.Z` 标签）见《开发指南》[development.md](./development.md)。
