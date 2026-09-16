# 配置参考

本文档是 MessageLoop 配置项的逐字段权威参考。所有字段名、类型、默认值与校验规则均对照源码核实（`config/config.go`、`internal/runtime/defaults.go`、`internal/stream/defaults.go`、`cmd/server/main.go`、`cmd/server/runtime.go`、`cmd/server/envconfig.go`、`pkg/redisbroker/options.go`、`proxy/`、`pkg/transport/` 下各传输）。协议与部署层面的说明见[《客户端协议参考》](../protocol.md) 与[《部署指南》](../deployment.md)。

## 概述

服务器通过单个 YAML 文件配置，路径由 `--config` 启动参数指定（默认 `./config.yaml`）。启动流程：

1. lynx 框架读取 YAML 并反序列化为 `config.Config`（结构定义见 `config/config.go`）；
2. 调用 `Config.Validate()`，校验失败则启动中止并返回 `invalid config: ...`；
3. 各组件构造时应用默认值（见下文各节"默认值"）。

### 环境变量覆盖（cmd/server/envconfig.go）

容器部署可以不重打包就改配置：`MESSAGELOOP_` 前缀的环境变量覆盖配置文件中的同名键，映射规则为「前缀 + 点分路径，点换下划线」，如 `broker.redis.addr` → `MESSAGELOOP_BROKER_REDIS_ADDR`，环境变量优先于文件。两类键可用：

- **注册白名单**（`envConfigKeys`）：部署相关的标量键——`server.http.*`、`server.grpc_admin.*`、`server.heartbeat.*`、`server.rpc_timeout`、`server.require_auth`、`server.namespace`、`server.limits.*`、四个传输的 addr/timeout/tls、`broker.*`、`cluster.*`。文件里省略这些键时，仅白名单键能凭环境变量出现。
- **文件内已有键**：`AutomaticEnv` 还会覆盖文件中已出现的任意标量键；字符串数组接受逗号分隔值。

复杂嵌套结构（`server.authorizer.rules`、`proxy` 后端数组）不支持环境变量，只能写配置文件。

### Config.Validate() 校验规则

`Validate()`（config/config.go）按以下顺序检查，返回第一条错误：

1. **传输地址必填**：`transport.websocket.addr`、`transport.websocket.path` 与 `transport.grpc.addr` 均必填（启动接线无条件构造 WebSocket 与客户端 gRPC 监听器，缺失地址会错绑或 panic）。`transport.quic.addr` / `transport.kcp.addr` 可选，空值表示不启动对应监听器；二者非空时必须配置 TLS 证书对或 `insecure: true`。
2. **时长格式**：以下字段若非空必须是合法的 Go duration 字符串（如 `"30s"`、`"1m30s"`）：`server.heartbeat.idle_timeout`、`server.heartbeat.ping_interval`、`server.heartbeat.ping_timeout`、`server.rpc_timeout`、`transport.websocket.read_timeout` / `write_timeout`、`transport.grpc.write_timeout`、`transport.quic.write_timeout` / `read_timeout`、`transport.kcp.write_timeout` / `read_timeout`。
3. **写超时必须为正**：四个传输的 `write_timeout` 显式配置 `<= 0` 一律拒绝（省略字段则用默认值；0 会让一个卡住的对端无限拖住投递）。
4. **心跳取值约束**：`idle_timeout` / `ping_interval` / `ping_timeout` 非 0 值必须 ≥1s；`ping_timeout` 显式 `"0s"` 仅在 `ping_interval > 0` 时被拒绝。`idle_timeout < ping_interval + ping_timeout` 时打印警告（探测窗口大于空闲超时，配置可疑但不阻断启动）。
5. **TLS 证书/密钥成对**：以下五处的 `cert_file` 与 `key_file` 必须同时设置或同时为空：`server.grpc_admin.tls`、`transport.websocket.tls`、`transport.grpc.tls`、`transport.quic.tls`、`transport.kcp.tls`。
6. **管理 gRPC 鉴权**（`validateAdminAuth`）：`server.grpc_admin.addr` 非空时必须至少配置一种凭证路径——`auth_tokens`、`proxy[].admin_auth: true` 指派或 `allow_insecure: true`，否则 Validate 失败；每把 `auth_tokens` ≥ 20 字符；`admin_auth_cache_ttl` 非空时必须是合法且为正的 Go duration；`proxy[].admin_auth` 至多一个条目可指派且该条目必须带 `name`（G3）；`allow_insecure` 只允许搭配回环 `server.grpc_admin.addr`（G5，非 loopback 即 Validate 错误）；`server.http.addr` 非 loopback 时必须配置 `server.http.auth_token`（admin HTTP 同规则对齐）。
7. **broker 校验**：`broker.type` 必须为 `memory` 或 `redis`（空等价于 `memory`）；为 `redis` 时 `broker.redis.addr` 必填；`broker.redis.consumer_group` 非空直接拒绝（字段声明但未实现）；`broker.redis.stream_approximate` 非 true（含显式 false 与省略）直接拒绝（只实现了近似截断，必须显式确认）。
8. **cluster 前置条件与 HMAC**：`cluster.enabled: true` 要求 `broker.type: redis`（`cluster requires broker.type=redis`）。启用集群时 HMAC 密钥源校验也在 `Validate()` 内执行：`hmac_key` 与 `hmac_key_file` 二选一（两源同设或均未设置即拒绝），内联 `hmac_key` 长度不足 32 字节即拒绝。
9. **Admin Capability 闭集**：`server.grpc_admin.capabilities` 的每个名字都必须在闭集内（见 [server 节](#server-节)）。
10. **已删除键拒绝**：`server.acl`、`server.channels`、`server.presence.cluster_emit` 任一出现即失败（无兼容期）。
11. **授权表**：`server.authorizer` 的规则 `pattern` 非空且是订阅 key 语言（`*` 单段、`**` 仅末尾、字面前缀非空）；`history_size` 设置时 ≥ 0；`history_ttl` / `max_survey_timeout` 非空时必须是合法 Go duration。
12. **namespace**：`server.namespace` 非空时按 `topics.ValidateNamespace` 校验标识符（`[a-z0-9-]`、1-32 字符、以 `[a-z0-9]` 开头结尾）；为空且 `require_auth` 为 false 时必须提供（没有代理可以提供 namespace，fail-closed）。
13. **管理地址必填**：`server.grpc_admin.addr` 必填（`prepareGRPCServers` 无条件预绑定该监听器）。

不在 `Validate()` 内的校验：`cluster.node_id` 必填在 `ClusterOptions.Normalize()`（internal/cluster/contracts.go）与启动接线（cmd/server/main.go 的 `normalizeClusterOptions`）两处检查；`hmac_key_file` 的文件读取与密钥长度在启动接线 `ClusterConfig.ResolveHMACKey()`（config/config.go）执行，失败即拒绝启动。

### 常量与默认值分布

默认值不集中在单一文件，分布如下：

| 常量 | 值 | 位置 | 用途 |
| --- | --- | --- | --- |
| `DefaultMaxMessageSize` | 64 KB | internal/runtime/defaults.go | `limits.max_message_size` 为 0 时生效 |
| `DefaultHeartbeatIdleTimeout` | 300s | internal/runtime/defaults.go | `heartbeat.idle_timeout` 为空或解析失败时回退 |
| `MaxRecoveredPublications` | 1000 | internal/runtime/defaults.go | 连接时历史恢复的最大投递条数 |
| `DefaultShutdownTimeout` | 10s | internal/runtime/defaults.go | 优雅关闭时排空连接的上限（进程级整体关闭由 lynx 的 30s 兜底） |
| `DefaultHistoryLimit` | 1000 | internal/stream/defaults.go | `History` 未指定 limit 时的返回条数上限 |
| `MaxPresenceSnapshotClients` | 256 | internal/occupancy/defaults.go | presence 快照 clients 条数全局默认 |
| `DefaultRPCTimeout` | 30s | proxy/proxy.go | `server.rpc_timeout` 与 `proxy[].timeout` 的默认 |
| 各传输写超时 | 10s | pkg/transport/{ws,grpc,quic,kcp}/ | 对应 `write_timeout` 未配置时的兜底 |
| QUIC 默认 idle/keepalive | 5min / 15s | pkg/transport/quic/server.go | 未联动心跳配置时的 QUIC 会话默认 |
| KCP 默认窗口 | 256/256 | pkg/transport/kcp/server.go | KCP 会话窗口 |

## 顶层结构

```yaml
server:      # 服务端行为：管理 HTTP、管理 gRPC、心跳、限流、授权、命名空间
transport:   # 客户端监听器：WebSocket、gRPC 与可选 QUIC/KCP
broker:      # 发布/订阅后端：memory 或 redis
cluster:     # 分布式控制面（可选）
proxy:       # 后端代理数组（可选）
```

## server 节

```yaml
server:
  http:
    addr: "127.0.0.1:8080"      # 管理 HTTP：/health 与 /metrics
    # auth_token: ""            # 设置后两个端点都要求 Bearer token
  grpc_admin:
    addr: "127.0.0.1:9091"      # 管理 gRPC API（必填）
    auth_tokens:                # 静态超管 Bearer token 列表（每把 ≥ 20 字符）；
      - "change-me-admin-token-min-20-chars"  # addr 非空时三种凭证路径至少其一
    # admin_auth_cache_ttl: "30s"  # API Key 正缓存 TTL（默认 30s），撤销传播上界
    allow_insecure: false       # 仅限回环绑定：非 loopback + true = Validate 错误
    # tls:
    #   cert_file: "./certs/admin.crt"
    #   key_file: "./certs/admin.key"
  heartbeat:
    idle_timeout: "300s"        # 空或解析失败 = 回退 300s；"0s" = 不做空闲断开
    ping_interval: "0s"         # 服务端主动 ping 间隔；0/空 = 不主动 ping（默认）
    ping_timeout: "3s"          # 服务端 ping 未应答判定；仅 ping_interval>0 时生效；空 = ping_interval
  rpc_timeout: "30s"
  namespace: "dev"              # 静态命名空间；require_auth 关闭时必填
  limits:
    max_connections_per_user: 0
    max_subscriptions_per_client: 0
    max_publishes_per_second: 0
    max_message_size: 65536
  authorizer:                   # 唯一授权表，见下节；可省略
    default:
      history: true
      history_size: 0           # 0 = broker 全局（memory 256 / redis stream_max_length）
      history_ttl: ""           # 空 = broker 全局；memory broker 忽略并 Warn
      presence: true
      recover: true
      survey: false             # 客户端 survey 默认关
      recover_limit: 0          # 0 = MaxRecoveredPublications
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
| `server.http.addr` | string | `127.0.0.1:8080` | 管理 HTTP 监听地址，暴露 `/health` 与 `/metrics`。为空时回退到 `127.0.0.1:8080`。指标说明见[《可观测性指南》](05-observability.md) |
| `server.http.auth_token` | string | 未设置 | 管理 HTTP 的 Bearer token。设置后 `/health` 与 `/metrics` 都要求 `Authorization: Bearer <token>`（常量时间比较，失败返回 401）。**绑定到非回环地址时必填**（G5 fail-closed：未设置且非 loopback 是 Validate 错误，不再是 WARN） |
| `server.grpc_admin.addr` | string | 未设置（**必填**） | 管理 gRPC API 监听地址。启动时无条件预绑定（见 [启动要求](#启动要求)）。接口清单见[《管理 API 参考》](03-admin-api.md) |
| `server.grpc_admin.tls.cert_file` / `.key_file` | string | 未设置 | 管理 gRPC 的 TLS 证书与私钥，二者必须成对设置（校验规则 5） |
| `server.grpc_admin.auth_tokens` | string[] | 未设置 | 静态超管 Bearer token **列表**，通过 `authorization: Bearer <token>` 头传递，逐把常数时间比较，任一命中即超管身份（namespace `["*"]` + 能力位上限）。**每把 ≥ 20 字符**（Validate 强制）。列表形态让轮换无空窗：加新 → 滚动重启 → 删旧。**no-compat 替换**：单值键 `auth_token` 已删除，配置中出现会被忽略（三种凭证路径齐缺时 Validate 拒绝启动） |
| `server.grpc_admin.admin_auth_cache_ttl` | duration | `30s` | API Key 经 `admin_auth` 指派 proxy 校验后的正缓存 TTL（`DefaultAdminAuthCacheTTL`），同时是 Key 撤销传播的时间上界；生效值还会被 Key 自带 `max_age_seconds` 收紧、下限 1s。省略取默认；显式值必须为合法正 duration（Validate） |
| `server.grpc_admin.allow_insecure` | bool | `false` | 显式放弃强制鉴权：无凭证请求注入 insecure 超管身份，每个请求记录 WARN。**仅限回环绑定**（G5 fail-closed）：非 loopback 的 `addr` 搭配 `true` 是 Validate 错误。仅限受控环境（开发/内网） |
| `server.grpc_admin.capabilities` | string[] | 未设置 | Admin Capability 闭集：`history.read` / `presence.read` / `channels.list` / `session.act` / `user.fanout` / `subscribe.any` / `presence.large_snapshot` / `survey.bypass_gate` / `pattern.global`（预留）。**省略 = 除 `pattern.global` 外全部位**；**显式 `[]` = 零位**，锁死 Admin 数据面（GetHistory / GetPresence / GetChannels / 代订 / 按 user 扇出全部失败）；未知名 → Validate 错误 |
| `server.heartbeat.idle_timeout` | string | 未设置 | 客户端空闲超时。为空或解析失败回退 300s；非 0 值必须 ≥1s；`"0s"` 表示不做空闲断开。idle 超时内无任何活动即 3511 断连。仅当 `idle_timeout` 与 `ping_interval` 都为 0 时心跳管理器完全不启动（只关 idle、开 ping 仍会跑探测循环）。WebSocket/QUIC/KCP 的读超时与其联动，见各传输节 |
| `server.heartbeat.ping_interval` | string | `0s`（不主动 ping） | 服务端主动探测半开连接：每 `ping_interval` 发一次 Outbound `Ping`（首次在一个 interval 之后，带 0.8~1.2 抖动防齐射），随后 `ping_timeout` 内未收到任何入站帧（Pong/Ping/业务均可）即断开 3511。非 0 值必须 ≥1s。**打开后旧客户端会被踢**：不回 Pong 的旧 SDK 需要先升级。集群 session lease 随此值缩短，见 [《分布式集群指南》](04-cluster.md) |
| `server.heartbeat.ping_timeout` | string | 等于 `ping_interval` | 服务端 ping 的应答窗口。仅 `ping_interval>0` 时有意义；留空按 `ping_interval` 取值；显式 `"0s"` 在 `ping_interval>0` 时被 Validate 拒绝；非 0 值必须 ≥1s |
| `server.rpc_timeout` | string | `30s` | RPC 转发请求的超时（`proxy.DefaultRPCTimeout`）。解析失败回退 30s。每个 RPC 请求以此值创建 context 截止时间，超时向客户端返回 `RPC_TIMEOUT` 错误。与代理级超时的关系见 [三层超时](#三层超时) |
| `server.namespace` | string | 未设置 | 多租户命名空间：每个客户端可见 channel 必须位于会话所属命名空间下（`ns:topic`，见[《客户端协议参考》](../protocol.md) Channel Naming）。来源优先级：鉴权代理响应 `UserInfo.namespace` > 该静态值；**两者都为空且 `require_auth` 开启时 Connect 被拒**（`NAMESPACE_REQUIRED` + 3500）；**`require_auth` 关闭时本字段必填**（Validate 强制）。标识符规则：`[a-z0-9-]`、1-32 字符、以 `[a-z0-9]` 开头结尾。会话建立后所有跨命名空间 channel 操作被 `NAMESPACE_MISMATCH` 拒绝；resume 跨命名空间接管被拒（3500）。hub 连接上限、admin 按 user 寻址、集群 user 索引均以 (namespace, user) 为作用域 |
| `server.limits.max_connections_per_user` | int | 0（不限） | 同一用户 ID 的最大并发连接数（按用户分片限制）。0 = 不限 |
| `server.limits.max_subscriptions_per_client` | int | 0（不限） | 单个客户端可订阅的频道数上限。0 = 不限 |
| `server.limits.max_publishes_per_second` | int | 0（不限） | 单客户端发布速率上限（令牌桶），超限返回 `RATE_LIMITED`。0 = 不限 |
| `server.limits.max_message_size` | int | 0 = 默认 64 KB | 入站消息大小上限（字节）。0 时取 `DefaultMaxMessageSize`。该限制同时作用于 WebSocket（读限制 + 解压后上限）与 gRPC（`MaxRecvMsgSize`）。注意 0 的语义是"默认值"而非"不限" |
| `server.authorizer.default` | 对象 | 见各字段默认 | 未命中任何规则的频道的兜底 Effects。各字段均可用同名键覆盖默认；`history_ttl` / `max_survey_timeout` 用字符串以区分「未设置」与 `"0s"` |
| `server.authorizer.rules` | 数组 | 空 | 授权表：pattern → allow 名单 / deny_all / Effects，按配置顺序 overlay（见下） |
| `server.require_auth` | bool | `false` | 拒绝空 token 的连接。开启后：连接未携带 token 直接拒绝（`AUTH_REQUIRED`）；携带 token 但没有匹配 `$authenticate` 路由的代理时同样拒绝——非空 token 不得绕过认证。实际认证总是由代理后端完成，见 [proxy 节](#proxy-节) |
| `server.presence.cluster_emit` | — | **键已删除** | 写进 YAML（无论 true/false）都会让 `Validate()` 失败。Occupancy 跨节点统一走 LiveBus 精确频道 + Interest 编译（见[《分布式集群指南》](04-cluster.md)） |

### Authorizer 求值语义

所有授权与频道策略来自 `server.authorizer` 一张表，由 `Authorizer.Decide` / `Authorizer.Effects`（internal/authz/authorizer.go）求值。`server.acl` 与 `server.channels` 键已被移除：YAML 出现这两个键会让 `Validate()` 失败。

- **订阅（SubscribePattern）**：默认放行。先做路由检查（`CompileInterest`，与 broker 同套规则）——不可路由的 pattern（`*.room`、裸 `**`）返回 `PATTERN_NOT_ROUTABLE`，先于 ACL；然后对该 principal 逐条 deny 规则（deny_all、空 allow 名单、不含该用户的名单）做语言求交，`L(订阅 pattern) ∩ L(deny 规则) ≠ ∅` → 整条拒绝（客户端信封 `PERMISSION_DENIED`）。deny 不可被更具体的 allow 打洞：要开洞就缩小 deny 的 pattern。
- **发布（Publish）**：精确频道，默认放行；`deny_all` 命中或 allow 名单未命中该用户 → 拒绝。不要求订阅覆盖。
- **Survey**：默认拒绝；`Effects.Survey==true` 且 存在 `allow_survey` 命中该精确频道 且 无 deny 命中才放行。Admin 无 `survey.bypass_gate` 能力位时同样受此名单约束。
- **恢复（Recover）/ 在场（Presence）**：精确频道；默认跟随 `Effects(ch)`；`deny_all` 命中或通配频道 → 拒绝。
- **Admin**：用户 ID 按 `"admin"` 匹配名单；另受 `server.grpc_admin.capabilities` 能力位约束。

**Effects（`Authorizer.Effects(ch)`）** = `DefaultChannelPolicy()` overlay `server.authorizer.default`，再按表顺序 overlay 每一条匹配规则（后写覆盖先写）——不是 first-match：通用规则写前面、特殊规则写后面即可。`TransientOnly` 强制 `History=false` 且 `Recover=false`。示例：

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

`game.tick.fps` 命中两条规则：`transient_only` 生效 → 强制瞬时、不可恢复，但先前的 `survey: true` overlay 保留。`game.room.1` 只命中 `game.**` → history + survey 开。

**规则内联 Effects 字段**（`server.authorizer.default` 与每条 `rules[]` 均可写）：

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `history` | bool | `true` | 是否写历史。`false` 与 `transient_only: true` 效果相同：发布改走瞬时（见下） |
| `history_size` | int | 0 = broker 全局 | 该前缀频道的历史容量：memory broker 每频道 ring 容量 / Redis 每条 `XADD` 的 `MAXLEN`。只在该频道历史首次创建时生效：已存在的内存 ring 不会因改配置立即重建，直到频道被回收 |
| `history_ttl` | string | 空 = broker 全局 | 历史保留时长（Redis：每次发布后刷新）。memory broker 无 TTL，配置了打 Warn 并忽略 |
| `presence` | bool | `true` | presence 开关。`false` 的频道不存 presence、不发 join/leave、无快照，`PresenceQuery` 返回 `POLICY_DENIED` |
| `recover` | bool | `true` | 恢复开关。`false` 时恢复被跳过（客户端要了 recover 则返回 `RECOVER_SKIPPED`） |
| `survey` | bool | `false` | 客户端 survey 开关。`false` 时客户端 `SurveyRequest` 返回 `SURVEY_DISABLED` 且零下发；`true` 时还须 `allow_survey` 规则与频道覆盖（`sessionCoversChannel`）全部通过才能发起 |
| `transient_only` | bool | `false` | 强制瞬时：发布只实时投递、绝不写历史。隐含 History=false、Recover=false（即使漏写）。对客户端：不带 `transient` 标志的发布也改走瞬时发布、ack offset=0、不报错；对 Admin：`add_history=true` 被拒绝（计失败、不发布），`add_history=false` 仍可瞬时发布 |
| `recover_limit` | int | 0 = `MaxRecoveredPublications` | 恢复条数上限。命中该上限（或请求级配额耗尽）时恢复结果标记 `truncated=true` |
| `max_survey_subscribers` | int | 256 | survey 订阅者上限。客户端 Survey 发起方本节点订阅者数（快路径，含通配命中）或集群预检总数超过该值 → `SURVEY_TOO_MANY_SUBSCRIBERS`、零条 outbound `SurveyRequest`。`0` = 不限制。Admin 无 `survey.bypass_gate` 时同样受此门限制 |
| `max_survey_timeout` | string | `5s` | 客户端 Survey 超时上限：请求 `timeout_ms` 被钳制在 `[100ms, min(本值, 10s)]`；`timeout_ms<=0` 用本值 |
| `legacy_presence_channel` | bool | `false` | 为 `true` 时 join/leave 额外以旧 JSON 瞬时发布到精确频道的 `ch/__presence` 伴生频道（通配订阅从不写伴生） |
| `presence_snapshot_limit` | int | 256 | `Connected.presence` / `SubscribeAck.presence` / `PresenceQuery` 快照的 clients 条数上限；`occupancy` 仍是全量计数，超出置 `truncated=true`。Admin `GetPresence` 无 `presence.large_snapshot` 能力位时同样截断到该上限 |

**transient_only 对客户端与 Admin 的差异**：

- **客户端 Publish**：策略强制瞬时时，即使客户端没带 `transient` 标志，也改走瞬时发布，返回 ack `offset=0`，不报错；同时 `messageloop_channel_policy_transient_forced_total` 指标 +1（客户端显式 `transient: true` 不计数）。消息不写历史，实时订阅者仍能收到。
- **Admin Publish**：`add_history=true` 但策略禁历史 → 打 Warn、计失败、不发布（避免误以为写入了）；`add_history=false`（或缺省）仍走瞬时发布。若同请求其他发布成功，RPC 仍成功（部分成功语义），只有全部失败才返回错误。

**容量与部署建议**：

- `history_size` 只影响 `Publish` 路径（broker 首次建 ring / 每条 `XADD` 时读取）。
- memory broker 无 TTL：配置 `history_ttl` 会在该频道首次带 TTL 发布时打一次 Warn 并忽略。
- 内存 ring 不因新配置重建：已存在的频道保持旧容量直到回收，改小 `history_size` 对已有频道不立即生效。
- IM 大容量历史请使用 Redis broker：memory 按 `history_size × 平均负载 × 频道数` 占内存（5000 × 512B × 1000 频道 ≈ 2.5 GB），单节点 memory 不适合大 IM；Redis 侧 `im.**` 5000 × 1KB ≈ 5 MB/频道，并可用 `history_ttl` 控制留存。`game.tick.**` 强制瞬时则对 Redis 零 Stream 写入，只剩 Pub/Sub。

## transport.websocket 节

```yaml
transport:
  websocket:
    addr: ":9080"
    path: "/ws"
    read_timeout: "60s"       # 可选
    write_timeout: "10s"      # 可选，显式 0 会被拒绝
    allow_all_origins: false  # 仅开发环境
    allowed_origins:
      - "https://example.com"
    compression: true         # permessage-deflate
    # tls:
    #   cert_file: "./certs/server.crt"
    #   key_file: "./certs/server.key"
    check_origin: false       # 已废弃，等价于 allow_all_origins
```

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `transport.websocket.addr` | string | 未设置 | WebSocket 监听地址（如 `:9080`）。**必填**（校验规则 1） |
| `transport.websocket.path` | string | 未设置 | 升级路径（如 `/ws`）。**必填且二进制不套用默认值**：`pkg/transport/ws` 包内虽有默认路径，但启动接线直接透传配置值，空路径会在路由注册时 panic，因此必须显式配置 |
| `transport.websocket.read_timeout` | string | 见说明 | 单次读操作的截止时间。规则：**心跳开启时**（`idle_timeout>0` 或 `ping_interval>0`）取 `max(2 × idle_timeout, 3 × ping_interval, 10s)` 作为下限，显式配置值可以放大但不能小于该下限；**心跳完全禁用时**（两者均 `0s`）取 60s，显式配置完全覆盖。每次成功读消息后重置 |
| `transport.websocket.write_timeout` | string | 未设置（传输默认 10s） | 单次写操作的截止时间。显式 `<= 0` 被 Validate 拒绝 |
| `transport.websocket.allow_all_origins` | bool | `false` | 允许任意 Origin 的跨域连接（仅限开发环境） |
| `transport.websocket.allowed_origins` | string[] | 未设置 | Origin 白名单，对 `Origin` 请求头做精确匹配。仅在 `allow_all_origins` 与 `check_origin` 均为 false 时生效 |
| `transport.websocket.compression` | bool | `false` | 启用 WebSocket 扩展 permessage-deflate。启用后解压输出同样受 `max_message_size` 约束（防解压炸弹） |
| `transport.websocket.tls.cert_file` / `.key_file` | string | 未设置 | 启用 HTTPS/WSS，二者必须成对设置（校验规则 5） |
| `transport.websocket.check_origin` | bool | `false` | **已废弃**：为 true 时行为与 `allow_all_origins: true` 完全一致，仅作向后兼容保留 |

### Origin 校验行为

启动接线的判定顺序：

1. `allow_all_origins` 或 `check_origin` 任一为 true → 放行一切来源；
2. 否则若 `allowed_origins` 非空 → 仅当 `Origin` 头与白名单精确匹配时放行（未携带 `Origin` 头的请求也会被拒——白名单模式不做"无 Origin 放行"特判）；
3. 否则交由 gorilla/websocket 的默认同源检查处理（无 `Origin` 头或与 `Host` 同源的请求放行，其余拒绝）。

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
| `transport.grpc.addr` | string | 未设置 | 客户端面向的 gRPC 流式监听地址（`clientpb.MessageLoopService`，双向流）。**必填**（校验规则 1） |
| `transport.grpc.write_timeout` | string | 10s | 流式下行写超时。显式设置后注入 handler；未设置时由传输层默认（10s）兜底。显式 `<= 0` 被 Validate 拒绝 |
| `transport.grpc.tls.cert_file` / `.key_file` | string | 未设置 | 为 gRPC 监听器启用 TLS，二者必须成对设置（校验规则 5） |

该监听器的 `MaxRecvMsgSize` 由 `server.limits.max_message_size` 统一决定，无需单独配置。

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
| `transport.quic.addr` | string | 空 | UDP 监听地址。可选：空值不启动 QUIC |
| `transport.quic.write_timeout` | string | `10s` | 单次写帧截止时间。显式 `<= 0` 被 Validate 拒绝 |
| `transport.quic.read_timeout` | string | 见说明 | 单次读帧截止时间。规则与 WebSocket 相同：心跳开启时取 `max(2×idle, 3×ping, 10s)` 为下限，显式配置只能放大 |
| `transport.quic.insecure` | bool | `false` | 未配置 TLS 文件时生成进程内自签名证书（ECDSA P-256、24 小时有效、仅 localhost）。仅开发/测试，生产必须配置 `tls` |
| `transport.quic.tls.cert_file` / `.key_file` | string | 未设置 | QUIC 强制 TLS 1.3；二者必须成对。`addr` 非空时必须提供证书对或 `insecure: true` |

QUIC 会话是一条双向流上的长度前缀帧（4 字节大端长度 + payload）。TLS ALPN 协商编码：`messageloop+proto` 为二进制 protobuf，`messageloop+json` / `messageloop` 为 protojson。入站帧大小受 `server.limits.max_message_size` 约束。Go SDK 用 `DialQUIC(addr, opts...)` 连接。

**与心跳的联动**：QUIC 自身的会话保活由两层参数控制——传输默认 idle 超时 5 分钟、keepalive 周期 15s、最大入站流数 8；心跳配置非零时，启动接线把 `MaxIdleTimeout` 覆盖为 `max(5min, 2 × idle_timeout)`、`KeepAlivePeriod` 覆盖为 `ping_interval`，使 QUIC 层的连接存活判定与消息层心跳一致。

## transport.kcp 节

```yaml
transport:
  kcp:
    addr: ":29900"          # 空 = 不启动 KCP 监听器
    write_timeout: "10s"
    read_timeout: "60s"
    data_shards: 0          # FEC 分片数；0/0 = 关闭 FEC（默认）
    parity_shards: 0
    insecure: false         # 仅开发：生成临时自签名证书
    # tls:
    #   cert_file: "./certs/server.crt"
    #   key_file: "./certs/server.key"
```

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `transport.kcp.addr` | string | 空 | UDP 监听地址。可选：空值不启动 KCP |
| `transport.kcp.write_timeout` | string | `10s` | 单次写帧截止时间。显式 `<= 0` 被 Validate 拒绝 |
| `transport.kcp.read_timeout` | string | 见说明 | 单次读帧截止时间。规则与 WebSocket/QUIC 相同：心跳开启时取 `max(2×idle, 3×ping, 10s)` 为下限。KCP 自身无 keepalive，静默对端由该截止时间剔除 |
| `transport.kcp.data_shards` / `.parity_shards` | int | `0` / `0` | Reed-Solomon 前向纠错分片数（kcp-go FEC）。两个值都必须 ≥0；`parity_shards > 0` 要求 `data_shards > 0`；客户端必须用相同分片数拨号 |
| `transport.kcp.insecure` | bool | `false` | 未配置 TLS 文件时生成进程内自签名证书（仅开发/测试）。生产必须配置 `tls` |
| `transport.kcp.tls.cert_file` / `.key_file` | string | 未设置 | KCP 自身不加密，必须叠加 TLS（TLS 1.2 起）；二者必须成对。`addr` 非空时必须提供证书对或 `insecure: true` |

KCP 会话是一条 TLS 加密的 KCP 流上的长度前缀帧（4 字节大端长度 + payload），帧格式与 QUIC 传输完全一致，ALPN 编码协商也相同。服务端对接受的会话统一启用流模式（`SetStreamMode(true)`，解除单次写 255 分片上限）、实时模式（`SetNoDelay(1, 10, 2, 1)`）与 256/256 窗口；客户端（SDK `DialKCP`）自动对齐这些参数。Go SDK 用 `DialKCP(addr, dataShards, parityShards, opts...)` 连接。

### 启动要求

`cmd/server/runtime.go` 在启动时无条件预绑定两个 gRPC 监听器（`transport.grpc.addr` 与 `server.grpc_admin.addr`），任一失败（地址为空、端口被占用等）都会中止启动——与 `Validate()` 的必填校验一致。预绑定失败时先启动的监听器会被释放，不会泄漏端口。

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
| `broker.type` | string | `memory` | broker 实现。空等价于 `memory`；`redis` 要求 `broker.redis.addr` 非空，其他值报 `unknown broker.type: %q (expected "memory" or "redis")` |

### memory broker

- 进程内实现，无任何 YAML 配置项（`MemoryBrokerOptions` 仅在代码中可用）。
- 每个频道维护固定容量环形缓冲作为历史记录，容量 256 条；满则覆盖最旧条目。
- 投递按 64 个分片 worker 串行化（同频道保序），队列满时对发布方背压。
- 适合单节点开发与测试；需要历史持久化或多节点时使用 Redis broker。

### broker.redis 字段

默认值全部来自 `pkg/redisbroker/options.go`。

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `broker.redis.addr` | string | 未设置 | Redis 地址（`host:port`）。`broker.type: redis` 时必填。启动时 Ping 不通（5s 超时）则节点启动失败 |
| `broker.redis.password` | string | 未设置 | Redis 认证密码 |
| `broker.redis.db` | int | 0 | 逻辑数据库编号。多套环境建议用不同 `db` 隔离 |
| `broker.redis.pool_size` | int | 10 | go-redis 连接池大小 |
| `broker.redis.min_idle_conns` | int | 5 | 连接池保持的最小空闲连接数 |
| `broker.redis.max_retries` | int | 3 | 网络错误时的最大重试次数 |
| `broker.redis.dial_timeout` | string | `5s` | 建立连接超时 |
| `broker.redis.read_timeout` | string | `3s` | 读操作超时 |
| `broker.redis.write_timeout` | string | `3s` | 写操作超时 |
| `broker.redis.stream_max_length` | int64 | 10000 | 每条频道 Stream 的最大条目数（`XADD` 的 `MAXLEN`） |
| `broker.redis.stream_approximate` | bool | 代码内 `true` | 是否使用 Stream `MAXLEN ~` 近似截断。**只实现了近似截断：显式 false 或省略该字段（反序列化为 false）都会被 `Validate()` 拒绝**，必须显式设为 `true` |
| `broker.redis.history_ttl` | string | `24h` | 频道 Stream 的空闲过期时间（每条发布后刷新） |
| `broker.redis.consumer_group` | string | 未设置 | **未实现**：配置非空会被 `Validate()` 直接拒绝。该字段仅存在于结构体声明，整个代码库没有任何读取点，应移除 |

以上时长字段（`dial_timeout` / `read_timeout` / `write_timeout` / `history_ttl`）不在 `Validate()` 校验范围内，解析失败会被静默忽略并保留默认值，因此无效时长不会导致启动失败。

### 发布路径与 Redis 键布局

单次发布由一条 Lua 脚本原子完成：`INCR` 每频道稠密 seq 计数键 → `XADD` 写入历史 Stream（`MAXLEN ~` 截断）→ 刷新 seq 键与 stream 键的 TTL；随后维护 `first_retained` 标记键并 `PUBLISH` 做实时分发。发布全路径共用一个 5 秒超时上下文；实时分发失败只记日志、不回滚——发布成功以日志写入为准，调用方仍拿到 offset。策略级 `HistorySize`/`HistoryTTL` 可在每次发布时覆盖全局值。

Redis broker 使用以下键前缀（pkg/redisbroker/options.go），同一 Redis 实例内与业务数据共存时可按前缀隔离：

| 前缀 / 键 | 用途 |
| --- | --- |
| `ml2:stream:<channel>` | 历史 Stream（Redis Streams） |
| `ml2:stream:seq:<channel>` | 每频道稠密 seq 计数器 |
| `ml2:stream:retained:<channel>` | `first_retained` 标记（gap 检测用） |
| `ml2:pubsub:<channel>` | 实时投递（Redis Pub/Sub） |
| `ml2:pubsub:__live__` | pub/sub 连接的控制频道 |
| `ml2:presence:` | 在线状态（成员键、频道索引、occupancy gen） |
| `ml2:broker:epoch` | Redis broker 代际（集群共享，SETNX 写入） |
| `ml2:cluster:*` | 集群控制面（节点/会话租约、快照、命令总线、频道投影、user 索引等，见[《分布式集群指南》](04-cluster.md)） |

## cluster 节

```yaml
cluster:
  enabled: false       # 启用分布式控制面
  node_id: node-a      # 逻辑节点唯一 ID
  backend: redis       # 当前仅 redis 有实际实现
  hmac_key_file: /path/to/cluster-hmac.key  # 命令总线 HMAC 密钥（与 hmac_key 二选一）
```

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `cluster.enabled` | bool | `false` | 启用 Redis 支撑的分布式控制面。启用时要求 `broker.type: redis`（校验规则 8） |
| `cluster.node_id` | string | 未设置 | 逻辑节点标识，集群内必须唯一；启用时必填（`ClusterOptions.Normalize` 检查） |
| `cluster.backend` | string | `redis` | 控制面后端。为空时默认 `redis`；接受 `redis` / `memory` / `noop`，其他值报错。仅 `redis` 在二进制中接入实际组件（会话目录、命令总线、查询投影、节点租约、投影修复） |
| `cluster.hmac_key` | string | 未设置 | 内联 HMAC-SHA256 命令总线密钥，至少 32 字节；与 `hmac_key_file` 二选一 |
| `cluster.hmac_key_file` | string | 未设置 | 密钥文件路径；文件尾单个换行（LF 或 CRLF）会被裁剪 |

**HMAC 密钥校验分两层**：

1. `Validate()`（校验规则 8）：两源同设、均未设置、内联 `hmac_key` 长度不足 32 字节（报错文案 `cluster.hmac_key must be at least 32 bytes`）都在配置校验期拒绝。
2. 启动接线 `ClusterConfig.ResolveHMACKey()`：读取 `hmac_key_file`（读失败报 `read cluster.hmac_key_file: ...`）并再次检查长度（报错文案 `cluster hmac key must be at least 32 bytes`），失败即拒绝启动。

集群内所有节点必须共用同一密钥；密钥不会写入任何 Redis 键、日志或 metrics 标签。

启用 Redis 集群时，控制面组件与 broker 共用同一个 `broker.redis` 配置，并使用 `ml2:cluster:` 前缀的键；启动前还要对该节点的 `node_epoch` 计数器发号一次（`INCR`，失败拒绝启动），得到进程实例标识 IncarnationID。集群模式下所有 `messageloop_*` 指标自动带 `node_id` 标签；`/health` 端点会附带 Redis 连通性探测（2s 超时）。

拓扑、会话迁移与故障转移语义见[《分布式集群指南》](04-cluster.md)。

## proxy 节

```yaml
proxy:
  - name: example-grpc          # 唯一标识
    endpoint: "127.0.0.1:10091" # gRPC: host:port；HTTP: 完整 URL
    timeout: "30s"              # 代理级超时，默认 30s
    # admin_auth: true          # 指派为管理 API Key 校验器（全配置唯一，G3）
    grpc:                       # gRPC 后端配置（二选一）
      insecure: true
      # tls:
      #   server_name: backend.example.com
      #   insecure_skip_verify: false
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
| `name` | string | 未设置 | 代理标识，用于日志与排障；`admin_auth: true` 时必填（按名解析校验器实例） |
| `endpoint` | string | 未设置 | 后端地址。gRPC 后端为 `host:port`；HTTP 后端为完整 URL（`http://` / `https://` 前缀） |
| `admin_auth` | bool | `false` | 将该代理指派为**管理 API 的 API Key 校验器**（proxy 契约的 `AuthenticateAdmin` RPC）。全配置至多一个条目可指派，两处指派即 Validate 错误（G3：显式布尔指派，不用方法名路由——**不存在 `$authenticate_admin`** 之类的管理面方法名）。指派与 `routes` 解耦：admin 认证不走 channel/method glob 匹配，客户端面 `$authenticate` 等钩子不受影响。指派语义与校验链详见[《管理 API 参考》](03-admin-api.md) |
| `timeout` | string | `30s` | 代理级请求超时。0 或未设置取默认 30s；解析失败在启动时报错。与 `server.rpc_timeout` 的关系见 [三层超时](#三层超时) |
| `http` | 对象 | 未设置 | HTTP 代理配置；`headers`（map[string]string）为附加请求头（默认注入 `Content-Type: application/json`）；`tls.insecure_skip_verify` 关闭证书校验；`tls.server_name` 设置 SNI。响应体上限 4 MiB |
| `grpc` | 对象 | 未设置 | gRPC 代理配置；`insecure: true` 使用明文连接，否则用系统 CA 池做 TLS；`tls.server_name` 覆盖 SNI；`tls.insecure_skip_verify` 关闭证书校验。gRPC 响应上限固定为 4 MB |
| `routes` | 数组 | 未设置 | 路由规则，见下 |

### 路由语义

每条路由由 `channel` + `method` 两个 glob 模式组成（gobwas/glob 语法），必须同时匹配才命中：

- `channel` 模式匹配目标频道，如 `chat.*`、`rpc.**`；
- `method` 模式匹配 RPC 方法名，`"*"` 匹配一切方法；
- **按配置顺序求值，首个匹配的代理生效**；
- 无任何路由匹配时 `ProxyRPC` 返回 `ErrNoProxyFound`，客户端收到软失败 `NO_PROXY` 错误信封（`type=request_error`，不回显请求体）；代理调用本身失败则返回 `PROXY_ERROR` 错误信封。

除路由外，代理类型选择规则：显式配置 `grpc` 段 → gRPC 代理；显式配置 `http` 段 → HTTP 代理；两者皆无时按 `endpoint` 前缀判断（`http://` / `https://` → HTTP，否则 gRPC）。

### 代理钩子

`Proxy` 接口（proxy/proxy.go）定义了 8 个钩子，分别在以下时机调用：

| 钩子 | 触发时机 | 路由匹配方式 |
| --- | --- | --- |
| `Authenticate` | 客户端携带 token 发起连接时 | 固定方法名 `$authenticate`（`SystemMethodAuthenticate`），频道为空；即路由需写成 `channel: "*", method: "$authenticate"` 才能接到鉴权。返回的 `UserInfo`（含 ID 与可选 namespace）用于后续授权与命名空间 |
| `RPC` | 客户端 RPC 请求 | 请求的 channel + method |
| `SubscribeAcl` | 订阅/恢复订阅时的频道 ACL 检查 | channel + 固定方法名 `"subscribe"` |
| `PublishAcl` | 发布前的频道 ACL 检查 | channel + 固定方法名 `"publish"` |
| `OnConnected` | 连接已加入 Hub 后 | 与连接同名的代理（即鉴权所用代理）；错误被忽略 |
| `OnSubscribed` | 订阅成功后 | 同上；错误被忽略 |
| `OnUnsubscribed` | 取消订阅后 | 同上；错误被忽略 |
| `OnDisconnected` | 客户端断开时 | 同上；错误被忽略 |
| `AuthenticateAdmin` | 管理 API 携带 API Key 凭证的一元请求 | **不走此路由表**：不按方法名/glob 匹配，唯一激活方式是 proxy 条目的 `admin_auth: true` 显式指派（全配置唯一，见上表）；静态 `auth_tokens` 凭证不经过 proxy |

**授权与代理的关系**：订阅/发布先过静态 `Authorizer.Decide`，再查代理——`SubscribeAcl` / `PublishAcl` 存在匹配路由时作为额外的门：代理拒绝只否决这一次请求，代理允许也不得跳过静态 deny。

### 三层超时

请求链路上存在三层超时，各自生效范围如下：

1. **请求级**：`server.rpc_timeout`（默认 30s）——仅作用于 `RPC` 转发，每个请求创建独立的 context 截止时间，超时返回 `RPC_TIMEOUT` 错误信封；
2. **代理级**：`proxy[].timeout`（默认 30s）——作用于 `Authenticate` / `SubscribeAcl` / `PublishAcl` / 生命周期通知等未携带 deadline 的调用（`withTimeout` 仅在 ctx 无截止时间时叠加）；HTTP 代理同时将其设为 `http.Client.Timeout`，因此 RPC 请求最迟也在该值内完成；
3. **传输级**：`transport.*.write_timeout`——下行消息写入对端的超时（各传输未配置时默认 10s）。

## 完整示例走查

以下逐段解读仓库根目录的 `config-example.yaml`（单节点 + Redis broker + 集群预留 + 一个 gRPC 代理的形态）：

```yaml
server:
  http:
    addr: "127.0.0.1:8080"
```

管理 HTTP 显式绑定回环地址（此处不写默认值也能工作，但显式声明更安全），对外仅暴露 `/health` 与 `/metrics`。如需给监控端点加鉴权，加一行 `auth_token`；绑定非回环地址且不设 token 会直接被 Validate 拒绝（G5 fail-closed）。

```yaml
  grpc_admin:
    addr: "127.0.0.1:9091"
    auth_tokens:
      - "change-me-admin-token-min-20-chars"
```

管理 gRPC 同样绑定回环地址。`addr` 必填，且非空时三种凭证路径（`auth_tokens`、`proxy[].admin_auth: true` 指派、`allow_insecure`）必须至少配置一种：只写 `addr` 会直接启动失败。每把 token ≥ 20 字符（Validate）；列表形态支持轮换无空窗（加新 → 滚动重启 → 删旧）。注释掉的 `admin_auth_cache_ttl` 是 API Key 正缓存 TTL（默认 30s，需要 `proxy[].admin_auth` 指派才有意义）。注释掉的 `capabilities` 块展示了 Admin 能力位闭集——省略时除 `pattern.global` 外全位，显式 `[]` 锁死 Admin 数据面。需要 TLS 时取消 `tls` 段注释并成对填写证书与私钥。注意：这个地址（连同 `transport.grpc.addr`）在启动时都会被无条件预绑定，必须可监听。

```yaml
  heartbeat:
    idle_timeout: "300s"
```

显式声明 300s 空闲断开——该值恰好等于回退默认值：留空或解析失败同样按 300s 生效，心跳无法通过留空禁用；`"0s"` 表示不做空闲断开（仅当 `ping_interval` 也为 0 时心跳管理器才完全不启动）。非 0 的 `idle_timeout` / `ping_interval` / `ping_timeout` 必须 ≥1s。注释掉的 `ping_interval` / `ping_timeout` 是服务端主动探测：打开后未应答即 3511，**必须同时升级 SDK 到能回 Pong 的版本**。集群 session lease 按公式随心跳缩短，默认配置仍为 600s（见 [04-cluster.md](04-cluster.md)）。`rpc_timeout: "30s"` 与代理默认值相同，可省略。

```yaml
  namespace: "dev"
```

静态命名空间兜底：鉴权代理响应未携带 namespace 时使用；`require_auth` 关闭（本示例形态）时此字段必填。所有客户端可见频道都必须位于 `dev:` 之下。

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
    default: { history: true, presence: true, recover: true, survey: false }
    rules:
      - pattern: "chat.public.*"
        allow_subscribe: ["*"]
        allow_publish: ["alice", "bob"]
      - pattern: "chat.private.*"
        deny_all: true
```

授权表：`chat.public.*` 任何已认证用户可订阅、仅 `alice`/`bob` 可发布；`chat.private.*` 整体封锁。求值语义为「语言包含 + deny 不打洞」：例如存在 `secret.**` deny_all 时，再写一条 `secret.lobby` 允许 `alice` 也不能让 `alice` 订进去。`default` 块覆盖兜底 Effects；注释掉的 `game.tick.**`（transient_only）与 `csurvey.**`（开放客户端 survey）演示了规则级 Effects。代理 `SubscribeAcl` / `PublishAcl` 命中时作为额外门，不能越过静态 deny。

```yaml
transport:
  websocket:
    addr: ":9080"
    path: "/ws"
    allow_all_origins: true
    compression: true
  grpc:
    addr: ":9090"
```

WebSocket 监听 `:9080`，路径 `/ws`（必须显式配置，二进制不会套用默认路径）。`allow_all_origins: true` 是典型的开发期配置，生产应改为 `allowed_origins` 白名单或依赖默认同源检查。`compression: true` 启用 permessage-deflate（解压输出同样受 64 KB 上限保护）。客户端 gRPC 监听 `:9090`。注释掉的 `quic:` / `kcp:` 段展示了可选 UDP 传输的形态——`insecure: true` 生成临时自签名证书，仅限开发。

```yaml
broker:
  type: redis
  redis:
    addr: 127.0.0.1:6379
    password: ""
    db: 10
    stream_approximate: true
```

Redis broker 连接本机 6379，选择数据库 10。`stream_approximate: true` 是必须显式写出的确认项（省略或 false 都会被 Validate 拒绝）。注释掉的字段（`pool_size` 等）展示的即默认值，无需显式写出。

```yaml
cluster:
  enabled: false
  node_id: node-a
  backend: redis
```

集群当前关闭。`enabled: true` 时要求 `broker.type: redis`（此处满足），`node_id` 必须填写并在集群内唯一，且必须恰好配置一个 HMAC 密钥源（注释掉的 `hmac_key_file` / `hmac_key` 二选一，密钥至少 32 字节，全集群共享）。`backend: redis` 是唯一在二进制中接入实际实现的取值。

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

注册名为 `example-grpc` 的 gRPC 代理，明文连接 `127.0.0.1:10091`，`channel: "*"` + `method: "*"` 匹配所有频道的所有方法。该代理同时承担 `$authenticate` 鉴权（`method: "*"` 覆盖了固定方法名）、`"subscribe"` / `"publish"` 的 ACL 裁决（作为静态 Authorizer 之外的额外门），以及 RPC 转发与四个连接生命周期通知。`timeout: 30s` 与默认一致。该代理未设 `admin_auth: true`，因此不承担管理 API 的 API Key 校验——需要时显式加上（全配置唯一指派，见 [proxy 节](#proxy-节) 字段表）。注释掉的 `example-http` 段展示了 HTTP 代理形态（完整 URL 端点、附加头、TLS 选项）。

## 多节点注意

部署多个节点时（如 `config-node1.yaml` / `config-node2.yaml`，二者已按此约定编写）：

- **共享同一套 Redis 设置**：集群控制面与 broker 共用 `broker.redis` 段，各节点必须指向同一 Redis 实例与数据库，才能共享会话目录、命令总线与查询投影。键空间通过 `ml2:` 前缀隔离，无需额外配置；
- **`cluster.node_id` 必须全局唯一**：它是节点租约、命令路由与会话所有权的标识，重复的 `node_id` 会导致租约冲突与命令投递错乱；
- **HMAC 密钥必须全集群一致**：命令总线验签用，任一节点密钥不同即无法通信；
- 各节点面向客户端的监听地址与 `server.http.addr` / `server.grpc_admin.addr` 应各不相同（负载均衡器对外暴露，节点间不直接互连）；
- 管理面与客户端面的端口分离语义见[《架构指南》](01-architecture.md)；完整的集群拓扑、会话迁移与故障恢复说明见[《分布式集群指南》](04-cluster.md)。
