# Server API 参考（服务端 API）

## 概述

Server API（服务端 API，曾称 Admin API / 管理 API）是 MessageLoop 服务端对外提供的服务端 gRPC 接口，用于以服务端身份执行发布、断连、订阅管理、在线状态查询、历史消息查询等操作。它与客户端协议（见 [../protocol.md](../protocol.md)）面向不同的使用方：客户端协议是客户端通过 WebSocket 或 gRPC 流式通道与服务器通信的协议；Server API 则是运维工具、内部服务与 SDK 后端集成使用的独立 gRPC 服务。

Server API 定义于 `protocol/server/v2/api.proto`，服务名为 `APIService`，完整限定名（fully-qualified name）为：

```
messageloop.server.v2.APIService
```

所有 RPC 均为普通一元调用（unary call），不涉及流式传输。Server API 监听在独立的端口上，地址由配置项 `server.api.addr` 指定（必填，见[《配置参考》](02-configuration.md)）。在进程内部，Server API 的处理器（internal/serverapi/api_handler.go）与客户端流共享同一个 `Node` 实例，因此Server API 操作直接作用于在线客户端会话。

服务共声明 8 个 RPC：

| RPC | 说明 |
| --- | --- |
| `Publish` | 服务端向频道、指定会话或指定用户发布消息 |
| `Disconnect` | 强制断开客户端会话 |
| `Subscribe` | 让某个会话订阅频道 |
| `Unsubscribe` | 让某个会话取消订阅频道 |
| `Survey` | 向频道所有订阅者发起调查（survey）并收集应答 |
| `GetPresence` | 查询频道内的在线客户端 |
| `GetHistory` | 查询频道的消息历史 |
| `GetChannels` | 列出活跃频道及其订阅者数量 |

## 传输与鉴权

Server API 使用标准的 gRPC 传输，监听地址为 `server.api.addr`。相关的配置键如下：

| 配置键 | 说明 |
| --- | --- |
| `server.api.addr` | Server API 监听地址（必填，监听器在启动预检阶段即绑定） |
| `server.api.auth_tokens` | 静态超管 Bearer token **列表**；每把 ≥ 20 字符（Validate 强制），任一命中即超管身份 |
| `server.api.auth_cache_ttl` | API Key 经 proxy 校验后的正缓存 TTL（默认 `30s`），是撤销传播的时间上界 |
| `server.api.allow_insecure` | 显式放弃强制鉴权（仅限回环绑定：非 loopback 地址 + `allow_insecure` 是 Validate 错误，fail-closed） |
| `server.api.tls.cert_file` / `server.api.tls.key_file` | TLS 证书与私钥，必须同时设置或同时留空 |
| `server.api.capabilities` | 节点能力位上限（每个调用方身份生效的上界），见下文[能力位](#能力位capabilities) |
| `proxy[].api_auth` | 将某个 proxy 条目指派为 API Key 校验器（全配置唯一），见下文[API Key 鉴权](#api-key-鉴权proxy-托管) |

**启动硬性要求**：`server.api.addr` 非空时，`auth_tokens`、`proxy[].api_auth: true` 指派、`allow_insecure: true` 三种凭证路径必须至少配置一种，否则配置校验失败——不存在"不配置即放行"的默认。

### 鉴权（三种凭证路径）

Server API 认证由挂在Server API gRPC 服务器上的 unary 拦截器完成（internal/serverapi/auth.go，装配见 internal/serverapi/server.go；客户端流 gRPC 监听器不安装该拦截器）。拦截器从 gRPC 元数据提取凭证：`authorization: Bearer <凭证>` 优先，`authorization` 头缺失或不是 `Bearer ` 前缀时回退 `x-api-key`。随后按固定顺序裁决：

1. **无凭证**：已配置 `auth_tokens` → `Unauthenticated`；配置了 `allow_insecure` → 注入 insecure 超管身份放行（每个请求记录 WARN）；否则 `Unauthenticated`。
2. **有凭证，先比静态列表**：凭证与 `auth_tokens` 逐把常数时间比较（`subtle.ConstantTimeCompare`，长度不匹配安全归零），任一命中即**超管身份**——KeyID `static-token`、principal `admin`、namespace 范围 `["*"]`、能力位 = 节点上限。静态比较刻意排在下文 Key 长度门**之前**（设计 D28）：较短的静态 token 不会被 Key 路径的 20 字符门误杀。
3. **未命中静态列表**：有 `api_auth` 指派代理 → 走 API Key 路径（见下节）；无指派 → `Unauthenticated`。

**静态 `auth_tokens` 列表**：

- 列表形态让轮换无空窗：加新 token → 滚动重启 → 删旧 token，全程不断连；
- 每把 ≥ 20 字符（Validate 强制，避免过小的暴力空间）；
- 语义冻结为超管（`["*"]` + 全部能力位 + 固定 principal `admin`），不支持限权——需要按调用方限权就发 API Key。它是桥接服务（mlbridge）的生产通道与运维 break-glass，不是兼容遗产。

**`allow_insecure`**：无凭证请求注入 insecure 超管身份（KeyID `insecure`），仅限受控环境。G5 fail-closed 启动门：非 loopback 的 `server.api.addr` 搭配 `allow_insecure: true` 是 **Validate 错误**（不再是启动 WARN）；admin HTTP 监听器同规则对齐——非 loopback 的 `server.http.addr` 必须配置 `server.http.auth_token`。

生产环境应将Server API 端口绑定到回环或私有网络接口并配置 `auth_tokens`（见 [../deployment.md](../deployment.md)）。

### API Key 鉴权（proxy 托管）

静态 token 之外，Server API 支持由后端代理托管的 **API Key** 凭证：Key 的生命周期（发放、撤销、审计）全部在代理后端（如 mlbridge → Torchwood），messageloop 自身不存储任何 Key。激活方式是 proxy 条目的显式布尔指派（全配置唯一）：

```yaml
proxy:
  - name: mlbridge
    api_auth: true   # 该代理成为Server API Key 的唯一校验器
```

- **唯一指派（G3）**：至多一个 proxy 条目可设 `api_auth: true`，两处指派即 Validate 错误；被指派条目必须带 `name`（按名解析代理实例）。指派与 `routes` 完全解耦——Server API 认证不走 channel/method glob 路由，不存在被 `method: "*"` 路由无声接管的可能；也不存在 `$authenticate_api_key` 之类的方法名。
- **凭证形态**：与静态 token 相同的两种头——`authorization: Bearer <key>`（优先）或 `x-api-key: <key>`。
- **校验流程**：静态列表未命中 → 无指派即拒 → 长度门（< 20 字符直接拒 + 负缓存，不触 proxy，防 Key 喷射）→ 调用被指派代理的 `AuthenticateAPIKey` RPC（入参 Key 明文 + 审计用来源地址）。
- **缓存**：校验结果按 Key 明文的 sha256 缓存（明文不进缓存、日志与错误信息），正负合计容量 1024 条、满则随机驱逐；同一 Key 的并发校验去重（合并为一次 proxy 调用）。
- **TTL 与撤销传播上界**：正缓存 TTL = `auth_cache_ttl`（默认 `30s`），并被 Key 自带的 `max_age_seconds` 收紧（生效 TTL = min(配置 TTL, max_age)，下限 1s）——**撤销传播上界 = TTL**，敏感 Key 由后端用 `max_age_seconds` 收紧到秒级。明确拒绝负缓存固定 5s（不配置化）。
- **proxy 不可用：fail-closed 且 fail-fast（D27）**：proxy 错误（网络/超时/未实现）一律拒绝（`Unauthenticated`，错误信息注明 verifier 不可用），绝不 fail-open；错误结果进 2s 短负缓存，**连续 5 次 proxy 错误熔断 30s**，熔断期内直接快速失败、不触 proxy——否则 proxy 宕机时每条 Key 调用都会挂满 rpcTimeout（默认 30s），fail-closed 退化为 DoS 放大。熔断只影响 Key 路径：静态 token 通道永不经过 proxy，完全不受影响。后端给出明确接受/拒绝（证明 proxy 健康）即重置错误连击并退出熔断。
- **能力/namespace 钳制（D17）**：proxy 返回的 `capabilities` 逐名映射能力位闭集（未知名 WARN 丢弃，版本偏斜容忍；全部未知 → 零能力）后与节点上限取交；`namespaces` 逐个过语法校验（非法项 WARN 丢弃），`"*"` 只允许单例（与精确项混用 → 整体坍缩为零范围，fail-closed），清洗后为空 → 零范围身份（全拒）。一句话：**proxy 决定"你是谁"，上限之内的授予是 proxy 的权限；上限本身（能力位闭集与 namespace 语法）永远是服务端配置的。**
- **key_id 是唯一 ID 不是显示名（D26）**：proxy 返回的 `key_id` 必须是后端 Key 的唯一 ID（显示名是自由文本、可能重名）。Key 的授权主体为 `key:<key_id>`：Authorizer allow 列表（`allow_publish` 等）可按 `"key:<id>"` 精确放行单把 Key；日志与指标（`messageloop_server_api_auth_requests_total{verifier,key_id,result}`）也只携带 key_id，保证可归因且不含 Key 明文。
- **标签约定（跨系统闭环）**：Key 上的能力标签采用 `messageloop.<能力位名>` 服务前缀语法（如 `messageloop.history.read`），由 Torchwood 存储校验、mlbridge 前缀过滤后映射为闭集能力名。该约定活在 Torchwood ↔ mlbridge 层，messageloop 契约只认闭集名；标签清单与三层知识边界见[设计文档](../design/2026-09-16-admin-api-key-authz.md) §2.6。
- **deny 不可打洞**：API Key 的 channel 引用照常受全局 Authorizer 表约束，与静态身份同一条规则表。

### 能力位（capabilities）

能力位检查集中在 scope 层的声明式能力表（internal/serverapi/scope.go，每个 RPC 进 handler 前求值；缺位返回 `PermissionDenied`，census 测试保证新增 RPC 必须登记）。能力位**按调用方身份生效**：静态 token / `allow_insecure` 身份持节点上限（`server.api.capabilities`）；API Key 身份持 proxy 授予 ∩ 节点上限（见上节钳制）。

| 能力位 | 控制的操作 |
| --- | --- |
| `session.act` | 会话定向操作：`Publish`（带 `sessions` 目标）、`Disconnect`（带 `sessions`）、`Subscribe` / `Unsubscribe`（带 `session_id`，代订阅） |
| `user.fanout` | 按 user 展开，叠加在 `session.act` 之上（`Publish.users` / `Disconnect.users` / `Subscribe.user_id` / `Unsubscribe.user_id`） |
| `history.read` | `GetHistory` |
| `presence.read` | `GetPresence` |
| `channels.list` | `GetChannels` |
| `subscribe.any` | 代订阅越过 Authorizer 的 `allow_subscribe` 名单（`deny_all` 仍不可打洞）。不是 Subscribe/Unsubscribe 的硬性门：无此位时按 principal 常规名单求值 |
| `survey.bypass_gate` | `Survey` 绕过客户端 Survey 的门限制（行为开关：无此位时与客户端 Survey 同门——Authorizer 拒绝或订阅者超 `max_survey_subscribers` 即 `PermissionDenied` / `ResourceExhausted`，见 [Survey](#survey)） |
| `presence.large_snapshot` | `GetPresence` 返回完整快照（无此位时按上限截断） |

频道-only 的 `Publish`（目标只有 `channels`）不需要任何能力位——发布权限的控制单元是 namespace 范围 + Authorizer 规则，不是能力位。`capabilities` 省略时默认为除 `pattern.global`（预留位）外的全部能力；显式 `[]` 表示零能力，锁死 Server API 数据面。完整语义见[《配置参考》](02-configuration.md) server 节。

### 命名空间

按 user 寻址必须携带命名空间：`Publication.Destination.namespace`（users 非空时必填）、`DisconnectRequest.namespace`（`users` 非空时必填）、`SubscribeRequest.namespace` / `UnsubscribeRequest.namespace`（`user_id` 非空时必填），缺失返回 `InvalidArgument`。会话按命名空间作用域隔离，user→sessions 展开只在该命名空间的索引内进行。

**身份的 namespace 范围**：每个已认证身份携带一个 namespace 范围——静态 token / `allow_insecure` / platform 级 Key 为 `["*"]`（全部）；API Key 为 proxy 授予的精确列表（空列表 = 拒绝一切，fail-closed）。范围限定了调用方能寻址的 channel、namespace 参数与 session：

- **channel 引用**：越界的 channel 对该身份不可见或直接拒绝（按 RPC 形态，见下表）；语法不合法的 channel（非 `ns:topic` 形态）对**全体身份**（含静态 token）一律 `InvalidArgument`——G4 全局面语法门，Server API 面与客户端面共享同一 channel 语法，死通道从静默浪费变成显式报错。
- **namespace 参数**：`users`/`user_id` 非空时携带的 `namespace` 参数越界 → 整个 RPC `PermissionDenied`（显式、可诊断）。
- **session ID**：跨范围 session 对该身份**不可见**（租约 namespace 无法解析或不落在范围内即视为不存在，不泄露存在性）。
- **`GetChannels` 结果**：按身份 namespace 过滤；channel 名解析不出命名空间的条目对受限身份同样隐藏（fail-closed）。

拒绝语义矩阵（scope 层在 RPC 进 handler 前执行，handler 收到的请求里根本没有越界目标——结构保证而非约定）：

| 载体 | 越界时的行为 |
| --- | --- |
| namespace 参数（`users`/`user_id` 非空时的 `namespace`） | 整个 RPC `PermissionDenied` |
| channel 引用（`Publish` 的 `destination.channels`） | 该条计 failed（部分成功语义继续） |
| channel 引用（`Subscribe`/`Unsubscribe` 的 `channels`） | `results[ch] = false` |
| channel 引用（`Survey`/`GetPresence`/`GetHistory` 单 channel） | 整个 RPC `PermissionDenied` |
| session ID 引用（`sessions`/`session_id`） | 不可见：`Publish` 跳过该投递（计入尝试，与会话不存在同型）、`Disconnect` 结果中不出现该键（失败形态）、`Subscribe`/`Unsubscribe` 抹除后按不存在处理 |
| 整个请求只剩越界目标（如 `Subscribe` 仅带一个跨范围 `session_id` 且无 `user_id`） | scope 层短路合成 not-found 响应（`Subscribe`/`Unsubscribe`：每个请求过的 channel 均 `results=false`；`Publish`：计尝试 + 失败），不进 handler——避免触发"session_id/user_id 不得同时为空"的 `InvalidArgument`，既保住 not-found 语义也不泄露"被特殊处理过" |

全局身份（`["*"]`）不受上述范围检查影响，但 G4 语法门对全体身份生效。

### TLS

当 `server.api.tls.cert_file` 与 `server.api.tls.key_file` 成对设置时，Server API 服务器以 TLS 方式服务；二者必须同时设置或同时留空（配置校验见《配置参考》[02-configuration.md](02-configuration.md)）。

### grpcurl 调用

服务器未注册 gRPC 反射服务（reflection），因此 `grpcurl` 无法通过 `list`/`describe` 自动发现服务，调用时需要显式指定 proto 文件路径：

```bash
grpcurl \
  -import-path ./protocol \
  -proto server/v2/api.proto \
  -H "authorization: Bearer <token>" \
  -plaintext \
  -d '{"channel": "dev:chat.general"}' \
  127.0.0.1:9091 \
  messageloop.server.v2.APIService/GetPresence
```

API Key 凭证也可用 `x-api-key` 头携带（`authorization: Bearer` 优先，二者任选其一）：

```bash
grpcurl \
  -import-path ./protocol \
  -proto server/v2/api.proto \
  -H "x-api-key: <api-key>" \
  -plaintext \
  -d '{"channel": "dev:chat.general"}' \
  127.0.0.1:9091 \
  messageloop.server.v2.APIService/GetPresence
```

- `-import-path ./protocol` 指向 `protocol/` 目录，使 `server/v2/api.proto` 中的 `shared/v2/errors.proto`、`shared/v2/types.proto` 等 import 可以解析；
- 启用 TLS 时去掉 `-plaintext`，并按需传入 `-cacert <ca.pem>`（使用自签证书时也可用 `-insecure`）；
- 请求体使用 proto3 JSON 映射，字段名为 lowerCamelCase（例如 `request_id` → `requestId`、`timeout_ms` → `timeoutMs`）。

## RPC 参考

以下各节按 `protocol/server/v2/api.proto` 中的声明顺序逐一说明。请求/响应消息的字段名一律采用 proto 中的原始名称。

### Publish

服务端向频道（channel）、指定会话（session）或指定用户（user）发布消息。

请求消息 `PublishRequest`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `request_id` | `string` | 请求标识，仅用于日志关联，不会回显 |
| `publications` | `repeated Publication` | 待发布的出版物列表，可包含多条 |

`Publication`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | `string` | 出版物标识；会话投递时作为消息的 `id` 透传给客户端 |
| `destination` | `Destination` | 投递目标：`sessions`（会话 ID 列表）、`channels`（频道列表）、`users`（用户 ID 列表）与 `namespace`（user 展开的作用域），可以同时指定前三者 |
| `options` | `Options` | 投递选项，目前仅声明 `add_history` |
| `payload` | `shared.v2.Payload` | 消息载荷，支持 `text`、`binary`、`json` 三种形式 |
| `metadata` | `shared.v2.Metadata` | 元数据键值对，随消息透传给客户端 |

`Publication.Options.add_history`：控制频道发布是否写入历史。
- `true`：写入 broker 历史，后续可通过 `GetHistory` 补拉。
- `false` 或未设置：以 transient 方式发布，不写历史。
- 会话目标与用户目标不受该选项影响，始终直接投递到会话。
- 频道的授权策略（`Authorizer` Effects）可以否决该选项：策略禁历史（`transient_only` 或 `history=false`）时 `add_history=true` 被拒绝——该条投递计为失败、不发布（避免误以为写入了历史）；`APICanPublish` 授权拒绝同样计为失败。

响应消息 `PublishResponse`：空消息，不返回任何字段。单条出版物的投递结果（例如 broker 分配的 offset）不会暴露给调用方。

语义：

- 载荷转换：`binary` 直接使用原始字节；`text` 按 UTF-8 字节发送；`json` 会被序列化为 JSON 字节后按文本发送。载荷为 nil 时发送空载荷。`metadata.entries` 随消息透传。
- 频道投递：默认以 transient 方式发布，不写历史；仅当 `options.add_history` 为 `true` 且频道策略允许时通过 broker 的 `Publish` 路径发布并写入历史，与客户端发布走同一管道（见[《架构指南》](01-architecture.md)）。
- 会话投递：向目标会话直接发送一条 `publication` 信封，消息的 `channel` 字段为空字符串（会话定向消息没有频道），`id` 为 `Publication.id`。目标会话不存在时跳过该投递，不报错、不计入失败（仅记录 debug 日志）。
- 用户投递：`destination.users` 里的每个用户先展开为该用户在 `destination.namespace` 下的全部 session（单节点来自本地 hub 的 user 索引；集群下并上 Redis user 索引），展开结果与 `destination.sessions` 取并集（去重）后走会话投递。`users` 非空时 `namespace` 必填，否则 `InvalidArgument`；`users` 含空字符串也是 `InvalidArgument`，且不做任何扫描（匿名连接不可按 user 寻址）。展开时始终校验 session lease 的 `UserID`：索引里的陈旧/投毒条目会被跳过；索引 miss 不做全集群扫描，靠周期 repair 收敛。
- 部分失败语义：由于 `PublishResponse` 没有按条目返回的字段，失败只能通过整体结果表达。每条失败投递（目标会话发送失败、目标频道发布失败或被策略/授权拒绝、载荷序列化失败、缺少 destination）都会记录错误日志；仅当所有投递尝试全部失败时，RPC 返回状态码 `Internal`（错误信息形如 `all N delivery attempt(s) failed`）；只要有一条成功，RPC 就返回空响应。
- destination 为 nil 或 `sessions`、`channels`、`users` 均为空时，该条出版物视为失败。
- 受限身份（如 API Key）的越界目标在进 handler 前已被 scope 层改写：越界 channel 该条计 failed、不可见 session 跳过（计入尝试），见[命名空间](#命名空间)的拒绝语义矩阵。

返回的错误码：`PermissionDenied`（能力位缺失——带 `sessions` 需 `session.act`、带 `users` 需 `user.fanout` + `session.act`；namespace 参数越界）、`InvalidArgument`（非法 channel 语法、users 含空 ID、按 user 寻址缺 `namespace`）、`Internal`（全部投递尝试失败）。

集群感知：见 [集群感知行为](#集群感知行为)。

### Disconnect

强制断开一个或多个客户端会话。

请求消息 `DisconnectRequest`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `sessions` | `repeated string` | 要断开的会话 ID 列表 |
| `code` | `uint32` | 断开码（disconnect code），原样传给客户端 |
| `reason` | `string` | 人类可读的断开原因，原样传给客户端 |
| `users` | `repeated string` | 要断开的用户 ID 列表；每个用户展开为其命名空间下的全部 session，与 `sessions` 取并集（去重）后逐个断开 |
| `namespace` | `string` | user 展开的作用域；`users` 非空时必填 |

响应消息 `DisconnectResponse`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `results` | `map<string, bool>` | 以会话 ID 为键、是否断开成功为值 |

语义：

- 对展开后的每个会话逐一执行断开。每个会话独立得到一个布尔结果：会话存在且断开成功为 `true`；会话不存在或断开过程出错为 `false`。RPC 本身不会因为个别会话失败而返回错误。
- 按 user 展开与 Publish 相同：始终校验 session lease 的 `UserID`，索引陈旧条目被跳过；`users` 中的空字符串或缺失 `namespace` 返回 `InvalidArgument` 且不扫描。
- 服务端会以指定的 `code` 与 `reason` 构造 `Disconnect` 并关闭客户端连接，客户端在协议层收到对应的断开通知（见[《客户端协议参考》](../protocol.md) 中的 Disconnect Codes 一节）。`code` 由调用方决定，服务端不做合法性校验；源码中内置的常量定义于 internal/protocol/disconnect.go，例如：

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
| `DisconnectInternal` | 3513 | `internal error` |
| `DisconnectUnsupportedVersion` | 3514 | `unsupported version` |

受限身份的不可见 session 在进 handler 前已被 scope 层抹除，响应 `results` 中不出现该键（失败形态，不泄露存在性），见[命名空间](#命名空间)。

返回的错误码：`PermissionDenied`（能力位缺失——带 `sessions` 需 `session.act`、带 `users` 需 `user.fanout` + `session.act`；namespace 参数越界）、`InvalidArgument`。

集群感知：见 [集群感知行为](#集群感知行为)。

### Subscribe

让指定会话订阅一个或多个频道。

请求消息 `SubscribeRequest`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `session_id` | `string` | 目标会话 ID |
| `channels` | `repeated string` | 要订阅的频道列表 |
| `user_id` | `string` | 目标用户 ID；展开为该用户命名空间下的全部 session，与 `session_id` 取并集 |
| `namespace` | `string` | user 展开的作用域；`user_id` 非空时必填。`session_id` 与 `user_id` 都为空时返回 `InvalidArgument` |

响应消息 `SubscribeResponse`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `results` | `map<string, bool>` | 以频道名为键、是否订阅成功为值 |

语义：

- 订阅以单个会话为目标（会话级Server API 操作，不是广播订阅），实际调用的是与客户端主动订阅等价的完整订阅路径（`AddSubscription`），包括 broker 订阅注册、在线状态（presence）登记与集群状态同步。
- 按 `user_id` 展开后逐会话执行：对每个频道，任一 session 订阅成功即记 `true` 并停止尝试其余 session（早停）；全部失败才为 `false`。会话不存在或订阅过程出错时对应频道为 `false`。RPC 本身不因个别频道失败而返回错误。
- 订阅已存在的频道是幂等的，重复订阅返回 `true` 且不产生副作用。
- `user_id` 的展开与 Publish/Disconnect 相同：校验 lease 的 `UserID`；`namespace` 缺失或 `session_id` 与 `user_id` 都为空时 `InvalidArgument` 且不扫描。
- 受限身份的越界 channel 在进 handler 前已被 scope 层移除并记 `results=false`；不可见 session 视同不存在（仅剩越界 session 且无 `user_id` 时 scope 层短路合成全部 `false` 的 not-found 响应），见[命名空间](#命名空间)。

返回的错误码：`PermissionDenied`（能力位缺失——`session_id` 需 `session.act`、`user_id` 需 `user.fanout` + `session.act`；namespace 参数越界）、`InvalidArgument`（非法 channel 语法、寻址字段缺失）。

集群感知：见 [集群感知行为](#集群感知行为)。

### Unsubscribe

让指定会话取消订阅一个或多个频道。

请求消息 `UnsubscribeRequest`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `session_id` | `string` | 目标会话 ID |
| `channels` | `repeated string` | 要取消订阅的频道列表 |
| `user_id` | `string` | 目标用户 ID；与 Subscribe 对称：展开为用户命名空间下的全部 session，与 `session_id` 取并集 |
| `namespace` | `string` | user 展开的作用域；`user_id` 非空时必填。都为空时 `InvalidArgument` |

响应消息 `UnsubscribeResponse`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `results` | `map<string, bool>` | 以频道名为键、是否取消订阅成功为值 |

语义：与 `Subscribe` 对称（多 session 时任一成功即早停）。对每个频道独立执行取消订阅，包括从 hub 移除订阅、broker 反注册、在线状态清除与集群状态同步。会话不存在、频道未被该会话订阅或操作出错时对应结果为 `false`。受限身份的越界 channel 记 `results=false`，不可见 session 视同不存在（scope 层处理，见[命名空间](#命名空间)）。

返回的错误码：`PermissionDenied`（能力位缺失——`session_id` 需 `session.act`、`user_id` 需 `user.fanout` + `session.act`；namespace 参数越界）、`InvalidArgument`（非法 channel 语法、寻址字段缺失）。

集群感知：见 [集群感知行为](#集群感知行为)。

### Survey

向频道内的所有订阅者发送调查请求（survey request），并在超时时间内收集应答。

请求消息 `SurveyRequest`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `request_id` | `string` | 请求标识，原样回显在响应中 |
| `channel` | `string` | 目标频道；所有订阅者都会收到调查 |
| `payload` | `shared.v2.Payload` | 调查载荷，支持 `text`、`binary`、`json`；发送给客户端时封装为二进制载荷 |
| `metadata` | `shared.v2.Metadata` | 已声明但当前处理器未使用，会被忽略 |
| `timeout_ms` | `int32` | 收集应答的等待时长（毫秒）；`<= 0` 时使用频道策略的 `max_survey_timeout`（默认 5 秒） |

响应消息 `SurveyResponse`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `request_id` | `string` | 回显请求标识 |
| `results` | `repeated SurveyResult` | 收集到的应答列表 |

`SurveyResult`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `session_id` | `string` | 应答来源会话 ID |
| `payload` | `shared.v2.Payload` | 应答载荷，统一以二进制形式返回 |
| `metadata` | `shared.v2.Metadata` | 附加元数据；集群模式下包含 `node_id` 与 `incarnation_id` 条目（见 [集群感知行为](#集群感知行为)） |
| `error` | `shared.v2.Error` | 该会话应答失败时的错误信息 |

语义：

- 调查只发送给目标频道的订阅者。发送前会记录被调查的会话集合，只有这些会话的应答才会被接受，来自其他会话的应答视为伪造并被丢弃。
- **载荷按原始字节读写**：发起时 `payload` 的任何变体（text/json/binary）到达客户端都是二进制载荷（客户端 SDK 以 `octet-stream` 呈现）；调用方读取 `SurveyResult.payload` 时应使用 binary 变体（如 Go 的 `GetBinary()`）——读 text/json 变体会得到空值。
- **门限制**：调用方不持 `survey.bypass_gate` 能力位时，与客户端发起的 Survey 走相同的门——Authorizer `Decide(Survey)` 拒绝（`SURVEY_DISABLED` / `PERMISSION_DENIED`）或订阅者总数超过频道策略 `max_survey_subscribers`（默认 256）时返回 `ResourceExhausted`，零条请求下发。
- 超时取值：`timeout_ms` 被钳制在 `[100ms, min(频道策略 max_survey_timeout, 10s)]`；`<= 0` 用策略默认（5s）。
- 每个订阅者的调查请求发送受独立超时约束（10 秒）：发送失败的会话会以一条 `error` 应答记录失败，不会阻塞整个调查。
- 应答按会话 ID 去重，同一会话的多次应答以最后一次为准。
- 单会话失败时，对应 `SurveyResult.error` 的 `code` 固定为 `SURVEY_FAILED`，`message` 为失败原因（例如发送超时、客户端传输错误）。
- 若调查无法执行（例如频道没有订阅者，或并发调查数量达到上限 1000），无订阅者时返回空结果；注册表已满时返回错误信息 `survey registry full (limit 1000)`。
- 结果为按 `(节点 ID, 实例 ID, 会话 ID)` 排序后的列表。

返回的错误码：`PermissionDenied`（channel 越界、无 `survey.bypass_gate` 时 Authorizer 拒绝）、`ResourceExhausted`（门限制）、`InvalidArgument`（非法 channel 语法）、`Unknown`（来自 Node 内部的错误原样透传）。

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
| `session_id` | `string` | 会话 ID，与映射键一致；旧版 Redis 记录缺少该字段时回退为记录中的 `client_id` 键 |
| `user_id` | `string` | 连接时声明的用户 ID，可为空 |
| `client_id` | `string` | Connect 的 `client_id`（设备/端标识），不是会话 ID；未声明时为空 |
| `connected_at` | `int64` | 该客户端在频道中登记的 Unix 毫秒时间戳 |

语义：

- 只返回订阅该频道时登记的在线客户端（订阅与在线状态登记见 [../protocol.md](../protocol.md)）。临时订阅（ephemeral）不登记在线状态。
- **快照截断**：调用方不持 `presence.large_snapshot` 能力位时，快照按频道策略 `presence_snapshot_limit`（默认 256）截断，`truncated` 语义经 `occupancy` 计数表达。
- 频道无在线数据时返回空映射，不报错。
- 受限身份（API Key）的越界 channel 在进 handler 前已被 scope 层拒绝（见[命名空间](#命名空间)）。

返回的错误码：`PermissionDenied`（`presence.read` 能力位缺失、channel 越界、Authorizer 规则拒绝）、`InvalidArgument`（非法 channel 语法）。

集群感知：见 [集群感知行为](#集群感知行为)。

### GetHistory

查询频道的消息历史（history）。

请求消息 `GetHistoryRequest`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `channel` | `string` | 目标频道 |
| `since` | `shared.v2.Position` | 起始位置（`stream_epoch` + 可选 `offset`），语义见下文；缺省（nil）表示从头读取 |
| `limit` | `int32` | 返回条数上限；`<= 0` 时使用默认上限 1000 |

响应消息 `GetHistoryResponse`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `publications` | `repeated HistoryPublication` | 命中的历史消息列表 |

`HistoryPublication`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `position` | `shared.v2.Position` | 该条消息在频道历史中的位置（`stream_epoch` + `offset`） |
| `payload` | `shared.v2.Payload` | 消息载荷；按原始 oneof 变体返回（`text` / `binary` / `json`） |
| `time` | `int64` | 消息时间（Unix 毫秒） |
| `id` | `string` | 发布方提供的消息标识，可为空 |
| `metadata` | `shared.v2.Metadata` | 发布方提供的元数据，无元数据时缺省 |

语义：

- 查询直接落到 broker 的历史存储。`since` 缺省（nil）表示从头读取（`limit` 以内）；`since.offset` 有值时从该偏移继续，两种实现下均为包含（inclusive）语义：返回 `offset >= since.offset` 的消息（`Broker.History` 契约；内存实现与 Redis 实现一致）。
- `since.stream_epoch` 非空时会与 broker 当前的 epoch 比对：不匹配说明调用方的游标属于上一代日志，RPC 返回 `FailedPrecondition`（`stream epoch mismatch: history belongs to a previous log generation`），且不读取 broker。
- 两种实现下，`limit <= 0` 都使用默认上限 `DefaultHistoryLimit`（1000 条）。
- 没有分页游标：`limit` 就是单次返回的硬上限，`since` 是唯一的前进指针。
- 历史被禁用（transient 消息）或频道无历史时返回空列表，不报错。
- 受限身份（API Key）的越界 channel 在进 handler 前已被 scope 层拒绝（见[命名空间](#命名空间)）。

返回的错误码：`PermissionDenied`（`history.read` 能力位缺失、channel 越界、Authorizer 规则拒绝——含 deny 与频道禁历史）、`InvalidArgument`（非法 channel 语法）、`FailedPrecondition`。

集群感知：见 [集群感知行为](#集群感知行为)。

### GetChannels

列出活跃频道及其订阅者数量。

请求消息 `GetChannelsRequest`：空消息，无分页参数，一次返回全部活跃频道。

响应消息 `GetChannelsResponse`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `channels` | `repeated ChannelInfo` | 活跃频道列表 |

`ChannelInfo`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `name` | `string` | 频道名 |
| `subscribers` | `int32` | 当前订阅者数量 |

受限身份（API Key）只看到自己 namespace 范围内的频道：范围外与解析不出命名空间的频道名都从结果中隐藏（fail-closed）；全局身份（`["*"]`）看到全部。

返回的错误码：`PermissionDenied`（`channels.list` 能力位缺失）。

集群感知：见 [集群感知行为](#集群感知行为)。

## 错误模型

Server API 的错误分两层：协议层错误消息与 gRPC 状态码。

### 错误消息（`messageloop.shared.v2.Error`）

`protocol/shared/v2/errors.proto` 中只声明了一个消息类型：

```protobuf
message Error {
  string code = 1;
  string type = 2;
  string message = 3;
  google.protobuf.Struct metadata = 4;
}
```

`code`、`type` 都是自由字符串（free-form string），`metadata` 为任意结构化数据；`errors.proto` 的注释将该表定位为全仓唯一的错误码表（客户端协议的错误信封取同一词汇表，见[《客户端协议参考》](../protocol.md) 的 Error Codes 一节）。

`Error` 消息在当前 Server API 中只出现于 `SurveyResult.error`，且 `code` 固定为 `SURVEY_FAILED`；`type` 与 `metadata` 未被填充。其余 Server API RPC 不通过 `Error` 消息报告失败，而是直接使用 gRPC 状态码。

### gRPC 状态码映射

Server API 处理器返回失败时使用的状态码如下：

| gRPC 状态码 | 触发条件 |
| --- | --- |
| `Unauthenticated` | 认证拦截器拒绝：缺少凭证且未配置 `allow_insecure`、静态 `auth_tokens` 不匹配、无 `api_auth` 指派、Key 长度不足 20 字符、被 proxy 明确拒绝、proxy 不可用（fail-closed + fail-fast，见[API Key 鉴权](#api-key-鉴权proxy-托管)） |
| `PermissionDenied` | scope 层能力位缺失（声明式能力表）、namespace 参数越界、单 channel 越界（Survey/GetPresence/GetHistory）、Authorizer 规则拒绝（Publish/Survey/GetPresence/GetHistory） |
| `InvalidArgument` | 按 user 字段中出现空字符串、按 user 寻址缺失 `namespace`、`Subscribe`/`Unsubscribe` 的 `session_id` 与 `user_id` 同时为空——均不做任何扫描；另：channel 引用语法不合法（G4 全局面语法门，对全体身份生效） |
| `ResourceExhausted` | 无 `survey.bypass_gate` 时频道订阅者数超过 `max_survey_subscribers` |
| `FailedPrecondition` | `GetHistory` 的 `since.stream_epoch` 与 broker 当前 epoch 不匹配（游标属于上一代日志） |
| `Internal` | `Publish` 请求中的所有投递尝试全部失败 |
| `Unknown` | 其余错误：来自 Node 内部方法的错误（例如 `Survey` 调查注册表已满、presence/history 存储错误）原样透传，gRPC 框架将其映射为 `Unknown` |

Server API 不定义自定义状态码（自定义 code 仅存在于 `shared.v2.Error` 的自由字符串 `code` 字段中）。调用方应同时处理 gRPC 状态码（区分错误类别）与 `SurveyResult.error`（区分单个会话的失败）。

## 示例

以下示例假设Server API 端口监听在 `127.0.0.1:9091`，且已配置 `auth_tokens`（示例中用 `<token>` 占位）。服务器未注册 gRPC 反射，所有命令都必须携带 `-import-path ./protocol -proto server/v2/api.proto`。JSON 载荷使用 proto3 JSON 映射的 lowerCamelCase 字段名；`binary` 载荷在 JSON 中以 base64 表示。频道名示例均带命名空间前缀（`dev:`）。

### Publish

发布到频道：

```bash
grpcurl \
  -import-path ./protocol \
  -proto server/v2/api.proto \
  -H "authorization: Bearer <token>" \
  -plaintext \
  -d '{
    "requestId": "api-publish-1",
    "publications": [{
      "id": "api-msg-1",
      "destination": {"channels": ["dev:chat.general"]},
      "payload": {"text": "hello from server api"}
    }]
  }' \
  127.0.0.1:9091 \
  messageloop.server.v2.APIService/Publish
```

发布到指定会话（JSON 载荷会按文本发送）：

```bash
grpcurl \
  -import-path ./protocol \
  -proto server/v2/api.proto \
  -plaintext \
  -H "authorization: Bearer <token>" \
  -d '{
    "publications": [{
      "id": "direct-msg-1",
      "destination": {"sessions": ["abc-123"]},
      "payload": {"json": {"type": "notice", "content": "server restarting"}}
    }]
  }' \
  127.0.0.1:9091 \
  messageloop.server.v2.APIService/Publish
```

按用户发布（只填 `users`，投递给该用户的全部 session；`namespace` 必填）：

```bash
grpcurl \
  -import-path ./protocol \
  -proto server/v2/api.proto \
  -H "authorization: Bearer <token>" \
  -plaintext \
  -d '{
    "publications": [{
      "id": "user-notice-1",
      "destination": {"users": ["alice"], "namespace": "dev"},
      "payload": {"text": "multi-device notice"}
    }]
  }' \
  127.0.0.1:9091 \
  messageloop.server.v2.APIService/Publish
```

### Survey

```bash
grpcurl \
  -import-path ./protocol \
  -proto server/v2/api.proto \
  -H "authorization: Bearer <token>" \
  -plaintext \
  -d '{
    "requestId": "api-survey-1",
    "channel": "dev:chat.general",
    "payload": {"text": "ping"},
    "timeoutMs": 3000
  }' \
  127.0.0.1:9091 \
  messageloop.server.v2.APIService/Survey
```

### Disconnect

使用内置断开码 3503（`DisconnectForceNoReconnect`）：

```bash
grpcurl \
  -import-path ./protocol \
  -proto server/v2/api.proto \
  -H "authorization: Bearer <token>" \
  -plaintext \
  -d '{
    "sessions": ["abc-123"],
    "code": 3503,
    "reason": "scheduled maintenance"
  }' \
  127.0.0.1:9091 \
  messageloop.server.v2.APIService/Disconnect
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
  -proto server/v2/api.proto \
  -H "authorization: Bearer <token>" \
  -plaintext \
  -d '{
    "sessionId": "abc-123",
    "channels": ["dev:chat.general", "dev:notifications"]
  }' \
  127.0.0.1:9091 \
  messageloop.server.v2.APIService/Subscribe
```

### Unsubscribe

```bash
grpcurl \
  -import-path ./protocol \
  -proto server/v2/api.proto \
  -H "authorization: Bearer <token>" \
  -plaintext \
  -d '{
    "sessionId": "abc-123",
    "channels": ["dev:chat.general"]
  }' \
  127.0.0.1:9091 \
  messageloop.server.v2.APIService/Unsubscribe
```

### GetPresence

```bash
grpcurl \
  -import-path ./protocol \
  -proto server/v2/api.proto \
  -H "authorization: Bearer <token>" \
  -plaintext \
  -d '{"channel": "dev:chat.general"}' \
  127.0.0.1:9091 \
  messageloop.server.v2.APIService/GetPresence
```

### GetHistory

```bash
grpcurl \
  -import-path ./protocol \
  -proto server/v2/api.proto \
  -H "authorization: Bearer <token>" \
  -plaintext \
  -d '{"channel": "dev:chat.general", "since": {"offset": 42}, "limit": 100}' \
  127.0.0.1:9091 \
  messageloop.server.v2.APIService/GetHistory
```

### GetChannels

请求为空消息，使用 `-d '{}'`：

```bash
grpcurl \
  -import-path ./protocol \
  -proto server/v2/api.proto \
  -H "authorization: Bearer <token>" \
  -plaintext \
  -d '{}' \
  127.0.0.1:9091 \
  messageloop.server.v2.APIService/GetChannels
```

## 集群感知行为

启用集群（`cluster.enabled: true`，要求 `broker.type: redis`）后，部分Server API 操作的行为发生变化。集群架构与配置详见[《分布式集群指南》](04-cluster.md)。

| 操作 | 集群模式下的行为 |
| --- | --- |
| `Publish`（会话投递） | 通过会话租约（session lease）解析会话所在节点；会话由远端节点持有时，投递请求经 Redis 命令总线（command bus）路由到该节点执行 |
| `Disconnect`、`Subscribe`、`Unsubscribe` | 与 `Publish` 会话投递相同：先解析会话租约，远端会话的操作经命令总线下发到持有该会话的节点执行；会话不存在时对应条目返回 `false` |
| 按 user 展开（`Publish.users`、`Disconnect.users`、`Subscribe/Unsubscribe.user_id`） | 本地 hub 的 user 索引并上 Redis user→sessions 索引（`ml2:cluster:user:sessions:<ns>:<user_id>` 集合 + `ml2:cluster:user:member:<ns>:<user_id>:<session_id>` 成员键，TTL 与 session lease 相同；键含命名空间段）；展开时对每个 session 校验 lease 的 `UserID`，不匹配或缺失则跳过；索引 miss 不做全集群扫描，靠周期 repair 收敛 |
| `Survey` | 除本地调查外，还会通过命令总线向集群内所有其他节点广播调查请求（排除自身），聚合各节点的应答后统一排序返回；每个 `SurveyResult` 附带 `node_id` 与 `incarnation_id` 元数据，标识应答来源节点；集群中某个节点执行调查失败时，该节点会以一条带 `error` 的 `SurveyResult` 表示（`code` 为 `SURVEY_FAILED`） |
| `GetChannels` | 不查询本地 hub，而是读取集群共享的频道投影（query store），返回全集群的活跃频道与订阅者数量 |
| `GetPresence` | 在线状态存储在集群模式下替换为 Redis 支撑的存储，查询返回全集群的在线客户端 |
| `GetHistory` | 从共享的 Redis Stream 读取历史，`since.offset` 为包含（inclusive）语义，`since.stream_epoch` 不匹配时返回 `FailedPrecondition`，数据跨节点一致 |

非集群模式下，会话定向操作只作用于本节点（未知会话返回 `false`），`Survey` 只调查本节点订阅者，`GetChannels` 与 `GetPresence` 只反映本节点状态。

## 实现说明

- **共享 Node，分离监听器**：Server API 处理器（internal/serverapi/api_handler.go）持有与客户端流服务器同一个进程内 `Node` 实例（装配见 cmd/server/runtime.go）。Server API RPC 与客户端流量在监听器层面完全分离：客户端流式 gRPC 监听 `transport.grpc.addr`，Server API 监听 `server.api.addr`；Server API 端口只注册 `APIService`，客户端端口只注册 `MessageLoopService`。
- **认证与授权链**：Server API 监听器安装 unary 认证拦截器（internal/serverapi/auth.go，三种凭证路径见[鉴权](#鉴权三种凭证路径)）；拦截器之后、handler 之前是 scope 层单一 choke point（internal/serverapi/scope.go）：声明式能力表 + 全局面 channel 语法门 + 拒绝语义矩阵改写，handler 对 scope 零感知。census 双测试（internal/serverapi/census_test.go）保证新增 RPC 与新请求字段必须登记，否则测试红。
- **身份可归因**：每条认证结果进 `messageloop_server_api_auth_requests_total{verifier,key_id,result}`（verifier ∈ static/insecure/proxy），每个 RPC 的调用归因进 `messageloop_server_api_rpc_total{method,key_id,result}`（`denied` = 认证层拒绝；`ok`/`error` = handler 结果）。两者只携带 key_id，永不含 Key 明文，完整指标见[《可观测性指南》](05-observability.md)。
- **监听器预绑定**：两个 gRPC 监听器都在启动预检阶段（`node.Run` 之前）完成 `net.Listen`，任一监听失败都不会留下已启动的 Node 副作用；两个监听器的组件名分别为 `grpc-client-server` 与 `grpc-api-server`。
- **RawCodec**：两个 gRPC 服务器都通过 `grpc.ForceServerCodec` 装配名为 `messageloop-proto` 的 `RawCodec`（pkg/transport/grpc/codec.go）。该 codec 对普通 proto 消息仍使用标准 `proto.Marshal`/`proto.Unmarshal`，因此Server API 的线上编码与标准 protobuf gRPC 完全兼容（这是 `grpcurl -proto` 方式可以正常调用的原因）；流式路径额外支持免二次编解码的原始帧（raw frame）优化。codec 按服务器注册而不是全局注册，避免覆盖进程内其他 gRPC 连接的默认 codec。
- **压缩**：gRPC 的 gzip 压缩编解码器已在服务器侧注册，客户端可在请求中声明 `grpc-accept-encoding: gzip`。
- **Server API 服务器未设置 `MaxRecvMsgSize`**：Server API 服务器使用 gRPC 默认的最大接收消息大小（4 MiB）；客户端流服务器则应用 `limits.max_message_size`（默认 64 KiB，见[《配置参考》](02-configuration.md)）。
- **调用方客户端**：Go SDK 的后端集成通过本Server API 与服务端通信（见[《Go SDK 指南》](07-sdk-go.md)）；TypeScript SDK 是纯 WebSocket 客户端，不包含Server API 客户端。Go SDK 生成的桩代码依赖 `server/v2/api.proto`，调用前请确保协议版本与服务器一致。
- **运维**：健康检查与指标走独立的 HTTP 管理面（`server.http.addr`），不属于本 API 范围（见[《可观测性指南》](05-observability.md)）。
