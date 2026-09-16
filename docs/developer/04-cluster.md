# 分布式集群指南

本文档描述 MessageLoop 的分布式集群（distributed cluster）机制：多个服务端节点如何通过共享的 Redis 构成一个逻辑集群，以及会话归属、远端接管、集群级 Survey、投影修复、Presence 聚合等集群特有行为的原理与运维要点。文中所有类型名、方法名、Redis 键、默认值与行为均以仓库源码为准。

配套文档：[《架构指南》](01-architecture.md)（通用架构）、[《配置参考》](02-configuration.md)（全部配置字段）、[《Server API 参考》](03-server-api.md)、[《可观测性指南》](05-observability.md)、[《开发指南》](06-development.md)，以及[《客户端协议参考》](../protocol.md) 与[《部署指南》](../deployment.md)。

## 1. 概述

MessageLoop 可以以两种形态运行：

- **单节点（single node）**：一个进程承载全部客户端连接。默认使用进程内内存 broker（`broker.type: memory`），发布/订阅、历史、在线状态全部局限在本进程内。
- **多节点（multi node）**：多个进程组成集群，客户端连接分散在不同节点上，但共享同一套消息管道与在线状态。集群模式要求 `broker.type: redis`（配置校验强制，报错信息为 `cluster requires broker.type=redis`）。

这里必须区分两个概念，它们经常被混淆：

| 概念 | 配置开关 | 作用 |
| --- | --- | --- |
| **Redis broker** | `broker.type: redis` | 消息管道：发布经 Redis Streams 写历史、经 Redis Pub/Sub 实时分发，所有节点共享同一份历史与实时流量 |
| **集群控制面（cluster control plane）** | `cluster.enabled: true` | 节点间协调：会话目录（session directory）、命令总线（command bus）、频道投影（query store）、节点租约（node lease）、投影修复（projection repair），实现跨节点会话管理与集群级操作 |

启用 `broker.type: redis` 但 `cluster.enabled: false` 时，各节点仍然共享消息与历史（例如多个无状态节点前端挂同一个 Redis），但节点之间互不感知：会话属于连接所在节点，Server API 操作只作用于本节点。只有 `cluster.enabled: true` 才开启分布式控制面——节点彼此发现、会话可以跨节点接管、Survey 与频道查询是全集群范围的。`cluster.enabled` 是控制面的总开关，这一点请与 `broker.type` 区分清楚。

适用场景：

- 单节点无法承载全部在线连接，需要横向扩容，且要求会话在节点间可迁移、断线可在任意节点恢复；
- 需要集群级 Server API 操作：远程断开/订阅/退订、跨节点会话定向投递、全集群 Survey；
- 需要全集群统一的频道列表与在线状态视图。

代价是引入对 Redis 的强依赖（见第 9 节故障与恢复）。

## 2. 开启条件与配置

### 2.1 前提

1. `broker.type: redis` 且 `broker.redis.addr` 已配置。集群控制面与 broker 共用同一个 `broker.redis` 配置段（`addr` / `password` / `db` 等，见 cmd/server/main.go 的 `setupCluster`），不单独配置 Redis 连接。
2. 集群内所有节点的 `broker.redis` 必须指向同一个 Redis 实例与同一个 DB，共享同一键空间（所有键以 `ml2:` 前缀隔离，见第 3 节）。指向不同实例或 DB 的节点彼此不可见。
3. 每个节点必须有全局唯一的 `cluster.node_id`。
4. 全集群共用同一个 HMAC 命令总线密钥（`cluster.hmac_key` 或 `cluster.hmac_key_file`，至少 32 字节）。

### 2.2 cluster 配置段

配置结构见 `config/config.go` 的 `ClusterConfig`，共五个字段：

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `cluster.enabled` | bool | `false` | 集群控制面总开关；启用要求 `broker.type: redis` |
| `cluster.node_id` | string | 无 | 逻辑节点标识，集群内必须唯一；启用集群时必填（`ClusterOptions.Normalize`，internal/cluster/contracts.go，缺失时报 `cluster node_id is required when cluster is enabled`） |
| `cluster.backend` | string | `redis` | 控制面后端；留空默认 `redis`；接受 `redis` / `memory` / `noop`，其他值报 `unsupported cluster backend` |
| `cluster.hmac_key` | string | 无 | 内联命令总线 HMAC-SHA256 密钥，与 `hmac_key_file` 二选一，至少 32 字节 |
| `cluster.hmac_key_file` | string | 无 | 密钥文件路径；文件尾单个换行（LF 或 CRLF）会被裁剪 |

`backend` 的取值说明：

- `redis`：唯一在服务端二进制中接入实际实现的取值。启用时装配会话目录、命令总线、查询投影、节点租约与投影修复，并把 Presence 存储替换为 Redis 实现（cmd/server/main.go 的 `setupCluster`）。
- `memory` / `noop`：no-op 组件，进程内 API 使用或测试用；控制面各接口退化为本地行为（例如命令总线返回 `ErrClusterCommandUnsupported`）。

HMAC 密钥校验分两层：`Validate()` 在配置期拒绝两源同设、均未设置与内联密钥不足 32 字节；启动接线 `ClusterConfig.ResolveHMACKey()` 负责读取密钥文件并做最终长度检查，失败即拒绝启动。详见[《配置参考》](02-configuration.md) cluster 节。

**进程实例标识（IncarnationID）**不来自配置：启动时对该节点的 node_epoch 计数器发号一次（Redis 后端 `INCR ml2:cluster:node_epoch:<nodeID>`，internal/runtime 装配；`memory`/`noop` 后端用进程内单调计数器），`IncarnationID` 就是 epoch 的十进制字符串（`"1"`、`"2"`……）。该键刻意不在 `ml2:cluster:node:` 前缀下，不会被节点租约扫描收割。Redis 后端无法分配时拒绝启动，不回落随机 ID。数值形式的 IncarnationID 可比较新旧（`NodeEpochNewer`，internal/cluster/epoch.go）；非数值形式的测试 ID（如 `inc-a`）原样保留、不可比较。节点的完整身份是 `(NodeID, IncarnationID)` 二元组：旧进程重启后得到更大的 IncarnationID，从而与旧实例的租约区分开。

### 2.3 两节点配置示例

仓库根目录的 `config-node1.yaml` 与 `config-node2.yaml` 是双节点演示的基础配置：两个节点监听不同端口（节点一 WebSocket `:19080` / gRPC `:19090` / 管理 HTTP `:18080`；节点二 `:29080` / `:29090` / `:28080`），并指向同一个 Redis 实例。注意这两个文件本身尚未包含 `cluster` 段——按 `config-example.yaml` 的字段补齐后才能启用集群。

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
  hmac_key_file: /path/to/cluster-hmac.key
```

节点二（`config-node2.yaml` 内容基础上补充，端口换为 28/29 前缀，`node_id` 不同）：

```yaml
cluster:
  enabled: true
  node_id: node-2
  backend: redis
  hmac_key_file: /path/to/cluster-hmac.key
```

两个节点除监听端口与 `node_id` 外其余配置相同，这是集群部署的典型形态：客户端可连任意节点的任意端口。

## 3. 架构与数据流

### 3.1 控制面组件与生命周期

`Cluster`（internal/runtime/cluster.go）是控制面组件的生命周期协调器，持有五个组件（接口定义见 internal/cluster/contracts.go，Redis 实现见 pkg/redisbroker/）：

| 组件 | 接口 | Redis 实现 | 职责 |
| --- | --- | --- | --- |
| 会话目录 | `SessionDirectory` | pkg/redisbroker/cluster_directory.go | 节点租约、会话租约与会话快照的读写，CAS 会话租约交换，user→sessions 索引 |
| 命令总线 | `ClusterCommandBus` | pkg/redisbroker/cluster_command_bus.go | 节点间命令投递（定向与广播）、结果回传、命令去重、HMAC 验签 |
| 查询存储 | `ClusterQueryStore` | pkg/redisbroker/cluster_query_store.go | 每节点频道的订阅计数投影（hash）与全集群聚合 |
| 节点租约管理 | `ClusterNodeLeaseManager` | 通用实现（cluster_state.go） | 周期性续租本节点存活记录 |
| 修复器 | `ClusterRepairer` | 通用实现（cluster_repair.go） | 单一控制面循环：周期性从本地 hub 全量重建本节点投影、收割死节点投影、重建 user→sessions 索引，并由短周期节点租约扫描驱动 membership `OnLeave` |

`node.Run` 启动时先启动集群（`Cluster.Start`）：按 会话目录 → 命令总线 → 查询存储 → 节点租约 → 修复器 的顺序启动；任一组件启动失败时，已启动的组件按逆序回滚关闭（回滚受 5 秒超时约束，`clusterStartRollbackTimeout`），实例保持可重试。`Node.Shutdown` 关闭集群时按逆序关闭全部组件。

`Cluster.Start` 对每个 Redis 组件先执行 `Ping` 预检：任何组件连不上 Redis，节点启动即失败——集群模式下 Redis 不可用是启动级故障。节点租约管理器启动时的首次租约写入同样属于组件启动：写失败即启动失败，不带着"集群里没有我"的状态继续运行。

### 3.2 节点注册与发现（节点租约）

每个节点的存在性由一个节点租约（node lease）记录表达（`ClusterNodeLease`：`NodeID`、`IncarnationID`、`StartedAt`、`ExpiresAt`）：

- **键**：`ml2:cluster:node:<nodeID>:<incarnationID>`（pkg/redisbroker/cluster_directory.go 的 `nodeLeaseKey`），值为租约 JSON。
- **TTL**：90 秒（`defaultClusterNodeLeaseTTL`）。
- **续租**：`ClusterNodeLeaseManager`（internal/runtime/cluster_state.go）启动时立即写一次租约（失败即组件启动失败），之后每 30 秒（`defaultClusterNodeLeaseRenewInterval`）续租一次。续租失败只记警告日志、不中断节点运行（节点租约随后到期自然退出集群）；失败连续升级：失败次数达到 TTL/间隔比值时日志级别升为 Error，并计入指标 `messageloop_cluster_node_lease_renew_failures_total`。
- **离开**：节点没有主动注销机制——进程退出后，其租约在 90 秒内自然过期消失（优雅关闭 `Cluster.Shutdown` 只是停止续租）。修复器的 membership 节拍（默认 5 秒 ±20% 抖动的节点租约扫描，见第 6 节）发现某 incarnation 从存活集合中消失（或租约已过期）即触发 `OnLeave`：立即删除该死 incarnation 名下的全部会话租约（作废其 fencing，同步清理 user 索引）并删除其 owner 投影，不必等 600 秒会话 TTL；宽限期 = 一次扫描周期。节点自身的 incarnation 永远不会被自己 `OnLeave`。

节点发现通过扫描节点租约键实现：命令总线的 `BroadcastCommand` 用 `SCAN ml2:cluster:node:*` 枚举存活节点，每个租约键对应一个可投递目标。租约过期即从广播目标中消失。节点身份是 `(NodeID, IncarnationID)`，同一 `node_id` 的旧实例（已重启产生新 `IncarnationID`）会与新实例并存一段时间，直到旧租约过期——期间广播可能向同一逻辑节点的两个实例各投递一份命令，但已死的旧实例不会应答，其命令以失败结果呈现，不产生实际副作用。

### 3.3 命令总线

命令总线（pkg/redisbroker/cluster_command_bus.go）是集群控制面的神经系统：请求经 Redis Stream + consumer group 投递（至少一次），应答经 Redis Pub/Sub 返回，构成请求/应答式的节点间命令投递。

**Redis 键**：

| 键 | 用途 |
| --- | --- |
| `ml2:cluster:cmd:stream:<nodeID>:<incarnationID>` | 请求 inbox 流：发给指定节点实例的命令 `XADD` 到该流（近似 `MAXLEN ~ 10000` 截断）；每条流带一个固定名为 `inbox` 的 consumer group，消费者名即本进程 incarnationID；节点实例启动时 `XGROUP CREATE ... MKSTREAM`（已存在则忽略 BUSYGROUP），用 `XREADGROUP ... >` 读新消息，并周期 `XAUTOCLAIM`（idle ≥ 30 秒）认领崩溃消费者留下的 pending 条目，处理完一律 `XACK` |
| `ml2:cluster:cmd:reply:<commandID>` | 应答通道：每个命令生成一个随机 UUID 应答通道，命令入流前把通道名写入命令元数据；应答经 Pub/Sub 返回 |
| `ml2:cluster:cmd:state:<commandID>` | 命令状态键：持久化命令的终态结果，也是命令去重（dedupe）的依据 |

**发送流程（`SendCommand`）**：

1. 生成 `CommandID`（未指定时），标记 `IssuedBy`（发送方 NodeID，仅审计用途，见下方信任边界）与 `IssuedAt`。
2. **目标存活预检**：读取目标节点的租约，目标实例没有存活租约时直接返回 `TARGET_NODE_NOT_ALIVE`——不入流、不等待超时。这让"向已死节点发命令"快速失败（例如跨节点恢复立即走死节点降级路径），而不是白等一个必然超时的应答。
3. 查询 `ml2:cluster:cmd:state:<commandID>`：若已有终态结果，直接返回（去重命中）；若处于 `pending`，返回 `in_progress`。
4. 创建随机应答通道并 `SUBSCRIBE`，把通道名写入命令元数据。
5. **签名**：`SignCommand` 以节点配置的 HMAC 密钥对命令的规范字节（internal/cluster/hmac，逐字节固定的行式编码，不含 `IssuedBy`）计算 `hex(HMAC-SHA256)` 写入 `Signature`；签名失败则不入流。
6. 把命令 JSON（含 `Signature`）作为单一 `payload` 字段 `XADD` 到目标节点的 inbox 流。
7. 在应答通道上等待 `CommandID` 匹配且验签通过的结果；伪造/未签名/偏斜的应答被记入指标并当作未收到继续等待；`CommandID` 不匹配的应答记录警告并继续等待。
8. 等待受命令级超时约束：调用方上下文无 deadline 时默认 5 秒（`defaultCommandTimeout`）。超时后先查状态键——若已有终态结果则返回该结果；否则返回 `unknown_final_state` 并计入指标。

**接收与执行（`handleMessage`）**：每个节点实例消费自己的 inbox 流，处理并发上限为 128（`clusterCommandHandlerConcurrency`）——读者循环在信号量上阻塞后才派发，饱和时排队而不是丢命令；每个处理器执行受 10 秒 deadline 约束（`clusterCommandHandlerTimeout`），超时返回 `CLUSTER_COMMAND_TIMEOUT`，卡死的处理器不会把命令钉死在 pending。**HMAC 硬门在最前**：未签名（`missing`）、坏签（`bad`）、`IssuedAt` 超出 ±30 秒时钟窗（`skew`）或无 `CommandID`（`id`）的命令直接拒绝——不认领、不执行 handler、不写去重状态键、不应答（但仍 ACK），只计入 `messageloop_cluster_command_hmac_reject_total{reason}` 并记警告日志。通过验签后才在状态键上 `SETNX` 抢占（claim），TTL 30 秒，处理期间每 10 秒续租；执行完成（或失败）后把终态写入状态键，TTL 10 分钟（`defaultCommandStateTTL`），停止续租，再把签过名的结果发布到应答通道。读循环断线以指数退避重连（1 秒起、上限 30 秒）。

**命令去重（command dedupe）**：同一 `CommandID` 的命令可能因重试、广播重复投递、崩溃后 `XAUTOCLAIM` 重投而多次到达（Stream 保证至少一次，幂等靠去重），去重发生在两个环节：

- 发送方：重发同一 `CommandID` 时，直接返回状态键中已存储的终态结果（或 `in_progress`），不再入流。
- 接收方：`SETNX` 抢占状态键。抢占失败说明另一实例正在处理或已有终态，向应答通道回 `in_progress`（`COMMAND_IN_PROGRESS`）或终态结果而不重复执行。若旧执行者崩溃，claim 租约 30 秒后过期，后续到达（或被 `XAUTOCLAIM` 重投）的命令可以重新抢占，而不是被钉在 pending 直到终态 TTL 过期。

去重命中与超时、`unknown_final_state` 均计入指标（见第 10 节）。注意去重的粒度是 `CommandID`：广播命令（`BroadcastCommand`）为每个目标节点复制命令并重新生成 `CommandID`，因此广播不会误去重。

**信任边界（trust boundary）**：集群命令经 Redis Stream 传输、应答经 Redis Pub/Sub 传输，两者都由 HMAC-SHA256 硬门保护（internal/cluster/hmac）：密钥只来自节点配置（`cluster.hmac_key` 或 `cluster.hmac_key_file`，至少 32 字节，启用集群时缺一即拒绝启动），从不写入任何 Redis 键、流条目、PUBLISH 载荷、日志或指标标签。能写 Redis 不等于能签发集群命令：未签名/坏签/偏斜的命令在认领之前被拒，伪造的 `succeeded` 应答不会让 `SendCommand` 成功。`IssuedBy` 字段只用于日志审计追溯（可伪造，不在规范字节内），不是安全边界。Redis 的网络隔离仍是纵深防御手段，但不再是唯一边界。

### 3.4 节点间命令路由

节点间命令路由以会话租约为索引（internal/runtime/cluster_commands.go 的 `dispatchSessionCommand`）：

1. `resolveSessionLease(sessionID)`：先查本地 hub（`LookupSession`），命中则用本地状态构造租约；未命中且集群启用时查会话目录（`GetSessionLease`）。
2. 目标即租约中的 `(NodeID, IncarnationID)`。若目标就是本节点（或集群未启用），直接在本地执行 `handleClusterCommand`；否则经命令总线 `SendCommand` 路由到目标节点执行。

命令类型（`ClusterCommandType`）：`disconnect`、`subscribe`、`unsubscribe`、`publish`、`takeover`、`survey`。命令结果状态（`ClusterCommandStatus`）：`pending`、`succeeded`、`failed`、`in_progress`、`unknown_final_state`。

远程 `subscribe`/`unsubscribe` 执行时还会附带本地副作用：presence 登记/清除与 join/leave 事件的发布，因此经Server API 远程订阅的会话在全集群的 presence 视图中同样可见。

## 4. 会话归属与接管

### 4.1 会话所有权模型

每个客户端会话在集群中有一份会话租约（session lease）与一份会话快照（session snapshot），由会话目录存储：

| 数据 | 键 | TTL | 内容要点 |
| --- | --- | --- | --- |
| 会话租约 | `ml2:cluster:session:lease:<sessionID>` | 默认 600 秒；按心跳配置缩短（`sessionLeaseTTL()` = `max(30s, 2×idle_timeout, 3×ping_interval, idle_timeout+20s)`，心跳禁用时保持 600s） | `SessionID`、`NodeID`、`IncarnationID`、`Namespace`、`UserID`、`ClientID`、`LeaseVersion`、`Authenticated`、`ConnectedAt`、`LastActivityAt`、`ExpiresAt` |
| 会话快照 | `ml2:cluster:session:snapshot:<sessionID>` | 24 小时（`defaultClusterSessionSnapshotTTL`） | 会话身份（namespace/user/client/protocol）、订阅列表、逐频道 `ChannelOffsets`（上次成功投递的历史 offset）与 `BrokerEpoch`（快照时刻的 broker 世代），供精确跨节点恢复（见 4.4） |

会话所有权 = 「会话租约指向的节点实例正在服务该会话」。`LeaseVersion` 是所有权代际计数：新连接从 1 起，每次 resume/takeover 递增。它被用于接管时的版本校验，防止旧代际的接管命令误伤新代际的会话。

**会话租约的写入只走 CAS，没有盲写**；且租约 CAS 与快照写入合成一次原子操作：`syncClusterSessionState`（internal/runtime/cluster_state.go）是唯一的热路径写入方，它经 `SessionStateCompareAndSwapper.CompareAndSwapSessionState`（未实现该接口的 Directory 回退到「CAS + 写快照」两步）把「四字段比对 + lease 写 + snapshot 写」压成一步——Redis 实现是一条 Lua 脚本，比对谓词为 `SessionID`/`NodeID`/`IncarnationID`/`LeaseVersion`（expected 为 nil 时要求键不存在），`ok=false` 时两个键都不写。这消除了「CAS 抢租成功 → 写快照」之间旧快照覆盖新状态的窗口。三种情形：

- **首次登记**：目录中无该 session 的租约 → `CAS(expected=nil)` 抢注（版本 1）。
- **same-fence 续约**：租约仍指向本节点实例且版本与本地一致 → `CAS(expected=当前租约)` 刷新 TTL / `LastActivityAt` / `UserID` 等，`LeaseVersion` 不递增。无条件 SET 的租约写入方法不存在于 `SessionDirectory` 接口：盲写会把已被他节点 CAS 抢走的所有权写回去。
- **fencing 失效（`ErrSessionFenced`）**：目录上的租约已不属于本节点实例（被其他节点 CAS 抢走），或版本比本地更新（本附件已陈旧）→ 返回错误，不写回。ping/pong 刷新路径收到该错误即以 3502（`DisconnectStale`）断开本连接，且不删除目录里的租约（那会误删新 owner 的 fencing）。

版本的唯一递增点在跨节点恢复的抢权 CAS（旧版本 +1 后原子写入）；本机接管把内存版本 +1 后经同节点的 CAS 写透，续约本身从不 +1。

会话状态的写入时机（`syncClusterSessionState`）：

- 连接建立（`AddClient`）；
- 每次订阅/退订（订阅 Saga 的 cluster 步骤，受 2 秒 `clusterStepTimeout` 约束，失败不阻塞客户端操作路径）；
- 客户端 ping/pong 触发的状态刷新，节流为最多每 10 秒一次（`pingClusterRefreshInterval`）。刷新只做 same-fence CAS，检测到 fencing 失效时以 3502 断开，其余错误维持 Warn 不断开（避免 Redis 抖动踢光全员）。

会话关闭时的清理（`deleteClusterSessionState`）使用 `DeleteSessionLeaseIfOwner`：一条原子的 compare-and-delete，只有租约仍指向本节点实例（或已过期）时才删除租约与快照并同步清理 user 索引。若租约有效且属于其他节点实例，说明该会话已被他处接管，本地不做任何删除——原子的 owner 检查防止「误删新 owner 的租约 → 双接管」。

### 4.2 本机接管（同节点 resume）

客户端携带 `SessionId` 重连且旧会话仍在同一节点的 hub 中时（internal/session/client.go 的 `handleConnect`）：

1. 复制旧会话状态（身份、订阅频道、租约版本、命名空间）；
2. 旧附件 `Detach`：只关旧传输附件、停 writer、丢队列；`Session` 对象本身留在 Hub，指针不动（不重建订阅、不改通配 matcher）；
3. 新附件 `Attach` 到同一 `Session`；新连接的读循环 shell 委托给该会话对象；
4. 内存租约版本 +1，经同节点的 CAS 写透到目录。

Attach 失败则走真正的 `Close`（presence Leave、撤订阅、删目录状态）。

### 4.3 远端接管（remote takeover）

客户端携带 `SessionId` 重连，但旧会话不在本节点（本地 `LookupSession` 未命中）时，走跨节点恢复路径 `resumeRemoteSession`（internal/runtime/cluster_resume.go）：

1. 读会话租约与会话快照；两者缺一即放弃恢复（按未恢复处理）。远端快照的 owner 或 namespace 与本连接不符时拒绝恢复（3500/`DisconnectInvalidToken`）。
2. 抢权：`CAS(expected=旧租约, version+1)` 原子夺取租约。若租约的 `NodeID` 就是本节点且本进程 IncarnationID 数值更新（`NodeEpochNewer`——node epoch 由 INCR 单调分配，旧世代进程必已死亡），直接持已抢到的租约进恢复，跳过注定失败的 takeover RPC；非数值形式的 IncarnationID 不跳过。
3. 否则向旧 owner 发送 `takeover` 命令（携带 `LeaseVersion` 与元数据 `new_node_id` / `new_incarnation_id`）。目标节点校验 `LeaseVersion` 与本地一致（不一致返回 `LEASE_VERSION_MISMATCH`）后，对旧连接执行 `Fence(DisconnectStale)`：撤本地订阅与 Hub 条目、关附件——不 Leave、不删目录条目（新 owner 的 fencing 已就位）。目标节点上会话已不存在时返回 `SESSION_NOT_FOUND`，发起方视为成功继续。目标节点进程完全不在（client 对象缺失）时同样按成功应答。
4. **接管失败时的降级**：takeover 命令失败（目标节点刚宕机、命令超时，或目标存活预检报 `TARGET_NODE_NOT_ALIVE`）时，检查目标节点的节点租约——节点租约也已不存在时，视为旧节点已死，继续执行恢复；节点租约仍在则中止恢复，并把抢占到的租约 CAS 回滚到原 owner（把 fencing 还回去，`rollbackSessionTakeover`）；节点租约查询本身失败时同样先尝试回滚再返回错误。
5. 恢复成功后在本地重建会话状态：身份字段、订阅集合、`clusterLeaseVersion = 旧租约版本 + 1`，并 `AddClient` 注册；随后 `restoreSessionSubscriptions` 逐频道重建订阅 + presence 登记 + 本节点投影 +1。**hydrate 是逐频道软失败**：某频道 restore/presence 失败 → 该频道不恢复（presence 失败会把刚加的订阅一并撤掉，投影从未 +1 故无需补偿）、记入失败列表、继续其余频道；不做整体回滚——会话以部分订阅存活，全部频道失败时亦然。`Connected` 发出之后，每个失败频道收到一个顶层 Error 信封：`code=RECOVER_FAILED`、`type=recover_error`、`metadata.entries["channel"]=<ch>`；失败频道同时被排除在快照频道的恢复续读集合之外，客户端按既有顶层错误路径自行重订。
6. **hydrate 不重新过 Authorizer/ACL**：恢复是已授权会话的延续，快照里的订阅关系在建立时已通过当时的 ACL；若权限在会话存活期间被回收，由 Server API（Disconnect/权限变更后的强制下线）而非恢复路径负责。

### 4.4 跨节点恢复与 epoch

跨节点恢复的历史续读逻辑与本地恢复共用同一套 Replayer（internal/runtime/recover.go）：

- **恢复起点**：`fresh=true` 或「resume 且快照 `BrokerEpoch` 与当前 broker epoch 不一致（两边都非空）」→ 从频道历史开头恢复；否则订阅 cursor 带 offset 时从 `offset+1` 续读；cursor 未带 offset 时回退服务端快照记录的逐频道 delivered offset（`ChannelOffsets[ch]+1`，由广播路径的投递确认填充，服务端记录优先于客户端携带值）；既无 cursor 又无服务端记录则跳过该频道的恢复。
- **epoch 的唯一门是快照 `BrokerEpoch`**：它记录快照时刻的 broker 代际，与当前 broker epoch（Redis 部署存于 `ml2:broker:epoch`，首节点 SETNX 写入、集群共享、跨重启持久）不一致时强制全量恢复。客户端协议层的 `Position.stream_epoch` 主要用于进出线转换与客户端自身的代际判断；服务端恢复路径不校验客户端携带的 epoch。
- 全量恢复仅发生在：客户端显式要求从头（`fresh=true`）、或快照世代与当前 broker 不一致（epoch 键被清理/重建、或快照来自极旧的会话）。

集群部署下的推论：epoch 键集群共享，快照的 `BrokerEpoch` 在任意节点上比对结果一致，因此跨节点恢复的续读位置判定与本地恢复完全相同。

### 4.5 按 user 展开的用户索引（user index）

Server API 支持按 `user_id` 对用户的全部 session 做 Publish / Disconnect / Subscribe / Unsubscribe（见[《Server API 参考》](03-server-api.md)）。展开 = 本地 `Hub.SessionsByUser` ∪ 集群 user 索引（按命名空间作用域），随后对每个 session 校验 lease 的 `UserID` 与 `Namespace`（索引不是权威），最后复用现有 session 级命令，不新增集群命令类型。

**本地索引**（internal/session/hub.go 的 `SessionsByUser`）：遍历 `connShard.users` 中该 user 所在分片。空 user_id 的匿名连接不进入按 user API。

**集群索引**（`SessionDirectory` 的 `AddUserSession` / `RemoveUserSession` / `ListUserSessions`，Redis 实现见 pkg/redisbroker/cluster_directory.go）：

| 键 | 类型 | TTL | 说明 |
| --- | --- | --- | --- |
| `ml2:cluster:user:member:<namespace>:<userID>:<sessionID>` | string（值 `"1"`） | 与 session lease 相同 | 成员键：续期时随 lease 一起刷新 |
| `ml2:cluster:user:sessions:<namespace>:<userID>` | set | 无（成员过期靠 repair 修剪） | 用户→session 集合；展开时 `SMEMBERS` 后逐个读 lease 校验 |

**维护**：所有 lease 写入路径共用单一 helper `SyncUserIndex`（internal/runtime/cluster_user_index.go），由 Redis directory 在 lease CAS 成功（唯一的写入方式）与 `DeleteSessionLeaseIfOwner` 之后调用：

- Delete：`RemoveUserSession(旧 user, session)`；
- CAS 成功：user 相同 → `AddUserSession`（刷新 TTL）；user 变了 → 先 Remove 旧 user 再 Add 新 user（resume 后 re-auth 换 user 的场景）；
- 空 `UserID`：只 Remove，匿名 session 不进索引。

索引写失败是 best-effort（记录警告，不影响 lease 本身）：索引是提示，陈旧条目靠 repair 与展开时的 lease 校验收敛。

**修复**：user 索引重建并入统一修复器 `clusterRepairer`（见第 6 节）：每 30 秒一轮，扫描会话租约键，对非空 `UserID` 以 lease 剩余 TTL 重建 `AddUserSession`。集群未启用时不运行。

**禁止**：索引 miss 时做全集群扫描（热路径）。陈旧索引靠 repair 收敛；展开时逐 lease 校验兜底（投毒/过期条目被跳过）。

## 5. 集群级 Survey

单节点 Survey（`Node.Survey`）只向本节点的频道订阅者发送请求并收集应答（`localSurvey`）。

集群模式下 `Survey` 变为两步：

1. **本地调查**：`localSurvey` 正常执行（含发送超时 10 秒、应答会话白名单校验、注册表上限 1000 等既有语义），结果为每条应答标注本节点的 `NodeID` / `IncarnationID`。
2. **集群广播**：经命令总线 `BroadcastCommand` 发送 `ClusterCommandSurvey`，元数据携带 `exclude_self=true` 与 `survey_timeout_ms`（调用方超时换算成毫秒）。广播目标由扫描节点租约键得出（见 3.2），因此只覆盖当前存活的节点实例。

远端节点执行 `handleClusterSurveyCommand`：在其本地执行 `localSurvey`（超时默认 5 秒，可被 `survey_timeout_ms` 覆盖），结果编码进应答元数据返回。客户端发起的 Survey 在广播前还有一步 `count_only` 预检命令（只统计订阅者数不下发请求，用于 `max_survey_subscribers` 门），Server API 路径不受影响。

聚合（`expandClusterSurveyResults`）：本地结果 + 各远端节点的结果合并；某个节点执行失败（命令失败、超时、结果解码失败）时，该节点以一条带 `error` 的 `SurveyResult` 表示（错误码如 `CLUSTER_COMMAND_SEND_FAILED`），整体调查不因此失败。最终结果按 `(NodeID, IncarnationID, SessionID)` 排序。

与单节点的差异可概括为：单节点只问本地订阅者；集群版先问本地、再问所有存活节点，远端失败以错误应答条目呈现而不是整体报错。

## 6. 修复器（repairer）与 membership OnLeave

**解决的问题**：集群级的活跃频道列表（Server API `GetChannels`、`Node.Channels`）来自共享查询投影。投影由每次订阅/退订的 ±1 增量维护（`AdjustChannelSubscriptions`）。若持有订阅的节点突然宕机，其增量（+N）永远无法回退，投影会出现「幽灵订阅者」——频道明明已无人订阅，计数却不为零。同理，user→sessions 索引与死节点的会话 fencing 也需要一个控制面循环来收敛。

**数据结构**：投影按节点隔离（pkg/redisbroker/cluster_query_store.go）。每个节点实例拥有一个 Redis hash：

```
ml2:cluster:channel:owner:<nodeID>:<incarnationID>
```

hash 的字段是频道名、值是本节点在该频道的订阅者计数。增量调整用 Lua 脚本原子执行：`HGET` 当前值 → 加/减 delta → 结果 ≤ 0 时 `HDEL` 该频道，hash 变空则 `DEL` 整个键；否则 `HSET` + 刷新 `EXPIRE`（TTL 10 分钟，`defaultClusterQueryProjectionTTL`）。`ListChannels` 用 `SCAN ml2:cluster:channel:owner:*` 枚举所有 owner hash，`HGETALL` 后按频道聚合求和，按频道名排序。

**修复流程**：所有派生视图的修复收敛为一个 `clusterRepairer`（internal/runtime/cluster_repair.go），`NewCluster` 只启动这一个修复组件。它由一条定时循环驱动两档节奏：

- **30 秒档**（`ClusterRepairerConfig.Interval`）每轮 `repairOnce`：从本地 hub 取活跃频道及真实订阅者数（`GetActiveChannels`），用 `ReplaceNodeChannels` 全量重建本节点的 owner hash（`DEL` + `HSET` + `EXPIRE`，事务管道内完成）；随后收割节点租约已消失的死节点 owner 投影；最后扫描会话租约重建 user→sessions 索引（见 4.5）。由此：本节点上的幽灵增量（因订阅 saga 部分失败等）每轮被纠正；节点宕机后，其 owner hash 在 10 分钟 TTL 内自然消失，全集群聚合计数随之回落——投影修复 + TTL 过期共同构成投影的最终一致。
- **membership 档**（`ClusterRepairerConfig.MembershipInterval`，默认 5 秒、每拍 ±20% 抖动）每拍 `SCAN ml2:cluster:node:*` 维护上一拍存活 incarnation 集合：上一拍有、本拍消失（或 `ExpiresAt` 已过）且非自身的 incarnation 触发 `OnLeave`——逐个删除其名下全部会话租约（走 `DeleteSessionLeaseIfOwner` 的 owner-exact 原子删除，同步清理 user 索引；不做 Evict，对方已死）并删除其 owner 投影，不必等 600 秒会话 TTL。第一拍只建集合不触发；自身 incarnation 永不触发。这是控制面循环，热路径（publish/subscribe/ping）不做任何扫描。

修复成功与失败分别计入 `messageloop_cluster_projection_repairs_total` / `messageloop_cluster_projection_repair_failures_total` 指标。

## 7. Presence 聚合

集群模式下 Presence 存储被替换为 Redis 实现（pkg/redisbroker/presence_redis.go），数据结构：

| 键 | 类型 | TTL | 内容 |
| --- | --- | --- | --- |
| `ml2:presence:member:<channel>:<clientID>` | string | 60 秒（`PresenceTTL`） | `PresenceInfo` JSON（`ClientID`、`UserID`、`ConnectedAt`） |
| `ml2:presence:idx:<channel>` | set | 60 秒（与成员键同 TTL） | 频道内在线客户端 ID 集合索引 |
| `ml2:presence:occ:gen:<channel>` | string（计数器） | 无 TTL | OccupancyGen：每次 `INCR` |

`Add`（订阅登记/心跳刷新）在一条流水线内完成 `SET` 成员 + `SADD` 索引 + `EXPIRE` 索引；`Remove`（退订/断开）`DEL` 成员 + `SREM` 索引；`Get`（查询）先 `SMEMBERS` 索引，再流水线读取每个成员并反序列化，发现成员键已过期缺失则顺手 `SREM` 清理索引残留，并对每个被清理的幽灵成员合成一条 leave 事件（取新 OccupancyGen，经 LiveBus 发布）。

聚合原理：所有节点把 presence 写入同一个 Redis 命名空间，因此任何节点调用 `Get` 拿到的都是全集群的在线集合——Presence 天然按频道聚合，无需额外协议。成员 TTL 由订阅侧通过 `Add` 刷新（客户端 ping 触发的刷新经节流，见 4.1），异常退出的会话会在 TTL 内自然消失。

**加入/离开事件（Occupancy）跨节点投递**：每次 Join/Leave 取单调 OccupancyGen（Redis：`INCR ml2:presence:occ:gen:<ch>`），存完 store 后只 `PublishOccupancy(ch, evt)` 走 live bus 精确频道（`ml2:pubsub:<ch>`，payload 类型 `occupancy`，与 `pub` 信封分开解析）。跨节点投递只依赖 LiveBus 的 Interest，与 `cluster.enabled` 相互独立：控制面关着也能靠 Redis broker 把 occupancy 事件扇到共享同一 Redis 的节点；反之集群开着事件同样只按 Interest 投递。接收端（本机与跨节点同一条 `onOccupancy`）：`interested()` 命中的节点拿到事件，按 `lastApplied[ch][session]` 去迟后 `deliverPresenceEvent` 扇到订阅者；事件主体不被扇回给自己。通配覆盖（`im.**` 的节点收到 `im.room.1` 的 join）由 `CompileInterest` 编译订阅在 Broker 层完成，事件本身只发在精确频道上。事件不进历史，不会混入恢复流。远端订阅（经命令总线，见 3.4）同样走 presence 策略开关。只有频道策略 `legacy_presence_channel: true` 时，精确频道才会额外把旧 JSON 瞬时发到 `ch/__presence` 伴生频道。

## 8. 历史消息

集群模式下历史由 Redis Streams 承载，是全集群共享的：

- 发布路径（pkg/redisbroker/redis.go 的 `Publish`）：Lua 脚本原子完成 `INCR ml2:stream:seq:<channel>`（每频道稠密 seq）+ `XADD` 写入 `ml2:stream:<channel>`（条目带 `s` 字段；`StreamMaxLength` 默认 10000 条、近似截断、`HistoryTTL` 默认 24 小时，seq 键与 stream 同 TTL），从 Stream ID 解析出 offset；再 `PUBLISH` 到 `ml2:pubsub:<channel>` 做实时分发。任意节点发布，全部节点共享同一份历史。
- 消费路径（pkg/redisbroker/pubsub.go）：每个节点只订阅本节点登记过 Interest 的频道（精确 `SUBSCRIBE` + 编译后的 pattern `PSUBSCRIBE`，见[《架构指南》](01-architecture.md)）；断线以指数退避重连（1 秒起、上限 30 秒）。
- **offset 语义**：offset 由 Stream ID 编码而来，`offset = ts<<20 | seq`（毫秒时间戳与序列号拼入 uint64，history.go）。历史查询 `History(ch, sinceOffset, limit)` 用包含起始 ID——Redis broker 与内存 broker 的 `since_offset` 均为包含（inclusive）语义，返回 `offset >= since_offset`；`limit <= 0` 时上限为 `DefaultHistoryLimit`（1000 条）。offset 编码不是稠密序号；中洞检测走条目旁的稠密 seq（`s` 字段）：页内相邻条目 seq 不连续 → `GapReason=Middle`，无 `s` 的 legacy 条目不参与判定（证据链断开时不诬报）。
- **catch-up 洞通知**：重连 catch-up 检出洞（中洞按稠密 seq，尾截按回放批被 `StreamMaxLength` 截断且 stream 仍有更新条目）时，除计数 + Warn 外，broker 经 `SetGapHandler` 第二管道上报一次（每频道每次 catch-up 至多一条），node 侧向该频道本地订阅者（精确 + 命中通配）扇出 `GapNotice` 信封（channel + `gap_reason` + 最后已知安全 position），指标 `messageloop_live_gap_notice_total{reason}`。
- 由于历史与 offset 都来自共享的 Redis Stream，跨节点查询历史得到的是同一份数据；跨节点恢复的世代判定也因 Redis epoch 集群共享而一致（见 4.4）。
- 瞬时消息（`PublishTransient`，occupancy 事件等）不写 Stream，offset 恒为 0，永不进入历史。

## 9. 故障与恢复

### 9.1 节点宕机的影响面

节点宕机时没有主动注销，一切靠 TTL 与探测收敛：

| 数据 | TTL | 宕机后的行为 |
| --- | --- | --- |
| 节点租约 `ml2:cluster:node:*` | 90 秒 | 过期后该节点从广播目标（Survey 等）中消失；向它发命令会在存活预检处直接失败（`TARGET_NODE_NOT_ALIVE`） |
| 会话租约 `ml2:cluster:session:lease:*` | 600 秒（或按心跳缩短） | 过期后会话不再被识别为属于任何存活节点；期间携带该 `SessionId` 的新连接会尝试 takeover，命令快速失败后经节点租约检查降级继续恢复（见 4.3） |
| 会话快照 `ml2:cluster:session:snapshot:*` | 24 小时 | 保留足够久，客户端重连到任意存活节点都能拿到订阅列表等恢复信息 |
| 节点投影 `ml2:cluster:channel:owner:*` | 10 分钟 | 过期后全集群频道列表自动收敛（见第 6 节） |
| presence 成员 | 60 秒 | 过期后在线状态自动收敛（见第 7 节） |

宕机节点承载的连接会被客户端感知为断线；客户端携带 `SessionId` 重连任意存活节点即走 resume 路径（本地命中则本机接管，否则远端接管 + 快照恢复订阅）。未开启 resume 的连接（客户端不带 `SessionId` 重连）自然是全新会话，不受影响。修复器的 membership 节拍（5 秒档）会在死节点租约消失后立即清掉其名下会话租约与投影，不必等 TTL 自然过期。

### 9.2 stale 节点与陈旧状态清理

`DeleteSessionLeaseIfOwner` 的 owner-exact 原子删除（4.1）保证：已死节点留下的会话状态不会被存活的无关节点误删；而指向已过期租约或确属本节点的陈旧状态可以被安全清理。`RemoveSessionIfMatches`（hub.go）保证失败的旧连接在 resume/takeover 后不会把新会话从 hub 驱逐（stale 保护）。

### 9.3 运维建议（建议性说明）

以下为部署建议，不构成对源码行为的承诺：

- **时钟同步**：租约与快照的 `ExpiresAt` 由写入节点本地时钟计算、由读取节点本地时钟比较。节点间时钟偏差过大会导致租约提前过期（会话被误判为 stale）或延迟过期（死节点残影变长），建议集群内使用 NTP 等同步手段。命令总线的 HMAC 时钟窗为 ±30 秒，偏差超过该值的节点发出的命令会被对端拒绝。
- **网络分区**：控制面没有 quorum/选举机制，所有决策依赖 Redis 的读写成功。分区两侧的节点若都能访问 Redis，可能同时认为自己持有某会话的租约（租约无互斥抢占，CAS 只在恢复路径使用）。对于「同一用户重复登录应踢旧连接」这类强一致需求，请在应用层设计容错（例如容忍短时间双活）。
- **Redis 高可用**：Redis 是消息管道与控制面的共同单点。集群模式没有内置的 Redis 故障转移逻辑，建议按生产标准为 Redis 配置持久化与高可用方案（复制、哨兵或集群模式），并在负载均衡/客户端侧考虑 Redis 故障时的降级策略。健康端点可观测 Redis 连通性（见第 10 节），可作为探活依据。

## 10. 可观测性

集群相关的 Prometheus 指标（全部定义于 internal/metrics/metrics.go，命名空间 `messageloop`；完整表见[《可观测性指南》](05-observability.md)）：

| 指标 | 类型 | 含义 |
| --- | --- | --- |
| `messageloop_cluster_command_dedupe_hits_total` | Counter | 命令去重命中次数（发送方命中已存结果、接收方抢占失败） |
| `messageloop_cluster_command_timeouts_total` | Counter | 命令应答等待超时次数 |
| `messageloop_cluster_command_unknown_final_state_total` | Counter | 命令进入「终态未知」的次数 |
| `messageloop_cluster_command_hmac_reject_total{reason}` | CounterVec | HMAC 验签拒绝数（reason ∈ `missing`/`bad`/`skew`/`id`） |
| `messageloop_cluster_node_lease_renew_failures_total` | Counter | 节点租约续期失败次数 |
| `messageloop_cluster_projection_repairs_total` | Counter | 投影修复成功轮次 |
| `messageloop_cluster_projection_repair_failures_total` | Counter | 投影修复失败轮次 |
| `messageloop_bind_fenced_total` | Counter | 会话绑定/接管被 fencing 淘汰的次数 |
| `messageloop_bind_refresh_fail_total` | Counter | 同 fence 续约被判 fenced 的次数 |
| `messageloop_evict_lag` | Histogram | takeover 命令发往远端旧主的往返时延 |
| `messageloop_session_dual_activation_seconds` | Histogram | takeover 重叠窗口时长（目标 0） |

指标解读提示：`dedupe_hits` 持续高位说明存在大量命令重试（可能源于广播重复投递或发送方重试）；`timeouts` 与 `unknown_final_state` 上升通常意味着目标节点不健康、Redis 网络抖动或命令处理超载（处理并发上限 128，见 3.3）。

集群模式下健康端点（`/health`，`server.http.addr`）附加 Redis 连通性探测：以 2 秒超时调用 broker 的 `Ping`，失败时返回 503、JSON 中 `status: "not ready"`、`redis: "unreachable"`（internal/runtime/health.go）。关键日志关键字：`cluster command received`（含 `command_id`、`issued_by`）、`cluster command dedupe hit`、`cluster command timed out waiting for reply`、`cluster repair failed`、`cluster node lease renewal failed`。

## 11. Server API 的集群感知行为

Server API 在集群模式下行为变化，概览如下（完整语义见[《Server API 参考》](03-server-api.md)）：

| 操作 | 集群模式下的行为 |
| --- | --- |
| `Publish`（会话投递） | 经会话租约解析目标节点，远端会话经命令总线路由执行 |
| `Disconnect` / `Subscribe` / `Unsubscribe`（会话定向） | 同上：租约解析 + 命令路由；远端订阅/退订还会在目标节点触发 presence 登记/清除与 join/leave 事件 |
| 按 user 展开 | 本地 user 索引并上集群索引（键含命名空间段），逐 lease 校验后复用 session 级命令 |
| `Survey` | 本地调查 + 向所有存活节点广播（`exclude_self`），聚合结果按节点/会话排序，每个结果带 `node_id` / `incarnation_id` 元数据，远端失败以带 `error` 的结果呈现 |
| `GetChannels` | 读集群共享投影（聚合所有 owner hash），返回全集群活跃频道 |
| `GetPresence` | Presence 存储为 Redis 实现，返回全集群在线成员 |
| `GetHistory` | 从共享 Redis Stream 读取，`since_offset` 为包含语义，跨节点数据一致 |

非集群模式下上述操作只作用于本节点。

## 12. 确定性模拟（测试）

internal/cluster/sim 提供一套进程内、无 Redis、无 `time.Sleep` 的确定性 fencing 模拟夹具：一个共享的内存 `Directory`（按 `SessionID, NodeID, IncarnationID, LeaseVersion` 谓词做真 CAS，check-and-swap 在一把锁下原子完成）、一个可编排的内存命令总线（默认在同一 goroutine 同步投递；`Hold`/`Flush`/`DropNext` 显式编排暂扣与丢命令），以及 `World` 两节点夹具——A（`node-a`）与 B（`node-b`）都是真 `*runtime.Node`，跑生产的 `syncClusterSessionState` / `resumeRemoteSession` / `Fence` 代码路径，incarnation 由脚本写死。该包只服务测试，cmd/server 不装配它。

宪法场景在 internal/runtime/cluster_sim_test.go（`TestSim_*`）：B 抢权后 A 的 ping 刷新不得写回；Bind 后 Evict 同步投递、旧节点 Fence 且不解绑新 owner；丢弃 Evict 后任意时刻至多一个 Attached、旧节点下一拍 sync 自 Fence；本机 Detach/Attach 指针与 fencing 不变；死节点经两次 membership 节拍（首拍只建集合）OnLeave 后可被 `CAS(nil)` 抢占；并发 `CAS(nil)` 恰好一个赢家。运行方式：

```bash
go test -run 'TestSim_' ./internal/runtime
go test ./internal/cluster/sim
```

均不访问 Redis。
