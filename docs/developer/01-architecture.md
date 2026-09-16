# 架构指南

本文档描述 MessageLoop 服务器的总体架构与核心组件设计，面向希望理解或修改服务端代码的开发者。文中所有类型名、方法名、常量与行为均以仓库源码为准。

配套文档：[《配置参考》](02-configuration.md)、[《管理 API 参考》](03-admin-api.md)、[《分布式集群指南》](04-cluster.md)、[《可观测性指南》](05-observability.md)、[《开发指南》](06-development.md)、[《客户端协议参考》](../protocol.md) 与[《部署指南》](../deployment.md)。

## 1. 概述

MessageLoop 的核心设计目标可以归纳为四点：

- **传输无关的核心**：连接管理、订阅、消息路由等核心逻辑全部构建在 `Transport` 接口之上，WebSocket、gRPC 流与可选的 QUIC/KCP 只是可替换的传输实现，核心代码不感知具体传输。
- **可插拔的 Broker**：发布/订阅与历史存储通过 `Broker` 接口抽象，提供进程内内存实现与 Redis 实现，二者在接口层面完全等价。
- **分片并发模型**：连接注册表与订阅注册表各自分为 64 个分片，订阅变更用 16384 把通道级锁串行化，内存 broker 的投递也按频道哈希分为 64 个分片 worker，减少全局锁与单点竞争。
- **会话感知的连接管理**：连接以服务端生成的会话 ID（`session ID`）为标识，支持断线恢复（resume）、新连接接管旧会话（takeover）与集群内跨节点恢复。

## 2. 总体布局

```
客户端 ──► WebSocket 监听器 (transport.websocket.addr) ──┐
客户端 ──► gRPC 流监听器  (transport.grpc.addr)        ──┤
客户端 ──► QUIC 监听器    (transport.quic.addr, 可选)  ──┤
客户端 ──► KCP 监听器     (transport.kcp.addr, 可选)   ──┤
管理工具 ─► gRPC 管理 API  (server.grpc_admin.addr)     ──┼─► Node（中央协调者）
运维系统 ─► HTTP 健康/指标  (server.http.addr)          ──┘
                                                              │
         ┌──────────────┬───────────────┬─────────────────────┼───────────────┬──────────────┬──────────────┐
         ▼              ▼               ▼                     ▼               ▼              ▼              ▼
      ┌──────┐      ┌────────┐      ┌──────────┐        ┌─────────┐      ┌────────┐     ┌─────────┐    ┌─────────┐
      │ Hub  │      │ Broker │      │ Presence │        │Authorizer│     │ Proxy  │     │ Cluster │    │ Metrics │
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

| 组件 | 位置 | 职责 |
| --- | --- | --- |
| `Node` | internal/runtime/node.go | 中央协调者，持有并装配所有子系统，对外暴露发布、订阅、Survey、代理等入口 |
| `Hub` | internal/session/hub.go | 连接与会话注册表（64 分片）、频道订阅注册表（64 分片）、通配符订阅匹配、每用户连接数限制 |
| `Session` | internal/session/session.go、client.go | 单条连接的生命周期与消息处理：状态机、鉴权、命名空间、resume、写队列、限制执行、入站消息路由 |
| `Broker` | internal/stream/broker.go、broker_memory.go、pkg/redisbroker/ | 发布/订阅与历史存储；内存实现与 Redis 实现 |
| `Presence` | internal/occupancy/、pkg/redisbroker/presence_redis.go | 频道内在线成员追踪与 join/leave 事件分发 |
| `Survey` | internal/survey/survey.go、internal/runtime/node.go | 向频道订阅者广播请求并带超时收集响应 |
| `Authorizer` | internal/authz/authorizer.go | 单一授权求值器：一个 `Decide`、一张 `server.authorizer` 表、一种通配语言；频道策略 Effects 与 Admin Capability 闭集 |
| `Proxy` | proxy/ | RPC 转发与鉴权/ACL/生命周期钩子的后端集成 |
| `Cluster` | internal/runtime/cluster.go 及 cluster_*.go（契约在 internal/cluster） | 可选的 Redis 支撑分布式控制面（详见[《分布式集群指南》](04-cluster.md)） |
| `Metrics` | internal/metrics/metrics.go | Prometheus 指标收集（详见[《可观测性指南》](05-observability.md)） |

## 3. 核心组件

### 3.1 Node（internal/runtime/node.go）

`Node` 是运行时装配根：它持有 `hub`、`broker`、`presence`、`cluster`、`proxy`、`authorizer`、`metrics`、`surveys` 等全部子系统，并通过一组 setter 方法注入实现。

关键方法与行为：

| 方法 | 说明 |
| --- | --- |
| `NewNode(cfg *config.Server)` | 构造默认装配：`session.NewHub(0, MaxConnectionsPerUser)`、`NewMemoryBroker`、`NewMemoryPresenceStore`，从配置构建 Authorizer（`server.authorizer` 一张表，永不 nil）与心跳管理器 |
| `Run(ctx)` | 先启动集群（若启用），注册 occupancy/gap 处理器，再以 `go n.broker.Start(ctx, handler)` 启动 broker，handler 即 `n.hub.BroadcastPublication`；若 broker 实现 `Ready()` 则等待其就绪 |
| `Shutdown()` | 以 `DisconnectForceNoReconnect` 排空全部连接，受 `DefaultShutdownTimeout`（10s）约束，然后关闭集群 |
| `AddClient(c)` | 注册连接（超限返回 `DisconnectConnectionLimit`），集群模式下同步会话状态 |
| `AddSubscription` / `RemoveSubscription` | 通过订阅 Saga 提交订阅变更（见下） |
| `Publish(ch, payload, isText)` | 经 broker 发布并返回 offset |
| `PublishTransient` | 仅实时投递、不写历史（presence 事件用） |
| `Survey(ctx, channel, payload, timeout)` | 本地收集 + 集群广播汇总 |
| `SetupProxy` / `FindProxy` / `ProxyRPC` | 代理装配与 RPC 转发入口 |
| `MaxMessageSize()` | 入站消息上限，配置为 0 时取 `DefaultMaxMessageSize`（64 KB） |

**订阅 Saga**：`AddSubscription` 与 `RemoveSubscription` 在每通道锁（`subLocks`，16384 把，见 §8）内串行执行，由 `runSubSaga`（internal/runtime/subscription_saga.go）分步提交：hub 订阅登记 → 客户端频道跟踪 → broker 订阅计数（仅首个订阅者真正 `Subscribe`）→ 集群会话/频道状态同步。任一步失败时按逆序回滚已执行的步骤。

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

`prepareGRPCServers`（cmd/server/runtime.go）在 `node.Run` 之前预绑定监听器，保证端口在服务启动前即被占用，避免启动窗口期端口被抢占。

### 3.2 Hub（internal/session/hub.go）

`Hub` 维护三类注册表：

- `sessions map[string]*Session`：会话 ID → 会话对象，受 `hub.mu` 保护，用于会话查找与 resume。本机 resume 保持会话指针恒等：不扫描订阅分片、不重建通配 matcher。
- `connShards [64]*connShard`：每个分片内含 `clients`（会话 ID → 会话）与 `users`（用户 ID → 会话集合），按 `index(userID)` 分片。
- `subShards [64]*subShard`：每个分片内含 `subs`（频道 → 会话 ID → `Subscriber`），按 `index(channel)` 分片。

关键方法与行为：

| 方法 | 说明 |
| --- | --- |
| `index(s, n)` | FNV-64a 哈希取模分片 |
| `addWithLimit` | 在 `connShard` 内注册会话；`maxConnsPerUser > 0` 且用户连接数已满时返回 `DisconnectConnectionLimit` |
| `AddSub` / `RemoveSub` | 通配符订阅走 `wcSubs`（键为 `sessionID:channel`）+ `matcher`，精确订阅走 `subShard` |
| `BroadcastPublication` | 合并精确与通配符订阅者（按会话 ID 去重，保证同一客户端只收到一次），小扇出（≤8）串行发送，大扇出用受 `broadcastParallelLimit`（64）限流的并发发送 |
| `LookupSession` / `LookupSubscriber` | 会话与订阅查找（返回 `*Session`） |
| `PrepareSessionUser` | 跨用户本机 resume 前原子执行：目标用户 `maxConnsPerUser` 限额检查 + `connShard` 用户归属迁移；失败不改动任何状态（旧会话保持 Attached） |
| `RemoveSessionIfMatches` | 仅在注册的会话与当前会话一致时移除，防止失败的旧连接把已接管/已恢复的会话驱逐出去 |
| `GetActiveChannels` | 管理 API 用的活跃频道列表（含订阅者数） |
| `DrainAll` | 并发向所有连接发送 `Close(disconnect)` 并等待关闭 |

消息 ID 规则：实时投递与恢复共用 `publicationID(channel, offset)`（`"频道-offset"`），客户端据此去重；瞬时事件（offset 为 0）回退为随机 UUID，避免同一频道所有瞬时事件共享同一个 ID。

### 3.3 Session（internal/session/session.go、client.go）

内核对象是 `Session`（可恢复的逻辑连接，`type Client = Session` 为过渡别名）。状态机为 `SessionAuthenticating → SessionAttached ⇄ SessionDetached → SessionClosed`：

- `Authenticating`：`NewClient` 出生即赋值，Connect 进行中。
- `Attached`：正常服务，写队列由一个 writer goroutine 排空。
- `Detached`：仅用于本机交接窗口——附件已撕、`Session` 留在 Hub、集群目录仍认本 fencing。被其他节点抢走的会话不走此状态（走 Fence）。
- `Closed`：终态。

**关闭三动词**：

| 动词 | 副作用 |
| --- | --- |
| `Close(reason)` | 真正离开：presence Leave + 撤订阅 + `RemoveSessionIfMatches` + 删除集群会话状态 + 关附件；对已 Closed 幂等 |
| `Fence(reason)` | 被接管：撤本地订阅与 Hub 条目 + 关附件；不 Leave、不删集群目录条目（新 owner 的 fencing 保留）；对已 Closed 幂等 |
| `Detach(reason)` | 本机交接：只关附件、停 writer、丢队列；`Session` 留在 Hub；对非 Attached 是 no-op |

**写队列**（挂在 Session 上，三种传输共享同一队列，不另设第二层有界缓冲）：Control 深度 32 / Data 深度 256，下一帧优先 Control。信封分类（`outboundFrameClass`）：Ping/Pong、各类 Ack、Connected、RecoverComplete、GapNotice、Error、SubRefreshAck 为 Control；Publication、PresenceEvent、Survey 为 Data。`Send` 入队并等待该帧落线，调用方同步观察写结果；Data/Control 队列满或写超时 → `Close(DisconnectSlowConsumer)`（3512）。错误映射：对端正常关闭（`io.EOF`、gRPC `Canceled`/`Unavailable`、WS close 1000/1001）→ 3000；`ErrSessionFenced` → `Fence(DisconnectStale)`（3502）。gRPC 传输的 `sendCh` 深度为 1，仅作向传输 worker 的 handoff。

**连接生命周期（`handleConnect`）**：

1. 若已鉴权再发 Connect，返回 `DisconnectBadRequest`。
2. 客户端可携带 `SessionId` 请求恢复；会话 ID 在鉴权之前就写入（鉴权代理需要它）。
3. 鉴权：若带 `Token`，查找方法为 `$authenticate`（`SystemMethodAuthenticate`）的代理并调用 `Authenticate`；`requireAuth` 开启但没有可验证 token 的代理时拒绝（`DisconnectInvalidToken`）。Connect 的协议版本过低时以 3514（`DisconnectUnsupportedVersion`，internal/protocol/disconnect.go 的版本门）拒绝。
4. **命名空间解析**：鉴权代理响应 `UserInfo.namespace` 优先，回退静态 `server.namespace`；两者皆空且 `requireAuth` 开启时 Connect 被拒（`NAMESPACE_REQUIRED` 错误信封 + 3500 断连）。会话绑定 namespace 后，所有客户端可见 channel 必须位于其下（见下文命名空间守卫）；resume 时 namespace 不一致与跨用户接管同样按 3500 拒绝。
5. 恢复：本地会话存在则会话指针不动——先查 `maxConnsPerUser`（跨用户走 `PrepareSessionUser`，失败则旧会话保持 Attached、新连接收到 `DisconnectConnectionLimit`），再 `Detach` 旧附件、`Attach` 新附件；Attach 失败走 `Close(DisconnectInternal)`。新连接上的临时 Authenticating 会话不进 Hub，读循环 shell 委托给被恢复的会话。本地不存在则尝试跨节点恢复（`resumeRemoteSession`，见[《分布式集群指南》](04-cluster.md)）。未鉴权连接不能驱逐仍被服务的会话。
6. `AddClient` 注册 + 连接指标计费，集群模式下同步会话状态（本机 resume 走 same-fence Bind，版本 +1 后 owner 仍是自己）。
7. 通知代理 `OnConnected`。
8. 处理 Connect 携带的订阅列表：先做订阅数上限检查（超限 `DisconnectChannelLimit`），逐频道命名空间守卫 / Authorizer/代理检查，`AddSubscription` + presence 登记 + 发布 join 事件。
9. 消息恢复（流式恢复，internal/runtime/recover.go）：先发裸 `Connected`（v2 协议的 Connected 不携带历史批次与 presence 列表），再对每个 `recover=true` 的频道走统一 Replayer：`fresh=true` 或 resume 时快照 epoch 与 broker epoch 不一致（两边都非空）才从频道历史开头恢复；否则 cursor 带 offset 时从 `offset+1` 续读，cursor 未带 offset 时回退服务端已记录的 delivered offset（有则续读，无则跳过）。`broker.History` 以 `MaxRecoveredPublications`（1000，请求级配额、多频道共享）为限，逐条 `Publication(replay=true)` 经 `Session.Send` 落线（每条受 `MaxMessageSize` 约束），最后每频道一条 `RecoverComplete{channel, position, truncated, gap, gap_reason, error?}`（标 Control）。恢复失败不撤订订阅。

**命名空间守卫（internal/session/namespace.go）**：会话携带 namespace 后，`precheckNamespace` 作为统一入口门挂在 `handleMessage` 分发点，对所有携带频道引用的信封做作用域检查，各 handler 内的 `checkNamespace` 作为纵深防御保留：

| 信封 | 拒绝行为 |
| --- | --- |
| `Subscribe`（含 Connect 携带订阅） | 逐频道检查，违规频道各回一条 `NAMESPACE_MISMATCH` 错误信封并丢弃，命名空间内的剩余频道照常处理 |
| `Publish` | 一条 `NAMESPACE_MISMATCH`，请求不分发 |
| `Unsubscribe` / `SubRefresh` | 一条 `NAMESPACE_MISMATCH`，请求不分发（连接保持） |
| `SurveyRequest` | 经 Survey 错误路径回 `NAMESPACE_MISMATCH`（与 handler 拒绝路径共用计数） |
| `PresenceQuery` | 一条 `NAMESPACE_MISMATCH`，请求不分发 |
| `RpcRequest` | `handleRPC` 在代理路由前自行执行 `checkNamespace` |
| `Connect` / `Ping` / `Pong` / `SurveyReply` | 豁免（Connect 自解析命名空间；Ping/Pong 无频道；SurveyReply 按请求 ID 路由且发起时已校验） |

空频道名留给 handler 的 `BAD_REQUEST` 检查；发布/Survey/Presence 查询的通配频道同样先由 handler 拒绝。违规只拒绝该次请求，不断连。订阅/发布/RPC/Survey/Presence 的完整校验链中，命名空间检查先于 Authorizer 与代理执行，代理放行不能越过。

**入站消息路由（`handleMessage`）**：

| 信封类型 | 处理函数 | 行为要点 |
| --- | --- | --- |
| `Connect` | `handleConnect` | 版本门、鉴权、命名空间解析、恢复、初始订阅与恢复（见上） |
| `Publish` | `handlePublish` | 未鉴权 → `DisconnectInvalidToken`；限速 → `RATE_LIMITED` 错误；静态 `Decide(Publish)` 后问代理 `PublishAcl`（代理允许不越过静态 deny）；Payload 的 Json/Binary/Text 变体统一转字节；成功回 `PublishAck`（`position` 携带 broker 分配的 offset，transient/无历史时 offset 缺省） |
| `Subscribe` | `handleSubscribe` | 订阅数上限检查、逐频道命名空间/Authorizer/代理校验、Saga 提交、presence 登记 + join 事件、代理 `OnSubscribed` 通知，先回裸 `SubscribeAck`（`recover` 状态 NONE/SKIPPED/PENDING，presence 快照随 Ack），再对 `recover=true` 频道流式恢复（同 Connect 的 Replayer） |
| `RpcRequest` | `handleRPC` | 命名空间检查后经 `node.ProxyRPC` 转发（详见 §3.8）；超时回 `RPC_TIMEOUT`；无匹配代理回 `NO_PROXY`；代理错误回 `PROXY_ERROR` |
| `Unsubscribe` | `handleUnsubscribe` | 移除订阅、presence 清理 + leave 事件、代理 `OnUnsubscribed` 通知 |
| `Ping` | `handlePing` | 刷新活动时间，回 `Pong`；presence/集群状态刷新被节流（`pingClusterRefreshInterval` 10s，CAS 保证单次） |
| `SubRefresh` | `handleSubRefresh` | 重新校验订阅 ACL，失败的频道被撤销并发布 leave |
| `SurveyRequest` | `handleSurvey` | 同步校验后派 worker 调 `Node.Survey`，异步回 `SurveyResult`（不阻塞读循环；默认策略关 + `Decide(Survey)` deny） |
| `SurveyReply` | `handleSurveyReply` | 校验请求 ID 后写入对应 Survey（`node.AddSurveyResponse`） |

**限制执行**：

- 订阅数：`limits.MaxSubscriptionsPerClient`（含继承自恢复会话的频道），超限 `DisconnectChannelLimit`。
- 发布速率：`limits.MaxPublishesPerSecond` 构造 `rate.Limiter`，超限回 `RATE_LIMITED` 错误信封（不断连）。
- 消息大小：`node.MaxMessageSize()`（默认 64 KB），WebSocket 端经 `SetReadLimit` 加解压后字节数上限共同强制，gRPC 端经 `MaxRecvMsgSize` 强制，各传输读取同一入口保证一致。

**写路径（`Send`/`enqueue`）**：从 `sync.Pool`（internal/session/pool.go，初始容量 4096）取缓冲 → `marshaler.MarshalAppend` 序列化 → 入 Session 写队列（Control/Data 双车道，下一帧优先 Control）并等待该帧落线；写入失败按错误映射处理（对端关闭 → 3000，写超时/队列满 → 3512，fenced → 3502）。Attached 期间由唯一 writer goroutine 排空；Detach/Fence/Close 停 writer 并丢弃队列。gRPC 传输对缓冲做拷贝后再交给 worker，因为池化缓冲在 `Write` 返回后可能被复用。

**断开（`Close`/`Fence`/`Detach`）**：`Close`：标记 Closed → 取消心跳 → 停 writer → 并发（≤16）移除全部订阅 → presence 清理 + 逐个发布 leave 事件 → `RemoveSessionIfMatches` 后删除集群会话状态 → 递减连接指标 → 通知代理 `OnDisconnected` → 关附件。被抢只准 `Fence`（无 Leave、不删集群条目）；本机交接用 `Detach`（只关附件、Session 留在 Hub）。

### 3.4 Broker（internal/stream/broker.go、broker_memory.go、pkg/redisbroker/）

`Broker` 接口（broker.go）：

| 方法 | 语义 |
| --- | --- |
| `Start(ctx, handler)` | 初始化并持续处理事件直到 ctx 取消；`handler` 接收每条发布 |
| `Subscribe(ch)` / `Unsubscribe(ch)` | 注册/注销节点对频道的兴趣；仅在首个/末个本地订阅者时由 Node 调用 |
| `Publish(ch, payload, isText)` | 发布并返回该发布分配到的 offset（历史被禁用时为 0） |
| `PublishTransient(ch, payload, isText)` | 仅实时投递，不写历史，offset 恒为 0 |
| `History(ch, sinceOffset, limit)` | 返回 `*HistoryPage`：offset ≥ sinceOffset 的发布 + `Truncated` / `Gap` / `GapReason` / `FirstRetained`；`limit <= 0` 时以 `DefaultHistoryLimit`（1000）为上限；`sinceOffset>0` 且空批禁止 `GapReason=None`；Redis 实现按页内相邻条目稠密 seq 不连续报 `Middle` |

`GapReason` 取值：`HeadTrimmed`（最旧保留条目晚于起点）、`EmptyExpired`（起点之后无保留条目）、`Middle`（页内稠密 seq 断档）、`ReplayTruncated`（回放批被截断）。

`Publication` 携带 `Channel`、`Offset`、`Seq`（每频道稠密序号，Redis broker 发号；transient / legacy / memory 实现为 0）、`Epoch`、`Payload`、`IsText`、`Time`。

**内存实现（broker_memory.go）**：每频道一个固定容量环形缓冲（`defaultMemoryHistorySize`，256），`nextOff` 从 1 起按发布递增，写满后覆盖最旧条目；频道历史在仍有订阅者或仍有条目时被保留，最后一个订阅者离开且历史为空时才回收。每次 `NewMemoryBroker` 生成随机 `Epoch`，进程重启即失效。**投递按 64 个分片 worker 序列化**（`dispatchShardCount=64`）：频道经 FNV-64a 哈希映射到分片，同一频道的发布由同一 worker 按序投递 handler，单个慢 handler 只拖慢哈希到同一分片的频道；分片队列（`dispatchShardDepth=256`）满时对发布方背压（阻塞 `Publish` 返回）。关停时已接受但未投递的发布被丢弃，其历史条目仍然成立。Interest 语义与 Redis 实现对齐：`Subscribe` 入口先经 `CompileInterest`（internal/channel/interest.go）校验——精确频道按引用计数、可编译的通配 pattern（字面前缀 + 末尾 `*`/`**`）登记到 matcher，不可路由 pattern（`*.room`、裸 `*`/`**` 等）返回 `ErrPatternNotRoutable`；`Publish` / `PublishTransient` 仅在 `interested(ch)`（精确计数 > 0 或 matcher 命中）时才调 handler，handler 的错误或 panic 只记日志，不影响发布结果（`Publish` 恒返回已分配 offset 与 `err=nil`）。`History` 返回带 gap 元数据的 `HistoryPage`：`sinceOffset>0` 且无保留条目 → `EmptyExpired`；最旧保留 offset > sinceOffset → `HeadTrimmed`。

**Redis 实现（pkg/redisbroker/redis.go）**：发布由一条 Lua 脚本原子完成——`INCR` 每频道稠密 seq 计数键（`ml2:stream:seq:<ch>`）+ `XADD` 写入 Redis Stream（前缀 `ml2:stream:`，`StreamMaxLength` 默认 10000 条、`HistoryTTL` 默认 24h）+ 两个键的 TTL 刷新；stream 条目带 `s` 字段（稠密 seq，发号与写入同脚本，崩溃不留假洞）。offset 从 Stream ID 解析（`offset = ts<<20 | seq`）。随后维护 `first_retained` 标记键（`ml2:stream:retained:<ch>`）并 `PUBLISH` 到 Redis Pub/Sub（前缀 `ml2:pubsub:`，实时载荷带 `seq`）做实时分发；发布全路径共用一个 5s 超时上下文，`XADD` 成功而 `PUBLISH` 失败只记日志、不回滚，对调用方仍返回 `(offset, nil)`——发布成功以日志写入为准。策略级 `HistorySize`/`HistoryTTL` 可在每次发布时覆盖全局值。

**实时订阅按 Interest 编译（interest.go 的 `CompileInterest`）**：精确频道 → `SUBSCRIBE ml2:pubsub:<ch>`；字面前缀 + 末尾 `*` → `PSUBSCRIBE ml2:pubsub:<prefix>.*`；末尾 `**` → 额外 `SUBSCRIBE ml2:pubsub:<prefix>`（零段情况）；其余 pattern（`*.room`、`im.*.tick`、裸 `*`/`**`）在 `Subscribe` 入口即被拒绝（`ErrPatternNotRoutable`），客户端收到 `PATTERN_NOT_ROUTABLE` 信封、不断连。不存在默认的 `PSubscribe(ml2:pubsub:*)`：每条 pub/sub 连接只订阅控制频道 `ml2:pubsub:__live__`（其 ack 关闭 `Ready()`）加上当前 Interest 的编译结果；Interest 增删经串行队列动态增删 Redis 订阅，重连后按当前 Interest 重建；收到消息后除 `interested()` 外还按段匹配（`MatchAfterCompile`）丢弃 Redis glob 的跨点过匹配（`im.room.*` 不会把 `im.room.a.b` 交给 handler）。断线以指数退避（1s 起、上限 30s）重连。catch-up 检出洞（中洞按稠密 seq、回放批被截断的尾截）时 broker 经 `SetGapHandler` 上报，node 向该频道本地订阅者扇出 `GapNotice` 信封（见[《分布式集群指南》](04-cluster.md)）。

**offset + epoch 语义**：内部 History 是频道内单调 uint64 offset（内存实现从 1 起，Redis 实现由 Stream ID 编码），客户端用它做断线续读；只在进出线转换为 v2 `Position{stream_epoch, offset?}`。线语义：`Subscription.cursor`（Position，offset 可缺省）+ `Subscription.fresh`；只有 `fresh=true` 或 resume 时 epoch 重置才从头，`offset==0` / cursor 缺省不是从头（`cursor.offset` 缺省 = 无提示，服务端回退已记录 delivered offset，无记录则跳过恢复）。epoch 用于判断 offset 是否仍属于当前 broker 代际——epoch 不匹配（且两边都非空）时视为 offset 无效，从历史开头恢复。两种实现的 epoch 来源不同：内存 broker 每进程实例启动时生成随机 UUID，重启即失效；Redis broker 的 epoch 存于固定键 `ml2:broker:epoch`，首个启动节点经 `SETNX` 写入随机 UUID，集群共享、跨重启持久（`initEpoch`，pkg/redisbroker/redis.go），因此 Redis 部署下 epoch 校验跨节点、跨重启均可通过（详见[《分布式集群指南》](04-cluster.md)）。

### 3.5 Presence / Occupancy（internal/occupancy/、pkg/redisbroker/presence_redis.go）

`PresenceStore` 接口与 `Broker` 分离，可独立替换：

- `Add(ctx, ch, info)`：订阅时登记，也用于长连接的心跳刷新；
- `Remove(ctx, ch, clientID)`：退订与断开时移除；
- `Get(ctx, ch)`：返回频道内全部在线成员。

`PresenceInfo` 含 `ClientID`、`UserID`、`ConnectedAt`。默认实现为 `NewMemoryPresenceStore`（进程内 map）；集群模式用 Redis 实现：每（频道, 客户端）一个带 TTL 的键（`PresenceTTL` 默认 60s）+ 每频道一个集合索引，`Get` 时清理过期成员。

join/leave 事件以 **Occupancy** 概念分发：每次 Join/Leave 取一个单调 **OccupancyGen**（memory：进程内每频道计数器；Redis：`INCR ml2:presence:occ:gen:<ch>`），存完 store 后只调用 `broker.PublishOccupancy(exactCh, evt)` 走 LiveBus 精确频道。订阅了精确频道 `C` 的客户端默认就能收到 `C` 上的 join/leave 与快照（`Connected.presence` / `SubscribeAck.presence`，或主动发 `PresenceQuery`）；通配订阅者收到其 pattern 覆盖的每个精确频道上的事件——跨节点只靠 LiveBus 的 Interest（精确或 `CompileInterest` 编译的 pattern）决定谁能收到，`im.**` 的节点能收到 `im.room.1` 的 join，只订 `chat.1` 的节点收不到。

接收（本机与跨节点同一条路径，`Node.onOccupancy`）：`gen==0` 或 `Event==nil` 丢弃；`gen <= lastApplied[ch][session]` 判为迟到（`ErrLateOccupancy`，计数），否则记 `lastApplied` 并经 `deliverPresenceEvent` 扇到 Coverage 订阅者（跳过 ephemeral 与事件主体自己）。事件不进 Stream、不是 Publication 信封、不计 `MessagesPublished`/`MessagesDelivered`；`Hub` 对 occupancy 帧没有任何改写逻辑。

只有频道策略 `legacy_presence_channel=true` 时，才额外把旧 JSON 格式（`__type: "presence"`、`action`、`channel`、`client_id`、`user_id`、`timestamp`）瞬时发布到 `presenceChannel(ch) = ch + "/__presence"` 伴生频道（`PublishPresenceJoin`/`PublishPresenceLeave`，仅精确频道，通配从不写伴生）。Redis 端 presence store `Get` 清理 TTL 蒸发的幽灵成员时，对该 session 合成一条 leave 并取新 gen 再 `PublishOccupancy`（memory store 无 TTL 无合成）。

### 3.6 Survey（internal/survey/survey.go、internal/runtime/node.go）

`Node.Survey` 流程：

1. 取频道全部精确 + 通配符订阅者（`GetMatchingSubscribers`）；
2. 创建 `Survey`（含 `responseCh` 缓冲 100）并登记所有被问询会话为 `expected`（其他会话的应答被视为伪造并丢弃）；
3. 注册到 `n.surveys`（容量上限 `maxActiveSurveys` = 1000，防无界增长）；
4. 并发向每个订阅者发送 `SurveyRequest`，单次发送受 `surveySendTimeout`（10s）约束——传输阻塞的慢消费者被记为错误应答而不是挂起整个 Survey；
5. `survey.Wait(ctx)` 收集应答直到超时或 ctx 取消；`timeout <= 0` 时回退到 `DefaultSurveyWaitTimeout`（5s）；
6. 集群模式下经命令总线广播 `ClusterCommandSurvey`，汇总各节点结果并统一排序。

**客户端发起的 Survey（`handleSurvey`，internal/session/client.go）**：客户端可对精确频道发起 Survey 并异步收集应答，与 Admin 流程独立：

1. 同步校验，任一失败即回顶层 Error 信封（不断连、不撤订阅）：channel 为空或是通配 → `BAD_REQUEST`；`sessionCoversChannel` 未覆盖（精确订阅或通配命中，授权放行不能偷看未加入的频道）→ `PERMISSION_DENIED`；Authorizer `Decide(Survey)` 拒绝——`Effects.Survey=false`（默认关）→ `SURVEY_DISABLED`，未配 `allow_survey` 或 deny 命中 → `PERMISSION_DENIED`；同会话已有一笔在途 Survey 或超过 1/s 限流 → `RATE_LIMITED`。
2. 通过校验后不阻塞读循环：标记 in-flight，worker goroutine 先做 `countMatchingSubscribers` 集群 `count_only` 预检（本地快路径 + 广播，超过 `max_survey_subscribers` → `SURVEY_TOO_MANY_SUBSCRIBERS`，零条 outbound `SurveyRequest`），再调 `Node.Survey`，汇总后异步回 `SurveyResult`（回显发起方 `request_id`）。
3. Admin `Node.Survey`：持有 `survey.bypass_gate` 能力位时不受 `Decide(Survey)` 与 `max_survey_subscribers` 门限制约；无此位则与客户端走相同的门。

### 3.7 Authorizer（internal/authz/authorizer.go）

单一授权求值器：一个 `Decide(principal, action, channel)`、一张 `server.authorizer` 表、一种通配语言（`*` 单段、`**` 仅末段、字面前缀不可为空）。频道策略（history/presence/recover/survey 等开关）是规则的 **Effects**，与授权名单在同一张表里表达。

`AuthorizerRule` 由 `pattern`、`DenyAll`、`AllowSubscribe`/`AllowPublish`/`AllowSurvey`（用户 ID 白名单，`"*"` 表示任意已认证用户；省略 = 不约束该 action，空列表 = 拒绝）与内联 Effects 组成。

`Decide` 语义：

- **订阅（SubscribePattern）**：默认放行，但 `L(p) ∩ L(d) ≠ ∅`（deny 规则 d 生效于该 principal）→ 整条拒绝（deny 不可被更具体的 allow 打洞）。语言求交按 exact/star/dstar 表驱动实现，不枚举频道。不可路由 pattern（`*.room`、裸 `**`）先于 ACL 给出 `not_routable`。
- **发布（Publish）**：精确频道，默认放行；`deny_all` 或未命中的 allow 名单 → 拒绝。不要求订阅覆盖（Coverage）。
- **Survey**：默认拒绝；`Effects.Survey==true` 且 有 `allow_survey` 命中 且 无 deny 命中才放行。
- **恢复（Recover）/ 在场（Presence）**：精确频道；默认跟随 `Effects(ch)`；`deny_all` 命中或通配频道 → 拒绝。

`Effects(ch)` = `DefaultChannelPolicy()` overlay `server.authorizer.default`，再按表顺序 overlay 每条匹配规则（后写覆盖先写，不是 first-match）；`TransientOnly` 强制 `History=false` 且 `Recover=false`。

**Admin Capability 闭集**：`history.read` / `presence.read` / `channels.list` / `session.act` / `user.fanout` / `subscribe.any` / `presence.large_snapshot` / `survey.bypass_gate` / `pattern.global`（预留）。`server.grpc_admin.capabilities` 省略 = 除 `pattern.global` 外全位；显式 `[]` = 零位（锁死 Admin 数据面）；未知名 = Validate 错误。`GetHistory`/`GetPresence`/`GetChannels`/代订/按 user 扇出必须持位，不得旁路。

**与代理 ACL 的关系**：订阅/发布先过静态 `Decide`，再问代理——代理命中时 `SubscribeAcl`/`PublishAcl` 作为额外的门；代理允许不得跳过静态 deny（见 `checkSubscribeACL` 与 `handlePublish`），代理拒绝只否决这一次请求。

**无代理 RPC**：`handleRPC` 遇 `proxy.ErrNoProxyFound` 回顶层 Error `code=NO_PROXY` `type=request_error`，不回显请求体。

### 3.8 Proxy（proxy/proxy.go、router.go、http.go、grpc.go）

`Proxy` 接口提供两类能力：RPC 转发（`RPC`）与后端集成钩子（`Authenticate`、`SubscribeAcl`、`PublishAcl`、`OnConnected`、`OnSubscribed`、`OnUnsubscribed`、`OnDisconnected`），后者在客户端连接生命周期中被 Node/Session 调用。

`Router`（router.go）按添加顺序保存路由，`Match(channel, method)` 返回首个 channel 与 method 同时匹配（gobwas/glob 编译）的路由；无匹配返回 `ErrNoProxyFound`。`Node.createProxy` 的选择规则：显式 `GRPC` 配置 → gRPC；显式 `HTTP` 或 Endpoint 以 `http://`/`https://` 开头 → HTTP；否则 gRPC。

- **HTTPProxy**（http.go）：JSON POST 到 Endpoint，`Content-Type: application/json`，支持自定义头与 TLS（`InsecureSkipVerify`/`ServerName`）；响应体上限 4 MiB。
- **GRPCProxy**（grpc.go）：连接 `proxypb.ProxyServiceClient`，接收消息上限 4 MB，支持 insecure/TLS 两种凭据与自定义 SNI/跳过校验。

**三层超时**：① 请求级——`node.rpcTimeout`（`server.rpc_timeout`，默认 `proxy.DefaultRPCTimeout` 30s）；② 代理级——每个 `ProxyConfig.Timeout`（默认同样 30s）；③ 传输级——HTTP client 超时 / gRPC 上下文 deadline。代理的 `withTimeout` 只在上下文没有 deadline 时才叠加自己的超时，嵌套时取最紧约束。

### 3.9 Metrics（internal/metrics/metrics.go）

`Metrics` 注册一组 Prometheus 指标：连接/订阅/活跃频道 gauge、发布/投递计数、发布与 RPC 耗时直方图、投递失败计数、恢复与 Survey 等策略行为计数，以及集群命令去重/超时/验签拒绝、节点租约续期失败、投影修复等集群指标。采集、标签与完整表见[《可观测性指南》](05-observability.md)。

## 4. 传输层

### 4.1 Transport 接口（internal/session/transport.go）

```go
type Transport interface {
    Write([]byte) error
    WriteMany(...[]byte) error
    Close(Disconnect) error
    RemoteAddr() string
}
```

核心逻辑只与字节流交互，编码由 `Marshaler` 负责，传输不感知协议消息结构。

### 4.2 WebSocket（pkg/transport/ws/）

- `server.go`：`lynx.Service` 实现，挂载 `/ws` 路径（默认 `:9080`），支持 TLS 与 `ReadHeaderTimeout`（10s）。
- `handler.go`：升级后按子协议协商编码（见 §4.5），`NewClient(..., WithProtocol("ws"))` 创建会话；读循环解码 `InboundMessage` 后交给 `Session.HandleMessage`；`SetReadLimit` 施加压缩线上字节的消息大小上限，启用 permessage-deflate 时解压输出另有同额度的解压上限（防解压炸弹）。读超时规则：心跳完全禁用（idle 与 ping_interval 均为 0）时取 60s 或显式配置；否则以 `max(2 × idle_timeout, 3 × ping_interval, 10s)` 为下限，显式配置只能放大。每次成功读消息后重置截止时间。
- `transport.go`：`writeMu` 串行化写入；关闭时发送 WebSocket close 帧（`Disconnect.Code` 直接作为 close code，code 0 回退为 1000）并等待对端确认。

### 4.3 gRPC 流（pkg/transport/grpc/）

- `client_server.go`：`PrepareClientServer` 注册 `MessageLoopService`，每个客户端一条双向流（handler.go 的 `MessageLoop` 方法），固定使用 Protobuf。
- `server.go`：共享的 `PrepareServer`——预绑定监听器、加载 TLS、施加 `ForceServerCodec(RawCodec)` 与 `MaxRecvMsgSize`，统一生命周期；`AdminAuthInterceptor` 为管理监听器提供 Bearer Token 校验（常量时间比较）。
- `transport.go`：写入经单 worker goroutine 串行化（`sendCh` 深度 1，仅作 handoff，不构成第二层缓冲），默认写超时 10s，入队前拷贝消息字节（调用方可能复用池化缓冲）。关闭时先投递 `DISCONNECT_ERROR` 错误信封（数值断连码编码在 metadata 的 `disconnect_code` 条目中，队列堵塞时以 1s 超时降级为直接关闭）再退出 worker。
- internal/admin/admin_server.go：`PrepareAdminServer` 在独立监听器注册 `APIService`（管理 API），详见[《管理 API 参考》](03-admin-api.md)。

### 4.4 RawCodec（pkg/transport/grpc/codec.go）

`RawCodec` 允许把已序列化的 protobuf 字节（rawFrame）直接发送，避免二次序列化；也兼容普通 `proto.Message`。其 `Name()` 返回 `"messageloop-proto"` 而不是默认的 `"proto"`，避免在进程级 codec 注册表中覆盖标准 proto codec，并采用每服务器 `ForceServerCodec` 注入。名称同时是内容子类型标签，须与 Go SDK 客户端（sdks/go/grpc.go）使用的 codec 名称一致。

### 4.5 Marshaler 与编码协商（shared/marshaler.go）

`Marshaler` 接口提供 `Marshal`/`MarshalAppend`/`Unmarshal`/`Name`：

| 实现 | Name | 说明 |
| --- | --- | --- |
| `JSONMarshaler` | `json` | proto 消息走 protojson（`UseProtoNames`），其他走 `encoding/json` |
| `ProtobufMarshaler` | `proto` | 二进制 protobuf |
| `ProtoJSONMarshaler` | `protojson` | protojson 专用实现，WebSocket 端默认回退项 |

WebSocket 子协议协商是精确匹配：服务端宣告 `messageloop`、`messageloop+json`、`messageloop+proto` 三种子协议，从客户端请求中选出第一个命中宣告集合的子协议；映射规则为精确 switch——只有 `messageloop+proto` 使用 `ProtobufMarshaler`（二进制帧），其余一切取值（含 `messageloop`、`messageloop+json` 与未知值）都使用 `ProtoJSONMarshaler`（文本帧），不做子串匹配（名称里含 "proto" 不会误选二进制）。gRPC 端固定 protobuf；QUIC/KCP 经 TLS ALPN 协商，`messageloop+proto` 为二进制，其余为 protojson。

## 5. 主题匹配（pkg/topics/）

`Matcher` 接口（matcher.go）定义三个操作：

```go
type Matcher interface {
    Subscribe(topic string, sub Subscriber) (*Subscription, error)
    Unsubscribe(sub *Subscription)
    Lookup(topic string) []Subscriber
}
```

主题以 `.` 与 `:` 分隔层级（`SplitSegments` 是唯一的共享切分实现），因此命名空间 channel `acme:chat.room1` 是三段 `[acme, chat, room1]`，`acme:*` / `acme:im.**` 的通配语义自然成立。`*` 匹配单个层级（如 `acme:chat.*` 匹配 `acme:chat.general`，不匹配 `acme:chat.rooms.1`）。命名空间语法（恰好一个 `:`、namespace 标识符规则）由 `ValidateChannel` / `NamespaceOf`（namespace.go）在会话边界执行；`ValidateTopic` 保持纯结构校验。

| 实现 | 文件 | 特点 |
| --- | --- | --- |
| `NewCSTrieMatcher` | cstrie.go | 基于 CAS 的无锁并发字典树（iNode/cNode/tNode 结构，原子指针切换），Hub 默认使用 |
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
  │                     │── 版本门（过低 → 3514）
  │                     │── 鉴权：FindProxy("","$authenticate")
  │                     │── 命名空间解析（代理响应优先，回退静态配置）
  │                     │── LookupSession(sessionId)
  │                     │── 旧会话存在（指针不动）：
  │                     │   跨用户先 PrepareSessionUser（限额检查+connShard 迁移）
  │                     │   → 旧会话 Detach（关旧附件，不 Leave、不撤订阅）
  │                     │   → Attach(新附件)（失败则 Close(Internal) 真走）
  │                     │   临时连接变成读循环 shell，委托给旧会话对象
  │                     │── AddClient + 连接指标计费（本机 resume 走 same-fence Bind）
  │                     │── 逐频道 命名空间/Authorizer/代理 → AddSubscription(saga)
  │                     │── 逐频道 History(offset+1)（epoch 重置则从头）
  │◄── Connected ───────┤      （裸 Connected：Resumed=true, StreamEpoch, Subscriptions）
  │◄── Publication×N ───┤      （replay=true，逐条 Send，受 MaxMessageSize 约束）
  │◄── RecoverComplete ─┤      （每频道一条：position/truncated/gap/gap_reason/error）
  │                     │
  │   连接中断（网络）   │
  │                     │   旧附件读循环结束时:
  │                     │   closeFn 按附件身份校验 → 不再匹配当前附件
  │                     │   → no-op（stale 保护，不驱逐被恢复的会话）
```

被抢（跨节点 takeover）：旧节点收 `Evict`/写路径 fence 后只准 `Fence`（撤本地订阅与 Hub 条目 + 关附件；不 Leave、不删集群条目），随后真走由新 owner 负责。

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
  │ (channel,method)  │── checkNamespace      │                  │
  │                   │── FindProxy ─────────►│ (glob 首匹配)    │
  │                   │◄── Proxy ─────────────┤                  │
  │                   │── ProxyRPC(rpcCtx) ────────────────────►│
  │                   │   （上下文含 rpcTimeout，代理层叠加）      │
  │                   │◄────────────── RPCResponse ─────────────┤
  │◄── RpcReply ──────┤（超时→RPC_TIMEOUT；无代理→NO_PROXY；        │
  │                   │  代理错误→PROXY_ERROR）                  │
```

### (d) Survey 请求 / 回复（Admin 与客户端两条路径）

```
Admin 路径：
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

客户端路径（handleSurvey）：
客户端 A              Node                         订阅客户端 B
  │── SurveyRequest ─►│                              │
  │ (channel,payload) │── 同步校验：channel 精确 /    │
  │   timeout_ms      │   命名空间 / sessionCovers /  │
  │                   │   策略 survey / Decide(Survey) │
  │                   │   限流（任一失败 → 顶层 Error）│
  │                   │── worker：count_only 集群预检  │
  │                   │── Node.Survey ── Outbound ───►│
  │                   │   SurveyRequest（服务端 request_id）
  │                   │◄──── Inbound SurveyReply ─────┤
  │◄── SurveyResult ──┤（异步，回显发起方 request_id；  │
  │                   │  读循环不被阻塞）              │
```

### (e) presence 加入 / 离开事件（Occupancy，只走 LiveBus 精确频道）

```
订阅客户端            Node                              LiveBus / 其他订阅者
  │── Subscribe ────►│                                  │
  │                  │── presence.Add(ch, info)         │
  │◄── SubscribeAck ─┤（含 presence 快照，含自己；       │
  │                  │  无 self-join 事件）              │
  │                  │── gen = nextOccupancyGen(ch)     │
  │                  │── PublishOccupancy(ch, join) ───►│ 跨节点：按 Interest
  │                  │                                 │ （精确 / im.** 编译）
  │                  │◄── onOccupancy(ch, join) ────────┤ 过滤；gen<=last_applied
  │                  │   deliverPresenceEvent 本机扇出   │ 弃迟到；
  │                  │   （精确 + matcher 通配命中，      │ 跳过 ephemeral 与事件主体，
  │                  │    按会话去重，events 不进历史）   │ 不计 Messages*
  │── Unsubscribe ──►│                                  │
  │                  │── presence.Remove                 │
  │                  │── PublishOccupancy(ch, leave) ───►│
  │                  │                                  │
  │                  │    仅 legacy_presence_channel=true 时额外写 ch/__presence 伴生
```

Occupancy 事件不是 Publication（走 broker 的实时 `occupancy` 消息类型，Redis 端与 `pub` 信封分开解析），不复用 `PublishTransient`，`Hub.broadcastPublication` 只扇 `Publication`。

### (f) 经管理 API 查询历史

```
管理工具                    Admin gRPC API               Node              Broker
  │── GetHistory ─────────►│                            │                  │
  │   (Bearer Token)       │── 鉴权拦截器 + 能力位检查     │                  │
  │                        │── node.Broker().History ───►                  │
  │                        │   (sinceOffset, limit)      │── 读取 ─────────►│
  │◄── HistoryResponse ────┤◄────────── publications ────┤◄── 环形缓冲/Stream│
```

## 7. 断连模型（internal/protocol/disconnect.go）

`Disconnect` 是携带 `Code` 与 `Reason` 的结构体并实现 `error` 接口：核心代码以返回错误的方式表达"应断开此连接"，`Session.HandleMessage` 用 `errors.As` 识别后调用 `close(disconnect)`，传输层把 Code 与 Reason 交给客户端（WebSocket 用 close 帧的 code/reason；gRPC 用 `DISCONNECT_ERROR` 错误信封，数值码编码在 metadata 的 `disconnect_code` 条目中）。`Code` 为 0 时表示正常关闭（WebSocket 端映射为 1000）。

| Code | 常量 | 含义 |
| --- | --- | --- |
| 3000 | `DisconnectConnectionClosed` | 连接被关闭，无服务端建议；可能是干净断开，也可能是网络中断，服务端无法区分 |
| 3500 | `DisconnectInvalidToken` | token 无效、`require_auth` 下未携带 token、命名空间无法解析或跨命名空间 resume |
| 3501 | `DisconnectBadRequest` | 协议帧格式错误（如重复 Connect） |
| 3502 | `DisconnectStale` | 会话 fencing 失效：集群租约被其他节点抢占、恢复/接管失败回滚、或写路径检出本附件已陈旧 |
| 3503 | `DisconnectForceNoReconnect` | 服务端要求客户端不要重连（如关停排空） |
| 3504 | `DisconnectConnectionLimit` | 超过每用户连接数上限 |
| 3505 | `DisconnectChannelLimit` | 超过每客户端订阅数上限 |
| 3506 | `DisconnectInappropriateProtocol` | 传输无法承载的数据（当前无触发点，保留定义） |
| 3507 | `DisconnectPermissionDenied` | 权限不足（当前无触发点，保留定义） |
| 3508 | `DisconnectNotAvailable` | 服务端无法处理该消息类型（当前无触发点，保留定义） |
| 3509 | `DisconnectTooManyErrors` | 客户端产生过多错误（当前无触发点，保留定义） |
| 3511 | `DisconnectIdleTimeout` | 心跳检测到超时未活动 |
| 3512 | `DisconnectSlowConsumer` | 客户端消费速度跟不上（写失败或写队列满触发） |
| 3513 | `DisconnectInternal` | connect 路径内部错误（如集群状态同步失败），连接被强制关闭 |
| 3514 | `DisconnectUnsupportedVersion` | Connect 携带的协议版本低于服务端版本门（internal/protocol/version.go） |

3510 在代码中未定义。客户端如何解读这些代码（重连策略、错误展示）见 [《客户端协议参考》](../protocol.md) 的 Disconnect Codes 一节。

## 8. 并发模型

- **64 分片**：`connShards` 与 `subShards` 各 64 个分片（`numHubShards`），`index()` 用 FNV-64a 哈希取模路由。连接操作只锁目标分片与 `hub.mu`（会话 map），订阅操作只锁目标频道分片，互不干扰；`BroadcastPublication` 在分片锁内拷贝订阅者列表后释放锁再发送，避免长持有锁。
- **16384 把订阅锁**：`Node.subLocks`（`numSubLocks`）按频道哈希为订阅变更（`AddSubscription`/`RemoveSubscription`）串行化，配合 Saga 保证同一频道的并发订阅/退订不会交错破坏 broker 订阅计数；高冲突频道之间几乎不互相阻塞。
- **无锁主题匹配**：`CSTrieMatcher` 用原子指针与 CAS 实现无锁并发字典树（写操作复制路径节点后 CAS 切换，读操作失败自旋重试），通配符订阅的增删查不依赖全局锁；Hub 侧仅用 `wcSubsMu` 保护注册表本身。
- **内存 broker 分片投递**：发布投递按频道哈希分给 64 个串行 worker，同频道保序；分片队列满时对发布方背压，慢 handler 只影响同分片频道。
- **写缓冲池**：`internal/session/pool.go` 的 `sync.Pool` 提供初始容量 4096 的字节缓冲，`write` 经 `MarshalAppend` 就地复用缓冲，gRPC 传输在入队时拷贝以适配池化复用。
- **广播限流**：订阅者 ≤ 8 时串行发送，超过则并发发送但并发数封顶 `broadcastParallelLimit`（64），防止超大频道的广播产生无界 goroutine。

## 9. 集群概述

集群模式是可选的 Redis 支撑分布式控制面：节点间通过 Redis 共享会话目录、命令总线与查询投影，支持跨节点会话恢复、远程订阅/退订/断开、远程发布与集群级 Survey，并由节点租约与投影修复机制维护最终一致。集群模式要求 `broker.type=redis`（配置校验强制）。

本文档不展开集群细节；设计、拓扑与运维说明见[《分布式集群指南》](04-cluster.md)。

## 10. 模块布局

| 路径 | 内容 |
| --- | --- |
| 仓库根（doc.go） | 模块根空壳：无导出符号 |
| internal/runtime/ | 编排层：`node.go`、`defaults.go`、`session_runtime.go`、`health.go`、`subscription_saga.go`、`recover.go`、Cluster 门面 `cluster.go` / `cluster_commands.go` / `cluster_state.go` / `cluster_resume.go` / `cluster_repair.go`、user 索引 `cluster_user_index.go`、Sim 钩子 `cluster_sim.go` |
| internal/session/ | Session Plane：`session.go`、`client.go`、`hub.go`、`namespace.go`（命名空间守卫）、`heartbeat.go`、`pool.go`、`transport.go`、`runtime.go`（`Runtime` 缝） |
| internal/stream/ | Broker 契约与内存实现：`broker.go`、`broker_memory.go`、`publication.go`、`defaults.go` |
| internal/channel/ | 频道策略默认值（`channel_policy.go`）与 Interest 编译（`interest.go`） |
| internal/occupancy/ | Occupancy 叶子类型：`occupancy.go`、`presence.go`、`presence_event.go`、`defaults.go` |
| internal/survey/ | Survey 叶子类型（编排留在 `internal/runtime` Node 上） |
| internal/authz/ | Authorizer（`authorizer.go`）与求值辅助 |
| internal/protocol/ | 协议常量：`disconnect.go`（断连码）、`version.go`（协议版本门） |
| internal/cluster/ | 集群控制面契约（`contracts.go`、`state.go`、`epoch.go`、`user_index.go`）与子包 `hmac/`、`sim/` |
| internal/admin/ | 管理 gRPC API：`admin_server.go`、`api_handler.go` |
| internal/metrics/ | Prometheus 指标定义（`metrics.go`） |
| cmd/server/ | 可执行入口：`main.go`（装配与监听器）、`runtime.go`（gRPC 预绑定与启动顺序）、`envconfig.go`（`MESSAGELOOP_*` 环境变量覆盖） |
| config/ | 配置结构（`config.go`）与校验 |
| shared/ | 独立 Go 模块：marshaler 实现（`marshaler.go`）、流帧与 ALPN 常量（`streamframe.go`）与生成的 protobuf 代码（`shared/genproto/`） |
| protocol/ | protobuf 源定义（client/、server/、shared/、proxy/ 四域，单一 buf module） |
| pkg/transport/ws/ | WebSocket 传输：`server.go`、`handler.go`、`transport.go` |
| pkg/transport/grpc/ | gRPC 流传输：`client_server.go`、`server.go`、`handler.go`、`transport.go`、`codec.go` |
| pkg/transport/quic/ | 可选 QUIC 传输：`server.go`、`handler.go`、`tls.go`、`transport.go` |
| pkg/transport/kcp/ | 可选 KCP 传输：`server.go`、`conn.go`、`handler.go`、`tls.go`、`transport.go` |
| pkg/topics/ | 主题匹配与命名空间：`matcher.go`、`cstrie.go`、`trie.go`、`naive.go`、`inverted_bitmap.go`、`optimized_inverted_bitmap.go`、`namespace.go` |
| pkg/redisbroker/ | Redis Broker（`redis.go`、`pubsub.go`、`history.go`、`options.go`、`message.go`、`client.go`）、Redis Presence（`presence_redis.go`）与集群支撑（`cluster_directory.go`、`cluster_command_bus.go`、`cluster_query_store.go`） |
| proxy/ | RPC 代理：`proxy.go`、`router.go`、`http.go`、`grpc.go` |
| sdks/go/ | Go SDK（`client.go`、`websocket.go`、`grpc.go`、`quic.go`、`kcp.go`、`message.go`、`presence.go`、`survey.go`、`gap.go`、`disconnect.go`、`proxy.go`、`mux.go`、`options.go`）与示例（`example/`） |
| sdks/ts/ | TypeScript SDK（`src/` 与示例 `examples/`） |
