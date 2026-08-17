# PR-KA-C2 实现规格：`node_epoch` 只准 INCR（KD-K27）

| 字段 | 值 |
| --- | --- |
| 标题 | `cluster: allocate incarnation as monotonic node_epoch; no UUID` |
| 状态 | **Ready** |
| 依赖 | C1 已合（`0adfdf3`）。在 `v2` 分支上做 |
| 设计来源 | [kernel-architecture.md](../kernel-architecture.md) 三把时钟、KD-K27、KD-K31 |
| 验收人 | 主 agent |

## 1. 目标

进程世代不再用随机 UUID。`Fencing.node_epoch` 按靶心发号：启动时取一次，**只准**单调 `INCR`（Redis）或进程内计数器（memory）。更大的 epoch 更新。

1. 生产路径（`IncarnationID` 空）**禁止** `uuid.NewString()`。
2. Redis：`INCR ml:cluster:node_epoch:{node_id}`，把返回值写成十进制字符串作为 `IncarnationID`。
3. memory / 单测未显式传入时：按 `node_id` 的进程内 `uint64` 计数器，每次分配 `+1`。
4. 测试与 C1 夹具 **显式传入** 的 `inc-a` / `inc-b` 原样保留，不走分配器。
5. CAS 谓词、HMAC 规范字节、命令总线通道名 **不改形状**：`IncarnationID` 仍是 `string`。十进制 `"1"` / `"2"` 就是 incarnation。

**不做：** 命令总线换 Redis Stream；改 A1 CAS 四字段谓词；改 B4 HMAC 签哪些字段；改 C1 场景（它们传入固定 ID）；`ml2:` 键前缀换代；整仓 `internal/*` 搬家；改 OccupancyGen / StreamEpoch 发号。

## 2. 允许改动的文件

- `cluster.go`：`ClusterOptions.normalize()` 去掉空 ID 时的 `uuid.NewString()`；可把空 ID 留给分配器
- `cluster_state.go`：`noopSessionDirectory` 如需实现分配器
- `cluster_epoch.go`（新，建议）+ `cluster_epoch_test.go`（新）：`NodeEpochAllocator`、`ParseNodeEpoch`、memory 计数器
- `pkg/redisbroker/cluster_directory.go` 及测试：`NextNodeEpoch` = `INCR` 上述键
- `cmd/server/main.go`：`normalizeClusterOptions` / `setupCluster` 先分配再接线（bus / lease manager 需要 ID）
- 所有因「空 IncarnationID 不再变 UUID」而失败的测试（给它们显式 ID，或走 memory 分配器）
- `docs/developer/04-cluster.md`：incarnation 发号改为 INCR
- `docs/v2/tasks/pr-ka-c2-epoch.md`（完成备注）

禁止：改 `protocol/**`、SDK、`client.go` / `hub.go` / `session.go` / `recover.go` / `occupancy.go` / `authorizer.go`、HMAC 规范字节、C1 `internal/cluster/sim` 宪法语义、git commit/push。

## 3. 现状（动手前再读）

- `ClusterOptions.normalize()`：`IncarnationID` 空 → `uuid.NewString()`。
- `cmd/server/main.go` `normalizeClusterOptions`：**又**生成一次 UUID，再拿去建 `NewClusterCommandBus` / `NewClusterNodeLeaseManager`。总线通道是 `ml:cluster:cmd:req:{node}:{inc}`，所以 ID 必须在 `NewCluster` 之前就有。
- `ClusterSessionLease` / `ClusterNodeLease` / HMAC `TargetIncarnationID` 都是字符串。CAS 比 `IncarnationID` 相等，不比大小。
- 节点租约键：`ml:cluster:node:{nodeID}:{inc}`。`ListNodeLeases` SCAN `ml:cluster:node:*`。epoch 键 **不得**落在这个前缀下（`ml:cluster:node_epoch:{id}` 可以：`node:` vs `node_`）。
- C1 / 绝大多数单测传入 `IncarnationID: "inc-a"`。这些必须继续能用。
- StreamEpoch（broker）仍是 UUID / SET NX，**本 PR 不动**。

## 4. 类型

```go
// NodeEpochAllocator issues the next node_epoch for nodeID.
// Redis: INCR ml:cluster:node_epoch:{nodeID} (existing prefix scheme).
// Memory: per-nodeID process-local uint64, each call +1 starting from 1.
type NodeEpochAllocator interface {
    NextNodeEpoch(ctx context.Context, nodeID string) (uint64, error)
}

// FormatNodeEpoch renders epoch as the IncarnationID string (decimal, no
// leading zeros except 0). Epoch 0 is not a valid issued value.
func FormatNodeEpoch(epoch uint64) string // strconv.FormatUint(epoch, 10)

// ParseNodeEpoch reports whether incarnationID is a decimal epoch issued
// by this allocator. "inc-a" → (_, false)；"12" → (12, true).
func ParseNodeEpoch(incarnationID string) (uint64, bool)
```

- `redisSessionDirectory` 实现 `NodeEpochAllocator`。不要把方法硬塞进 `SessionDirectory` 接口（避免所有 fake 都要加空方法）。需要时 type-assert。
- memory 分配器：根包导出 `MemoryNodeEpochAllocator`（或包级函数），按 `nodeID` 分桶，进程内单调。不同 `nodeID` 互不影响。
- `INCR` 从 1 起（Redis 对不存在的键 INCR → 1）。memory 同样第一次返回 1。

比较（靶心「更大的 epoch 更新」）：

```go
// NodeEpochNewer reports whether a is a strictly newer process generation
// of the same node than b. Both IDs must ParseNodeEpoch OK；否则 false。
func NodeEpochNewer(a, b string) bool
```

本 PR **不必**改 Bind / CAS 去用 `NodeEpochNewer`。CAS 仍是四字段相等。提供函数 + 单测即可，供后续 Membership / 拒收旧 incarnation 命令使用。

## 5. 算法

### 5.1 分配时机

`cmd/server` `setupCluster`（redis 后端）顺序钉死：

```
dir = NewSessionDirectory(...)
epoch, err = dir.(NodeEpochAllocator).NextNodeEpoch(ctx, nodeID)
if err != nil: 拒启动
opts.IncarnationID = FormatNodeEpoch(epoch)
再 NewClusterCommandBus / QueryStore / NodeLeaseManager / Repairer / NewCluster
```

禁止：先 UUID 再覆盖；INCR 失败仍带着空/随机 ID 启动。

`NewCluster` / `normalize()`：

| 输入 | 行为 |
| --- | --- |
| 调用方已设 `IncarnationID`（测试、C1） | 原样使用 |
| 空 + 能 type-assert 到 `NodeEpochAllocator` 的 Directory | 分配并填上 |
| 空 + memory/noop、无 Redis | 用 memory 分配器 |
| 空 + redis 后端且 Directory 不能分配 | **error**，禁止回落 UUID |

从 `cluster.go` 和 `cmd/server` **删除** 为 incarnation 调用 `uuid.NewString()`。

### 5.2 键与形状

| 适配器 | 发号 |
| --- | --- |
| Redis | `INCR ml:cluster:node_epoch:{nodeID}`（走现有 Options 的集群前缀风格；**不要**写成 `ml:cluster:node:{id}:epoch`，以免被 node-lease SCAN 吃掉） |
| memory | `map[nodeID]*atomic.Uint64` 或等价，第一次 1 |

`IncarnationID` = `FormatNodeEpoch(n)`，例如 `"1"`、`"2"`。禁止 UUID、禁止 `node-a-1` 这种拼装（比较和通道名会变复杂）。

### 5.3 不改的合同

- CAS 仍比 `SessionID, NodeID, IncarnationID, LeaseVersion`。
- HMAC 仍签 `TargetIncarnationID` 字符串。
- 总线通道仍是 `req:{nodeID}:{incarnationID}`。
- C1 `World` 继续写死 `inc-a` / `inc-b`。
- 已有传入 `"inc-a"` 的测试保持合法（`ParseNodeEpoch("inc-a")==false`，它们不是生产发号）。

## 6. 接入

- `docs/developer/04-cluster.md`：写明启动时 INCR，incarnation = 十进制 epoch；旧文档若写 UUID 世代则改掉。
- 生产日志可以打 `node_id` + `incarnation_id` + 解析出的 epoch。不要把分配器当热路径（只在启动取一次）。

## 7. 必须存在的测试

1. **Redis INCR**：两次 `NextNodeEpoch` 同一 `nodeID` → `2 == 1+1`；`FormatNodeEpoch` 为 `"1"` 再 `"2"`。无 Redis 则 Skip。
2. **memory 计数器**：同一 `nodeID` 连续分配严格 +1；另一 `nodeID` 从 1 另起。
3. **无 UUID**：`cluster.go` / `cmd/server/main.go` 生产代码里，为 `IncarnationID` 服务的 `uuid.NewString()` 为零（可用测试读源文件，或 `git grep` 断言）。broker StreamEpoch 的 UUID **保留**。
4. **显式 ID**：`NewCluster(..., IncarnationID: "inc-a")` 启动后 `Cluster.IncarnationID()=="inc-a"`。
5. **空 ID + memory**：`NewCluster` 不传入 ID 时得到可 `ParseNodeEpoch` 的十进制，且两次独立 `NewCluster` 同 `nodeID` 得到不同且递增的值。
6. **空 ID + redis 无分配器**：`NewCluster` 返回 error，文案含 `node_epoch` 或 `incarnation`。
7. **`NodeEpochNewer`**：`"2"` 新于 `"1"`；`"10"` 新于 `"2"`（数值不是字典序）；`"inc-a"` 与任何值 → false。
8. C1 `go test -run 'TestSim_' .` 仍绿（不改场景语义）。
9. `go test ./...`；`go test -race . ./pkg/redisbroker`。

禁止固定长 Sleep。宪法 / 分配测试不依赖 Redis 墙钟。

## 8. 验收清单

1. 生产分配 incarnation 不再调用 `uuid.NewString()`。
2. Redis 用指定键 `INCR`；memory 按 node 单调。
3. 显式 `IncarnationID`（含 C1）不受影响。
4. CAS / HMAC / 总线通道形状未改。
5. epoch 键不被 node-lease SCAN 收割。
6. 未做 Stream / `ml2:` / 改 OccupancyGen。
7. 测试命令绿。

## 9. 完成报告

- 文件列表
- §8 逐条证据
- 测试命令与结果
- 偏离（应无）

## 10. 实现备注（完成后填写）

（实现者填写）
