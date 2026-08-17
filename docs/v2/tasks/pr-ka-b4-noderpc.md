# PR-KA-B4 实现规格：NodeRPC HMAC、repair 合一、范围化 `internal/cluster`

| 字段 | 值 |
| --- | --- |
| 标题 | `cluster: HMAC command bus; reject unsigned; one repairer; scoped internal/cluster` |
| 状态 | **Ready** |
| 依赖 | B3 已合（`782061b`）。在 `v2` 分支上做 |
| 设计来源 | [kernel-architecture.md](../kernel-architecture.md) Cluster / NodeRPC、KD-K4、KD-K6、KD-K26、KD-K29、KD-K30、KD-K31 |
| 验收人 | 主 agent |

## 1. 目标

能写 Redis 不再等于能签集群命令。派生视图由 **一个** 控制面循环修复。热路径继续没有盲写。

1. 集群命令总线 **HMAC-SHA256 硬门**：密钥只来自节点配置（环境 / 文件 / YAML），**不进 Redis**。签字段与靶心一致。`IssuedAt` 允许 ±30s。`command_id` 去重 TTL ≥ 15s（今日 10 分钟终态键已满足，保持或写明）。
2. **拒绝未签名命令与未签名应答。** 没有「旧节点收未签名」窗口（KD-K31）。
3. 从 `SessionDirectory` **删除** `PutSessionLease`。生产热路径 A1 已不调用；本 PR 在类型上消灭盲写。
4. 投影修复与 user→sessions 修复合为 **一个** repairer。同一循环（可两档节奏）驱动 Membership `OnLeave`：短周期 SCAN 节点租约（默认 5s±jitter），作废该死 incarnation 名下 session fencing，不必等 600s 会话 TTL。
5. 抽出 `internal/cluster/hmac`（签名/验签）。**禁止**整仓搬到 `internal/*`。

**传输层（范围钉死）：** 本 PR **在现有 Redis Pub/Sub 请求/应答总线上加 HMAC**。不要把总线改成 Redis Stream + consumer group。KD-K6「第一实现即可走 Stream」是许可，不是本步必须；完成标准是「无盲写、无未签名命令」，不是 `XADD`。Stream 换代留给 B4 之后的独立刀。

**不做：** 流式恢复 / client 信封（B3 已合）；改 A1 CAS 算法、A2 `History` gap、A3 `CompileInterest`、A4 Decide、B1 写队列、B2 Occupancy 投递面；确定性模拟（KD-K20）；整仓 `internal/runtime|session|channel|…`；把 Broker 改名为 LiveBus；切 admin 到 `server.v2`；Redis Cluster / sharded；跨区域 Directory；把 HMAC 密钥写入任何 Redis 键。

## 2. 允许改动的文件

- `internal/cluster/hmac/`（新）：`SignCommand` / `VerifyCommand` / `SignResult` / `VerifyResult`、规范字节、常量时间比较。包级测试不依赖 Redis。
- `cluster.go`：`ClusterCommand` / `ClusterCommandResult` 加 `Signature`（及 result 的 `IssuedAt`）；`SessionDirectory` **删除** `PutSessionLease`；可加 `OnLeave` 回调登记；`ClusterDependencies` 收成一个 repairer（见 §5.3）
- `cluster_commands.go`：仅当发送前需要经过已签名的 bus（通常不用改业务 handler）
- `cluster_state.go`：`noopSessionDirectory` 去掉 `PutSessionLease`；禁止把 Put 引回热路径
- `cluster_projection_repair.go` / `cluster_user_index_repair.go`：收成一个类型（可保留文件名作别名，但只许一条 ticker 实现）
- `cluster_user_index.go`：仅当 OnLeave / Delete 路径要复用 `SyncUserIndex`
- `pkg/redisbroker/cluster_command_bus.go` 及 `cluster_command_bus_test.go`：发送前签名；`handleMessage` 先验签再 claim；应答签名；构造函数收密钥
- `pkg/redisbroker/cluster_directory.go`：删除 `PutSessionLease`（测试改走 `CompareAndSwapSessionLease`）
- `config/config.go`、`config/config_test.go`、`config-example.yaml`：`cluster.hmac_key` / `cluster.hmac_key_file`；`enabled` 且二者皆空 → `Validate` 失败
- `cmd/server/main.go`：把密钥交给 `NewClusterCommandBus`；文件读取失败则拒启动
- 所有因删 `PutSessionLease` / 改 `NewClusterCommandBus` 签名而编译失败的 fake 与测试（`cluster_*_test.go`、`cluster_remote_test.go`、`client_fix_test.go`、`pkg/redisbroker/*_test.go` 等）
- `docs/developer/04-cluster.md`、`docs/deployment.md`：信任边界改为 HMAC 硬门
- `docs/v2/tasks/pr-ka-b4-noderpc.md`（完成备注）

禁止：改 `protocol/**`、SDK 业务、`client.go` 热路径（ping 已走 A1 CAS，不要动）、`hub.go` 扇出、`recover.go`、`occupancy.go`、`authorizer.go`、`interest.go`、git commit/push。

## 3. 现状（动手前再读）

- `pkg/redisbroker/cluster_command_bus.go` 包注释写明：**无签名**；`IssuedBy` 仅审计。`SendCommand` 填 `IssuedBy` + `IssuedAt` + `CommandID`，JSON `PUBLISH` 到 `ml:cluster:cmd:req:{node}:{inc}`。`handleMessage` 解 JSON 后直接 claim / 执行。
- 应答同样无签名：`publishCommandResult` 把 `ClusterCommandResult` JSON 发到 `reply_channel`。
- 去重：`ml:cluster:cmd:state:{commandID}`，终态 TTL 10 分钟（≥ 15s，保留）。handler timeout 10s；claim 租约 30s。
- `SessionDirectory.PutSessionLease` 仍在接口与 `redisSessionDirectory` 上。生产调用方：`git grep` 热路径应为 **零**（A1）。测试 fake 仍实现该方法。
- 两个独立 repair 循环：`cluster_projection_repair.go`（30s，重发本机频道投影 + 收割死节点投影）与 `cluster_user_index_repair.go`（30s，SCAN 会话租约 `AddUserSession`）。**没有** `OnLeave`。死节点旁路只在 resume：`GetNodeLease(旧)==nil` 时允许继续；旧 session lease 仍在则 `CAS(nil)` 不能当首次 Bind。
- `config.ClusterConfig` 只有 `enabled` / `node_id` / `backend`。`cmd/server/main.go` `setupCluster` 直接 `NewClusterCommandBus(redis, nodeID, inc)`。
- 仓库无 HMAC 实现、无 `internal/cluster`。

## 4. 类型与规范字节

### 4.1 配置

```go
// config.ClusterConfig 增加（名称可同语义）：
HMACKey     string `yaml:"hmac_key"`
HMACKeyFile string `yaml:"hmac_key_file"`
```

`Validate`（`cluster.enabled==true` 时）：

| 条件 | 结果 |
| --- | --- |
| `hmac_key` 与 `hmac_key_file` 都空 | 失败，文案含 `cluster.hmac_key is required` |
| 两者都非空 | 失败，文案含 `only one of hmac_key or hmac_key_file` |
| `hmac_key` 非空且 `len([]byte(key)) < 32` | 失败，文案含 `at least 32 bytes` |
| `enabled==false` | 不要求密钥 |

`hmac_key_file`：启动时读入，trim 末尾单一换行；内容 < 32 字节则拒启动。密钥 **禁止** 出现在 Info 日志、metrics label、Redis SET/PUBLISH 载荷。

测试可用 32 字节字面量（例如 32 个 `k`）。示例 YAML 写 `hmac_key_file` 或注释掉的 `hmac_key`，默认 `enabled: false`。

### 4.2 信封字段

```go
type ClusterCommand struct {
    // 现有字段保留（含 IssuedBy 审计）
    Signature string // hex(HMAC-SHA256(canonical(command)))
}

type ClusterCommandResult struct {
    // 现有字段保留
    IssuedAt  time.Time
    Signature string // hex(HMAC-SHA256(canonical(result)))
}
```

`IssuedBy` **不**进入规范字节（可伪造，不构成安全边界）。

### 4.3 规范字节（必须逐字节一致）

命令（UTF-8，`\n` 分隔，**最后一行也有** `\n`）：

```
v1
{Type}
{SessionID}
{TargetIncarnationID}
{LeaseVersion decimal, no leading zeros except 0}
{hex.EncodeToString(sha256(Payload))}
{CommandID}
{IssuedAt.UTC().Unix() decimal}
```

- `Payload==nil` 与 `Payload==[]byte{}` 都按空切片做 `sha256`（同一摘要）。
- `Type` 用 `string(cmd.Type)`。
- 禁止 JSON 规范化、禁止把整个命令 JSON 拿去签（字段顺序不稳）。

应答：

```
v1-result
{CommandID}
{Status}
{ErrorCode}
{SessionID}
{NodeID}
{IncarnationID}
{IssuedAt.UTC().Unix() decimal}
```

签名：`hex.EncodeToString(HMAC-SHA256(key, canonical))`。比较：`hmac.Equal`（解码 hex 后比原始 MAC，或比 hex 字节；不要 `==` 字符串后短路）。

`Verify` 失败原因（给指标 / 日志，**不要**执行 handler）：

| 原因 | 条件 |
| --- | --- |
| `missing` | `Signature` 空 |
| `bad` | hex 非法或 MAC 不匹配 |
| `skew` | `IssuedAt` 零值，或 `|now.UTC().Unix() - IssuedAt.Unix()| > 30` |
| `id` | `CommandID` 空（命令） |

时钟用 `time.Now()`；测试可注入 `now func() time.Time`（hmac 包或 bus 上）。

### 4.4 构造

```go
// hmac 包
func SignCommand(key []byte, cmd *messageloop.ClusterCommand) error
func VerifyCommand(key []byte, cmd *messageloop.ClusterCommand, now time.Time) error
func SignResult(key []byte, res *messageloop.ClusterCommandResult) error
func VerifyResult(key []byte, res *messageloop.ClusterCommandResult, now time.Time) error
```

`NewClusterCommandBus(cfg, nodeID, incarnationID, hmacKey []byte)`。`len(hmacKey) < 32` → `Start` 返回错误（即使 Validate 已挡，bus 自己再挡一层）。

单机 / `backend=memory` / noop bus：**没有** Redis 跳，不要求 HMAC。不要给内存 bus 发明一套密钥协议。

## 5. 算法

### 5.1 发送

```
SendCommand:
    现有：填 CommandID / IssuedAt / IssuedBy / 去重 / targetAlive
    SignCommand(key, cmd)          // 失败则返回 error，禁止 PUBLISH
    PUBLISH json(cmd)              // JSON 含 Signature
    waitForReply:
        解 JSON
        VerifyResult(key, result, now)
        失败 → 记指标，当作未收到（继续等到 deadline，最终 unknown_final_state）
        成功且 CommandID 匹配 → 返回
```

禁止：先 PUBLISH 再补签；把密钥放进 `Metadata` / Payload / Redis 状态键。

`BroadcastCommand` 每份拷贝各自 `SignCommand`（每份已有新 `CommandID`）。

### 5.2 接收

```
handleMessage:
    unmarshal
    VerifyCommand(key, cmd, now)   // 失败：指标 + 日志，return；不 claim、不跑 handler、不回 succeeded
    此后才是今日的 claim / handler / store / publishCommandResult
publishCommandResult:
    result.IssuedAt = now（若零）
    SignResult(key, result)
    PUBLISH json(result)
```

验签失败 **不要** 把失败写成 `command_id` 终态（攻击者可选用受害者的 id 污染去重）。没有合法 `CommandID` 时不写 state 键。

包注释与 `docs/deployment.md` / `04-cluster.md` 必须改成：未签名命令被拒；密钥在节点配置；Redis 隔离仍是纵深防御，不再是唯一边界。

### 5.3 一个 repairer + OnLeave

删除「两个独立 `ClusterLifecycle` 各开一条 ticker」的装配。允许：

- 新类型 `clusterRepairer`（建议新文件 `cluster_repair.go`）实现投影重发、user-index 重建、死投影收割、OnLeave；**或**
- 保留两个类型名作薄封装，但 `NewCluster` 只 `Start` **一个** 对象。

节奏：

| 工作 | 默认周期 |
| --- | --- |
| SCAN 节点租约 → `OnLeave` | **5s ± jitter**（建议 ±20%，`time.Duration` 可测） |
| 重发本机频道投影 + 收割死投影 + 重建 user 索引 | 30s（可与今日相同，可挂在同一 ticker 用计数器隔拍） |

`OnLeave(incarnation)`：

1. 维护上一拍活着的 `(NodeID, IncarnationID)` 集合。第一拍只建集合，不 OnLeave。
2. 上一拍有、本拍 `GetNodeLease` 为空（或 `ExpiresAt` 已过）且不是自己 → OnLeave。
3. `ListSessionLeases`（已有）中 `NodeID+IncarnationID` 匹配者：`DeleteSessionLease`（走现有 user-index 同步）。不要 `Evict`（对方已死）。不要等 600s。
4. `DeleteNodeProjection`（可与今日收割合并）。
5. 若登记了回调则调用（测试用）。宽限期 = 一次 SCAN 周期。

这是控制面，允许 `if cluster`。热路径（publish / subscribe / ping）不准 SCAN。

`NewCluster` / `setupCluster`：只装配这一个 repairer。`UserIndexRepairer` 字段可删，或改成指向同一实例；更新 fake。

### 5.4 删除 `PutSessionLease`

- 接口、`redisSessionDirectory`、所有 fake **删除** 该方法。
- 测试需要「写入一条 lease」时用 `CompareAndSwapSessionLease(nil, lease, ttl)` 或测试辅助（不得是无条件 SET 的导出方法）。
- 生产 `.go`（非 `_test.go`）`git grep PutSessionLease` 必须为零。

### 5.5 `internal/cluster` 范围

**必须：** `internal/cluster/hmac`（或 `internal/cluster` 根文件）承载规范字节与 HMAC。`pkg/redisbroker` 的 bus **import 该包**，不要在 bus 里复制一份哈希拼接。

**禁止：**

- 移动 `client.go` / `hub.go` / `session.go` / `node.go` / `recover.go` / `occupancy.go` / `authorizer.go`
- 新建 `internal/runtime`、`internal/session`、`internal/protocol`、`internal/channel`、`internal/stream`、`internal/occupancy`、`internal/authz`、`internal/rpc`、`internal/survey`、`internal/admin`
- 为了搬家改模块路径或把根包改成空壳 re-export

**允许：** 仅当无 import 环时，把 `cluster_command_bus.go` / `cluster_directory.go` 挪到 `internal/cluster`。有环就留在 `pkg/redisbroker`。验收不要求搬家。

## 6. 接入

- `cmd/server/main.go` `setupCluster`：从 `hmac_key` 或读 `hmac_key_file` 得到 `[]byte`，传入 `NewClusterCommandBus`。读文件失败 → 返回 error，进程不得带着空密钥启动。
- Redis 集成测试：给 bus 32 字节测试密钥；未带密钥的旧 `NewClusterCommandBus` 调用全部改掉。
- 指标（建议挂现有 `Metrics`）：`cluster_command_hmac_reject_total{reason}`，`reason∈missing,bad,skew,id`。没有现成 counter 就加一个。不要把密钥当 label。
- Directory 与业务数据面 Redis ACL 分离：文档提一句即可，**不**做运行时双客户端（本 PR 不做）。

## 7. 必须存在的测试

1. **HMAC 向量**：同一输入两次 `SignCommand` hex 相同；手工改 `Type` / `SessionID` / `LeaseVersion` / `Payload` / `CommandID` / `IssuedAt` 任一字段 → `Verify` 失败。
2. **未签名命令**：Redis bus 注入无 `Signature` 的 JSON → handler **零次**；无终态键（或至少不是 `succeeded`）。
3. **坏签名**：合法信封改最后一个 hex 字符 → handler 零次。
4. **时钟**：`IssuedAt = now+31s` 与 `now-31s` → `skew` 拒绝；`now+29s` 接受。用注入时钟，禁止 `Sleep(31s)`。
5. **空 CommandID**：即使 MAC 按空 id 签对 → 拒绝（`id`）。
6. **合法往返**：A 签、B 验、handler 跑一次、应答带签名、A `VerifyResult` 成功。
7. **伪造应答**：waitForReply 收到无签 / 坏签 result → 不当成功；最终超时路径（可用短 timeout）。
8. **密钥不进 Redis**：spy 或 SCAN 本测试用前缀下的 SET/PUBLISH 载荷，不得出现密钥字节。
9. **配置**：`enabled` + 无密钥 → `Validate` 失败；`hmac_key` < 32 字节失败；`enabled: false` 无密钥通过。
10. **无 `PutSessionLease`**：接口与生产代码 grep 为零；fake 编译即可。
11. **repair 合一**：`NewCluster` 之后只有一条 repair 实现在跑（可用计数器 / 单类型断言）。user-index 缺成员仍会被补上；死节点投影仍被收割（迁移今日两份测试）。
12. **OnLeave**：写入 session lease 指向 inc-B；删除 / 过期 B 的 node lease；推进 repair（注入 ticker 或导出 `repairOnce`，禁止固定长 Sleep）→ 该 session lease 被删；随后 `CAS(nil, newLease, ttl)` 成功。自己的 incarnation 不得 OnLeave。
13. `go test ./...`；`go test -race . ./pkg/redisbroker`。`internal/cluster/hmac` 随 `./...` 跑。

禁止固定长 Sleep 代替 Ready / 导出的 `repairOnce` / 注入时钟。

无 Redis 时 HMAC 包测试与 Validate 测试仍须跑；Redis bus 测试沿用现有 Skip。

## 8. 验收清单

1. Redis 命令总线拒绝未签名 / 坏签 / 偏斜命令；handler 不跑。
2. 应答同样校验；伪造成功应答不能让 `SendCommand` 返回 succeeded。
3. 密钥只来自配置；任何 Redis 载荷都不含密钥。
4. `PutSessionLease` 从接口与生产代码消失。
5. 一个 repairer；OnLeave 作废死 incarnation 的 session fencing。
6. 存在 `internal/cluster/hmac`；根包业务文件未搬家。
7. 未改 A1 CAS、A2/A3/A4、B1/B2/B3 热路径。
8. 未把总线改成 Redis Stream。
9. 测试命令绿。

## 9. 完成报告

- 文件列表
- §8 逐条证据
- 测试命令与结果
- 偏离（应无）

## 10. 实现备注（完成后填写）

（实现者填写）
