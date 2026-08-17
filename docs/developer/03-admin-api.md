# 管理 API 参考

## 概述

管理 API（Admin API）是 MessageLoop 服务端对外提供的 gRPC 管理接口，用于以服务端身份执行发布、断连、订阅管理、在线状态查询、历史消息查询等操作。它与客户端协议（见 [../protocol.md](../protocol.md)）面向不同的使用方：客户端协议是客户端通过 WebSocket 或 gRPC 流式通道与服务器通信的协议；管理 API 则是运维工具、内部服务与 SDK 后端集成使用的独立 gRPC 服务。

管理 API 定义于 `protocol/server/v1/api.proto`，服务名为 `APIService`，完整限定名（fully-qualified name）为：

```
messageloop.server.v1.APIService
```

所有 RPC 均为普通一元调用（unary call），不涉及流式传输。管理 API 监听在独立的端口上，地址由配置项 `server.grpc_admin.addr` 指定（见[《配置参考》](02-configuration.md)）。在进程内部，管理 API 的处理器与客户端流共享同一个 `Node` 实例，因此管理操作直接作用于在线客户端会话。

服务共声明 8 个 RPC：

| RPC | 说明 |
| --- | --- |
| `Publish` | 服务端向频道或指定会话发布消息 |
| `Disconnect` | 强制断开客户端会话 |
| `Subscribe` | 让某个会话订阅频道 |
| `Unsubscribe` | 让某个会话取消订阅频道 |
| `Survey` | 向频道所有订阅者发起调查（survey）并收集应答 |
| `GetPresence` | 查询频道内的在线客户端 |
| `GetHistory` | 查询频道的消息历史 |
| `GetChannels` | 列出活跃频道及其订阅者数量 |

## 传输与鉴权

管理 API 使用标准的 gRPC 传输，监听地址为 `server.grpc_admin.addr`。相关的配置键如下：

| 配置键 | 说明 |
| --- | --- |
| `server.grpc_admin.addr` | 管理 API 监听地址（监听器在启动预检阶段即绑定） |
| `server.grpc_admin.auth_token` | 管理 API 访问令牌；`addr` 非空时该字段与 `allow_insecure` 必须至少设置一个（否则配置校验失败） |
| `server.grpc_admin.allow_insecure` | 显式放弃强制鉴权（仅限开发/受控环境）；置为 true 时 `auth_token` 可留空 |
| `server.grpc_admin.tls.cert_file` / `server.grpc_admin.tls.key_file` | TLS 证书与私钥，必须同时设置或同时留空 |

### 鉴权

当 `server.grpc_admin.auth_token` 配置了非空值时，管理服务器会为所有一元 RPC 安装一个 unary interceptor（见 `pkg/grpcstream/server.go` 中的 `adminAuthInterceptor`）。该拦截器要求每个请求的 gRPC 元数据（metadata）中带有 `authorization` 头，格式为：

```
authorization: Bearer <token>
```

校验规则如下：

- 缺少元数据或缺少 `authorization` 头时，返回 gRPC 状态码 `Unauthenticated`；
- `authorization` 头不是 `Bearer ` 前缀格式时，返回 `Unauthenticated`；
- 令牌不匹配时，返回 `Unauthenticated`。

令牌比较使用常数时间比较（constant-time comparison），避免通过响应时间探测令牌内容。注意：鉴权仅校验令牌是否匹配，不区分调用方身份。

生产环境必须配置 `server.grpc_admin.auth_token`，并将管理端口绑定到回环或私有网络接口（见 [../deployment.md](../deployment.md)）。鉴权未启用时，任何能访问该端口的主机都可以执行全部管理操作。

### TLS

当 `server.grpc_admin.tls.cert_file` 与 `server.grpc_admin.tls.key_file` 成对设置时，管理服务器以 TLS 方式服务；二者必须同时设置或同时留空（配置校验见《配置参考》[02-configuration.md](02-configuration.md)）。

### grpcurl 调用

服务器未注册 gRPC 反射服务（reflection），因此 `grpcurl` 无法通过 `list`/`describe` 自动发现服务，调用时需要显式指定 proto 文件路径：

```bash
grpcurl \
  -import-path ./protocol \
  -proto server/v1/api.proto \
  -H "authorization: Bearer <token>" \
  -plaintext \
  -d '{"channel": "chat.general"}' \
  127.0.0.1:9091 \
  messageloop.server.v1.APIService/GetPresence
```

- `-import-path ./protocol` 指向 `protocol/` 目录，使 `server/v1/api.proto` 中的 `shared/v1/errors.proto`、`shared/v1/types.proto` 等 import 可以解析；
- 启用 TLS 时去掉 `-plaintext`，并按需传入 `-cacert <ca.pem>`（使用自签证书时也可用 `-insecure`）；
- 请求体使用 proto3 JSON 映射，字段名为 lowerCamelCase（例如 `request_id` → `requestId`、`timeout_ms` → `timeoutMs`）。

## RPC 参考

以下各节按 `protocol/server/v1/api.proto` 中的声明顺序逐一说明。请求/响应消息的字段名一律采用 proto 中的原始名称。

### Publish

服务端向频道（channel）或指定会话（session）发布消息。

请求消息 `PublishRequest`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `request_id` | `string` | 请求标识，仅用于日志关联，不会回显 |
| `publications` | `repeated Publication` | 待发布的出版物列表，可包含多条 |

`Publication`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | `string` | 出版物标识；会话投递时作为消息的 `id` 透传给客户端 |
| `destination` | `Destination` | 投递目标：`sessions`（会话 ID 列表）、`channels`（频道列表）或 `users`（用户 ID 列表），可以同时指定 |
| `options` | `Options` | 投递选项，目前仅声明 `add_history` |
| `payload` | `shared.v1.Payload` | 消息载荷，支持 `text`、`binary`、`json` 三种形式 |
| `metadata` | `shared.v1.Metadata` | 已声明但当前处理器未使用，会被忽略 |

`Publication.Options.add_history`：控制频道发布是否写入历史。
- `true`：写入 broker 历史，后续可通过 `GetHistory` 补拉。
- `false` 或未设置：以 transient 方式发布，不写历史。
- 会话目标（`destination.sessions`）与用户目标（`destination.users`）不受该选项影响，始终直接投递到会话。

响应消息 `PublishResponse`：空消息，不返回任何字段。单条出版物的投递结果（例如 broker 分配的 offset）不会暴露给调用方。

语义：

- 载荷转换：`binary` 直接使用原始字节；`text` 按 UTF-8 字节发送；`json` 会被序列化为 JSON 字节后按文本发送。载荷为 nil 时发送空载荷。
- 频道投递：默认以 transient 方式发布，不写历史；仅当 `options.add_history` 为 `true` 时通过 broker 的 `Publish` 路径发布并写入历史，与客户端发布走同一管道（见[《架构指南》](01-architecture.md)）。
- 会话投递：向目标会话直接发送一条 `publication` 信封，消息的 `channel` 字段为空字符串（会话定向消息没有频道），`id` 为 `Publication.id`。目标会话不存在时**跳过**该投递，不报错、不计入失败（仅记录 debug 日志）。
- 用户投递：`destination.users` 里的每个用户先展开为该用户的**全部 session**（单节点来自本地 hub 的 user 索引；集群下并上 Redis user 索引），展开结果与 `destination.sessions` **取并集（去重）**后走会话投递；`channels` 可同时指定或留空。**只有 users 的 destination 是合法的**，与 sessions 一样按会话逐个 best-effort 投递。展开时**始终校验** session lease 的 `UserID`（本地客户端则校验 `Client.UserID()`）：索引里的陈旧/投毒条目会被跳过；索引 miss 不做全集群 SCAN，靠周期 repair 收敛。
- 部分失败语义：由于 `PublishResponse` 没有按条目返回的字段，失败只能通过整体结果表达。每条失败投递（目标会话发送失败、目标频道发布失败、载荷序列化失败、缺少 destination）都会记录错误日志；仅当**所有**投递尝试全部失败时，RPC 返回状态码 `Internal`（错误信息形如 `all N delivery attempt(s) failed`）；只要有一条成功，RPC 就返回空响应。
- destination 为 nil 或 `sessions`、`channels`、`users` 均为空时，该条出版物视为失败。
- `destination.users`（以及其它按 user 字段）中的**空字符串**是客户端错误：RPC 返回 `InvalidArgument`，且**不做任何扫描**（匿名连接不可按 user 寻址）。

返回的错误码：`Internal`、`InvalidArgument`。

集群感知：见 [集群感知行为](#集群感知行为)。

### Disconnect

强制断开一个或多个客户端会话。

请求消息 `DisconnectRequest`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `sessions` | `repeated string` | 要断开的会话 ID 列表 |
| `code` | `uint32` | 断开码（disconnect code），原样传给客户端 |
| `reason` | `string` | 人类可读的断开原因，原样传给客户端 |
| `users` | `repeated string` | 要断开的用户 ID 列表；每个用户展开为其全部 session，与 `sessions` 取并集（去重）后逐个断开 |

响应消息 `DisconnectResponse`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `results` | `map<string, bool>` | 以会话 ID 为键、是否断开成功为值 |

语义：

- 对展开后的每个会话逐一执行断开（`users` 展开 + `sessions` 并集、去重）。每个会话独立得到一个布尔结果：会话存在且断开成功为 `true`；会话不存在或断开过程出错为 `false`。RPC 本身不会因为个别会话失败而返回错误。
- 按 user 展开与 Publish 相同：展开时始终校验 session lease 的 `UserID`（或本地 `Client.UserID()`），索引陈旧条目被跳过；`users` 中的空字符串返回 `InvalidArgument` 且不扫描。
- 服务端会以指定的 `code` 与 `reason` 构造 `Disconnect` 并关闭客户端连接，客户端在协议层收到对应的断开通知（见[《客户端协议参考》](../protocol.md) 中的 Disconnect Codes 一节）。`code` 由调用方决定，服务端不做合法性校验；源码中内置的常量定义于 `disconnect.go`，例如：

| 常量 | code | reason |
| --- | --- | --- |
| `DisconnectConnectionClosed` | 3000 | `connection closed` |
| `DisconnectInvalidToken` | 3500 | `invalid token` |
| `DisconnectBadRequest` | 3501 | `bad request` |
| `DisconnectStale` | 3502 | `stale` |
| `DisconnectForceNoReconnect` | 3503 | `force disconnect` |
| `DisconnectConnectionLimit` | 3504 | `connection limit` |
| `DisconnectChannelLimit` | 3505 | `channel limit` |
| `DisconnectPermissionDenied` | 3507 | `permission denied` |
| `DisconnectIdleTimeout` | 3511 | `idle timeout` |
| `DisconnectSlowConsumer` | 3512 | `slow consumer` |

集群感知：见 [集群感知行为](#集群感知行为)。

### Subscribe

让指定会话订阅一个或多个频道。

请求消息 `SubscribeRequest`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `session_id` | `string` | 目标会话 ID |
| `channels` | `repeated string` | 要订阅的频道列表 |
| `user_id` | `string` | 目标用户 ID；展开为该用户的全部 session，与 `session_id` 取并集；`session_id` 与 `user_id` **都为空时**返回 `InvalidArgument` |

响应消息 `SubscribeResponse`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `results` | `map<string, bool>` | 以频道名为键、是否订阅成功为值 |

语义：

- 订阅以单个会话为目标（会话级管理操作，不是广播订阅），实际调用的是客户端协议中「连接时附带订阅」之外的完整订阅路径（`AddSubscription`），包括 broker 订阅注册、在线状态（presence）登记与集群状态同步，因此与客户端主动订阅行为等价。
- 按 `user_id` 展开后，对每个频道在每个会话上执行订阅；**任一 session 成功则该频道结果为 `true`**，全部失败才为 `false`。会话不存在或订阅过程出错时对应频道为 `false`。RPC 本身不因个别频道失败而返回错误。
- 订阅已存在的频道是幂等的，重复订阅返回 `true` 且不产生副作用。
- `user_id` 的展开与 Publish/Disconnect 相同：校验 lease 的 `UserID`，`session_id` 与 `user_id` 都为空时 `InvalidArgument` 且不扫描。

集群感知：见 [集群感知行为](#集群感知行为)。

### Unsubscribe

让指定会话取消订阅一个或多个频道。

请求消息 `UnsubscribeRequest`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `session_id` | `string` | 目标会话 ID |
| `channels` | `repeated string` | 要取消订阅的频道列表 |
| `user_id` | `string` | 目标用户 ID；与 Subscribe 对称：展开为用户全部 session，与 `session_id` 取并集；都为空时 `InvalidArgument` |

响应消息 `UnsubscribeResponse`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `results` | `map<string, bool>` | 以频道名为键、是否取消订阅成功为值 |

语义：与 `Subscribe` 对称。对每个频道独立执行取消订阅（多 session 时任一 session 成功则该频道为 `true`），包括从 hub 移除订阅、broker 反注册、在线状态清除与集群状态同步。会话不存在、频道未被该会话订阅或操作出错时对应结果为 `false`。

集群感知：见 [集群感知行为](#集群感知行为)。

### Survey

向频道内的所有订阅者发送调查请求（survey request），并在超时时间内收集应答。

请求消息 `SurveyRequest`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `request_id` | `string` | 请求标识，原样回显在响应中 |
| `channel` | `string` | 目标频道；所有订阅者都会收到调查 |
| `payload` | `shared.v1.Payload` | 调查载荷，支持 `text`、`binary`、`json`；发送给客户端时封装为二进制载荷 |
| `metadata` | `shared.v1.Metadata` | 已声明但当前处理器未使用，会被忽略 |
| `timeout_ms` | `int32` | 收集应答的等待时长（毫秒）；`<= 0` 时使用默认等待时长 5 秒 |

响应消息 `SurveyResponse`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `request_id` | `string` | 回显请求标识 |
| `results` | `repeated SurveyResult` | 收集到的应答列表 |

`SurveyResult`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `session_id` | `string` | 应答来源会话 ID |
| `payload` | `shared.v1.Payload` | 应答载荷，统一以二进制形式返回 |
| `metadata` | `shared.v1.Metadata` | 附加元数据；集群模式下可能包含 `node_id` 与 `incarnation_id` 条目（见 [集群感知行为](#集群感知行为)） |
| `error` | `shared.v1.Error` | 该会话应答失败时的错误信息 |

语义：

- 调查只发送给目标频道的订阅者。发送前会记录被调查的会话集合，只有这些会话的应答才会被接受，来自其他会话的应答视为伪造并被丢弃。
- 每个订阅者的调查请求发送受独立超时约束（10 秒）：发送失败的会话会以一条 `error` 应答记录失败，不会阻塞整个调查。
- 等待应答的总时长由 `timeout_ms` 决定；`timeout_ms <= 0` 时按 5 秒处理（`defaultSurveyWaitTimeout`），而不是立即超时。
- 应答按会话 ID 去重，同一会话的多次应答以最后一次为准。
- 单会话失败时，对应 `SurveyResult.error` 的 `code` 固定为 `SURVEY_FAILED`，`message` 为失败原因（例如发送超时、客户端传输错误）。
- 若调查无法执行（例如频道没有订阅者，或并发调查数量达到上限 1000），RPC 返回错误：无订阅者时返回空结果；注册表已满时返回错误信息 `survey registry full (limit 1000)`。
- 结果为按会话 ID 排序后的列表（集群模式下排序键为节点 ID、实例 ID、会话 ID）。

返回的错误码：`Unknown`（来自 Node 内部的错误原样透传）。

集群感知：见 [集群感知行为](#集群感知行为)。

### GetPresence

查询某个频道的在线客户端（presence）列表。

请求消息 `GetPresenceRequest`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `channel` | `string` | 目标频道 |

响应消息 `GetPresenceResponse`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `clients` | `map<string, PresenceInfo>` | 在线客户端列表，键为客户端标识 |

`PresenceInfo`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `client_id` | `string` | 客户端标识；当前实现中即会话 ID（session ID），与映射键一致 |
| `user_id` | `string` | 连接时声明的用户 ID，可为空 |
| `connected_at` | `int64` | 该客户端在频道中登记的 Unix 毫秒时间戳 |

语义：

- 只返回订阅该频道时登记的在线客户端（订阅与在线状态登记见 [../protocol.md](../protocol.md)）。临时订阅（ephemeral）不登记在线状态。
- 频道无在线数据时返回空映射，不报错。

集群感知：见 [集群感知行为](#集群感知行为)。

### GetHistory

查询频道的消息历史（history）。

请求消息 `GetHistoryRequest`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `channel` | `string` | 目标频道 |
| `since_offset` | `uint64` | 起始偏移（offset），语义见下文 |
| `limit` | `int32` | 返回条数上限；`<= 0` 时使用默认上限 1000 |

响应消息 `GetHistoryResponse`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `publications` | `repeated HistoryPublication` | 命中的历史消息列表 |

`HistoryPublication`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `offset` | `uint64` | 该条消息在频道历史中的偏移 |
| `payload` | `shared.v1.Payload` | 消息载荷；文本消息以 `text` 返回，其余以 `binary` 返回 |
| `is_text` | `bool` | 是否文本消息 |
| `time` | `int64` | 消息时间（Unix 毫秒） |

语义：

- 查询直接落到 broker 的历史存储。两种实现下 `since_offset` 均为**包含（inclusive）**语义：返回 `offset >= since_offset` 的消息（`Broker.History` 契约，`broker.go:105-108`；内存实现与 Redis 实现一致）。

- 两种实现下，`limit <= 0` 都使用默认上限 `DefaultHistoryLimit`（1000 条）。
- 没有分页游标：`limit` 就是单次返回的硬上限，`since_offset` 是唯一的前进指针。
- 历史被禁用（transient 消息）或频道无历史时返回空列表，不报错。

集群感知：见 [集群感知行为](#集群感知行为)。

### GetChannels

列出活跃频道及其订阅者数量。

请求消息 `GetChannelsRequest`：空消息，**无分页参数**，一次返回全部活跃频道。

响应消息 `GetChannelsResponse`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `channels` | `repeated ChannelInfo` | 活跃频道列表 |

`ChannelInfo`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `name` | `string` | 频道名 |
| `subscribers` | `int32` | 当前订阅者数量 |

集群感知：见 [集群感知行为](#集群感知行为)。

## 错误模型

管理 API 的错误分两层：协议层错误消息与 gRPC 状态码。

### 错误消息（`messageloop.shared.v1.Error`）

`protocol/shared/v1/errors.proto` 中只声明了一个消息类型：

```protobuf
message Error {
  string code = 1;
  string type = 2;
  string message = 3;
  google.protobuf.Struct metadata = 4;
}
```

注意：`errors.proto` 中**没有枚举**——`code`、`type` 都是自由字符串（free-form string），`metadata` 为任意结构化数据。这与客户端协议中的错误信封是一致的（见[《客户端协议参考》](../protocol.md) 的 Error Codes 一节），但取值上不共享同一个受控词汇表。

`Error` 消息在当前管理 API 中只出现于 `SurveyResult.error`，且 `code` 固定为 `SURVEY_FAILED`；`type` 与 `metadata` 未被填充。其余管理 RPC 不通过 `Error` 消息报告失败，而是直接使用 gRPC 状态码。

### gRPC 状态码映射

管理处理器返回失败时使用的状态码如下：

| gRPC 状态码 | 触发条件 |
| --- | --- |
| `Unauthenticated` | 鉴权拦截器判定失败：缺少 `authorization` 元数据、格式不是 `Bearer <token>`、或令牌不匹配 |
| `InvalidArgument` | 按 user 字段中出现空字符串（`destination.users`、`Disconnect.users` 的空条目，或 `Subscribe`/`Unsubscribe` 的 `session_id` 与 `user_id` 同时为空）——**不做任何扫描** |
| `Internal` | `Publish` 请求中的所有投递尝试全部失败 |
| `Unknown` | 其余错误：来自 Node 内部方法的错误（例如 `Survey` 调查注册表已满、presence/history 存储错误）原样透传，gRPC 框架将其映射为 `Unknown` |

管理 API 不定义自定义状态码（自定义 code 仅存在于 `shared.v1.Error` 的自由字符串 `code` 字段中）。调用方应同时处理 gRPC 状态码（区分错误类别）与 `SurveyResult.error`（区分单个会话的失败）。

## 示例

以下示例假设管理端口监听在 `127.0.0.1:9091`，且已配置 `auth_token`（示例中用 `<token>` 占位）。服务器未注册 gRPC 反射，所有命令都必须携带 `-import-path ./protocol -proto server/v1/api.proto`。JSON 载荷使用 proto3 JSON 映射的 lowerCamelCase 字段名；`binary` 载荷在 JSON 中以 base64 表示。

### Publish

发布到频道：

```bash
grpcurl \
  -import-path ./protocol \
  -proto server/v1/api.proto \
  -H "authorization: Bearer <token>" \
  -plaintext \
  -d '{
    "requestId": "admin-publish-1",
    "publications": [{
      "id": "admin-msg-1",
      "destination": {"channels": ["chat.general"]},
      "payload": {"text": "hello from admin"}
    }]
  }' \
  127.0.0.1:9091 \
  messageloop.server.v1.APIService/Publish
```

发布到指定会话（JSON 载荷会按文本发送）：

```bash
grpcurl \
  -import-path ./protocol \
  -proto server/v1/api.proto \
  -H "authorization: Bearer <token>" \
  -plaintext \
  -d '{
    "publications": [{
      "id": "direct-msg-1",
      "destination": {"sessions": ["abc-123"]},
      "payload": {"json": {"type": "notice", "content": "server restarting"}}
    }]
  }' \
  127.0.0.1:9091 \
  messageloop.server.v1.APIService/Publish
```

按用户发布（只填 `users`，投递给该用户的全部 session）：

```bash
grpcurl \
  -import-path ./protocol \
  -proto server/v1/api.proto \
  -H "authorization: Bearer <token>" \
  -plaintext \
  -d '{
    "publications": [{
      "id": "user-notice-1",
      "destination": {"users": ["alice"]},
      "payload": {"text": "multi-device notice"}
    }]
  }' \
  127.0.0.1:9091 \
  messageloop.server.v1.APIService/Publish
```

### Survey

```bash
grpcurl \
  -import-path ./protocol \
  -proto server/v1/api.proto \
  -H "authorization: Bearer <token>" \
  -plaintext \
  -d '{
    "requestId": "admin-survey-1",
    "channel": "chat.general",
    "payload": {"text": "ping"},
    "timeoutMs": 3000
  }' \
  127.0.0.1:9091 \
  messageloop.server.v1.APIService/Survey
```

### Disconnect

使用内置断开码 3503（`DisconnectForceNoReconnect`）：

```bash
grpcurl \
  -import-path ./protocol \
  -proto server/v1/api.proto \
  -H "authorization: Bearer <token>" \
  -plaintext \
  -d '{
    "sessions": ["abc-123"],
    "code": 3503,
    "reason": "scheduled maintenance"
  }' \
  127.0.0.1:9091 \
  messageloop.server.v1.APIService/Disconnect
```

响应示例（按会话返回结果）：

```json
{
  "results": {
    "abc-123": true
  }
}
```

### Subscribe

```bash
grpcurl \
  -import-path ./protocol \
  -proto server/v1/api.proto \
  -H "authorization: Bearer <token>" \
  -plaintext \
  -d '{
    "sessionId": "abc-123",
    "channels": ["chat.general", "notifications"]
  }' \
  127.0.0.1:9091 \
  messageloop.server.v1.APIService/Subscribe
```

### Unsubscribe

```bash
grpcurl \
  -import-path ./protocol \
  -proto server/v1/api.proto \
  -H "authorization: Bearer <token>" \
  -plaintext \
  -d '{
    "sessionId": "abc-123",
    "channels": ["chat.general"]
  }' \
  127.0.0.1:9091 \
  messageloop.server.v1.APIService/Unsubscribe
```

### GetPresence

```bash
grpcurl \
  -import-path ./protocol \
  -proto server/v1/api.proto \
  -H "authorization: Bearer <token>" \
  -plaintext \
  -d '{"channel": "chat.general"}' \
  127.0.0.1:9091 \
  messageloop.server.v1.APIService/GetPresence
```

### GetHistory

```bash
grpcurl \
  -import-path ./protocol \
  -proto server/v1/api.proto \
  -H "authorization: Bearer <token>" \
  -plaintext \
  -d '{"channel": "chat.general", "sinceOffset": 42, "limit": 100}' \
  127.0.0.1:9091 \
  messageloop.server.v1.APIService/GetHistory
```

### GetChannels

请求为空消息，使用 `-d '{}'`：

```bash
grpcurl \
  -import-path ./protocol \
  -proto server/v1/api.proto \
  -H "authorization: Bearer <token>" \
  -plaintext \
  -d '{}' \
  127.0.0.1:9091 \
  messageloop.server.v1.APIService/GetChannels
```

## 集群感知行为

启用集群（`cluster.enabled: true`，要求 `broker.type: redis`）后，部分管理操作的行为发生变化。集群架构与配置详见[《分布式集群指南》](04-cluster.md)。

| 操作 | 集群模式下的行为 |
| --- | --- |
| `Publish`（会话投递） | 通过会话租约（session lease）解析会话所在节点；会话由远端节点持有时，投递请求经 Redis 命令总线（command bus）路由到该节点执行 |
| `Disconnect`、`Subscribe`、`Unsubscribe` | 与 `Publish` 会话投递相同：先解析会话租约，远端会话的操作经命令总线下发到持有该会话的节点执行；会话不存在时对应条目返回 `false` |
| 按 user 展开（`Publish.users`、`Disconnect.users`、`Subscribe/Unsubscribe.user_id`） | 本地 hub 的 user 索引（`Hub.SessionsByUser`）并上 Redis user→sessions 索引（`ml2:cluster:user:sessions:<user_id>` 集合 + `ml2:cluster:user:member:<user_id>:<session_id>` 成员键，TTL 与 session lease 相同）；展开时对每个 session 校验 lease 的 `UserID`，不匹配或缺失则跳过；索引 miss 不做全集群 SCAN，靠周期 repair 收敛 |
| `Survey` | 除本地调查外，还会通过命令总线向集群内所有其他节点广播调查请求（排除自身，`exclude_self`），聚合各节点的应答后统一排序返回；每个 `SurveyResult` 会附带 `node_id` 与 `incarnation_id` 元数据，标识应答来源节点；集群中某个节点执行调查失败时，该节点会以一条带 `error` 的 `SurveyResult` 表示（`code` 为 `SURVEY_FAILED`） |
| `GetChannels` | 不再查询本地 hub，而是读取集群共享的频道投影（query store），返回全集群的活跃频道与订阅者数量 |
| `GetPresence` | 在线状态存储在集群模式下替换为 Redis 支撑的存储，查询返回全集群的在线客户端 |
| `GetHistory` | 从共享的 Redis Stream 读取历史，`since_offset` 为包含（inclusive）语义，数据跨节点一致 |

非集群模式下，会话定向操作只作用于本节点（未知会话返回 `false`），`Survey` 只调查本节点订阅者，`GetChannels` 与 `GetPresence` 只反映本节点状态。

## 实现说明

- **共享 Node，分离监听器**：管理 API 处理器（`pkg/grpcstream/api_handler.go`）持有与客户端流服务器同一个进程内 `Node` 实例（装配见 `cmd/server/runtime.go`）。管理 RPC 与客户端流量在监听器层面完全分离：客户端流式 gRPC 监听 `transport.grpc.addr`，管理 API 监听 `server.grpc_admin.addr`；管理端口只注册 `APIService`，客户端端口只注册 `MessageLoopService`。
- **监听器预绑定**：两个 gRPC 监听器都在启动预检阶段（`node.Run` 之前）完成 `net.Listen`，任一监听失败都不会留下已启动的 Node 副作用；两个监听器的组件名分别为 `grpc-client-server` 与 `grpc-admin-server`。
- **RawCodec**：两个 gRPC 服务器都通过 `grpc.ForceServerCodec` 装配名为 `messageloop-proto` 的 `RawCodec`（`pkg/grpcstream/codec.go`）。该 codec 对普通 proto 消息仍使用标准 `proto.Marshal`/`proto.Unmarshal`，因此管理 API 的线上编码与标准 protobuf gRPC 完全兼容（这是 `grpcurl -proto` 方式可以正常调用的原因）；流式路径额外支持免二次编解码的原始帧（raw frame）优化。codec 按服务器注册而不是全局注册，避免覆盖进程内其他 gRPC 连接的默认 codec。
- **压缩**：gRPC 的 gzip 压缩编解码器已在服务器侧注册，客户端可在请求中声明 `grpc-accept-encoding: gzip`。
- **管理服务器未设置 `MaxRecvMsgSize`**：管理服务器使用 gRPC 默认的最大接收消息大小（4 MiB）；客户端流服务器则应用 `limits.max_message_size`（默认 64 KiB，见[《配置参考》](02-configuration.md)）。
- **调用方客户端**：Go SDK 的后端集成通过本管理 API 与服务端通信（见[《Go SDK 指南》](07-sdk-go.md)）；TypeScript SDK 是纯 WebSocket 客户端，不包含管理 API 客户端。Go SDK 生成的桩代码依赖 `server/v1/api.proto`，调用前请确保协议版本与服务器一致。
- **运维**：健康检查与指标走独立的 HTTP 管理面（`server.http.addr`），不属于本 API 范围（见[《可观测性指南》](05-observability.md)）。
