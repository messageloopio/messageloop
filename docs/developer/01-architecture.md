# 架构指南

本文档描述 MessageLoop 服务器的总体架构与核心组件设计，面向希望理解或修改服务端代码的开发者。文中所有类型名、方法名、常量与行为均以仓库源码为准。

配套文档：[《配置参考》](02-configuration.md)、[《管理 API 参考》](03-admin-api.md)、[《分布式集群指南》](04-cluster.md)、[《可观测性指南》](05-observability.md)、[《开发指南》](06-development.md)、[《客户端协议参考》](../protocol.md) 与[《部署指南》](../deployment.md)。

## 1. 概述

MessageLoop 的核心设计目标可以归纳为四点：

- **传输无关的核心**：连接管理、订阅、消息路由等核心逻辑全部构建在 `Transport` 接口之上，WebSocket 与 gRPC 流只是可替换的传输实现，核心代码不感知具体传输。
- **可插拔的 Broker**：发布/订阅与历史存储通过 `Broker` 接口抽象，提供进程内内存实现（`memory`）与 Redis 实现（`redis`），二者在接口层面完全等价。
- **分片并发模型**：连接注册表与订阅注册表各自分为 64 个分片，订阅变更用 16384 把通道级锁串行化，减少全局锁竞争。
- **会话感知的连接管理**：连接以服务端生成的会话 ID（`session ID`）为标识，支持断线恢复（resume）、新连接接管旧会话（takeover）与集群内跨节点恢复。

## 2. 总体布局

```
客户端 ──► WebSocket 监听器 (transport.websocket.addr) ──┐
客户端 ──► gRPC 流监听器  (transport.grpc.addr)        ──┤
管理工具 ─► gRPC 管理 API  (server.grpc_admin.addr)     ──┼─► Node（中央协调者）
运维系统 ─► HTTP 健康/指标  (server.http.addr)          ──┘
                                                              │
         ┌──────────────┬───────────────┬─────────────────────┼───────────────┬──────────────┬──────────────┐
         ▼              ▼               ▼                     ▼               ▼              ▼              ▼
      ┌──────┐      ┌────────┐      ┌──────────┐        ┌─────────┐      ┌────────┐     ┌─────────┐    ┌─────────┐
      │ Hub  │      │ Broker │      │ Presence │        │  ACL    │      │ Proxy  │     │ Cluster │    │ Metrics │
      │(连接/ │      │(发布/  │      │ (在线   │        │ (访问   │      │ (RPC   │     │ (集群   │    │ (Prom-  │
      │ 订阅  │      │ 历史)  │      │  状态)  │        │  控制)  │      │  转发) │     │  控制面)│    │ etheus) │
      │ 注册表)│      └────────┘      └──────────┘        └─────────┘      └────────┘     └─────────┘    └─────────┘
      └──────┘        │  │                                  ▲                  ▲
                       ▼  ▼                                 │                  │
              ┌─────────┴───────┐                 ┌─────────┴──────┐   ┌──────┴────────┐
              │memory / redis  │                 │ 内置规则（回退） │   │ HTTP / gRPC   │
              │(Redis Streams +│                 └─────────────────┘   │ 后端服务       │
              │ Pub/Sub)       │                                       └───────────────┘
              └─────────────────┘
```

各组件职责：

| 组件 | 文件 | 职责 |
| --- | --- | --- |
| `Node` | node.go | 中央协调者，持有并装配所有子系统，对外暴露发布、订阅、Survey、代理等入口 |
| `Hub` | hub.go | 连接与会话注册表（64 分片），通道订阅注册表（64 分片），通配符订阅匹配，每用户连接数限制 |
| `Client` | client.go | 单条连接的生命周期与消息处理：鉴权、resume、限制执行、入站消息路由、写路径 |
| `Broker` | broker.go、broker_memory.go、pkg/redisbroker/ | 发布/订阅与历史存储；内存实现与 Redis 实现 |
| `Presence` | presence.go、presence_event.go、pkg/redisbroker/presence_redis.go | 频道内在线成员追踪与 join/leave 事件分发 |
| `Survey` | survey.go、node.go | 向频道订阅者广播请求并带超时收集响应 |
| `ACL` | acl.go | 基于频道 glob 模式与用户白名单的访问控制 |
| `Proxy` | proxy/ | RPC 转发与鉴权/ACL/生命周期钩子的后端集成 |
| `Cluster` | cluster.go、cluster_*.go | 可选的 Redis 支撑分布式控制面（详见[《分布式集群指南》](04-cluster.md)） |
| `Metrics` | metrics.go | Prometheus 指标收集（详见[《可观测性指南》](05-observability.md)） |

## 3. 核心组件

### 3.1 Node（node.go）

`Node` 是运行时装配根：它持有 `hub`、`broker`、`presence`、`cluster`、`proxy`、`acl`、`metrics`、`surveys` 等全部子系统，并通过一组 setter 方法注入实现。

关键方法与行为：

| 方法 | 说明 |
| --- | --- |
| `NewNode(cfg *config.Server)` | 构造默认装配：`newHub(0, MaxConnectionsPerUser)`、`NewMemoryBroker`、`NewMemoryPresenceStore`，从配置构建内置 ACL 与心跳管理器 |
| `Run(ctx)` | 先启动集群（若启用），再以 `go n.broker.Start(ctx, handler)` 启动 broker，handler 即 `n.hub.broadcastPublication`；若 broker 实现 `Ready()` 则等待其就绪 |
| `Shutdown()` | 以 `DisconnectForceNoReconnect` 排空全部连接，受 `DefaultShutdownTimeout`（10s）约束，然后关闭集群 |
| `AddClient(c)` | 注册连接（超限返回 `DisconnectConnectionLimit`），集群模式下同步会话状态 |
| `AddSubscription` / `RemoveSubscription` | 通过订阅 Saga 提交订阅变更（见下） |
| `Publish(ch, payload, isText)` | 经 broker 发布并返回 offset |
| `PublishTransient` | 仅实时投递、不写历史（presence 事件用） |
| `Survey(ctx, channel, payload, timeout)` | 本地收集 + 集群广播汇总 |
| `SetupProxy` / `FindProxy` / `ProxyRPC` | 代理装配与 RPC 转发入口 |
| `MaxMessageSize()` | 入站消息上限，配置为 0 时取 `DefaultMaxMessageSize`（64 KB） |

**订阅 Saga**：`AddSubscription` 与 `RemoveSubscription` 在每通道锁（`subLock`，16384 分片，见 §8）内串行执行，由 `runSubSaga`（subscription_saga.go）分步提交：hub 订阅登记 → 客户端频道跟踪 → broker 订阅计数（仅首个订阅者真正 `Subscribe`）→ 集群会话/频道状态同步。任一步失败时按逆序回滚已执行的步骤。

**启动顺序**（cmd/server/main.go + cmd/server/runtime.go）：

```
加载配置并 Validate
  └─► NewNode(&cfg.Server)
  └─► NewMetrics(registry); node.SetMetrics
  └─► setupCluster: 构造 Cluster 并装配 Redis 会话目录/命令总线/查询存储/租约/投影修复/Presence
  └─► newBroker: 按 broker.type 选 memory/redis; node.SetBroker
  └─► 若 broker 支持 Ping，注册为健康探针 (SetHealthCheck)
  └─► setupProxy: 从配置构造 HTTP/gRPC 代理并注册路由
  └─► prepareGRPCServers: 预绑定客户端 gRPC 与管理 gRPC 两个监听器（失败则释放已绑定的）
  └─► newWebSocketServer / newAdminServer
  └─► app.OnStart(node.Run)   // broker 就绪后开始服务
  └─► app.OnStop(node.Shutdown)
```

`prepareGRPCServers`（runtime.go）在 `node.Run` 之前预绑定监听器，保证端口在服务启动前即被占用，避免启动窗口期端口被抢占。

### 3.2 Hub（hub.go）

`Hub` 维护三类注册表：

- `sessions map[string]*Client`：会话 ID → 连接，受 `hub.mu` 保护，用于会话查找与 resume。
- `connShards [64]*connShard`：每个分片内含 `clients`（会话 ID → 连接）与 `users`（用户 ID → 会话集合），按 `index(userID)` 分片。
- `subShards [64]*subShard`：每个分片内含 `subs`（频道 → 会话 ID → `Subscriber`），按 `index(channel)` 分片。

关键方法与行为：

| 方法 | 说明 |
| --- | --- |
| `index(s, n)` | FNV-64a 哈希取模分片 |
| `addWithLimit` | 在 `connShard` 内注册连接；`maxConnsPerUser > 0` 且用户连接数已满时返回 `DisconnectConnectionLimit` |
| `addSub` / `removeSub` | 通配符订阅走 `wcSubs`（键为 `sessionID:channel`）+ `matcher`，精确订阅走 `subShard` |
| `broadcastPublication` | 合并精确与通配符订阅者（按会话 ID 去重，保证同一客户端只收到一次），小扇出（≤8）串行发送，大扇出用受 `broadcastParallelLimit`（64）限流的并发发送 |
| `LookupSession` / `LookupSubscriber` | 会话与订阅查找 |
| `ReplaceSession` | resume 时原子替换会话指向的新 `Client`，并重写全部订阅分片与通配符订阅 |
| `RemoveSessionIfMatches` | 仅在注册的客户端与当前连接一致时移除会话，防止失败的旧连接把已接管/已恢复的会话驱逐出去 |
| `GetActiveChannels` | 管理 API 用的活跃频道列表（含订阅者数） |
| `DrainAll` | 并发向所有连接发送 `Close(disconnect)` 并等待关闭 |

消息 ID 规则：实时投递与恢复共用 `publicationID(channel, offset)`（`"频道-offset"`），客户端据此去重；瞬时事件（offset 为 0）回退为随机 UUID，避免同一频道所有瞬时事件共享同一个 ID。

### 3.3 Client（client.go）

`Client` 代表一条连接，状态机为 `statusConnecting → statusConnected → statusClosed`。

**连接生命周期（`handleConnect`）**：

1. 若已鉴权再发 Connect，返回 `DisconnectBadRequest`。
2. 客户端可携带 `SessionId` 请求恢复；会话 ID 在鉴权之前就写入（鉴权代理需要它）。
3. 鉴权：若带 `Token`，查找方法为 `$authenticate`（`SystemMethodAuthenticate`）的代理并调用 `Authenticate`；`requireAuth` 开启但无代理可验证 token 时拒绝（`DisconnectInvalidToken`）。
4. 恢复：本地会话存在则复制其状态（频道、用户、租约版本）、`closeQuiet` 旧连接（不触发 presence 清理）、`ReplaceSession` 接管；本地不存在则尝试跨节点恢复（`resumeRemoteSession`）。未鉴权连接不能驱逐仍被服务的会话。
5. `AddClient` 注册 + `MarkMetricsCharged`，集群模式下同步会话状态。
6. 通知代理 `OnConnected`。
7. 处理 Connect 携带的订阅列表：先做订阅数上限检查（超限 `DisconnectChannelLimit`），逐频道 ACL 检查，`AddSubscription` + presence 登记 + 异步发布 join 事件。
8. 消息恢复：`sub.Recover` 时以 `sub.Offset+1` 为起点调 `broker.History`；若 broker 有 epoch（`Epoch()`）且与客户端携带的 `sub.Epoch` 不一致（或客户端未携带），从 0（开头）恢复；恢复总量受 `MaxRecoveredPublications`（1000）封顶。全部结果随 `Connected` 信封返回（含 `Resumed` 与 `Epoch` 字段）。

**入站消息路由（`handleMessage`）**：

| 信封类型 | 处理函数 | 行为要点 |
| --- | --- | --- |
| `Connect` | `handleConnect` | 鉴权、恢复、初始订阅与恢复（见上） |
| `Publish` | `handlePublish` | 未鉴权 → `DisconnectInvalidToken`；限速 → `RATE_LIMITED` 错误；代理 `PublishAcl` 或内置 ACL；Payload 的 Json/Binary/Text 变体统一转字节；成功回 `PublishAck`（含 broker 分配的 offset） |
| `Subscribe` | `handleSubscribe` | 订阅数上限检查、逐频道 ACL、Saga 提交、presence 登记 + join 事件、代理 `OnSubscribed` 通知，回 `SubscribeAck` |
| `RpcRequest` | `handleRPC` | 经 `node.ProxyRPC` 转发（详见 §3.7）；超时回 `RPC_TIMEOUT`；无匹配代理时回显请求（echo）；代理错误回 `PROXY_ERROR` |
| `Unsubscribe` | `handleUnsubscribe` | 移除订阅、presence 清理 + leave 事件、代理 `OnUnsubscribed` 通知 |
| `Ping` | `handlePing` | 刷新活动时间，回 `Pong`；presence/集群状态刷新被节流（`pingClusterRefreshInterval` 10s，CAS 保证单次） |
| `SubRefresh` | `handleSubRefresh` | 重新校验订阅 ACL，失败的频道被撤销并发布 leave |
| `SurveyRequest` | `handleSurvey` | 记录请求 ID，默认回显请求 Payload 作为应答 |
| `SurveyReply` | `handleSurveyReply` | 校验请求 ID 后写入对应 Survey（`node.AddSurveyResponse`） |

**限制执行**：

- 订阅数：`limits.MaxSubscriptionsPerClient`（含继承自恢复会话的频道），超限 `DisconnectChannelLimit`。
- 发布速率：`limits.MaxPublishesPerSecond` 构造 `rate.Limiter`，超限回 `RATE_LIMITED` 错误信封（不断连）。
- 消息大小：`node.MaxMessageSize()`（默认 64 KB），WebSocket 端经 `SetReadLimit` 强制，gRPC 端经 `MaxRecvMsgSize` 强制，两个传输读取同一入口保证一致。

**写路径（`write`）**：从 `sync.Pool`（pool.go，初始容量 4096）取缓冲 → `marshaler.MarshalAppend` 序列化 → `transport.Write`；写入失败则异步 `close(DisconnectSlowConsumer)`。gRPC 传输会对缓冲做拷贝后再交给 worker，因为池化缓冲在 `Write` 返回后可能被复用。

**断开（`close`）**：标记关闭 → 取消心跳 → 并发（≤16）移除全部订阅 → presence 清理 + 逐个发布 leave 事件 → `RemoveSessionIfMatches` 后删除集群会话状态 → 递减连接指标 → 通知代理 `OnDisconnected` → `transport.Close(disconnect)`。

### 3.4 Broker（broker.go、broker_memory.go、pkg/redisbroker/）

`Broker` 接口（broker.go）：

| 方法 | 语义 |
| --- | --- |
| `Start(ctx, handler)` | 初始化并持续处理事件直到 ctx 取消；`handler` 接收每条发布（goroutine 中调用） |
| `Subscribe(ch)` / `Unsubscribe(ch)` | 注册/注销节点对频道的兴趣；仅在首个/末个本地订阅者时由 Node 调用 |
| `Publish(ch, payload, isText)` | 发布并返回该发布分配到的 offset（历史被禁用时为 0） |
| `PublishTransient(ch, payload, isText)` | 仅实时投递，不写历史，offset 恒为 0 |
| `History(ch, sinceOffset, limit)` | 返回 offset ≥ sinceOffset 的发布；`limit <= 0` 时以 `DefaultHistoryLimit`（1000）为上限 |

`Publication` 携带 `Channel`、`Offset`、`Epoch`、`Payload`、`IsText`、`Time`。

**内存实现（broker_memory.go）**：每频道一个固定容量环形缓冲（`defaultMemoryHistorySize`，256），`nextOff` 从 1 起按发布递增；缓冲写满后覆盖最旧条目。频道历史在仍有订阅者或仍有条目时被保留，最后一个订阅者离开且历史为空时回收，保证断开重连的恢复仍然可用。`Start` 仅登记 handler 并阻塞到 ctx 取消，`Ready()` 在 handler 注册后关闭。每次 `Start` 生成的实例带随机 `Epoch`。

**Redis 实现（pkg/redisbroker/redis.go）**：发布先 `XAdd` 写入 Redis Stream（前缀 `ml:stream:`，`StreamMaxLength` 默认 10000 条、`HistoryTTL` 默认 24h），从 Stream ID 解析出 offset，再经 Redis Pub/Sub（前缀 `ml:pubsub:`）实时分发；消费者以 `PSubscribe(ml:pubsub:*)` 模式订阅，仅处理本节点登记过兴趣的频道，断线以指数退避（1s 起、上限 30s）重连。`History` 用 `XRangeN` 以包含起始 ID（`"ts-seq"`，`streamStartID`）读取，offset 编码为 `ts<<20|seq`（Redis Stream ID 的毫秒时间戳与序列号拼入 uint64），与内存实现同为 `offset >= sinceOffset` 语义。

**offset + epoch 语义**：offset 是频道内的单调序号（内存实现从 1 起，Redis 实现由 Stream ID 编码），客户端用它做断线续读；epoch 用于判断 offset 是否仍属于当前 broker 代际——epoch 不匹配（或客户端未携带）时视为 offset 无效，从历史开头恢复。两种实现的 epoch 来源不同：**内存 broker** 每进程实例启动时生成随机 UUID（`broker_memory.go:33`），重启即失效；**Redis broker** 的 epoch 存于固定键 `ml:broker:epoch`，首个启动节点经 `SETNX` 写入随机 UUID，集群共享、跨重启持久（`pkg/redisbroker/redis.go` 的 `initEpoch`），因此 Redis 部署下 epoch 校验跨节点、跨重启均可通过（详见[《分布式集群指南》](04-cluster.md) 第 4.4 节）。

### 3.5 Presence（presence.go、presence_event.go）

`PresenceStore` 接口与 `Broker` 分离，可独立替换：

- `Add(ctx, ch, info)`：订阅时登记，也用于长连接的心跳刷新；
- `Remove(ctx, ch, clientID)`：退订与断开时移除；
- `Get(ctx, ch)`：返回频道内全部在线成员。

`PresenceInfo` 含 `ClientID`、`UserID`、`ConnectedAt`。默认实现为 `NewMemoryPresenceStore`（进程内 map）；集群模式用 Redis 实现（pkg/redisbroker/presence_redis.go）：每（频道, 客户端）一个带 TTL 的键（`PresenceTTL` 默认 60s）+ 每频道一个集合索引，`Get` 时清理过期成员。

join/leave 事件经伴生频道分发：`presenceChannel(ch) = ch + "/__presence"`。`PublishPresenceJoin`/`PublishPresenceLeave` 以 JSON 序列化 `PresenceEvent`（`__type: "presence"`、`action: join|leave`、`channel`、`client_id`、`user_id`、`timestamp`），并走 `PublishTransient`——事件只实时投递、永不进入 broker 历史，因此不会混入恢复消息流。

### 3.6 Survey（survey.go、node.go）

`Node.Survey` 流程：

1. 取频道全部精确 + 通配符订阅者（`GetMatchingSubscribers`）；
2. 创建 `Survey`（含 `responseCh` 缓冲 100）并登记所有被问询会话为 `expected`（其他会话的应答被视为伪造并丢弃）；
3. 注册到 `n.surveys`（容量上限 `maxActiveSurveys` = 1000，防无界增长）；
4. 并发向每个订阅者发送 `SurveyRequest`，单次发送受 `surveySendTimeout`（10s）约束——传输阻塞的慢消费者被记为错误应答而不是挂起整个 Survey；
5. `survey.Wait(ctx)` 收集应答直到超时或 ctx 取消；`timeout <= 0` 时回退到 `defaultSurveyWaitTimeout`（5s）；
6. 集群模式下经命令总线广播 `ClusterCommandSurvey`，汇总各节点结果并统一排序。

### 3.7 ACL（acl.go）

`ACLRule` 由三个维度组成：`ChannelPattern`（glob 模式，如 `chat.public.*`）、`AllowSubscribe`/`AllowPublish`（用户 ID 白名单，`"*"` 表示任意已认证用户）、`DenyAll`（整条规则封禁）。频道匹配用 Go 标准库 `path.Match`。

`CanSubscribe`/`CanPublish` 采用"最严格者优先"（worst-match-first）语义：

- 任一匹配规则设置 `DenyAll` → 直接拒绝（宽松规则无法绕过）；
- 否则由最后一条带白名单的匹配规则决定（确定性的 last-write-wins）；
- 没有规则匹配时默认放行。

**与代理 ACL 的优先级**：订阅/发布先查代理——`FindProxy(channel, "subscribe")` / `FindProxy(channel, "publish")` 命中则调用 `SubscribeAcl`/`PublishAcl`，以代理结论为准；内置规则仅在无代理匹配时作为回退生效（见 `checkSubscribeACL` 与 `handlePublish`）。

### 3.8 Proxy（proxy/proxy.go、router.go、http.go、grpc.go）

`Proxy` 接口提供两类能力：RPC 转发（`RPC`）与后端集成钩子（`Authenticate`、`SubscribeAcl`、`PublishAcl`、`OnConnected`、`OnSubscribed`、`OnUnsubscribed`、`OnDisconnected`），后者在客户端连接生命周期中被 Node/Client 调用。

`Router`（router.go）按添加顺序保存路由，`Match(channel, method)` 返回**首个** channel 与 method 同时匹配（gobwas/glob 编译）的路由；无匹配返回 `ErrNoProxyFound`。`Node.createProxy` 的选择规则：显式 `GRPC` 配置 → gRPC；显式 `HTTP` 或 Endpoint 以 `http://`/`https://` 开头 → HTTP；否则 gRPC。

- **HTTPProxy**（http.go）：JSON POST 到 Endpoint，`Content-Type: application/json`，支持自定义头与 TLS（`InsecureSkipVerify`/`ServerName`）。
- **GRPCProxy**（grpc.go）：连接 `proxypb.ProxyServiceClient`，接收消息上限 4 MB，支持 insecure/TLS 两种凭据。

**三层超时**：① 客户端级——`node.rpcTimeout`（`server.rpc_timeout`，默认 `proxy.DefaultRPCTimeout` 30s，见 client.go `handleRPC`）；② 代理级——每个 `ProxyConfig.Timeout`（默认同样 30s）；③ 传输级——HTTP client 超时 / gRPC 上下文 deadline。代理的 `withTimeout` 只在上下文没有 deadline 时才叠加自己的超时，嵌套时取最紧约束。

### 3.9 Metrics（metrics.go）

`Metrics` 注册一组 Prometheus 指标：连接/订阅/活跃频道 gauge、发布/投递计数、发布与 RPC 耗时直方图、投递失败计数，以及集群命令去重命中、命令超时、投影修复等集群指标。采集与暴露细节见《可观测性指南》。

## 4. 传输层

### 4.1 Transport 接口（transport.go）

```go
type Transport interface {
    Write([]byte) error
    WriteMany(...[]byte) error
    Close(Disconnect) error
    RemoteAddr() string
}
```

核心逻辑只与字节流交互，编码由 `Marshaler` 负责，传输不感知协议消息结构。

### 4.2 WebSocket（pkg/websocket/）

- `server.go`：`lynx.Service` 实现，挂载 `/ws` 路径（默认 `:9080`），支持 TLS 与 `ReadHeaderTimeout`（10s）。
- `handler.go`：升级后按子协议协商编码（见 §4.5），`NewClient(..., WithProtocol("ws"))` 创建会话；读循环解码 `InboundMessage` 后交给 `client.HandleMessage`；`SetReadLimit` 施加消息大小上限；读超时为 60s 或 2 × 心跳 idle 超时。
- `transport.go`：`writeMu` 串行化写入；关闭时发送 WebSocket close 帧（`Disconnect.Code` 直接作为 close code，code 0 回退为 1000）并等待对端确认。

### 4.3 gRPC 流（pkg/grpcstream/）

- `client_server.go`：`PrepareClientServer` 注册 `MessageLoopService`，每个客户端一条双向流（`handler.go` 的 `MessageLoop` 方法），固定使用 `ProtobufMarshaler`。
- `admin_server.go`：`PrepareAdminServer` 在独立监听器注册 `APIService`（管理 API），可选 Bearer Token 拦截器（常量时间比较防时序泄漏）。
- `server.go`：共享的 `prepareServer`——预绑定监听器、加载 TLS、施加 `ForceServerCodec(RawCodec)` 与 `MaxRecvMsgSize`，统一生命周期。
- `transport.go`：写入经单 worker goroutine 串行化（`sendCh` 缓冲 64，默认写超时 10s），入队前拷贝消息字节（调用方可能复用池化缓冲），关闭时先投递 `DISCONNECT_ERROR` 错误帧再退出 worker。

### 4.4 RawCodec（codec.go）

`RawCodec` 允许把**已序列化的 protobuf 字节**（`rawFrame`）直接发送，避免二次序列化；也兼容普通 `proto.Message`。其 `Name()` 返回 `"messageloop-proto"` 而不是默认的 `"proto"`，避免在进程级 codec 注册表中覆盖标准 proto codec，并采用每服务器 `ForceServerCodec` 注入。名称同时是内容子类型标签，须与 Go SDK 客户端（sdks/go/grpc.go）使用的 codec 名称一致。

### 4.5 Marshaler 与编码协商（shared/marshaler.go、marshaler.go）

`Marshaler` 接口提供 `Marshal`/`MarshalAppend`/`Unmarshal`/`Name`：

| 实现 | Name | 说明 |
| --- | --- | --- |
| `JSONMarshaler` | `json` | proto 消息走 protojson（`UseProtoNames`），其他走 `encoding/json` |
| `ProtobufMarshaler` | `proto` | 二进制 protobuf |
| `ProtoJSONMarshaler` | `json` | protojson 专用实现，根包默认回退项 |

WebSocket 端通过子协议协商：服务端宣告 `messageloop`、`messageloop+json`、`messageloop+proto` 三种子协议，按客户端请求中第一个包含 marshaler 名称（`json`/`proto`）的子协议选定；`messageloop+proto` 使用二进制帧，其余使用文本帧。gRPC 端固定 protobuf。

## 5. 主题匹配（pkg/topics/）

`Matcher` 接口（matcher.go）定义三个操作：

```go
type Matcher interface {
    Subscribe(topic string, sub Subscriber) (*Subscription, error)
    Unsubscribe(sub *Subscription)
    Lookup(topic string) []Subscriber
}
```

主题以 `.` 分隔层级，`*` 匹配**单个层级**（如 `chat.*` 匹配 `chat.general`，不匹配 `chat.rooms.1`）。

| 实现 | 文件 | 特点 |
| --- | --- | --- |
| `NewCSTrieMatcher` | cstrie.go | 基于 CAS 的无锁并发字典树（iNode/cNode/tNode 结构，原子指针切换），**Hub 默认使用** |
| `NewTrieMatcher` | trie.go | `sync.RWMutex` 保护的传统字典树 |
| `NewNaiveMatcher` | naive.go | 哈希表 + 查找时全表扫描比对 |
| `NewInvertedBitmapMatcher` | inverted_bitmap.go | 倒排位图实现 |
| `NewOptimizedInvertedBitmapMatcher` | optimized_inverted_bitmap.go | 位图实现的优化变体 |

Hub 的 `wcSubs` 记录每个通配符订阅（键 `sessionID:channel`），广播时先精确订阅再 `matcher.Lookup`，按会话 ID 去重。

## 6. 消息流走查

### (a) 连接与 resume / stale 接管

```
客户端 A              服务端                          客户端 B（同一 SessionId 重连）
  │                     │                                  │
  │── Connect ─────────►│                                  │
  │                     │── 鉴权：FindProxy("","$authenticate")
  │                     │── LookupSession(sessionId)
  │                     │── 旧会话存在：
  │                     │   复制旧状态 → 旧会话 closeQuiet
  │                     │   → Hub.ReplaceSession(sessionId, 新Client)
  │                     │── AddClient + MarkMetricsCharged
  │                     │── 逐频道 ACL → AddSubscription(saga)
  │                     │── broker.History(offset+1)（校验 Epoch）
  │◄── Connected ───────┤      （Resumed=true, Publications=恢复的消息）
  │                     │
  │   连接中断（网络）   │
  │                     │   失败的旧连接 close() 时:
  │                     │   RemoveSessionIfMatches(sessionId, 旧Client)
  │                     │   → 不匹配，不驱逐新会话（stale 保护）
```

### (b) 订阅 → 发布 → 投递（ack / offset）

```
发布客户端              Node                 Broker          订阅客户端
  │── Subscribe ──────►│                     │                  │
  │                    │── AddSubscription   │                  │
  │                    │   (hub.addSub →     │                  │
  │                    │    broker.Subscribe)│                  │
  │◄── SubscribeAck ───┤                     │                  │
  │── Publish ────────►│                     │                  │
  │                    │── Publish(ch,data) ─► (分配 offset)     │
  │                    │◄── offset ──────────┤                  │
  │◄── PublishAck ─────┤（含 offset）         │                  │
  │                    │── handler(ch,pub) ──► broadcastPublication
  │                    │                        │ 合并精确+通配符订阅者
  │                    │                        │ 消息 ID = "ch-offset"
  │                    │                        ▼ (Send, 池化缓冲)
  │                    │                    订阅客户端 ◄── Publication
```

### (c) RPC 请求 / 回复经 proxy

```
客户端               Node (handleRPC)        Router           后端 Proxy 服务
  │── RpcRequest ────►│                       │                  │
  │ (channel,method)  │── FindProxy ─────────►│ (glob 首匹配)    │
  │                   │◄── Proxy ─────────────┤                  │
  │                   │── ProxyRPC(rpcCtx) ────────────────────►│
  │                   │   （上下文含 rpcTimeout，代理层叠加）      │
  │                   │◄────────────── RPCResponse ─────────────┤
  │◄── RpcReply ──────┤（超时→RPC_TIMEOUT；无代理→回显；          │
  │                   │  代理错误→PROXY_ERROR）                  │
```

### (d) Survey 请求 / 回复

```
管理方               Node (Survey)          SurveyRegistry        订阅客户端
  │── Survey ───────►│                       │                      │
  │                  │── GetMatchingSubscribers
  │                  │── NewSurvey(id,ch,payload,timeout)
  │                  │── registerSurvey(id) ─► (上限 1000)
  │                  │── 并发 sendSurveyRequest（每发送限 10s）────►│
  │                  │◄──────────── SurveyReply ───────────────────┤
  │                  │   （仅 expected 会话可应答，否则丢弃）        │
  │                  │── survey.Wait(超时) → 排序
  │◄── 汇总结果 ──────┤  （集群模式：广播 ClusterCommandSurvey 再合并）
```

### (e) presence 加入 / 离开事件

```
订阅客户端            Node                     伴生频道 ch/__presence     观察者
  │── Subscribe ────►│                            │                        │
  │                  │── presence.Add(ch, info)   │                        │
  │                  │── PublishPresenceJoin ────►│                        │
  │                  │   （PublishTransient：      │── 实时投递 ───────────►│
  │                  │     不进历史/恢复流）        │                        │
  │── Unsubscribe ──►│                            │                        │
  │                  │── presence.Remove          │                        │
  │                  │── PublishPresenceLeave ───►│── 实时投递 ───────────►│
```

### (f) 经管理 API 查询历史

```
管理工具                    Admin gRPC API               Node              Broker
  │── GetHistory ─────────►│                            │                  │
  │   (Bearer Token)       │── 校验拦截器（可选）         │                  │
  │                        │── node.Broker().History ───►                  │
  │                        │   (sinceOffset, limit)      │── 读取 ─────────►│
  │◄── HistoryResponse ────┤◄────────── publications ────┤◄── 环形缓冲/Stream│
```

## 7. 断连模型（disconnect.go）

`Disconnect` 是携带 `Code` 与 `Reason` 的结构体并实现 `error` 接口：核心代码以返回错误的方式表达"应断开此连接"，`Client.HandleMessage` 用 `errors.As` 识别后调用 `close(disconnect)`，传输层把 Code 与 Reason 交给客户端（WebSocket 用 close 帧的 code/reason；gRPC 用 `DISCONNECT_ERROR` 错误信封，数值码随错误信封传递——目标语义，由传输修复实现后生效，见 `pkg/grpcstream/transport.go:106-121`）。`Code` 为 0 时表示正常关闭（WebSocket 端映射为 1000）。

| Code | 常量 | 含义 |
| --- | --- | --- |
| 3000 | `DisconnectConnectionClosed` | 连接被关闭，无服务端建议；可能是干净断开，也可能是网络中断，服务端无法区分 |
| 3500 | `DisconnectInvalidToken` | token 无效或 `require_auth` 下未携带 token |
| 3501 | `DisconnectBadRequest` | 协议帧格式错误（如重复 Connect） |
| 3502 | `DisconnectStale` | 集群会话恢复失败（远端租约 CAS 抢占失败，或恢复回滚，`cluster_resume.go:77`、`client.go:571`） |
| 3503 | `DisconnectForceNoReconnect` | 服务端要求客户端不要重连（如关停排空） |
| 3504 | `DisconnectConnectionLimit` | 超过每用户连接数上限 |
| 3505 | `DisconnectChannelLimit` | 超过每客户端订阅数上限 |
| 3506 | `DisconnectInappropriateProtocol` | 传输无法承载的数据（如 JSON 客户端收到二进制） |
| 3507 | `DisconnectPermissionDenied` | 权限不足 |
| 3508 | `DisconnectNotAvailable` | 服务端无法处理该消息类型 |
| 3509 | `DisconnectTooManyErrors` | 客户端产生过多错误 |
| 3511 | `DisconnectIdleTimeout` | 心跳检测到超时未活动 |
| 3512 | `DisconnectSlowConsumer` | 客户端消费速度跟不上（写失败触发） |
| 3513 | `DisconnectInternal` | connect 路径内部错误（如集群状态同步失败），连接被强制关闭（`client.go` `disconnectOnConnectError`） |

客户端如何解读这些代码（重连策略、错误展示）见 [《客户端协议参考》](../protocol.md) 的 Disconnect Codes 一节。

## 8. 并发模型

- **64 分片**：`connShards` 与 `subShards` 各 64 个分片（`numHubShards`），`index()` 用 FNV-64a 哈希取模路由。连接操作只锁目标分片与 `hub.mu`（会话 map），订阅操作只锁目标频道分片，互不干扰；`broadcastPublication` 在分片锁内拷贝订阅者列表后释放锁再发送，避免长持有锁。
- **16384 把订阅锁**：`Node.subLocks`（`numSubLocks`）按频道哈希为订阅变更（`AddSubscription`/`RemoveSubscription`）串行化，配合 Saga 保证同一频道的并发订阅/退订不会交错破坏 broker 订阅计数；16384 把锁让高冲突频道之间几乎不互相阻塞。
- **无锁主题匹配**：`CSTrieMatcher` 用原子指针与 CAS 实现无锁并发字典树（写操作复制路径节点后 CAS 切换，读操作失败自旋重试），通配符订阅的增删查不依赖全局锁；Hub 侧仅用 `wcSubsMu` 保护注册表本身。
- **写缓冲池**：`pool.go` 的 `sync.Pool` 提供初始容量 4096 的字节缓冲，`write` 经 `MarshalAppend` 就地复用缓冲，gRPC 传输在入队时拷贝以适配池化复用。
- **广播限流**：订阅者 ≤ 8 时串行发送，超过则并发发送但并发数封顶 `broadcastParallelLimit`（64），防止超大频道的广播产生无界 goroutine。

## 9. 集群概述

集群模式是可选的 Redis 支撑分布式控制面：节点间通过 Redis 共享会话目录、命令总线与查询投影，支持跨节点会话恢复、远程订阅/退订/断开、远程发布与集群级 Survey，并由节点租约与投影修复机制维护最终一致。注意集群模式要求 `broker.type=redis`（配置校验强制）。

本文档不展开集群细节；设计、拓扑与运维说明见[《分布式集群指南》](04-cluster.md)。

## 10. 模块布局

| 路径 | 内容 |
| --- | --- |
| 仓库根（*.go） | 核心包：`node.go`、`hub.go`、`client.go`、`broker.go`、`broker_memory.go`、`presence.go`、`presence_event.go`、`survey.go`、`acl.go`、`disconnect.go`、`transport.go`、`pool.go`、`heartbeat.go`、`metrics.go`、`marshaler.go`、`defaults.go`、`health.go`、`subscription_saga.go`，以及集群相关 `cluster.go`、`cluster_commands.go`、`cluster_state.go`、`cluster_resume.go`、`cluster_projection_repair.go` |
| cmd/server/ | 可执行入口：`main.go`（装配与监听器）、`runtime.go`（gRPC 预绑定与启动顺序） |
| config/ | 配置结构（`config.go`）与校验 |
| shared/ | 独立 Go 模块：marshaler 实现（`shared/marshaler.go`）与生成的 protobuf 代码（`shared/genproto/`） |
| protocol/ | protobuf 源定义（client/、server/、shared/、event/、proxy/、includes/） |
| pkg/websocket/ | WebSocket 传输：`server.go`、`handler.go`、`transport.go` |
| pkg/grpcstream/ | gRPC 流传输与管理 API：`client_server.go`、`admin_server.go`、`server.go`、`handler.go`、`transport.go`、`codec.go`、`api_handler.go` |
| pkg/topics/ | 主题匹配：`matcher.go`、`cstrie.go`、`trie.go`、`naive.go`、`inverted_bitmap.go`、`optimized_inverted_bitmap.go` |
| pkg/redisbroker/ | Redis Broker（`redis.go`、`pubsub.go`、`history.go`、`options.go`、`message.go`、`client.go`）、Redis Presence（`presence_redis.go`）与集群支撑（`cluster_*`） |
| proxy/ | RPC 代理：`proxy.go`、`router.go`、`http.go`、`grpc.go` |
| sdks/go/ | Go SDK（`client.go`、`websocket.go`、`grpc.go`、`proxy.go`、`mux.go`、`options.go`、`message.go`）与示例（`example/`） |
| sdks/ts/ | TypeScript SDK（`src/` 与示例 `examples/`） |
