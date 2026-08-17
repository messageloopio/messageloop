# 分布式集群指南

本文档描述 MessageLoop 的分布式集群（distributed cluster）机制：多个服务端节点如何通过共享的 Redis 构成一个逻辑集群，以及会话归属、远端接管、集群级 Survey、投影修复、Presence 聚合等集群特有行为的原理与运维要点。文中所有类型名、方法名、Redis 键前缀、默认值与行为均以仓库源码为准。

配套文档：[《架构指南》](01-architecture.md)（通用架构）、[《配置参考》](02-configuration.md)（全部配置字段）、[《管理 API 参考》](03-admin-api.md)、[《可观测性指南》](05-observability.md)、[《开发指南》](06-development.md)，以及[《客户端协议参考》](../protocol.md) 与[《部署指南》](../deployment.md)。

## 1. 概述

MessageLoop 可以以两种形态运行：

- **单节点（single node）**：一个进程承载全部客户端连接。默认使用进程内内存 broker（`broker.type: memory`），发布/订阅、历史、在线状态全部局限在本进程内。
- **多节点（multi node）**：多个进程组成集群，客户端连接分散在不同节点上，但共享同一套消息管道与在线状态。集群模式要求 `broker.type: redis`（配置校验强制，见 `config/config.go` 的 `Validate()`，报错信息为 `cluster requires broker.type=redis`）。

这里必须区分两个概念，它们经常被混淆：

| 概念 | 配置开关 | 作用 |
| --- | --- | --- |
| **Redis broker** | `broker.type: redis` | 消息管道：发布经 Redis Streams 写历史、经 Redis Pub/Sub 实时分发，所有节点共享同一份历史与实时流量 |
| **集群控制面（cluster control plane）** | `cluster.enabled: true` | 节点间协调：会话目录（session directory）、命令总线（command bus）、频道投影（query store）、节点租约（node lease）、投影修复（projection repair），实现跨节点会话管理与集群级操作 |

启用 `broker.type: redis` 但 `cluster.enabled: false` 时，各节点仍然共享消息与历史（例如多个无状态节点前端挂同一个 Redis），但节点之间互不感知：会话属于连接所在节点，管理操作只作用于本节点。只有 `cluster.enabled: true` 才开启分布式控制面——节点彼此发现、会话可以跨节点接管、Survey 与频道查询是全集群范围的。`cluster.enabled` 是控制面的总开关，这一点请与 `broker.type` 区分清楚。

适用场景：

- 单节点无法承载全部在线连接，需要横向扩容，但要求会话在节点间可迁移、断线可在任意节点恢复；
- 需要集群级管理操作：远程断开/订阅/退订、跨节点会话定向投递、全集群 Survey；
- 需要全集群统一的频道列表与在线状态视图。

代价是引入对 Redis 的强依赖（见第 9 节故障与恢复）。

## 2. 开启条件与配置

### 2.1 前提

1. `broker.type: redis` 且 `broker.redis.addr` 已配置。集群控制面与 broker 共用同一个 `broker.redis` 配置段（`addr` / `password` / `db` 等，见 `cmd/server/main.go` 的 `setupCluster`），不单独配置 Redis 连接。
2. 集群内所有节点的 `broker.redis` 必须指向**同一个 Redis 实例与同一个 DB**，共享同一命名空间（所有键以 `ml:` 前缀隔离，见第 3 节）。指向不同实例或 DB 的节点彼此不可见。
3. 每个节点必须有全局唯一的 `cluster.node_id`。

### 2.2 cluster 配置段

配置结构见 `config/config.go` 的 `ClusterConfig`，只有三个字段：

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `cluster.enabled` | bool | `false` | 集群控制面总开关 |
| `cluster.node_id` | string | 无 | 逻辑节点标识，集群内必须唯一；启用集群时必填（`cluster.go` 的 `normalize()`，缺失时报 `cluster node_id is required when cluster is enabled`） |
| `cluster.backend` | string | `redis` | 控制面后端；留空默认 `redis`；接受 `redis` / `memory` / `noop`，其他值报 `unsupported cluster backend` |

`backend` 的取值说明（`cluster.go:22-54`）：

- `redis`：唯一在服务端二进制中接入实际实现的取值。启用时装配会话目录、命令总线、查询投影、节点租约与投影修复，并把 Presence 存储替换为 Redis 实现（`cmd/server/main.go:111-142`）。
- `memory` / `noop`：no-op 组件，进程内 API 使用或测试用；控制面各接口退化为本地行为（例如命令总线返回 `ErrClusterCommandUnsupported`，见 `cluster_state.go`）。

另外，`ClusterOptions` 中还有自动生成的 `IncarnationID`（进程实例标识）：每次进程启动生成一个随机 UUID，不来自配置。节点的完整身份是 `(NodeID, IncarnationID)` 二元组，旧进程重启后会得到新的 `IncarnationID`，从而与旧实例的租约区分开。

### 2.3 两节点配置示例

仓库根目录的 `config-node1.yaml` 与 `config-node2.yaml` 是双节点演示的基础配置：两个节点监听不同端口（节点一 WebSocket `:19080` / gRPC `:19090` / 管理 HTTP `:18080`；节点二 `:29080` / `:29090` / `:28080`），并指向同一个 Redis 实例（`127.0.0.1:6379`、DB 10）。注意这两个文件本身**尚未包含 `cluster` 段**——按 `config-example.yaml` 的字段补齐后才能启用集群。

节点一（`config-node1.yaml` 内容基础上补充）：

```yaml
server:
  http:
    addr: ":18080"
  heartbeat:
    idle_timeout: "300s"
  rpc_timeout: "10s"

transport:
  websocket:
    addr: ":19080"
    path: "/ws"
  grpc:
    addr: ":19090"

broker:
  type: redis
  redis:
    addr: 127.0.0.1:6379
    password: "123456"
    db: 10

cluster:
  enabled: true
  node_id: node-1
  backend: redis
```

节点二（`config-node2.yaml` 内容基础上补充，端口换为 28/29 前缀，`node_id` 不同）：

```yaml
server:
  http:
    addr: ":28080"
  heartbeat:
    idle_timeout: "300s"
  rpc_timeout: "10s"

transport:
  websocket:
    addr: ":29080"
    path: "/ws"
  grpc:
    addr: ":29090"

broker:
  type: redis
  redis:
    addr: 127.0.0.1:6379
    password: "123456"
    db: 10

cluster:
  enabled: true
  node_id: node-2
  backend: redis
```

两个节点除监听端口与 `node_id` 外其余配置相同，这是集群部署的典型形态：客户端可连任意节点的任意端口。

## 3. 架构与数据流

### 3.1 控制面组件与生命周期

`Cluster`（`cluster.go`）是控制面组件的生命周期协调器，持有五个组件（接口定义见 `cluster.go`，Redis 实现见 `pkg/redisbroker/`）：

| 组件 | 接口 | Redis 实现 | 职责 |
| --- | --- | --- | --- |
| 会话目录 | `SessionDirectory` | `pkg/redisbroker/cluster_directory.go` | 节点租约、会话租约与会话快照的读写，CAS 会话租约交换 |
| 命令总线 | `ClusterCommandBus` | `pkg/redisbroker/cluster_command_bus.go` | 节点间命令投递（定向与广播）、结果回传、命令去重 |
| 查询存储 | `ClusterQueryStore` | `pkg/redisbroker/cluster_query_store.go` | 每节点频道的订阅计数投影（hash）与全集群聚合 |
| 节点租约管理 | `ClusterNodeLeaseManager` | 通用实现（`cluster_state.go`） | 周期性续租本节点存活记录 |
| 修复器 | `ClusterRepairer` | 通用实现（`cluster_repair.go`） | 单一控制面循环：周期性从本地 hub 全量重建本节点投影、收割死节点投影、重建 user→sessions 索引，并由短周期节点租约 SCAN 驱动 membership `OnLeave` |

`node.Run` 启动时先启动集群（`Cluster.Start`，`cluster.go`）：按 会话目录 → 命令总线 → 查询存储 → 节点租约 → 修复器 的顺序启动；任一组件启动失败时，已启动的组件按逆序回滚关闭（回滚受 5 秒超时约束，`clusterStartRollbackTimeout`），实例保持可重试（后续 `Start` 会重新启动全部组件）。`Node.Shutdown` 关闭集群时按逆序关闭全部组件。

`Cluster.Start` 对每个 Redis 组件先执行 `Ping` 预检：任何组件连不上 Redis，节点启动即失败——集群模式下 Redis 不可用是启动级故障。

### 3.2 节点注册与发现（节点租约）

每个节点的存在性由一个**节点租约（node lease）**记录表达（`ClusterNodeLease`：`NodeID`、`IncarnationID`、`StartedAt`、`ExpiresAt`）：

- **键**：`ml:cluster:node:<nodeID>:<incarnationID>`（`cluster_directory.go` 的 `nodeLeaseKey`），值为租约 JSON。
- **TTL**：90 秒（`defaultClusterNodeLeaseTTL`）。
- **续租**：`ClusterNodeLeaseManager`（`cluster_state.go:347-418`）启动时立即写一次租约，之后每 30 秒（`defaultClusterNodeLeaseRenewInterval`）续租一次；续租失败只记警告日志，不中断节点运行（节点租约随后到期）。
- **离开**：节点没有主动注销机制——进程退出后，其租约在 90 秒内自然过期消失（优雅关闭 `Cluster.Shutdown` 只是停止续租）。修复器的 membership 节拍（默认 5 秒 ±20% 抖动的节点租约 SCAN，见第 6 节）发现某 incarnation 从存活集合中消失（或租约已过期）即触发 `OnLeave`：立即删除该死 incarnation 名下的全部会话租约（作废其 fencing，同步清理 user 索引）并删除其 owner 投影，**不必等 600 秒会话 TTL**；宽限期 = 一次 SCAN 周期。节点自身的 incarnation 永远不会被自己 OnLeave。

节点发现通过**扫描节点租约键**实现：命令总线的 `BroadcastCommand` 用 `SCAN ml:cluster:node:*` 枚举存活节点（`cluster_command_bus.go:274-339`），每个租约键对应一个可投递目标。租约过期即从广播目标中消失。节点身份是 `(NodeID, IncarnationID)`，同一 `node_id` 的旧实例（已重启产生新 `IncarnationID`）会与新实例并存一段时间，直到旧租约过期——期间广播可能向同一逻辑节点的两个实例各投递一份命令，但已死的旧实例不会应答，其命令以超时/失败结果呈现（见 3.3 的去重与超时语义），不产生实际副作用。

### 3.3 命令总线

命令总线（`pkg/redisbroker/cluster_command_bus.go`）是集群控制面的神经系统，基于 Redis Pub/Sub 实现请求/应答式的节点间命令投递。

**Redis 键**：

| 键 | 用途 |
| --- | --- |
| `ml:cluster:cmd:req:<nodeID>:<incarnationID>` | 请求通道：发给指定节点实例的命令发布到该通道；每个节点实例在启动时 `SUBSCRIBE` 自己的通道 |
| `ml:cluster:cmd:reply:<commandID>` | 应答通道：每个命令生成一个随机 UUID 应答通道，命令发布前把通道名写入命令元数据（`reply_channel`） |
| `ml:cluster:cmd:state:<commandID>` | 命令状态键：持久化命令的终态结果，也是命令去重（dedupe）的依据 |

**发送流程（`SendCommand`）**：

1. 生成 `CommandID`（未指定时），标记 `IssuedBy`（发送方 NodeID，仅审计用途，见下方信任边界），打上 `IssuedAt`。
2. 查询 `ml:cluster:cmd:state:<commandID>`：若已有终态结果，直接返回（去重命中）；若处于 `pending`，返回 `in_progress`。
3. 创建随机应答通道并 `SUBSCRIBE`，把通道名写入命令元数据（`reply_channel`）。
4. **签名**：`SignCommand` 以节点配置的 HMAC 密钥对命令的规范字节（`internal/cluster/hmac`，逐字节固定的行式编码，不含 `IssuedBy`）计算 `hex(HMAC-SHA256)` 写入 `Signature`；签名失败则不发布。
5. 把命令 JSON（含 `Signature`）发布到目标节点的 `req:` 通道。
6. 在应答通道上等待 `CommandID` 匹配且**验签通过**的结果；伪造/未签名/偏斜的应答被记入指标并当作未收到继续等待；`CommandID` 不匹配的应答记录警告并继续等待（防御性处理）。
7. 等待受命令级超时约束：调用方上下文无 deadline 时默认 5 秒（`defaultCommandTimeout`）。超时后先查状态键——若已有终态结果则返回该结果；否则返回 `unknown_final_state` 并计入指标（见第 10 节）。

**接收与执行（`handleMessage`）**：每个节点实例消费自己的 `req:` 通道，处理并发上限为 128（`clusterCommandHandlerConcurrency`）——读者循环在信号量上阻塞后才派发，饱和时排队而不是丢命令。**HMAC 硬门在最前**：未签名（`missing`）、坏签（`bad`）、`IssuedAt` 超出 ±30 秒时钟窗（`skew`）或无 `CommandID`（`id`）的命令直接拒绝——不 claim、不执行 handler、不写去重状态键、不应答，只计入 `messageloop_cluster_command_hmac_reject_total{reason}` 并记警告日志。通过验签后才在状态键上 `SETNX` 抢占（claim），TTL 30 秒（`clusterCommandClaimLeaseTTL`），处理期间每 10 秒续租（`renewClaimLease`）；执行完成（或失败）后把终态写入状态键，TTL 10 分钟（`defaultCommandStateTTL`），停止续租，再把**签过名**（`SignResult`，带 `IssuedAt`）的结果发布到 `reply_channel`。每个处理器执行受 10 秒 deadline 约束（`clusterCommandHandlerTimeout`），超时返回 `CLUSTER_COMMAND_TIMEOUT`，卡死的处理器不会把命令钉死在 `pending`。

**命令去重（command dedupe）**：同一 `CommandID` 的命令可能因重试、广播重复投递而多次到达，去重发生在两个环节：

- 发送方：重发同一 `CommandID` 时，`resolveExistingCommand` 直接返回状态键中已存储的终态结果（或 `in_progress`），不再发布（`cluster_command_bus.go:483-499`）。
- 接收方：`claimCommandExecution` 用 `SETNX` 抢占状态键。抢占失败说明另一实例正在处理，向应答通道回 `in_progress`（`COMMAND_IN_PROGRESS`）而不重复执行（`cluster_command_bus.go:501-542`）。若旧执行者崩溃，claim 租约 30 秒后过期，后续到达的命令可以重新抢占，而不是被钉在 `pending` 直到 10 分钟终态 TTL。

去重命中与超时、`unknown_final_state` 均计入指标（见第 10 节）。注意去重的粒度是 `CommandID`，广播命令（`BroadcastCommand`）为每个目标节点复制命令并重新生成 `CommandID`（`cluster_command_bus.go:303-305`），因此广播不会误去重。

**信任边界（trust boundary，PR-KA-B4 起）**：集群命令与应答经 Redis Pub/Sub 传输，由 **HMAC-SHA256 硬门**保护（`internal/cluster/hmac`）：密钥只来自节点配置（`cluster.hmac_key` 或 `cluster.hmac_key_file`，至少 32 字节，`enabled` 时缺一即拒绝启动；bus 的 `Start` 再挡一层），**从不写入任何 Redis 键、PUBLISH 载荷、日志或指标标签**。能写 Redis 不再等于能签发集群命令：未签名/坏签/偏斜的命令在 claim 之前被拒，伪造的 `succeeded` 应答不会让 `SendCommand` 成功。`IssuedBy` 字段只用于日志审计追溯（可伪造，不在规范字节内），不是安全边界。Redis 的网络隔离仍是纵深防御手段，但不再是唯一边界。

### 3.4 节点间命令路由

节点间命令路由以**会话租约**为索引（`cluster_commands.go` 的 `dispatchSessionCommand`）：

1. `resolveSessionLease(sessionID)`：先查本地 hub（`LookupSession`），命中则用本地状态构造租约；未命中且集群启用时查会话目录（`GetSessionLease`）。
2. 目标即租约中的 `(NodeID, IncarnationID)`。若目标就是本节点（或集群未启用），直接在本地执行 `handleClusterCommand`；否则经命令总线 `SendCommand` 路由到目标节点执行。

命令类型（`ClusterCommandType`）：`disconnect`、`subscribe`、`unsubscribe`、`publish`、`takeover`、`survey`。命令结果状态（`ClusterCommandStatus`）：`pending`、`succeeded`、`failed`、`in_progress`、`unknown_final_state`。

远程 `subscribe`/`unsubscribe` 执行时还会附带本地副作用：presence 登记/清除与 join/leave 事件的发布（`cluster_commands.go:202-240`），因此经管理 API 远程订阅的会话在全集群的 presence 视图中同样可见。

## 4. 会话归属与接管

### 4.1 会话所有权模型

每个客户端会话在集群中有一份**会话租约（session lease）**与一份**会话快照（session snapshot）**，由会话目录存储：

| 数据 | 键 | TTL | 内容要点 |
| --- | --- | --- | --- |
| 会话租约 | `ml:cluster:session:lease:<sessionID>` | 默认 600 秒；按心跳配置缩短（`sessionLeaseTTL` = `max(30s, 2×idle_timeout, 3×ping_interval, idle_timeout+10s+10s)`，须覆盖心跳周期并留出续约抖动余量；心跳禁用时保持 600s） | `SessionID`、`NodeID`、`IncarnationID`、`UserID`、`ClientID`、`LeaseVersion`、`Authenticated`、`ConnectedAt`、`LastActivityAt`、`ExpiresAt` |
| 会话快照 | `ml:cluster:session:snapshot:<sessionID>` | 24 小时（`defaultClusterSessionSnapshotTTL`） | 会话身份（user/client/protocol）、订阅列表（`Subscriptions`）、`AuthContext`；另含逐频道 `ChannelOffsets`（上次成功投递的历史 offset）与 `BrokerEpoch`（快照时刻的 broker 世代），供精确跨节点恢复（见 4.4） |

会话所有权 = 「会话租约指向的节点实例正在服务该会话」。`LeaseVersion` 是所有权代际计数：新连接从 1 起，每次 resume/takeover 递增（`client.go`、`cluster_resume.go`）。它被用于接管时的版本校验，防止旧代际的接管命令误伤新代际的会话。

**会话租约的写入只走 CAS，没有盲写（PR-KA-A1）**。`syncClusterSessionState`（`cluster_state.go`）是唯一的热路径写入方，它的三种情形：

- **首次登记**：Directory 中无该 session 的租约 → `CompareAndSwapSessionLease(expected=nil)` 抢注（版本 1）。
- **same-fence 续约**：租约仍指向本节点实例且版本与本地一致 → `CompareAndSwapSessionLease(expected=当前租约)` 刷新 TTL / `LastActivityAt` / `UserID` 等，**`LeaseVersion` 不递增**。无条件 `SET` 的 lease put 方法已从 `SessionDirectory` 接口与全部实现中删除（PR-KA-B4）：盲写会把已被他节点 CAS 抢走的所有权写回去，任何分支都不允许绕过 CAS。
- **fencing 失效（`ErrSessionFenced`）**：Directory 上的租约已不属于本节点实例（被其他节点 CAS 抢走），或版本比本地更新（本附件已陈旧）→ 返回错误，**不写回**。ping/pong 刷新路径收到该错误即用 3502（`DisconnectStale`）断开本连接，且**不删除** Directory 里的租约（那会误删新 owner 的 fencing）。

版本的唯一递增点在 `resumeRemoteSession` 的抢权 CAS（旧版本 +1 后原子写入）；本机接管（同节点 resume）把内存版本 +1 后经同节点的 CAS 写透，续约本身从不 `+1`。

会话状态的写入时机（`syncClusterSessionState`）：

- 连接建立（`AddClient`）；
- 每次订阅/退订（订阅 Saga 的 `cluster.session` 步骤，`node.go`，受 2 秒 `clusterStepTimeout` 约束，失败不阻塞客户端操作路径）；
- 客户端 ping/pong 触发的状态刷新，节流为最多每 10 秒一次（`pingClusterRefreshInterval`）。刷新只做 same-fence CAS，且检测到 fencing 失效（`ErrSessionFenced`）时以 3502 断开，其余错误维持 Warn 不断开（避免 Redis 抖动踢光全员）。

会话关闭时的清理（`deleteClusterSessionState`，`cluster_state.go:217-242`）带所有权检查：只有租约已过期、或租约确属本节点实例时才删除租约与快照；若租约仍有效且属于**其他**节点实例，说明该会话已被他处接管，本地状态必须保留。

### 4.2 本地接管（同节点 resume）

客户端携带 `SessionId` 重连且旧会话仍在同一节点的 hub 中时（`client.go` 的 `handleConnect`）：

1. 复制旧会话状态（用户、客户端标识、订阅频道、租约版本）；
2. `closeQuiet` 静默关闭旧连接（不触发 presence 清理、不删 hub 条目）；
3. `Hub.ReplaceSession` 原子替换会话指向的新 `Client`；
4. 新连接的租约版本 = 旧版本 + 1。

### 4.3 远端接管（remote takeover）

客户端携带 `SessionId` 重连，但旧会话不在本节点（本地 `LookupSession` 未命中）时，走跨节点恢复路径 `resumeRemoteSession`（`cluster_resume.go:34-88`）：

1. 读会话租约与会话快照；两者缺一即放弃恢复（返回未恢复）。
2. 若租约有效且属于其他节点实例：向该节点发送 `takeover` 命令（`requestSessionTakeover`，`cluster_resume.go:90-110`）。takeover 命令携带 `LeaseVersion` 与元数据 `new_node_id` / `new_incarnation_id`；目标节点执行 `handleClusterTakeoverCommand`（`cluster_commands.go:242-267`）：先校验 `LeaseVersion` 与本地一致（不一致返回 `LEASE_VERSION_MISMATCH`），再 `evictSessionForTakeover` 驱逐旧连接。
3. **接管失败时的降级**：若 takeover 命令失败（例如目标节点刚宕机、命令超时），则检查目标节点的节点租约——节点租约也已不存在时，视为旧节点已死，继续执行恢复；节点租约仍在则中止恢复，并把抢占到的租约 **CAS 回滚**到原 owner（把 fencing 还回去，`cluster_resume.go` 的 `rollbackSessionTakeover`）；节点租约查询本身失败时同样先尝试回滚再返回错误。
4. 恢复成功后在本地重建会话状态：身份字段、订阅集合、`clusterLeaseVersion = 旧租约版本 + 1`，并 `AddClient` 注册；随后 `restoreSessionSubscriptions`（`cluster_resume.go:112-127`）逐频道重建订阅 + presence 登记 + 本节点投影 +1，任一频道失败则回滚已恢复的频道（含投影补偿 -1）。

**驱逐（`evictSessionForTakeover`，`cluster_resume.go:196-249`）**：标记旧连接关闭、取消心跳、逐个移除其全部频道订阅并同步投影 -1；任何频道移除失败都会把已移除的频道整体回滚（恢复订阅 + 投影 +1），保证不留下"半驱逐"状态；最后从 hub 移除会话并关闭传输。集成测试 `TestClusterRedis_RemoteResumeTakeover` 验证了完整链路：node B 上的新连接把 node A 上的旧连接驱逐，新连接收到 `Connected{Resumed: true}` 且订阅被恢复。

### 4.4 跨节点恢复与 epoch 校验

跨节点恢复中，**历史消息的续读**走客户端协议原有的 epoch 校验逻辑（`client.go:600-689`）：Redis broker 的 epoch 存于固定键 **`ml:broker:epoch`**（`defaultEpochKey`，`options.go:18`），首个启动的节点经 `SETNX` 写入随机 UUID（`redis.go` 的 `initEpoch`），之后所有节点读取同一值——**集群共享、跨节点一致、跨重启持久**（`epoch_test.go` 的 `SharedAcrossNodes` / `PersistedAcrossRestart` 两测试为证）。订阅者请求恢复（`sub.Recover`）时，携带的 `sub.Epoch` 与当前节点的 broker epoch 不一致（包括未携带 epoch）即视为 offset 无效，从历史开头（offset 0）恢复；epoch 匹配则从 `sub.Offset+1` 续读。

集群部署下的推论：由于 epoch 是集群共享的，客户端在节点 A 建立订阅时拿到的 epoch 在节点 B 依然有效——跨节点恢复时**epoch 校验可以通过**，续读位置由服务端快照中的逐频道 `ChannelOffsets` 决定（`client.go`、`recover.go`）：`ChannelOffsets` 记录本节点对该会话逐频道**最后一次成功投递**的历史 offset（由 hub 广播路径的 `DeliveredOffset` 填充，`hub.go`），跨节点恢复时从 `ChannelOffsets[ch]+1` 续读，**服务端记录优先于客户端携带的 offset**；快照缺失该频道的 offset（从未投递过历史或纯瞬时消息）则跳过恢复。快照同时携带 `BrokerEpoch`（快照时刻的 broker 世代），世代与当前不一致时强制全量恢复。全量恢复仅发生在客户端未携带 epoch（旧 SDK）、携带陈旧 epoch（epoch 键被清理/重建）或快照世代不匹配时。

### 4.5 按 user 展开的用户索引（user index）

管理 API 支持按 `user_id` 对用户的**全部 session** 做 Publish / Disconnect / Subscribe / Unsubscribe（PR-06，见 [《管理 API 参考》](03-admin-api.md)）。展开 = 本地 `Hub.SessionsByUser` ∪ 集群 user 索引，随后对每个 session 校验 lease 的 `UserID`（本地客户端则用 `Client.UserID()`，即 KD-13：**索引不是权威**），最后复用现有 session 级命令（`PublishToSession` / `DisconnectSession` / `SubscribeSession` / `UnsubscribeSession`），不新增集群命令类型。

**本地索引**（`hub.go` 的 `SessionsByUser`）：遍历 `connShard.users` 中该 user 所在分片（user 已按 `index(userID)` 分片）。空 user_id 的匿名连接不进入按 user API（`SessionsByUser("")` 恒为空；Admin 侧空 user_id 直接 `InvalidArgument`，不做扫描）。

**集群索引**（`SessionDirectory` 新增 `AddUserSession` / `RemoveUserSession` / `ListUserSessions`，Redis 实现见 `pkg/redisbroker/cluster_directory.go`）：

| 键 | 类型 | TTL | 说明 |
| --- | --- | --- | --- |
| `ml:cluster:user:member:<userID>:<sessionID>` | string（值 `"1"`） | 与 session lease 相同（`sessionLeaseTTL()`） | 成员键：续期时随 lease 一起刷新 |
| `ml:cluster:user:sessions:<userID>` | set | 无（成员过期靠 repair 修剪） | 用户→session 集合；展开时 `SMEMBERS` 后逐个 `GET` lease 校验 |

**维护**：所有 lease 写入路径共用单一 helper `SyncUserIndex`（根包 `cluster_user_index.go`），由 Redis directory 在 lease 写（`CompareAndSwapSessionLease` 成功——唯一的写入方式）/ `DeleteSessionLease` 之后调用（Delete 先 `GET` lease 以得知 user 再 `SREM`）：

- Delete（`newLease == nil`）：`RemoveUserSession(旧 user, session)`；
- CAS 成功：user 相同 → `AddUserSession`（刷新 TTL）；user 变了 → 先 `RemoveUserSession` 旧 user 再 `AddUserSession` 新 user（resume 后 re-auth 换 user 的场景，CAS 以旧 lease 为 `expected` 完成比对）；
- 空 `UserID`：只 Remove，匿名 session 不进索引。

索引写失败是 best-effort（记录警告，不影响 lease 本身）：索引是提示，陈旧条目靠 repair 与展开时的 lease 校验收敛。

**修复**：user 索引重建并入统一修复器 `clusterRepairer`（根包 `cluster_repair.go`，PR-KA-B4，`NewCluster` 自动装配——directory 实现 `ClusterSessionLeaseLister` 才生效，否则跳过该项工作）。每 30 秒一轮：`SCAN ml:cluster:session:lease:*`（复用 `scanKeys`，`pkg/redisbroker/cluster_query_store.go`），读 lease JSON，对非空 `UserID` 以 lease 剩余 TTL `AddUserSession`。集群未启用时不运行。

**禁止**：索引 miss 时做全集群 SCAN（热路径）。陈旧索引靠 repair 收敛；展开时 `GetSessionLease` 校验兜底（投毒/过期条目被跳过）。跨节点验证：`TestAdmin_DisconnectUsersAcrossNodes`、`TestClusterRedis_ResumeUserChangeMigratesIndex`（`cluster_redis_integration_test.go`）。

## 5. 集群级 Survey

单节点 Survey（`Node.Survey`）只向**本节点**的频道订阅者发送请求并收集应答（`localSurvey`，`node.go:607-647`）。

集群模式下（`node.go:572-605`）`Survey` 变为两步：

1. **本地调查**：`localSurvey` 正常执行（含发送超时 10 秒、应答会话白名单校验、注册表上限 1000 等既有语义），结果为每条应答标注本节点的 `NodeID` / `IncarnationID`（`annotateSurveyResults`）。
2. **集群广播**：经命令总线 `BroadcastCommand` 发送 `ClusterCommandSurvey`，元数据携带 `exclude_self=true` 与 `survey_timeout_ms`（调用方超时换算成毫秒）。广播目标由扫描节点租约键得出（见 3.2），因此只覆盖当前存活的节点实例。

远端节点执行 `handleClusterSurveyCommand`（`cluster_commands.go:269-293`）：在其本地执行 `localSurvey`（超时默认 5 秒，可被 `survey_timeout_ms` 覆盖），结果编码进应答元数据的 `survey_results` 字段返回。

聚合（`expandClusterSurveyResults`，`node.go:673-706`）：本地结果 + 各远端节点的结果合并；某个节点执行失败（命令失败、超时、结果解码失败）时，该节点以一条带 `error` 的 `SurveyResult` 表示（错误码如 `CLUSTER_COMMAND_SEND_FAILED`），整体调查不因此失败。最终结果按 `(NodeID, IncarnationID, SessionID)` 排序（`sortSurveyResults`）。

与单节点的差异可概括为：单节点只问本地订阅者；集群版先问本地、再问所有存活节点，远端失败以错误应答条目呈现而不是整体报错。

## 6. 修复器（repairer）与 membership OnLeave

**解决的问题**：集群级的活跃频道列表（管理 API `GetChannels`、`Node.Channels`）来自共享查询投影。投影由每次订阅/退订的 ±1 增量维护（`AdjustChannelSubscriptions`）。若持有订阅的节点突然宕机，其增量（+N）永远无法回退，投影会出现「幽灵订阅者」——频道明明已无人订阅，计数却不为零。同理，user→sessions 索引与死节点的会话 fencing 也需要一个控制面循环来收敛。

**数据结构**：投影按节点隔离（`cluster_query_store.go`）。每个节点实例拥有一个 Redis hash：

```
ml:cluster:channel:owner:<nodeID>:<incarnationID>
```

hash 的字段是频道名、值是本节点在该频道的订阅者计数。增量调整用 Lua 脚本原子执行（`adjustChannelSubscriptionsScript`）：`HGET` 当前值 → 加/减 delta → 结果 ≤ 0 时 `HDEL` 该频道，hash 变空则 `DEL` 整个键；否则 `HSET` + 刷新 `EXPIRE`（TTL 10 分钟，`defaultClusterQueryProjectionTTL`）。`ListChannels` 用 `SCAN ml:cluster:channel:owner:*` 枚举所有 owner hash，`HGETALL` 后按频道聚合求和，按频道名排序。

**修复流程（PR-KA-B4 起）**：所有派生视图的修复收敛为**一个** `clusterRepairer`（根包 `cluster_repair.go`），`NewCluster` 只启动这一个修复组件。它由一条定时循环驱动两档节奏：

- **30 秒档**（`ClusterRepairerConfig.Interval`，默认 `defaultClusterRepairInterval`）每轮 `repairOnce`：从本地 hub 取活跃频道及真实订阅者数（`GetActiveChannels`），用 `ReplaceNodeChannels` **全量重建**本节点的 owner hash（`DEL` + `HSET` + `EXPIRE`，事务管道内完成）；随后收割节点租约已消失的死节点 owner 投影；最后 SCAN 会话租约重建 user→sessions 索引（见 4.5）。由此：
  - 本节点上的幽灵增量（因订阅 saga 部分失败、驱逐回滚遗漏等）每轮被纠正；
  - 节点宕机后，其 owner hash 在 10 分钟 TTL 内自然消失，全集群聚合计数随之回落——投影修复 + TTL 过期共同构成投影的最终一致（eventual consistency）。
- **membership 档**（`ClusterRepairerConfig.MembershipInterval`，默认 5 秒、每拍 ±20% 抖动）每拍 `SCAN ml:cluster:node:*` 维护上一拍存活 incarnation 集合：上一拍有、本拍消失（或 `ExpiresAt` 已过）且非自身的 incarnation 触发 `OnLeave`——删除其名下全部会话租约（走 `DeleteSessionLease`，同步清理 user 索引；**不做** Evict，对方已死）并删除其 owner 投影，不必等 600 秒会话 TTL。第一拍只建集合不触发；自身 incarnation 永不触发。这是控制面循环，热路径（publish/subscribe/ping）不做任何 SCAN。

集成测试 `TestClusterRedis_ProjectionRepairRestoresChannels` 验证了删除 owner 键后修复器自动重建投影。修复成功与失败分别计入 `ClusterProjectionRepairs` / `ClusterProjectionRepairFailures` 指标。

## 7. Presence 聚合

集群模式下 Presence 存储被替换为 Redis 实现（`pkg/redisbroker/presence_redis.go`，`main.go:131`），数据结构（`Options` 默认前缀见 `options.go`）：

| 键 | 类型 | TTL | 内容 |
| --- | --- | --- | --- |
| `ml:presence:member:<channel>:<clientID>` | string | 60 秒（`PresenceTTL`） | `PresenceInfo` JSON（`ClientID`、`UserID`、`ConnectedAt`） |
| `ml:presence:idx:<channel>` | set | 60 秒（`PresenceTTL`，与成员键同 TTL） | 频道内在线客户端 ID 集合索引 |
| `ml:presence:occ:gen:<channel>` | string（计数器） | 无 TTL | OccupancyGen：每次 `INCR`（B2 §4） |

`Add`（订阅登记/心跳刷新）在一条流水线内完成 `SET` 成员 + `SADD` 索引 + `EXPIRE` 索引；`Remove`（退订/断开）`DEL` 成员 + `SREM` 索引；`Get`（查询）先 `SMEMBERS` 索引，再流水线 `GET` 每个成员并反序列化，读取时发现成员键已过期缺失则顺手 `SREM` 清理索引中的残留，并对每个被清理的幽灵成员合成一条 leave 事件（取新 OccupancyGen，经 LiveBus `PublishOccupancy`，B2 §5.3；`presence_redis.go`）。

聚合原理：所有节点把 presence 写入**同一个 Redis 命名空间**，因此任何节点调用 `Get` 拿到的都是全集群的在线集合——Presence 天然按频道聚合，无需额外协议。成员 TTL 由订阅侧通过 `Add` 刷新（客户端 ping 触发的刷新经节流，见 4.1），异常退出的会话会在 TTL 内自然消失。

**加入/离开事件（Occupancy）**：每次 Join/Leave 取单调 OccupancyGen（Redis：`INCR ml:presence:occ:gen:<ch>`），存完 store 后**只** `PublishOccupancy(ch, evt)` 走 live bus 精确频道（`ml:pubsub:<ch>`，payload `t:"occupancy"`，**不是** `pub`）。跨节点投递只依赖 LiveBus 的 Interest：内存 broker 同步进 handler，Redis 把 PUBLISH 扇回给所有订阅该精确/编译频道的节点（含本进程自身），每个节点 `runPubSub` 在 `interested()` 之后按类型分支——`pub` 进 `deliverOnce`，`occupancy` 直接进 occupancy handler（**无 stream offset，不走 deliverOnce，不进 Publication handler**）。接收端 `gen <= last_applied[ch][session]` 弃迟到。`server.presence.cluster_emit` 已删除（写进 YAML 会被 `Validate` 拒绝），不再有 `ml.type=presence` 帧改写。只有频道策略 `legacy_presence_channel: true` 时，精确频道才会额外把旧 JSON 瞬时发到伴生频道。事件不进历史，不会混入恢复流。远端订阅（经命令总线，见 3.4）同样走 `shouldTrackPresence` 门闩。

### 7.1 Presence 跨节点（LiveBus + OccupancyGen，B2）

一等 `presence_event` 的跨节点投递统一经 **LiveBus 精确频道**，与 `cluster.enabled` 相互独立：控制面（`cluster.enabled`）关着也能靠 Redis broker 把 occupancy 事件扇到共享同一 Redis 的节点；反之 `cluster.enabled: true` 时事件同样只按 Interest 投递。

- 发送：`presenceJoin`/`presenceLeave` 存完 store 后**只**调 `broker.PublishOccupancy(exactCh, evt)`（事件带 OccupancyGen）。
- 接收（本机与跨节点同一条 `onOccupancy`）：`interested()` 命中的节点拿到事件，按 `last_applied[ch][session]` 去迟后 `deliverPresenceEvent` 扇到 Coverage 订阅者；事件主体（`evt.Info.SessionId`）不被扇回给自己。
- 通配广播（`im.**` 的节点收到 `im.room.1` 的 join）由 `CompileInterest` 编译订阅在 Broker 层完成，事件本身只发在**精确频道**上；只订 `chat.1` 的节点收不到 `im.room.1` 的 occupancy。
- 不设写侧开关：写 YAML 的 `server.presence.cluster_emit`（无论 true/false）都会被 `Validate` 以 `cluster_emit is removed` 拒绝。

## 8. 历史消息

集群模式下历史由 Redis Streams 承载，是全集群**共享**的：

- 发布路径（`redis.go` 的 `Publish`）：先 `XADD` 写入 `ml:stream:<channel>`（`StreamMaxLength` 默认 10000 条、`StreamApproximate` 默认 true，`HistoryTTL` 默认 24 小时），从 Stream ID 解析出 offset；再 `PUBLISH` 到 `ml:pubsub:<channel>` 做实时分发。任意节点发布，全部节点共享同一份历史。
- 消费路径（`pubsub.go`）：每个节点 `PSUBSCRIBE ml:pubsub:*` 模式订阅，只处理本节点登记过兴趣（`Subscribe`）的频道；断线以指数退避重连（1 秒起、上限 30 秒）。
- **offset 语义**：offset 由 Stream ID 编码而来，`offset = ts<<20 | seq`（毫秒时间戳与序列号拼入 uint64，`history.go`）。历史查询 `History(ch, sinceOffset, limit)` 用**包含**起始 ID（`"ts-seq"`，`streamStartID`）——Redis broker 与内存 broker 的 `since_offset` 均为**包含**（inclusive）语义，返回 `offset >= since_offset`（契约见 `broker.go:105-108`）；`limit <= 0` 时上限为 `DefaultHistoryLimit`（1000 条）。
- 由于历史与 offset 都来自共享的 Redis Stream，跨节点查询历史得到的是同一份数据；跨节点**恢复**的 epoch 校验也因 Redis epoch 集群共享而可通过（见 4.4）。
- 瞬时消息（`PublishTransient`，presence 事件等）不写 Stream，offset 恒为 0，永不进入历史。

## 9. 故障与恢复

### 9.1 节点宕机的影响面

节点宕机时没有主动注销，一切靠 TTL 与探测收敛：

| 数据 | TTL | 宕机后的行为 |
| --- | --- | --- |
| 节点租约 `ml:cluster:node:*` | 90 秒 | 过期后该节点从广播目标（Survey 等）中消失 |
| 会话租约 `ml:cluster:session:lease:*` | 600 秒 | 过期后会话不再被识别为属于任何存活节点；期间携带该 `SessionId` 的新连接会尝试 takeover 并向已死节点发送命令，命令超时后经节点租约检查降级继续恢复（见 4.3） |
| 会话快照 `ml:cluster:session:snapshot:*` | 24 小时 | 保留足够久，客户端重连到任意存活节点都能拿到订阅列表等恢复信息 |
| 节点投影 `ml:cluster:channel:owner:*` | 10 分钟 | 过期后全集群频道列表自动收敛（见第 6 节） |
| presence 成员 | 60 秒 | 过期后在线状态自动收敛（见第 7 节） |

宕机节点承载的连接会被客户端感知为断线；客户端携带 `SessionId` 重连任意存活节点即走 resume 路径（本地命中则本地接管，否则远端接管 + 快照恢复订阅）。**未开启 resume 的连接**（客户端不带 `SessionId` 重连）自然是全新会话，不受影响。

### 9.2 stale 节点与陈旧状态清理

`deleteClusterSessionState` 的所有权检查（4.1）保证：已死节点留下的会话状态不会被存活的无关节点误删；而指向已过期租约的陈旧状态可以被安全清理。`RemoveSessionIfMatches`（hub.go）保证失败的旧连接在 resume/takeover 后不会把新会话从 hub 驱逐（stale 保护）。

### 9.3 运维建议（建议性说明）

以下为部署建议，不构成对源码行为的承诺：

- **时钟同步**：租约与快照的 `ExpiresAt` 由写入节点本地时钟计算、由读取节点本地时钟比较（如 `deleteClusterSessionState` 的 `ExpiresAt.After(time.Now())`）。节点间时钟偏差过大会导致租约提前过期（会话被误判为 stale）或延迟过期（死节点残影变长），建议集群内使用 NTP 等同步手段。
- **网络分区**：控制面没有 quorum/选举机制，所有决策依赖 Redis 的读写成功。分区两侧的节点若都能访问 Redis，可能同时认为自己持有某会话的租约（租约无互斥抢占，`CompareAndSwapSessionLease` 由跨节点 resume 使用(CAS 抢占)）。对于「同一用户重复登录应踢旧连接」这类强一致需求，请在应用层设计容错（例如容忍短时间双活）。
- **Redis 高可用**：Redis 是消息管道与控制面的共同单点。集群模式没有内置的 Redis 故障转移逻辑，建议按生产标准为 Redis 配置持久化与高可用方案（复制、哨兵或集群模式），并在负载均衡/客户端侧考虑 Redis 故障时的降级策略。健康端点可观测 Redis 连通性（见第 10 节），可作为探活依据。

## 10. 可观测性

集群相关的 Prometheus 指标（全部定义于 `metrics.go`，命名空间 `messageloop`）：

| 指标 | 类型 | 含义 | 触发点 |
| --- | --- | --- | --- |
| `messageloop_cluster_command_dedupe_hits_total` | Counter | 命令去重命中次数 | 发送方命中已存结果、接收方抢占失败（`recordDedupeHit`，阶段 `send` / `owner`） |
| `messageloop_cluster_command_timeouts_total` | Counter | 命令应答等待超时次数 | 发送方在 deadline 内未收到终态应答（`recordCommandTimeout`） |
| `messageloop_cluster_command_unknown_final_state_total` | Counter | 命令进入「终态未知」的次数 | 超时后状态键无终态、或结果未能持久化（`recordUnknownFinalState`） |
| `messageloop_cluster_command_hmac_reject_total{reason}` | CounterVec | HMAC 验签拒绝的信封数（reason ∈ `missing`/`bad`/`skew`/`id`） | 未签名/坏签/偏斜/无 id 的命令或伪造应答被拒（`recordHMACReject`） |
| `messageloop_cluster_projection_repairs_total` | Counter | 投影修复成功轮次 | 修复器 `repairOnce` 的投影重建成功（`cluster_repair.go`） |
| `messageloop_cluster_projection_repair_failures_total` | Counter | 投影修复失败轮次 | `ReplaceNodeChannels` 失败（同上） |

指标解读提示：`dedupe_hits` 持续高位说明存在大量命令重试（可能源于广播重复投递或发送方重试）；`timeouts` 与 `unknown_final_state` 上升通常意味着目标节点不健康、Redis 网络抖动或命令处理超载（处理并发上限 128，见 3.3）。

集群模式下健康端点（`/health`，`server.http.addr`）附加 Redis 连通性探测：以 2 秒超时调用 broker 的 `Ping`，失败时返回 503、JSON 中 `status: "not ready"`、`redis: "unreachable"`（`health.go`）。关键日志关键字：`cluster command received`（含 `command_id`、`issued_by`）、`cluster command dedupe hit`、`cluster command timed out waiting for reply`、`cluster projection repair applied`（debug 级）。

指标采集、暴露与告警建议见[《可观测性指南》](05-observability.md)。

## 11. 管理 API 的集群感知行为

管理 API（`messageloop.server.v1.APIService`，详见[《管理 API 参考》](03-admin-api.md)）在集群模式下行为变化，概览如下：

| 操作 | 集群模式下的行为 |
| --- | --- |
| `Publish`（会话投递） | 经会话租约解析目标节点，远端会话经命令总线路由执行（`PublishToSession` → `dispatchSessionCommand`） |
| `Disconnect` / `Subscribe` / `Unsubscribe`（会话定向） | 同上：租约解析 + 命令路由；远端订阅/退订还会在目标节点触发 presence 登记/清除与 join/leave 事件 |
| `Survey` | 本地调查 + 向所有存活节点广播（`exclude_self`），聚合结果按节点/会话排序，每个结果带 `node_id` / `incarnation_id` 元数据，远端失败以带 `error` 的结果呈现 |
| `GetChannels` | 不再读本地 hub，改读集群共享投影（聚合所有 owner hash），返回全集群活跃频道 |
| `GetPresence` | Presence 存储为 Redis 实现，返回全集群在线成员 |
| `GetHistory` | 从共享 Redis Stream 读取，`since_offset` 为包含（inclusive）语义，跨节点数据一致 |

非集群模式下上述操作只作用于本节点。完整语义、请求/响应格式与示例见[《管理 API 参考》](03-admin-api.md) 的「集群感知行为」一节。
