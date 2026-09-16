# Go SDK 指南

本文介绍官方 Go 客户端 SDK 的安装与使用。SDK 位于仓库 `sdks/go/` 目录，是一个独立的 Go module，包含客户端（WebSocket / gRPC / QUIC / KCP 四种传输）、消息类型以及代理（proxy）后端支持。协议层面的细节（子协议、消息信封、断连码）请参阅[《客户端协议参考》](../protocol.md)，本文不再重复。

## 概述

- **模块路径**：`github.com/messageloopio/messageloop/sdks/go`（见 `sdks/go/go.mod`）。
- **Go 版本要求**：`go 1.25.5`。
- **功能范围**：
  - WebSocket、gRPC、QUIC 与 KCP 客户端，共享同一套 `Client` 接口与消息模型；
  - 订阅/退订（默认等待服务端确认，见[订阅生效契约](#订阅生效契约subscribe--unsubscribe-等待服务端-ack)）、发布（含瞬时发布与确认发布）、请求-响应式 RPC；
  - 自动重连与会话恢复（session resumption，携带 epoch 与逐频道 offset）；
  - 恢复订阅（流式回放 + `RecoverComplete`）、Presence（事件/快照/查询）、Survey（发起与应答）、GapNotice；
  - 心跳（ping/pong，含服务端 Ping 应答）；
  - 代理后端支持：在业务服务中以 gRPC 实现 RPC 处理、认证、ACL 与生命周期钩子，供服务端回调。
- **依赖**：`gorilla/websocket`、`google.golang.org/grpc`、`github.com/quic-go/quic-go`、`github.com/xtaci/kcp-go/v5`、`google.golang.org/protobuf`、`github.com/google/uuid`，以及同仓库的 `github.com/messageloopio/messageloop/shared`（生成代码与序列化器）。仓库内通过 `replace github.com/messageloopio/messageloop/shared => ./../../shared` 指向本地目录。
- **与其他 SDK 的关系**：TypeScript SDK 提供等价能力，API 设计与本文描述的概念一一对应，参见[《TypeScript SDK 指南》](08-sdk-ts.md)。

## 安装

SDK 是独立 module，在项目中引入依赖即可：

```bash
go get github.com/messageloopio/messageloop/sdks/go
```

引入后以别名导入使用：

```go
import messageloopgo "github.com/messageloopio/messageloop/sdks/go"
```

发布版本遵循 `sdks/go/vX.Y.Z` 形式的模块标签（见[《开发指南》](06-development.md) 的发布流程一节）。仓库内开发时，SDK 通过 `replace` 指令直接引用本地的 `shared` 模块。

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

	// 连接建立后再订阅更多频道（阻塞等待服务端 SubscribeAck）
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

- `Dial(url, opts...)` 只建立底层连接并创建客户端，不执行协议握手；`Connect(ctx)` 才会发送 Connect 消息并阻塞等待服务端的 Connected 响应，成功返回 nil，失败返回错误。连接建立超时默认 30 秒。
- 建议在 `Connect` 之前注册 `OnConnected` / `OnMessage` / `OnError` 等回调，避免错过连接成功事件。
- `Subscribe` / `Publish` / `Unsubscribe` / `RPC` 等请求类方法在未连接时返回错误（`not connected`）。

gRPC 客户端的用法与 WebSocket 完全一致，只是把 `Dial` 换成 `DialGRPC(addr, opts...)`（见 [example/basicgrpc](../../sdks/go/example/basicgrpc)）。QUIC 客户端同样共享 `Client` 接口，入口是 `DialQUIC(addr, opts...)`（见 [example/basicquic](../../sdks/go/example/basicquic)）。QUIC 强制 TLS 1.3：对接 `transport.quic.insecure` 的开发服务器时传 `WithInsecureSkipVerify()`。

KCP 客户端入口是 `DialKCP(addr, dataShards, parityShards, opts...)`：一条 TLS 加密的 KCP 会话，帧格式与 QUIC 传输一致。KCP 自身不加密，始终叠加 TLS，对接 `transport.kcp.insecure` 的开发服务器时同样传 `WithInsecureSkipVerify()`。`dataShards`/`parityShards` 是服务端 `transport.kcp` 的 FEC 分片配置，两端必须一致（服务端未配置 FEC 时传 `0, 0`）。

## 客户端选项

所有选项均为函数式选项（`Option func(*Options)`），通过 `Dial` / `DialGRPC` / `DialQUIC` / `DialKCP` 的变参传入。完整列表见 `options.go`：

| 选项函数 | 参数 | 作用 | 默认值 |
| --- | --- | --- | --- |
| `WithEncoding` | `EncodingType` | 消息编码：`EncodingJSON`（protojson）或 `EncodingProtobuf`（二进制） | `EncodingJSON` |
| `WithDialTimeout` | `time.Duration` | 建连超时：WebSocket 握手与 QUIC/KCP 拨号共用 | `10s` |
| `WithClientID` | `string` | 客户端标识（client ID） | 空 |
| `WithClientType` | `string` | 客户端类型，如 `"mobile"`、`"web"`、`"server"` | `"sdk"` |
| `WithToken` | `string` | 认证令牌（随 Connect 发送） | 空 |
| `WithVersion` | `string` | 客户端版本号 | `"2.0.0"` |
| `WithAutoSubscribe` | `...string` | 连接建立时自动订阅的频道列表（随 Connect 帧提交，不走 Ack 等待） | 无 |
| `WithPingInterval` | `time.Duration` | 心跳 Ping 的发送间隔；`<= 0` 时禁用心跳 | `30s` |
| `WithPingTimeout` | `time.Duration` | 等待 Pong 的超时：超时未收到 Pong 时连接视为半开，SDK 关闭 transport 并（若开启自动重连）进入重连流程 | `10s` |
| `WithRPCTimeout` | `time.Duration` | RPC 的默认超时（ctx 无 deadline 时套用），同时也是 `Subscribe`/`Unsubscribe` 等待 Ack 的预算；`<= 0` 表示不设默认超时 | `30s` |
| `WithAutoReconnect` | `bool` | 断线后自动重连并尝试会话恢复 | `false` |
| `WithReconnectBackoff` | `initial, max time.Duration, factor float64` | 重连退避：初始延迟、最大延迟、指数因子 | `1s` / `30s` / `2.0` |
| `WithReconnectMaxAttempts` | `int` | 最大重连次数，`0` 表示不限次 | `0` |
| `WithTLSConfig` | `*tls.Config` | QUIC/KCP 拨号使用的 TLS 配置（会按 Encoding 补 `NextProtos`） | `nil` |
| `WithInsecureSkipVerify` | （无） | QUIC/KCP 跳过服务端证书校验（仅开发） | 关 |

订阅级选项（用于 `SubscribeWith`，见下文）：`WithRecover(cursor)`、`WithFresh()`、`WithEphemeral(bool)`（临时订阅，不登记 presence）、`WithSubscriptionToken(token)`（订阅级 token）。

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

## 订阅生效契约（Subscribe / Unsubscribe 等待服务端 Ack）

`Subscribe` / `SubscribeWith` / `Unsubscribe` 默认**阻塞等待服务端 Ack**，返回 nil 即服务端已完成注册/移除。因此 `await` 之后的发布（哪怕来自另一条连接）不会再与在途订阅竞速导致消息静默丢失：

```go
func (c *Client) Subscribe(channels ...string) error              // 等待 SubscribeAck
func (c *Client) SubscribeAsync(channels ...string) error         // 尽力而为，不等待
func (c *Client) SubscribeWith(channel string, opts ...SubscribeOption) error // 等待（同 Subscribe）
func (c *Client) Unsubscribe(channels ...string) error            // 等待 UnsubscribeAck
func (c *Client) UnsubscribeAsync(channels ...string) error       // 尽力而为，不等待
```

规则：

- **Id 关联**：请求以生成的消息 Id 发送，pending 登记先于发送注册；服务端在 SubscribeAck/UnsubscribeAck 信封的 Id 上回显该值，SDK 按其唤醒调用方。
- **超时预算**：复用 `WithRPCTimeout`（默认 30s；`<=0` 表示不设默认超时，仅由 Ack / 断连 / `Close()` 解除等待）。超时错误：`subscribe ack timeout` / `unsubscribe ack timeout`。
- **拒绝即失败**：服务端以回显请求 Id 的顶层 Error 信封拒绝时立即返回 `subscribe rejected: <message> (code: <code>)`（退订为 `unsubscribe rejected: ...`），不等超时。
- **断连 / Close**：所有 in-flight 等待立即以 `connection lost before subscribe ack: ...` / `client closed before subscribe ack`（退订对应文案）失败，不悬挂。
- **`*Async` 变体**是显式的尽力而为路径：请求写入传输即返回（fire-and-forget），随后立即发布仍可能与订阅注册竞速。
- 未连接时四种变体均返回 `not connected`。
- 自动重连的内部重订阅与 `WithAutoSubscribe` 都随 Connect 帧建立订阅，不走 Ack 等待路径——不存在「等待发生在接收循环里」的自锁。
- 与 `RPC` / `Presence` / `Survey` 一样，等待发生在调用方 goroutine：不要在 `OnMessage` 等收包回调里同步调用 `Subscribe` / `Unsubscribe`。

## 消息 API

SDK 以 `Message` 与 `Data` 为核心抽象（`message.go`），发布、订阅回调和 RPC 请求/响应都围绕这两类类型工作。

### Data

`Data` 携带消息体与 MIME 类型（content type），通过以下构造函数创建：

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
- `(*Message) String() string`——按数据类型的字符串表示（调试用）；
- `Position(streamEpoch string, offset uint64) *sharedv2.Position`——构造恢复游标（供 `WithRecover` 使用）。

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

内部还有 `ReceivedMessage` 结构体（`ID`、`Channel`、`Offset`、`OffsetSet`、`Position`、`Replay`、`Message` 字段）承载完整的线上信息：`OffsetSet` 表示 offset 是否有效，`Position` 是频道位置（epoch + offset），`Replay` 标记恢复回放的消息。

## 传输

### WebSocket 客户端

`Dial(url string, opts ...Option) (Client, error)` 创建 WebSocket 客户端，URL 形如 `ws://localhost:9080/ws`。

编码通过 WebSocket 子协议（subprotocol）协商：`WithEncoding(EncodingJSON)` 对应子协议 `messageloop+json`（文本帧），`WithEncoding(EncodingProtobuf)` 对应 `messageloop+proto`（二进制帧），与协议规范（[../protocol.md](../protocol.md)）中的子协议一一对应。客户端在握手时通过 `Sec-WebSocket-Protocol` 头声明子协议；`WithDialTimeout` 同时作为握手超时。握手中的 Ping/Pong 控制帧由传输层自动应答。

### gRPC 客户端

`DialGRPC(addr string, opts ...Option) (Client, error)` 创建 gRPC 客户端，地址为 `host:port` 形式（如 `localhost:9090`），使用 `MessageLoopService/MessageLoop` 双向流传输协议消息。gRPC 传输固定使用 protobuf，无编码协商；连接使用 insecure 凭据，通过 `ForceCodec` 按连接注入名为 `messageloop-proto` 的原始编解码器，避免覆盖进程级全局 proto codec。

### QUIC / KCP 客户端

`DialQUIC(addr, opts...)` 与 `DialKCP(addr, dataShards, parityShards, opts...)` 复用同一套 TLS 配置逻辑：`WithTLSConfig` 提供的配置会按所选 Encoding 自动补 ALPN `NextProtos`；`WithInsecureSkipVerify` 跳过证书校验。断连时传输错误统一转换为 `DisconnectError`（见[错误处理](#错误处理)）。

### 重连与会话恢复

重连默认关闭，通过 `WithAutoReconnect(true)` 开启（client.go 的 `reconnectLoop` / `reconnect`）：

1. 接收循环报错且客户端未显式关闭时，触发重连流程；
2. 重连期间停止心跳循环，按指数退避重试：初始延迟 `ReconnectInitialDelay`（默认 1s），每次失败乘以 `ReconnectBackoffFactor`（默认 2.0），上限 `ReconnectMaxDelay`（默认 30s）；`ReconnectMaxAttempts` 限制总次数，`0` 为不限；用尽后 `OnError` 上报最终失败；
3. 每次尝试重新拨号（各传输复用原地址与编码），发送携带原 `SessionId` 的 Connect 消息，并对每个已订阅频道携带 `Recover: true` 与恢复游标（`Position{epoch, offset}`；无已记录 offset 的频道携带空 cursor，由服务端按其记录的投递位置决定续读点）——即会话恢复；服务端恢复成功时会在 Connected 响应中标记 `resumed`；
4. 重连期间通过连接代际（generation）计数丢弃旧连接的过期 Connected 响应，避免污染重连状态。

重连相关回调：

- `OnReconnecting(fn func(attempt int))`——每次重连尝试之前调用；
- `OnReconnected(fn func(sessionID string))`——重连成功之后调用。

### 心跳

开启后（`PingInterval > 0`，默认 30s），客户端按固定间隔发送 Ping 协议消息；收到 Pong 作为存活确认。`PingTimeout`（默认 10s）内未收到对应 Pong，连接视为半开：SDK 关闭当前 transport，接收循环观察到失败后（若开启自动重连）进入重连流程。

**服务端 Ping**：服务端也会主动向客户端发送 Ping（如开启 `server.heartbeat.ping_interval` 运维探测）。SDK 收到 Outbound `Ping` 会立即以同 id 的 Inbound `Pong` 应答，并把该次交换计为存活证据（与收到 Pong 同等处理），避免「服务端在 ping、客户端自己的 PingTimeout 却把连接掐了」。开启服务端 `ping_interval` 必须使用本版本 SDK——旧 SDK 会静默丢弃 Outbound Ping。

## 恢复订阅（Recover）

`SubscribeWith(channel, opts...)` 按历史位置恢复订阅，恢复语义由订阅级选项表达：

```go
// 从已知位置恢复：epoch "ep" 的 offset 42 之后
client.SubscribeWith("chat.recover", messageloopgo.WithRecover(messageloopgo.Position("ep", 42)))

// 无游标恢复（服务端按自身记录决定续读点；连接时首订等效于「无提示」）
client.SubscribeWith("chat.recover", messageloopgo.WithRecover(nil))

// 显式从头恢复（新鲜订阅）
client.SubscribeWith("chat.recover", messageloopgo.WithFresh())
```

- **恢复是流式的**：服务端先回 `SubscribeAck`，随后以 `Publication(replay=true)` 逐条回放，最后每频道一条 `RecoverComplete{position, truncated, gap, ...}`。注意 `offset 0` 不表示从头——那是一个具体的 offset；要从头用 `WithFresh()`。
- 回放消息走与普通消息同一条 `OnMessage` 投递路径；本地恢复游标只从两个来源推进：`RecoverComplete.position` 与普通（非 replay）实时消息的位置。回放消息本身不推进游标。
- 频道级恢复失败时，`RecoverComplete.error` 非空（或服务端以 `RECOVER_FAILED` 顶层错误信封拒绝），该频道需自行重订。
- 重连时的会话恢复（`resumeSubscriptions`）自动为所有已订阅频道携带 `Recover=true` + 已记录游标，本选项只影响首次订阅。

## Presence

SDK 暴露事件、快照与查询三类 Presence 能力：

```go
// join/leave 事件
client.OnPresence(func(ev messageloopgo.PresenceEvent) {
    // ev.Channel / ev.Action ("join" | "leave") / ev.Info.{SessionID,UserID,ClientID,ConnectedAt}
})

// 快照：SubscribeAck 携带，以及 Presence() 查询结果
client.OnPresenceSnapshot(func(snap messageloopgo.PresenceSnapshot) {
    // snap.Channel / snap.Clients []PresenceInfo / snap.Truncated / snap.Occupancy
})

// 主动查询（阻塞，等待服务端快照回复）
snap, err := client.Presence(ctx, "dev:room.x")
```

- 连接建立（Connected）本身不携带 presence 列表：服务端在连接后以独立的 Presence 信封推送快照，SDK 把未匹配 pending 查询的快照派发给 `OnPresenceSnapshot`；`SubscribeAck.presence` 在订阅状态写回后同样派发。
- `Presence(ctx, channel)` 发送 PresenceQuery，等待匹配的快照返回，并再调一次 `OnPresenceSnapshot`；失败（如 `PERMISSION_DENIED`）返回带服务端 code/message 的错误。断连与 `Close()` 会清掉 pending 查询。
- 未连接时返回 `not connected`。

## Survey（发起与应答）

### 客户端发起

```go
answers, err := client.Survey(ctx, "dev:chat.x", reqMsg, 2*time.Second)
for _, a := range answers {
    // a.SessionID / a.UserID（来自 metadata.entries["user_id"]）/ a.Payload / a.Error
}
```

- 发送 Inbound `SurveyRequest{request_id, channel, payload, timeout_ms}`；`timeout <= 0` 时 `timeout_ms=0`，由服务端策略上限决定。
- 等待发生在调用方 goroutine，接收循环只负责填充 pending 结果；结果按 `request_id` 与 `SurveyResult` 匹配，同步拒绝（如 `SURVEY_DISABLED`）以同 id 顶层 Error 返回；服务端 worker 失败可能不带 id——此时仅当恰好一个 in-flight `Survey()` 时按拒绝码（`SURVEY_DISABLED` / `SURVEY_TOO_MANY_SUBSCRIBERS` / `BAD_REQUEST` / `PERMISSION_DENIED` / `RATE_LIMITED` / `INTERNAL_ERROR`）交给它。
- `SurveyResult.error` 非空时整个调用返回该错误（answers 仍附带）。`ctx` 取消/超时、`Close()`、断连都会让 pending Survey 失败。
- `SurveyAnswer.UserID` 从 `metadata.entries["user_id"]` 读取，缺失时为空。

### 应答侧

```go
client.OnSurvey(func(requestID string, req *Message) (*Message, error) { ... })        // 旧签名，兼容
client.OnSurveyRequest(func(requestID, channel string, req *Message) (*Message, error) { ... }) // 新签名，带频道
```

收到 Outbound `SurveyRequest`：设了 `OnSurveyRequest` 用新签名；否则有 `OnSurvey` 用旧签名（忽略频道）；都没有则默认 echo 请求 payload。旧 `OnSurvey` 签名不变，现有应用无需改动。`SendSurveyReply(ctx, requestID, reply, replyErr)` 也可用于显式发送应答。

## 发布、订阅与频道管理

- `Publish(channel string, msg *Message) error`——向频道发布消息；可变参数 `transient ...bool` 传 `true` 时以瞬时方式发布（不写历史）。
- `PublishWith(channel, msg, opts...)` + `WithPublishToken(token)`——携带订阅级/发布级 token 的发布。
- `PublishWithAck(ctx, channel, msg, opts...) (uint64, error)`——发布并等待服务端 `PublishAck`，返回 broker 分配的 offset。
- `Subscribe` / `SubscribeAsync` / `Unsubscribe` / `UnsubscribeAsync`——见[订阅生效契约](#订阅生效契约subscribe--unsubscribe-等待服务端-ack)。
- `SubRefresh(ctx, channels ...string)`——ACL（如 token 轮换）变更后请服务端重新校验订阅；发送型方法，不等待 Ack。
- 订阅集合可在连接时一次性声明：`WithAutoSubscribe(...)` 会把频道随 Connect 消息一起提交，服务端确认后即生效。

动态订阅示例（参考 `example/dynamicsub`）：

```go
// 连接建立后按需订阅（阻塞等待 Ack）
for _, ch := range []string{"dev:channel.1", "dev:channel.2"} {
	if err := client.Subscribe(ch); err != nil {
		log.Printf("failed to subscribe %s: %v", ch, err)
	}
}

// 不再需要时退订
if err := client.Unsubscribe("dev:channel.1"); err != nil {
	log.Printf("failed to unsubscribe: %v", err)
}
```

会话恢复时，已订阅频道会带恢复游标重新声明，见上文「重连与会话恢复」。

## GapNotice

频道历史出现空洞（Redis broker catch-up 检出中洞或回放截断）时，服务端会向订阅者扇出 `GapNotice` 信封：

```go
client.OnGapNotice(func(n messageloopgo.GapNotice) {
    // n.Channel / n.GapReason / n.StreamEpoch / n.Offset（最后已知安全位置，n.OffsetSet 表示有效）
    // 应用层可据此触发全量重拉或提示用户
})
```

## RPC 与代理

### 客户端发起 RPC

```go
req := messageloopgo.NewMessageWithData("getUser", messageloopgo.NewJSONData(map[string]any{
	"userId": "123",
}))
resp := messageloopgo.NewMessage("")

// 阻塞直到收到响应、出错或超时
err := client.RPC(ctx, "dev:user.service", "GetUser", req, resp)
if err != nil {
	log.Fatal(err)
}
log.Printf("RPC response: %s", resp.String())
```

`RPC(ctx context.Context, channel, method string, req, resp *Message) error` 的行为：

- 请求带自增消息 ID，按 ID 与响应匹配（pendingRPC 表）；响应写入调用方传入的 `resp` 指针；
- **ctx 无 deadline 时自动套用 `WithRPCTimeout` 的默认超时（30s）**；
- 若服务端返回错误信封（Error 或 RpcReply 内嵌错误），RPC 立即以 `rpc error: <message> (code: <code>)` 形式失败，而非挂起至超时；
- 遵循 `ctx` 取消/超时；客户端 `Close()` 时所有挂起 RPC 也会被清理并返回错误。

### 代理（proxy）后端

RPC 的业务实现位于代理后端：服务端把客户端 RPC 请求转发到后端 gRPC 服务（`proxy/v2` 的 `ProxyService`），后端处理后返回。SDK 的 `proxy.go` 提供整套后端实现骨架：

- `RPCHandler` 接口：`HandleRPC(ctx context.Context, req *RPCRequest) (*RPCResponse, error)`，其中 `RPCRequest{ID, Channel, Method, Payload *Message}`、`RPCResponse{Payload *Message, Error *sharedv2.Error}`；
- `AuthHandler`：`Authenticate(ctx, *AuthenticateRequest) (*AuthenticateResponse, error)`，配合 `UserInfo`（含 ID 与可选 `Namespace`）返回用户信息；
- `ACLHandler`：`CheckSubscribeACL(ctx, channel, token string) error` 与 `CheckPublishACL(ctx, channel, token string) error`；
- `LifecycleHandler`：`OnConnected` / `OnDisconnected`（携带 sessionID 与 username）/ `OnSubscribed` / `OnUnsubscribed` 生命周期钩子；
- 默认实现 `RPCHandlerImpl` / `AuthHandlerImpl` / `ACLHandlerImpl` / `LifecycleHandlerImpl`（未实现时返回 `UNIMPLEMENTED`/`AUTH_NOT_IMPLEMENTED` 或放行）；
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

`NewProxyServer(opts ProxyServerOptions, handler proxypb.ProxyServiceServer) (*ProxyServer, error)` 创建 gRPC 代理服务。注意：`Insecure=false` 时服务端当前仍以明文监听（构造器不安装 TLS 凭据）并打印警告，需要 TLS 时请在前面自行挂 TLS 终结层。服务端侧的代理集成（路由、超时）见[《架构指南》](01-architecture.md) 与[《配置参考》](02-configuration.md)。

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
- **`DisconnectError`**：服务端主动断开时，SDK 把断连事件转换为 `*DisconnectError`（可用 `errors.As` 提取），暴露 `Code`（数值断连码）与 `Reason`。三种传输的断连载体统一到该类型：WebSocket close 帧、gRPC 错误信封的 `disconnect_code` metadata、QUIC application error code。各码值语义见[《可观测性指南》](05-observability.md) 的断连码参考。
- 重连策略：默认关闭；开启后按「重连与会话恢复」一节所述退避重试，达到 `ReconnectMaxAttempts` 后调用 `OnError` 上报最终失败。SDK 不依据断连码区分重连决策（3503 `DisconnectForceNoReconnect` 也会重试），需要区分时在应用层检查 `DisconnectError.Code`。

## 会话信息

- `SessionID() string`——当前会话 ID（连接建立后有效）；
- `IsConnected() bool`——是否已连接。

## 示例清单

`example/` 下的可运行示例：

| 目录 | 说明 |
| --- | --- |
| `basicwebsocket` | WebSocket 客户端最小闭环：连接、订阅、发布、收消息（JSON 编码） |
| `basicgrpc` | gRPC 客户端：连接、订阅、发布，并在同一连接上发起 RPC |
| `basicquic` | QUIC 客户端：`DialQUIC` + `WithInsecureSkipVerify` 对接开发服务器 |
| `dynamicsub` | 动态订阅管理：连接后逐个订阅、定时退订 |
| `protobuf` | 使用 `EncodingProtobuf` 的 WebSocket 客户端与发布 |
| `wsrpc` | 在 WebSocket 连接上发起 RPC 调用并读取响应 |
| `proxyserver` | 代理后端 gRPC 服务：RPC 处理（switch 与 RPCMux 两种写法）、认证、ACL、生命周期钩子 |

## 迁移

仓库内 `sdks/go/MIGRATION_GUIDE.md` 纠正早期示例残留的 CloudEvents 用法：当前 SDK 不是 CloudEvents API，发布、RPC 与订阅回调统一使用 `Message` / `Data`（如 `Publish(channel, event)` → `Publish(channel, msg)`、`NewCloudEvent(...)` → `NewMessageWithData(type, data)`、`OnMessage(func([]*cloudevents.Event))` → `OnMessage(func([]*Message))`）；凡是出现 `CloudEvent`、`NewCloudEvent` 或 `cloudevents.Event` 的 Go SDK 示例均视为历史文档，以 `message.go`、`client.go` 与 `example/` 为准。

## 构建与测试

SDK 是独立 Go module，必须在 `sdks/go/` 目录内构建与测试（根目录的 `go build ./...` 不会覆盖它）：

```bash
cd sdks/go
go build ./...
go test ./...
```

测试套件包含 `client_test.go`、`message_test.go`、`proxy_test.go`、`subscribe_ack_test.go`（订阅生效契约）等（重连、RPC 竞态、处理器覆盖等场景）。环境要求、仓库模块划分与发布流程（`sdks/go/vX.Y.Z` 标签）见[《开发指南》](06-development.md)。
