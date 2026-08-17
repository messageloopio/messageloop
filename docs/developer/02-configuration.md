# 配置参考

本文档是 MessageLoop 配置项的逐字段权威参考。所有字段名、类型、默认值与校验规则均对照源码核实（`config/config.go`、`defaults.go`、`cmd/server/main.go`、`cmd/server/runtime.go`、`pkg/redisbroker/`、`proxy/`、`pkg/websocket/`、`pkg/grpcstream/`）。协议与部署层面的说明见[《客户端协议参考》](../protocol.md) 与[《部署指南》](../deployment.md)。

## 概述

服务器通过单个 YAML 文件配置，路径由 `--config` 启动参数指定（默认 `./config.yaml`，见 `cmd/server/main.go:88`）。启动流程：

1. lynx 框架读取 YAML 并反序列化为 `config.Config`（结构定义见 `config/config.go`）；
2. 调用 `Config.Validate()`，校验失败则启动中止并返回 `invalid config: ...`（`cmd/server/main.go:34-36`）；
3. 各组件构造时应用默认值（见下文各节"默认值"）。

默认值并不集中在 `defaults.go`：该文件只定义少量全局常量（见 [defaults.go](#defaultsgo)），其余默认值分布在 `node.go`（服务端行为）、`pkg/redisbroker/options.go`（Redis broker）、`cmd/server/main.go`（HTTP 管理地址、broker 类型）等组件构造点。

### Config.Validate() 校验规则

`Validate()`（`config/config.go:157-242`）按以下顺序检查，返回第一条错误：

1. **传输地址必填**：`transport.websocket.addr`、`transport.websocket.path` 与 `transport.grpc.addr` 均必填。启动接线无条件构造 WebSocket 与客户端 gRPC 监听器（`newWebSocketServer` 与 `prepareGRPCServers`），缺失地址会错绑或 panic，故在此提前拒绝。`transport.quic.addr` 可选，空值表示不启动 QUIC：
   - `transport.websocket.addr` 为空 → `transport.websocket.addr is required`；
   - `transport.websocket.path` 为空 → `transport.websocket.path is required when websocket transport is enabled`（空 path 会在 `http.ServeMux` 注册时 panic，见 [transport.websocket 节](#transportwebsocket-节)）；
   - `transport.grpc.addr` 为空 → `transport.grpc.addr is required`；
   - `transport.quic.addr` 非空且未配置 TLS 成对证书、也未设 `insecure: true` → `transport.quic requires tls cert_file and key_file, or set insecure: true...`。
2. **时长格式**：以下字段若非空必须是合法的 Go duration 字符串（如 `"30s"`、`"1m30s"`）：
   - `server.heartbeat.idle_timeout`
   - `server.rpc_timeout`
   - `transport.websocket.read_timeout`
   - `transport.websocket.write_timeout`
   - `transport.grpc.write_timeout`
   - `transport.quic.write_timeout`
   - `transport.quic.read_timeout`
   
   注意：`proxy[].timeout` 与 `broker.redis.*` 各时长字段**不在** `Validate()` 检查范围内，它们在启动阶段解析（见各节）。
3. **TLS 证书/密钥成对**：以下四处的 `cert_file` 与 `key_file` 必须同时设置或同时为空：
   - `server.grpc_admin.tls`
   - `transport.websocket.tls`
   - `transport.grpc.tls`
   - `transport.quic.tls`
4. **管理 gRPC 鉴权**：`server.grpc_admin.addr` 非空时，`auth_token` 与 `allow_insecure` 必须至少设置一个，否则报 `server.grpc_admin requires auth_token, or set allow_insecure: true to explicitly run without authentication`（`config.go:207-209`）。
5. **broker 校验**：
   - `broker.type` 必须为 `memory` 或 `redis`（空等价于 `memory`），否则报 `unknown broker.type: ...`；
   - 为 `redis` 时 `broker.redis.addr` 必填，否则报 `broker.redis.addr is required when broker.type is redis`；
   - `broker.redis.consumer_group` 非空 → 直接拒绝：`broker.redis.consumer_group is not implemented; remove it from the configuration`（该字段声明但从未被消费，见 [broker.redis 字段](#brokerredis-字段)）；
   - `broker.redis.stream_approximate` 非 true（含显式 false）→ 直接拒绝：`broker.redis.stream_approximate: false is not supported (only approximate trimming is implemented); remove the field or set it to true`（`config.go:222-231`）。
6. **cluster 前置条件**：`cluster.enabled: true` 要求 `broker.type: redis`，否则报 `cluster requires broker.type=redis`。
7. **授权表**（`server.authorizer`，见 [server.authorizer 节](#serverauthorizer-节)）：规则 `pattern` 非空且是订阅 key 语言（`*` 单段、`**` 仅末尾、字面前缀非空，`a.**.b` / `*.room` / 裸 `**` 非法）；`history_size` 设置时 `>= 0`；`history_ttl` / `max_survey_timeout` 非空时必须是合法 Go duration；`grpc_admin.capabilities` 必须在闭集内。**`server.acl` / `server.channels` 键出现即失败**（已删除，KD-K31）。

`cluster.node_id` 是否必填不在 `Validate()` 中，而在 `ClusterOptions.normalize()`（`cluster.go:27-30`）检查：启用集群时 `node_id` 为空直接报错。

### defaults.go 常量

`defaults.go` 定义以下全局常量，供各组件使用：

| 常量 | 值 | 用途 |
| --- | --- | --- |
| `DefaultMaxMessageSize` | 64 KB（65536 字节） | `limits.max_message_size` 为 0 时生效 |
| `DefaultHeartbeatIdleTimeout` | 300s | `heartbeat.idle_timeout` 为空或解析失败时回退的默认值 |
| `DefaultHistoryLimit` | 1000 | `History` 未指定 limit 时的返回条数上限 |
| `MaxRecoveredPublications` | 1000 | 连接时历史恢复的最大投递条数 |
| `DefaultShutdownTimeout` | 10s | 优雅关闭时排空连接的上限 |

## 顶层结构

```yaml
server:      # 服务端行为：管理 HTTP、管理 gRPC、心跳、限流、ACL
transport:   # 客户端监听器：WebSocket、gRPC 与可选 QUIC
broker:      # 发布/订阅后端：memory 或 redis
cluster:     # 分布式控制面（可选）
proxy:       # 后端代理数组（可选）
```

| 小节 | 类型 | 说明 |
| --- | --- | --- |
| `server` | 对象 | 管理接口、心跳、RPC 超时、限流、ACL、认证要求，见 [server 节](#server-节) |
| `transport` | 对象 | 客户端接入监听器，见 [transport.websocket 节](#transportwebsocket-节)、[transport.grpc 节](#transportgrpc-节) 与 [transport.quic 节](#transportquic-节) |
| `broker` | 对象 | 消息路由后端，见 [broker 节](#broker-节) |
| `cluster` | 对象 | 多节点控制面，见 [cluster 节](#cluster-节) |
| `proxy` | 数组 | 后端代理与路由规则，见 [proxy 节](#proxy-节) |

## server 节

```yaml
server:
  http:
    addr: "127.0.0.1:8080"      # 管理 HTTP：/health 与 /metrics
  grpc_admin:
    addr: "127.0.0.1:9091"      # 管理 gRPC API
    auth_token: ""              # Bearer token；addr 非空时必须设置 auth_token 或 allow_insecure
    allow_insecure: false       # 仅限开发：跳过强制鉴权
    # tls:
    #   cert_file: "./certs/admin.crt"
    #   key_file: "./certs/admin.key"
  heartbeat:
    idle_timeout: "300s"        # 空或解析失败 = 回退 300s；"0s" = 禁用心跳
    ping_interval: "0s"         # 服务端主动 ping 间隔；0/空 = 不主动 ping（默认）
    ping_timeout: "3s"          # 服务端 ping 未应答判定；仅 ping_interval>0 时生效；空 = ping_interval
  rpc_timeout: "30s"
  limits:
    max_connections_per_user: 0
    max_subscriptions_per_client: 0
    max_publishes_per_second: 0
    max_message_size: 65536
  authorizer:               # 唯一授权表（PR-KA-A4），见下节；可省略
    default:
      history: true
      history_size: 0       # 0 = broker 全局（memory 256 / redis stream_max_length）
      history_ttl: ""       # 空 = broker 全局；memory broker 忽略并 Warn
      presence: true
      recover: true
      survey: false         # 客户端 survey 默认关（KD-6）
      recover_limit: 0      # 0 = MaxRecoveredPublications
      max_survey_subscribers: 256
      max_survey_timeout: "5s"
      legacy_presence_channel: false
      presence_snapshot_limit: 256
    rules:
      - pattern: "chat.public.*"
        allow_subscribe: ["*"]
        allow_publish: ["alice", "bob"]
      - pattern: "chat.private.*"
        deny_all: true
  require_auth: false
```

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `server.http.addr` | string | `127.0.0.1:8080` | 管理 HTTP 监听地址，暴露 `/health` 与 `/metrics`（`cmd/server/main.go:217-225`）。为空时回退到 `127.0.0.1:8080`。集群模式下 `/health` 会附带探测 Redis 连通性（`cmd/server/main.go:57-59`）。指标说明见[《可观测性指南》](05-observability.md) |
| `server.grpc_admin.addr` | string | 未设置 | 管理 gRPC API 监听地址（`serverpb.APIService`）。启动时必须有效：`prepareGRPCServers` 会无条件预绑定该监听器（见 [启动要求](#启动要求)）。接口清单见[《管理 API 参考》](03-admin-api.md) |
| `server.grpc_admin.tls.cert_file` / `.key_file` | string | 未设置 | 管理 gRPC 的 TLS 证书与私钥，二者必须成对设置（`Config.Validate()` 规则 3）。设置后经 `credentials.NewServerTLSFromFile` 加载（`pkg/grpcstream/server.go:56-62`） |
| `server.grpc_admin.auth_token` | string | 未设置 | 管理 API 的 Bearer token，通过 `authorization: Bearer <token>` 头传递，采用常量时间比较（`pkg/grpcstream/server.go:81-103`）。**`server.grpc_admin.addr` 非空时，`auth_token` 与 `allow_insecure` 必须至少设置一个**（`config.go:184-189`，否则启动校验失败）；`auth_token` 为空时管理 API 不鉴权，该形态仅在 `allow_insecure: true` 下合法，生产环境不建议 |
| `server.grpc_admin.allow_insecure` | bool | `false` | 显式放弃强制鉴权：`addr` 非空且 `auth_token` 为空时，置为 true 才能通过启动校验（`config.go:184-189`）；此时管理 API 完全不鉴权并在启动时记录 WARN 日志（`config.go:64-67`）。仅限受控环境（开发/内网） |
| `server.heartbeat.idle_timeout` | string | 未设置 | 客户端空闲超时。**为空或解析失败回退 `DefaultHeartbeatIdleTimeout`（300s）**。**非 0 值必须 ≥1s**。`idle_timeout` 内无任何活动即 3511。**仅当 `idle_timeout` 与 `ping_interval` 都为 0 时** `HeartbeatManager.Start` 才不启动（只关 idle、开 ping 仍会跑探测循环）。WebSocket 读超时与其联动，见 [transport.websocket 节](#transportwebsocket-节) |
| `server.heartbeat.ping_interval` | string | `0s`（不主动 ping） | 服务端主动探测半开连接：每 `ping_interval` 发一次 Outbound `Ping`（首次在一个 interval 之后，带 0.8~1.2 抖动防齐射），随后 `ping_timeout` 内未收到**任何**入站帧（Pong/Ping/业务均可）即断开 3511（策略 B，不等 idle；`heartbeat.go:74-96`）。**非 0 值必须 ≥1s**。**打开后旧客户端会被踢**：旧 SDK 不认识 Outbound Ping 也不回 Pong，需要同时升级 SDK（PR-08）才能开启。集群 session lease 随此值缩短，见 [集群文档](04-cluster.md) |
| `server.heartbeat.ping_timeout` | string | 等于 `ping_interval` | 服务端 ping 的应答窗口：发出 Ping 后到「断开 3511」的最长等待。仅 `ping_interval>0` 时有意义；**留空按 `ping_interval` 取值**（`node.go:105-110`），**显式 `"0s"` 被 `Validate` 拒绝**，非 0 值必须 ≥1s |
| `server.rpc_timeout` | string | `30s` | RPC 转发请求的超时（`proxy.DefaultRPCTimeout`，`proxy/proxy.go:155`）。节点默认取 30s；配置非空时解析，解析失败同样回退 30s（`node.go:74-80`）。每个 RPC 请求以此值创建 context 截止时间，超时向客户端返回 `RPC_TIMEOUT` 错误（`client.go:742-745, 773-789`）。与代理级超时的关系见 [三层超时](#三层超时) |
| `server.limits.max_connections_per_user` | int | 0（不限） | 同一用户 ID 的最大并发连接数（`node.go:66`，按用户分片限制，`hub.go:386-397`）。0 = 不限 |
| `server.limits.max_subscriptions_per_client` | int | 0（不限） | 单个客户端可订阅的频道数上限（`client.go:553-560, 954-960`）。0 = 不限 |
| `server.limits.max_publishes_per_second` | int | 0（不限） | 单客户端发布速率上限（令牌桶，`client.go:516-518, 855`），超限返回 `RATE_LIMITED`。0 = 不限 |
| `server.limits.max_message_size` | int | 0 = 默认 64 KB | 入站消息大小上限（字节）。0 时取 `DefaultMaxMessageSize`（64 KB，`defaults.go:8`，`node.go:564-569`）；非 0 即用配置值。该限制同时作用于 WebSocket（`conn.SetReadLimit`，`pkg/websocket/handler.go:60-62`）与 gRPC（`grpc.MaxRecvMsgSize`，`cmd/server/runtime.go:46`），两个传输保持一致。注意 0 的语义是"默认值"而非"不限" |
| `server.grpc_admin.capabilities` | string[] | 未设置 | Admin Capability 闭集（PR-KA-A4 §7）。**省略（nil）= `DefaultAdminCapabilities`**：除 `pattern.global` 外全部位（history.read / presence.read / channels.list / session.act / user.fanout / subscribe.any / presence.large_snapshot / survey.bypass_gate）。**显式 `[]` = 零位**，锁死 Admin 数据面（GetHistory / GetPresence / GetChannels / 代订 / 按 user 扇出全部软失败）。未知名 → `Validate` 错误 |
| `server.authorizer.default` | 对象 | 见各字段默认 | 未命中任何规则的频道的兜底 Effects（history/presence/recover/survey/transient 等）。各字段均可用指针覆盖默认；`history_ttl` / `max_survey_timeout` 用字符串以区分「未设置」与 `"0s"` |
| `server.authorizer.rules` | 数组 | 空 | 授权表：pattern → allow 名单 / deny_all / Effects，按配置顺序求值（见下） |
| `server.authorizer.rules[].pattern` | string | 必填 | 频道模式，与订阅 key 同一语言（authorizer.go `compilePattern`）：`*` 匹配恰好一个非空段、`**` 只允许在末尾、字面前缀不可为空。**`a.**.b`、`*.room`、`im.*.tick`、裸 `*` / `**` 均被 `Validate()` 拒绝**（§5.1） |
| `server.authorizer.rules[].deny_all` | bool | `false` | 为 true 时对该 pattern 的订阅 / 发布 / Survey / 恢复 / 在场全部拒绝。**deny 不可被更具体的 allow 打洞**：要开洞就缩小 deny 的 pattern |
| `server.authorizer.rules[].allow_subscribe` | string[] | 未设置 | 允许订阅的用户 ID 列表；`"*"` 表示任何已认证用户。**未设置 = 不约束订阅；显式 `[]` = 拒绝订阅**；非空名单不含该用户 = 拒绝。订阅判定是**语言包含**：`L(订阅 pattern) ∩ L(deny 规则) ≠ ∅` 即整条拒绝（§5.2，表驱动求交，不枚举频道） |
| `server.authorizer.rules[].allow_publish` | string[] | 未设置 | 允许发布的用户 ID 列表；语义同 `allow_subscribe`，判定对象是精确频道 |
| `server.authorizer.rules[].allow_survey` | string[] | 未设置 | 允许发起客户端 Survey 的用户 ID 列表；**未设置 = 不打开 survey**（Survey 默认拒绝，与 subscribe/publish 的默认放行相反），即使 Effects.survey=true。Admin 无 `survey.bypass_gate` 时同样受此名单约束 |
| `server.require_auth` | bool | `false` | 拒绝空 token 的连接（`config.go:32` 注释：Reject connections with empty token）。开启后：连接未携带 token 直接拒绝（`AUTH_REQUIRED`，`client.go:405-416`）；携带 token 但**没有**匹配 `$authenticate` 路由的代理时同样拒绝——非空 token 不得绕过认证（`client.go:389-404`）。实际认证总是由代理后端完成，见 [proxy 节](#proxy-节) |
| `server.presence.cluster_emit` | — | **已删除** | **PR-KA-B2 移除**：Occupancy 跨节点统一走 LiveBus 精确频道 + `CompileInterest`（见 [presence 跨节点](./04-cluster.md)）。写进 YAML（无论 true/false）都会让 `Validate()` 以 `cluster_emit is removed` 失败；`server.presence` 块现在为空（仅保留以兼容解析） |

### Authorizer 求值语义（PR-KA-A4 §5）

旧的 `server.acl`（last-write-wins、中段 `**`）与 `server.channels`（first-match 平行表）**已删除**：YAML 仍写这两个键会让 `Validate()` 失败（KD-K31，无兼容期）。所有授权与频道策略来自 `server.authorizer` 一张表，由根包 `Authorizer.Decide` / `Authorizer.Effects` 求值：

- **订阅（SubscribePattern）**：默认放行。先做路由检查（`CompileInterest`，与 A3 同套规则）——不可路由的 pattern（`*.room`、裸 `**`）返回 `PATTERN_NOT_ROUTABLE`，先于 ACL；然后对该 principal 逐条 deny 规则（deny_all、空 allow 名单、不含该用户的名单）做**语言求交**，`L(p) ∩ L(d) ≠ ∅` → 拒绝（客户端信封 `ACL_DENIED`）。deny 不可被更具体的 allow 打洞（§9.2）。
- **发布（Publish）**：精确频道，默认放行；`deny_all` 命中或 allow 名单未命中该用户 → 拒绝。**不要求 Coverage**（KD-K21）。
- **Survey**：默认拒绝；`Effects.Survey==true` **且** 存在 `allow_survey` 命中该精确频道 **且** 无 deny 命中才放行。
- **恢复（Recover）/ 在场（Presence）**：精确频道；默认跟随 `Effects(ch)`；`deny_all` 命中或通配频道 → 拒绝。
- **Admin**：用户 ID 按 `"admin"` 匹配名单；另受 [Capability 闭集](#server-节) 约束（`GetHistory`/`GetPresence`/`GetChannels`/代订/按 user 扇出必须持位）。

**Effects（`Authorizer.Effects(ch)`）** = `DefaultChannelPolicy()` overlay `server.authorizer.default`，再按表顺序 overlay **每一条**匹配规则（后写覆盖先写）——**不是 first-match**：通用规则写前面、特殊规则写后面即可。`TransientOnly` 强制 `History=false` 且 `Recover=false`。示例：

```yaml
server:
  authorizer:
    rules:
      - pattern: "game.**"         # 通用规则在前
        history: true
        survey: true
      - pattern: "game.tick.**"    # 特殊规则在后，覆盖先前的字段
        transient_only: true
        recover: false
```

`game.tick.fps` 命中两条规则：`transient_only` 生效 → 强制瞬时、不可恢复（`History=false` 强制），但先前的 `survey: true` overlay 保留。`game.room.1` 只命中 `game.**` → history + survey 开。

**规则内联 Effects 字段**（`server.authorizer.default` 与每条 `rules[]` 均可写）：

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `history` | bool | `true` | 是否写历史。`false` 与 `transient_only: true` 效果相同：发布改走瞬时（见下） |
| `history_size` | int | 0 = broker 全局 | 该前缀频道的历史容量：memory broker 每频道 ring 容量 / Redis 每条 `XADD` 的 `MAXLEN`。**只在该频道 ring 首次创建时生效**：已存在的内存 ring 不会因改大/改小立即重建，直到频道被回收；**改小 `history_size` 对已有频道不立即生效** |
| `history_ttl` | string | 空 = broker 全局 | 历史保留时长（Redis：每次发布后 `EXPIRE` 刷新）。**memory broker 无 TTL，配置了打 Warn 并忽略** |
| `presence` | bool | `true` | presence 开关。由 `shouldTrackPresence` 读取：`false` 的频道不存 presence、不发 join/leave、无快照，`PresenceQuery` 返回 `POLICY_DENIED` |
| `recover` | bool | `true` | 恢复开关。由 `recoverSubscription` 读取：`false` 时恢复被跳过（客户端要了 recover 则返回 `RECOVER_SKIPPED`） |
| `survey` | bool | `false` | 客户端 survey 开关（KD-6 默认关）。`false` 时客户端 `SurveyRequest` 返回 `SURVEY_DISABLED` 且零下发；`true` 时还须 `allow_survey` 规则与频道覆盖（`sessionCoversChannel`）全部通过才能发起 |
| `transient_only` | bool | `false` | 强制瞬时：发布只实时投递、绝不写历史。**隐含 History=false、Recover=false**（即使漏写）。对客户端：不带 `transient` 标志的发布也改走 `PublishTransient`、ack offset=0、不报错；对 Admin：`add_history=true` 被**拒绝**（计失败、不发布），`add_history=false` 仍可瞬时发布 |
| `recover_limit` | int | 0 = `MaxRecoveredPublications` | 恢复条数上限。由 `recoverSubscription` 读取：命中该上限（或请求级配额耗尽）时恢复结果标记 `truncated=true` |
| `max_survey_subscribers` | int | 256 | survey 订阅者上限。客户端 Survey 发起方本节点订阅者数（快路径，含通配命中）或集群预检总数超过该值 → `SURVEY_TOO_MANY_SUBSCRIBERS`、**零条** outbound `SurveyRequest`。`0` = 不限制。Admin 无 `survey.bypass_gate` 时同样受此门限制 |
| `max_survey_timeout` | string | `5s` | 客户端 Survey 超时上限：请求 `timeout_ms` 被钳制在 `[100ms, min(本值||5s, 10s)]`；`timeout_ms<=0` 用本值（默认 5s） |
| `legacy_presence_channel` | bool | `false` | 为 `true` 时 join/leave 额外以旧 JSON 瞬时发布到精确频道的 `ch/__presence` 伴生频道（通配订阅从不写伴生）。默认不写伴生 |
| `presence_snapshot_limit` | int | 256 | `Connected.presence` / `SubscribeAck.presence` / `PresenceQuery` 快照的 clients 条数上限；`occupancy` 仍是全量计数，超出置 `truncated=true`。`0` = 全局默认 `MaxPresenceSnapshotClients`（256）。Admin `GetPresence` 无 `presence.large_snapshot` 时同样截断到该上限 |

以上 `default` 与每条 `rules[].pattern` 均由 `Validate()` 校验：pattern 非空且合法（订阅 key 语言，见 §5.1）、`history_size >= 0`、两个 duration 可解析（`config/config.go` 的 `validateChannelPolicySpec`）。

**transient_only 对客户端与 Admin 的差异**：

- **客户端 Publish**（`client.go` `handlePublish`）：策略强制瞬时时，即使客户端没带 `transient` 标志，也改走 `PublishTransient`，返回 ack `offset=0`，**不报错**；同时 `messageloop_channel_policy_transient_forced_total` 指标 +1（客户端显式 `transient: true` 不计数）。消息不写历史，实时订阅者仍能收到。
- **Admin Publish**（`pkg/grpcstream/api_handler.go`）：`add_history=true` 但策略禁历史 → Warn「admin add_history denied by channel policy」、**计失败、不发布**（避免误以为写入了）；`add_history=false`（或缺省）仍走 `PublishTransient`。若同请求其他发布成功，RPC 仍成功（部分成功语义），只有全部失败才返回错误。

**容量与部署建议**：

- `history_size` 只影响 `Publish` 路径（`Node.Publish` 把策略注入 `Publication.HistorySize`，broker 首次建 ring / 每条 `XADD` 时读取）。
- **memory broker 无 TTL**：配置 `history_ttl` 会在该频道首次带 TTL 发布时打一次 Warn 并忽略。
- **内存 ring 不因新配置重建**：已存在的频道保持旧容量直到回收，改小 `history_size` 对已有频道不立即生效。
- **IM 大容量历史请使用 Redis broker**：memory 按 `history_size × 平均负载 × 频道数` 占内存（5000 × 512B × 1000 频道 ≈ 2.5 GB），单节点 memory 不适合大 IM；Redis 侧 `im.**` 5000 × 1KB ≈ 5 MB/频道，并可用 `history_ttl` 控制留存。`game.tick.**` 强制瞬时则对 Redis 零 Stream 写入，只剩 Pub/Sub。

## transport.websocket 节

```yaml
transport:
  websocket:
    addr: ":9080"
    path: "/ws"
    read_timeout: "60s"       # 可选
    write_timeout: "10s"      # 可选
    allow_all_origins: false  # 仅开发环境
    allowed_origins:
      - "https://example.com"
    compression: true         # permessage-deflate
    # tls:
    #   cert_file: "./certs/server.crt"
    #   key_file: "./certs/server.key"
    check_origin: false       # 已废弃
```

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `transport.websocket.addr` | string | 未设置 | WebSocket 监听地址（如 `:9080` 或 `127.0.0.1:9080`）。**必填**（`Validate()` 规则 1）：`transport.websocket.addr` 与 `transport.grpc.addr` 均必须配置 |
| `transport.websocket.path` | string | 未设置 | 升级路径（如 `/ws`）。**注意：二进制未应用默认值**——`pkg/websocket` 包内虽有默认路径 `/ws`（`pkg/websocket/server.go:32-37`），但 `cmd/server/main.go:190-191` 直接透传配置值，空路径不会替换为 `/ws`，且空 pattern 会在 `http.ServeMux` 注册时触发 panic（`pkg/websocket/server.go:45`），因此建议始终显式配置 |
| `transport.websocket.read_timeout` | string | 见说明 | 单次读操作的截止时间。规则（`pkg/websocket/handler.go:81-107`）：**探测开启时**（`idle>0` 或 `ping_interval>0`）取 `max(2 × idle_timeout, 3 × ping_interval, 10s)` 作为下限，显式配置值可以放大但**不能小于该下限**；**心跳完全禁用时**（`idle=0s` 且 `ping_interval=0s`）取 60s，显式配置完全覆盖。每次成功读消息后重置截止时间 |
| `transport.websocket.write_timeout` | string | 未设置 | 单次写操作的截止时间（`pkg/websocket/transport.go:43-52`）。为空则不设置写截止时间 |
| `transport.websocket.allow_all_origins` | bool | `false` | 允许任意 Origin 的跨域连接（仅限开发环境，`cmd/server/main.go:201-203`）。为 true 时 `CheckOrigin` 恒返回 true |
| `transport.websocket.allowed_origins` | string[] | 未设置 | Origin 白名单，对 `Origin` 请求头做**精确匹配**（`cmd/server/main.go:204-212`）。仅在 `allow_all_origins` 与 `check_origin` 均为 false 时生效 |
| `transport.websocket.compression` | bool | `false` | 启用 WebSocket 扩展 permessage-deflate（`pkg/websocket/handler.go:32` 的 `EnableCompression`） |
| `transport.websocket.tls.cert_file` / `.key_file` | string | 未设置 | 为 WebSocket 监听器启用 HTTPS/WSS，二者必须成对设置（`Validate()` 规则 3）；设置后以 `ListenAndServeTLS` 启动（`pkg/websocket/server.go:73-76`） |
| `transport.websocket.check_origin` | bool | `false` | **已废弃**（`config.go:90-91` 标注 Deprecated: Use AllowAllOrigins instead）。为 true 时行为与 `allow_all_origins: true` 完全一致（`cmd/server/main.go:201`），仅作向后兼容保留 |

### Origin 校验行为（源码核实）

`cmd/server/main.go:201-212` 的判定顺序：

1. `allow_all_origins` 或 `check_origin` 任一为 true → `CheckOrigin` 恒返回 true，放行一切来源；
2. 否则若 `allowed_origins` 非空 → 仅当 `Origin` 头与白名单精确匹配时放行（未携带 `Origin` 头的请求也会被拒——白名单模式不做"无 Origin 放行"特判）；
3. 否则 `CheckOrigin` 保持 nil → 交由 gorilla/websocket 的默认同源检查处理（无 `Origin` 头或与 `Host` 同源的请求放行，其余拒绝）。

## transport.grpc 节

```yaml
transport:
  grpc:
    addr: ":9090"
    write_timeout: "10s"
    # tls:
    #   cert_file: "./certs/server.crt"
    #   key_file: "./certs/server.key"
```

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `transport.grpc.addr` | string | 未设置 | 客户端面向的 gRPC 流式监听地址（`clientpb.MessageLoopService`，双向流）。**必填**（`Validate()` 规则 1）：与 `transport.websocket.addr` 均必须配置 |
| `transport.grpc.write_timeout` | string | 10s | 流式下行写超时。显式设置后通过 `WithWriteTimeout` 注入 handler（`cmd/server/runtime.go:48-52`、`pkg/grpcstream/client_server.go:13-15`）；未设置时由 `pkg/grpcstream/transport.go:19,66` 的 `defaultWriteTimeout`（10s）兜底 |
| `transport.grpc.tls.cert_file` / `.key_file` | string | 未设置 | 为 gRPC 监听器启用 TLS，二者必须成对设置（`Validate()` 规则 3） |

该监听器的 `MaxRecvMsgSize` 由 `server.limits.max_message_size` 统一决定（`cmd/server/runtime.go:46`），无需单独配置。

## transport.quic 节

```yaml
transport:
  quic:
    addr: ":4433"           # 空 = 不启动 QUIC 监听器
    write_timeout: "10s"
    read_timeout: "60s"
    insecure: false         # 仅开发：生成临时自签名证书
    # tls:
    #   cert_file: "./certs/server.crt"
    #   key_file: "./certs/server.key"
```

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `transport.quic.addr` | string | 空 | UDP 监听地址。**可选**：空值不启动 QUIC。启动接线见 `cmd/server/main.go` 的 `newQUICServer` |
| `transport.quic.write_timeout` | string | `10s` | 单次写帧截止时间。空则用 `pkg/quicstream.DefaultWriteTimeout`（10s） |
| `transport.quic.read_timeout` | string | 见说明 | 单次读帧截止时间。规则与 WebSocket 相同：心跳开启时取 `max(2×idle, 3×ping, 10s)` 为下限，显式配置只能放大 |
| `transport.quic.insecure` | bool | `false` | 未配置 TLS 文件时生成进程内自签名证书（仅开发/测试）。生产必须配置 `tls` |
| `transport.quic.tls.cert_file` / `.key_file` | string | 未设置 | QUIC 强制 TLS 1.3；二者必须成对。`addr` 非空时必须提供证书对或 `insecure: true` |

QUIC 会话是一条双向流上的长度前缀帧（4 字节大端长度 + payload）。TLS ALPN 协商编码：`messageloop+proto` 为二进制 protobuf，`messageloop+json` / `messageloop` 为 protojson。入站帧大小受 `server.limits.max_message_size` 约束。Go SDK 用 `DialQUIC(addr, opts...)` 连接。

### 启动要求

`cmd/server/runtime.go:67-80` 会在启动时**无条件预绑定两个 gRPC 监听器**（`transport.grpc.addr` 与 `server.grpc_admin.addr`），任一失败（地址为空、端口被占用等）都会中止启动——这与 `Validate()` 规则 1 的必填校验一致：三个客户端面地址（`transport.websocket.addr` / `transport.websocket.path` / `transport.grpc.addr`）均必须给出可绑定的值。预绑定失败时先启动的监听器会被释放，不会泄漏端口。

## broker 节

```yaml
broker:
  type: redis          # "memory" 或 "redis"
  redis:
    addr: "127.0.0.1:6379"
    password: ""
    db: 0
    # 其余字段均有默认值，见下表
```

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `broker.type` | string | `memory` | broker 实现。空等价于 `memory`（`cmd/server/main.go:147-150`）；`redis` 要求 `broker.redis.addr` 非空，其余值报 `unknown broker.type`（`Validate()` 规则 4） |

### memory broker

- 进程内实现，无任何 YAML 配置项（`MemoryBrokerOptions` 仅在代码中可用，`broker_memory.go:14-20`）。
- 每个频道维护固定容量环形缓冲作为历史记录，容量 256 条（`defaultMemoryHistorySize`，`broker_memory.go:12`）；满则覆盖最旧条目（`broker_memory.go:132-141`）。
- 适合单节点开发与测试；需要历史持久化或多节点时使用 Redis broker。

### broker.redis 字段

默认值全部来自 `pkg/redisbroker/options.go:9-27, 55-117`。

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `broker.redis.addr` | string | 未设置 | Redis 地址（`host:port`）。`broker.type: redis` 时必填（`Validate()` 规则 4）。连接失败（Ping 不通）时节点启动失败（`pkg/redisbroker/redis.go:42-53`，Ping 超时 5s） |
| `broker.redis.password` | string | 未设置 | Redis 认证密码（`AUTH`） |
| `broker.redis.db` | int | 0 | 逻辑数据库编号（SELECT）。多套环境建议用不同 `db` 隔离 |
| `broker.redis.pool_size` | int | 10 | go-redis 连接池大小 |
| `broker.redis.min_idle_conns` | int | 5 | 连接池保持的最小空闲连接数 |
| `broker.redis.max_retries` | int | 3 | 网络错误时的最大重试次数 |
| `broker.redis.dial_timeout` | string | `5s` | 建立连接超时 |
| `broker.redis.read_timeout` | string | `3s` | 读操作超时 |
| `broker.redis.write_timeout` | string | `3s` | 写操作超时 |
| `broker.redis.stream_max_length` | int64 | 10000 | 每条频道 Stream 的最大条目数（XADD 的 `MAXLEN`，`pkg/redisbroker/redis.go:85-90`） |
| `broker.redis.stream_approximate` | bool | `true` | 是否使用 Stream `MAXLEN ~` 近似截断（`Approx` 标志）。**注意：仅实现近似截断，显式写 `stream_approximate: false`（或省略后反序列化为 false）会被 `Validate()` 拒绝**（`Validate()` 规则 5，`config.go:229-231`），必须删除该字段或设为 `true` |
| `broker.redis.history_ttl` | string | `24h` | 频道 Stream 的空闲过期时间（每条发布后刷新 `EXPIRE`，`pkg/redisbroker/redis.go:94-96`） |
| `broker.redis.consumer_group` | string | 未设置 | **未实现：配置非空会被 `Validate()` 直接拒绝**（`broker.redis.consumer_group is not implemented`，`Validate()` 规则 5）。该字段仅存在于 `config.RedisConfig`（`config/config.go:153`）声明，整个代码库没有任何读取点；配置它会启动失败，应移除 |

以上时长字段（`dial_timeout` / `read_timeout` / `write_timeout` / `history_ttl`）不在 `Validate()` 校验范围内，解析失败会被静默忽略并保留默认值（`options.go:89-114`），因此无效时长不会导致启动失败。

### Redis 键布局

Redis broker 使用以下键前缀（`pkg/redisbroker/options.go:10-17`），同一 Redis 实例内与业务数据共存时可按前缀隔离：

- `ml2:stream:` + 频道名 —— 历史 Stream（Redis Streams）
- `ml2:pubsub:` + 频道名 —— 实时投递（Redis Pub/Sub）
- `ml2:presence:` —— 在线状态
- `ml2:cluster:` 系列 —— 集群控制面（会话租约、快照、频道投影等）

发布路径：XADD（写历史并取 offset）→ EXPIRE（刷新 TTL）→ PUBLISH（实时投递），单次发布的操作超时均为 5s（`pkg/redisbroker/redis.go:73-111`）。

## cluster 节

```yaml
cluster:
  enabled: false       # 启用分布式控制面
  node_id: node-a      # 逻辑节点唯一 ID
  backend: redis       # 当前仅 redis 有实际实现
```

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `cluster.enabled` | bool | `false` | 启用 Redis 支撑的分布式控制面。启用时要求 `broker.type: redis`（`Validate()` 规则 5） |
| `cluster.node_id` | string | 未设置 | 逻辑节点标识，集群内必须唯一；启用时必填（`cluster.go:27-30`） |
| `cluster.backend` | string | `redis` | 控制面后端。为空时默认 `redis`；接受 `redis` / `memory` / `noop`，其他值报错（`cluster.go:32-41`）。仅 `redis` 在二进制中接入实际组件（会话目录、命令总线、查询投影、节点租约、投影修复，`cmd/server/main.go:111-142`） |

启用 Redis 集群时，控制面组件与 broker 共用同一个 `broker.redis` 配置（`cmd/server/main.go:116-131`），并使用 `ml2:cluster:` 前缀的键。另外集群模式下 `/health` 端点会附带 Redis 连通性探测（`cmd/server/main.go:57-59`）。

拓扑、会话迁移与故障转移语义见[《分布式集群指南》](04-cluster.md)。

## proxy 节

```yaml
proxy:
  - name: example-grpc          # 唯一标识
    endpoint: "127.0.0.1:10091" # gRPC: host:port；HTTP: 完整 URL
    timeout: "30s"              # 代理级超时，默认 30s
    grpc:                       # gRPC 后端配置（二选一）
      insecure: true
    # http:                     # HTTP 后端配置（二选一）
    #   headers:
    #     X-Backend: messageloop
    #   tls:
    #     insecure_skip_verify: false
    #     server_name: backend.example.com
    routes:
      - channel: "*"            # 频道 glob 模式
        method: "*"             # 方法 glob 模式
```

`proxy` 是数组，可配置多个代理；每个代理通过 `routes` 声明自己的匹配域。

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `name` | string | 未设置 | 代理标识，用于日志与排障 |
| `endpoint` | string | 未设置 | 后端地址。gRPC 后端为 `host:port`；HTTP 后端为完整 URL（`http://` / `https://` 前缀，`proxy/proxy.go:100-102`） |
| `timeout` | string | `30s` | 代理级请求超时（`proxy.DefaultRPCTimeout`）。0 或未设置取默认 30s（`proxy/http.go:41-44`、`proxy/grpc.go:32-35`）。解析失败会在启动时报错（`cmd/server/main.go:172-178`）。与 `server.rpc_timeout` 的关系见 [三层超时](#三层超时) |
| `http` | 对象 | 未设置 | HTTP 代理配置；`headers`（map[string]string）为附加请求头（默认注入 `Content-Type: application/json`，`proxy/http.go:56-63`）；`tls.insecure_skip_verify` 关闭证书校验；`tls.server_name` 设置 SNI 服务器名（`proxy/http.go:46-54`） |
| `grpc` | 对象 | 未设置 | gRPC 代理配置；`insecure: true` 使用明文连接，否则用系统 CA 池做 TLS（`proxy/grpc.go:37-46`）。gRPC 响应上限固定为 4 MB（`MaxCallRecvMsgSize`，`proxy/grpc.go:38`） |
| `routes` | 数组 | 未设置 | 路由规则，见下 |

### 路由语义

每条路由由 `channel` + `method` 两个 glob 模式组成（gobwas/glob 语法，`proxy/router.go:35-56`），必须同时匹配才命中：

- `channel` 模式匹配目标频道，如 `chat.*`、`rpc.**`；
- `method` 模式匹配 RPC 方法名，`"*"` 匹配一切方法；
- **按配置顺序求值，首个匹配的代理生效**（`proxy/router.go:33-34, 60-71`）；
- 无任何路由匹配时 `ProxyRPC` 返回 `ErrNoProxyFound`（`proxy/router.go:11`），客户端收到**软失败** `NO_PROXY` 错误信封（`type=request_error`，PR-KA-A4 §8.3——不再回显请求体）；代理调用本身失败则返回 `PROXY_ERROR` 错误信封（`client.go:806-822`）。

除路由外，代理类型选择规则（`node.go:505-517`）：显式配置 `grpc` 段 → gRPC 代理；显式配置 `http` 段 → HTTP 代理；两者皆无时按 `endpoint` 前缀判断（`http://` / `https://` → HTTP，否则 gRPC）。

### 代理钩子

`Proxy` 接口（`proxy/proxy.go:12-42`）定义了 8 个钩子，分别在以下时机调用：

| 钩子 | 触发时机 | 路由匹配方式 |
| --- | --- | --- |
| `Authenticate` | 客户端携带 token 发起连接时（`client.go:387-452`） | 固定方法名 `$authenticate`（`SystemMethodAuthenticate`，`client.go:351`），频道为空；即路由需写成 `channel: "*", method: "$authenticate"` 才能接到鉴权。返回的 `UserInfo.ID` 作为用户 ID 用于后续 ACL |
| `RPC` | 客户端 RPC 请求（`client.go:733-846`） | 请求的 channel + method |
| `SubscribeAcl` | 订阅/恢复订阅时的频道 ACL 检查（`client.go:661-697, 1125-1126`） | channel + 固定方法名 `"subscribe"` |
| `PublishAcl` | 发布前的频道 ACL 检查（`client.go:873-901`） | channel + 固定方法名 `"publish"` |
| `OnConnected` | 连接已加入 Hub 后（`client.go:537-544`） | 与连接同名的代理（即鉴权所用代理）；错误被忽略 |
| `OnSubscribed` | 订阅成功后（`client.go:1009-1014`） | 同上；错误被忽略 |
| `OnUnsubscribed` | 取消订阅后（`client.go:1069-1074`） | 同上；错误被忽略 |
| `OnDisconnected` | 客户端断开时（`client.go:211-215`） | 同上；错误被忽略 |

**授权与代理的关系（PR-KA-A4 §8.1）**：订阅/发布**先**过静态 `Authorizer.Decide`，再查代理——`SubscribeAcl` / `PublishAcl` 存在匹配路由时作为**额外的门**：代理拒绝只否决这一次请求，代理允许也**不得**跳过静态 deny（`client.go:685-695, 902-916`）。两者皆无时按静态 `Decide` 默认叙事（订阅/发布默认放行）。

### 三层超时

请求链路上存在三层超时，各自生效范围如下：

1. **请求级**：`server.rpc_timeout`（默认 30s）——仅作用于 `RPC` 转发，每个请求创建独立的 context 截止时间（`client.go:742-745`），超时返回 `RPC_TIMEOUT` 错误信封；
2. **代理级**：`proxy[].timeout`（默认 30s）——作用于 `Authenticate` / `SubscribeAcl` / `PublishAcl` / `OnConnected` / `OnSubscribed` / `OnUnsubscribed` / `OnDisconnected` 等未携带 deadline 的调用（`proxy/http.go:402-407`、`proxy/grpc.go:238-243` 的 `withTimeout` 仅在 ctx 无截止时间时叠加）；HTTP 代理同时将其设为 `http.Client.Timeout`（`proxy/http.go:68-71`），因此 RPC 请求最迟也在该值内完成；
3. **传输级**：`transport.websocket.write_timeout` / `transport.grpc.write_timeout`——下行消息写入对端的超时（`pkg/websocket/transport.go:43-52`；gRPC 未配置时默认 10s）。

## 完整示例走查

以下逐段解读仓库根目录的 `config-example.yaml`（单节点 + Redis broker + 集群预留 + 一个 gRPC 代理的形态）：

```yaml
server:
  http:
    addr: "127.0.0.1:8080"
```

管理 HTTP 显式绑定回环地址 `127.0.0.1:8080`（此处不写默认值也能工作，但显式声明更安全），对外仅暴露 `/health` 与 `/metrics`。

```yaml
  grpc_admin:
    addr: "127.0.0.1:9091"
    auth_token: "change-me"
```

管理 gRPC 同样绑定回环地址，并建议放置于私有网卡。注意 `addr` 非空时 `auth_token` 与 `allow_insecure` 必须至少设置一个（校验规则 6）：`auth_token: ""` 会直接启动失败，需要填写 token 或显式加 `allow_insecure: true`（仅限开发）；需要 TLS 时取消 `tls` 段注释并成对填写证书与私钥。注意：这两个地址（连同 `transport.grpc.addr`）在启动时都会被无条件预绑定，必须可监听。

```yaml
  heartbeat:
    idle_timeout: "300s"
    ping_interval: "0s"   # 服务端 ping 默认关；游戏/IoT 秒级探活再打开
    ping_timeout: "3s"
  rpc_timeout: "30s"
```

`idle_timeout: "300s"` 显式声明 300s 空闲断开——该值恰好等于回退默认值：留空或解析失败同样按 300s 生效，心跳无法通过留空禁用；唯一禁用方式是显式配置 `"0s"`（`HeartbeatManager.Start` 直接返回，不启动心跳）。非 0 的 `idle_timeout` / `ping_interval` / `ping_timeout` 必须 ≥1s。`ping_interval` 默认 0（不主动 ping，旧行为）；打开后服务端按 `ping_timeout` 等待应答、未应答即 3511，**必须同时升级 SDK 到能回 Pong 的版本**，否则旧客户端会被踢。集群 session lease 按公式 `max(30s, 2×idle, 3×ping_interval, idle+10s+10s)` 随心跳缩短，默认配置仍为 600s（见 [04-cluster.md](04-cluster.md)）。`rpc_timeout: "30s"` 与代理默认值相同，可省略。

```yaml
  limits:
    max_connections_per_user: 0
    max_subscriptions_per_client: 0
    max_publishes_per_second: 0
    max_message_size: 65536
```

前三项为 0（不限）；`max_message_size: 65536` 显式写出 64 KB，与 0（取默认 64 KB）效果一致。需要大于 64 KB 的负载时在此调大，WebSocket 与 gRPC 同步生效。

```yaml
  authorizer:
    rules:
      - pattern: "chat.public.*"
        allow_subscribe: ["*"]
        allow_publish: ["alice", "bob"]
      - pattern: "chat.private.*"
        deny_all: true
```

授权表：`chat.public.*` 任何已认证用户可订阅、仅 `alice`/`bob` 可发布；`chat.private.*` 整体封锁。求值语义为「语言包含 + deny 不打洞」（见 [Authorizer 求值语义](#authorizer-求值语义pr-ka-a4-5)）：例如存在 `secret.**` deny_all 时，再写一条 `secret.lobby` 允许 `alice` 也不能让 `alice` 订进去。代理 `SubscribeAcl` / `PublishAcl` 命中时作为额外门，不能越过这里的静态 deny。

```yaml
transport:
  websocket:
    addr: ":9080"
    path: "/ws"
    allow_all_origins: true
    compression: true
```

WebSocket 监听 `:9080`，路径 `/ws`（必须显式配置，二进制不会套用默认路径）。`allow_all_origins: true` 是典型的开发期配置，生产应改为 `allowed_origins` 白名单或依赖默认同源检查。`compression: true` 启用 permessage-deflate。

```yaml
  grpc:
    addr: ":9090"
```

客户端 gRPC 流式监听 `:9090`。`write_timeout` 与 `tls` 未配置，分别使用传输层默认（10s）与明文。

```yaml
broker:
  type: redis
  redis:
    addr: 127.0.0.1:6379
    password: ""
    db: 10
```

Redis broker 连接本机 6379，选择数据库 10。注释掉的 `pool_size` 等字段展示的即默认值（10 / 5 / 3 / 5s / 3s / 3s / 10000 / true / 24h），无需显式写出。

```yaml
cluster:
  enabled: false
  node_id: node-a
  backend: redis
```

集群当前关闭。`enabled: true` 时要求 `broker.type: redis`（此处满足），且 `node_id` 必须填写并在集群内唯一；`backend: redis` 是唯一在二进制中接入实际实现的取值。

```yaml
proxy:
  - name: example-grpc
    endpoint: 127.0.0.1:10091
    timeout: 30s
    grpc:
      insecure: true
    routes:
      - channel: "*"
        method: "*"
```

注册名为 `example-grpc` 的 gRPC 代理，明文连接 `127.0.0.1:10091`，`channel: "*"` + `method: "*"` 匹配所有频道的所有方法。该代理同时承担 `$authenticate` 鉴权（`method: "*"` 覆盖了固定方法名）、`"subscribe"` / `"publish"` 的 ACL 裁决（作为静态 Authorizer 之外的额外门），以及 RPC 转发与四个连接生命周期通知——注意代理允许**不会**越过 `server.authorizer` 里的静态 deny（PR-KA-A4 §8.1）。`timeout: 30s` 与默认一致。注释掉的 `example-http` 段展示了 HTTP 代理形态（完整 URL 端点、附加头、TLS 选项）。

## 多节点注意

部署多个节点时（如 `config-node1.yaml` / `config-node2.yaml`，二者已按此约定编写）：

- **共享同一套 Redis 设置**：集群控制面与 broker 共用 `broker.redis` 段（`addr` / `password` / `db`，`cmd/server/main.go:116-131`），各节点必须指向同一 Redis 实例与数据库，才能共享会话目录、命令总线与查询投影。键空间通过 `ml2:` 前缀隔离，无需额外配置；
- **`cluster.node_id` 必须全局唯一**：它是节点租约、命令路由与会话所有权的标识（`cluster.go:27-30`），重复的 `node_id` 会导致租约冲突与命令投递错乱；
- 各节点面向客户端的监听地址（`transport.websocket.addr` / `transport.grpc.addr`）与 `server.http.addr` / `server.grpc_admin.addr` 应各不相同（负载均衡器对外暴露，节点间不直接互连）；
- 管理面与客户端面的端口分离语义见[《架构指南》](01-architecture.md)；完整的集群拓扑、会话迁移与故障恢复说明见[《分布式集群指南》](04-cluster.md)。
