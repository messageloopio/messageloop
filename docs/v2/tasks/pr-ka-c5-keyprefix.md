# PR-KA-C5 实现规格：Redis 键前缀换代 `ml:` → `ml2:`（KD-K31）

| 字段 | 值 |
| --- | --- |
| 标题 | `redisbroker: bump Redis key prefix generation to ml2:` |
| 状态 | **Accepted**（2026-08-17 主 agent 终验通过，尚未 commit） |
| 依赖 | C4 已合（`d99cd86`）。在 `v2` 分支上做 |
| 设计来源 | [kernel-architecture.md](../kernel-architecture.md) 舰队 / KD-K31 |
| 验收人 | 主 agent |

## 1. 目标

v2 是独立大版本（KD-K31），不与 v0.2/v1.0 组网。但 Redis 键前缀仍是旧代的 `ml:`：与旧树共用 Redis DB 时，同形键会互相覆盖（stream、pubsub 通道、presence、cluster 租约、命令总线全部撞名）。本 PR 把**全部** Redis 键前缀换代为 `ml2:`:

1. `pkg/redisbroker/options.go` 的 9 个默认值常量从 `ml:` 改为 `ml2:`（其余部分不变）。
2. `pkg/redisbroker/cluster_command_bus.go` 的 3 个包级前缀常量（`cmd:stream:` / `cmd:reply:` / `cmd:state:`）同步换代——它们**不**派生自 `ClusterPrefix`，只改 options 默认值带动不了。
3. 换代后生产代码中不得再出现 `ml:` 字面量（注释除外，注释里的键形也要改成 `ml2:`）。
4. 文档中的键形同步更新。

**不做：** 新增前缀可配项（配置层今日不暴露前缀，本 PR 不引入）；改任何键的**结构**/数据类型/TTL/语义；把命令总线前缀重构为派生自 `ClusterPrefix`（保持包级常量，只改值）；proto / SDK；旧代数据迁移（独立版本无迁移义务）；`client.go` / `hub.go` / `session.go`；memory broker / sim / HMAC。

## 2. 允许改动的文件

- `pkg/redisbroker/options.go`：9 个默认值常量
- `pkg/redisbroker/cluster_command_bus.go`：3 个包级前缀常量及相关注释
- `pkg/redisbroker/cluster_command_bus_test.go`：硬编码 `ml:cluster:cmd:*` 字面量的 4 处（见 §3）
- `pkg/redisbroker/cluster_directory.go`、根包 `cluster_epoch.go`：仅注释中的键形
- 因换代而必须改断言的其他测试文件（原则上不应有——其余测试都经 `opts.*` 取前缀，自动跟随）
- `docs/developer/01-architecture.md`、`02-configuration.md`、`03-admin-api.md`、`04-cluster.md`、`docs/deployment.md`：键形 `ml:` → `ml2:`
- `docs/v2/kernel-architecture.md`：仅「舰队」一节（换代建议改为已落地表述）
- `docs/v2/tasks/pr-ka-c5-keyprefix.md`（完成备注）

禁止：改键结构/TTL/语义；改配置 schema（`config.RedisConfig` 不加字段）；改 proto、SDK、memory broker、`internal/cluster/*`；改 `client.go`/`hub.go`/`session.go`；动 `docs/design`、`docs/review`、`docs/archive`、`docs/v2/tasks/` 下既有规格（历史文档保持原样）；git commit/push。

## 3. 现状（动手前再读）

### 3.1 前缀全清单（12 个常量）

`pkg/redisbroker/options.go:10-18`（9 个，`NewOptions` 一律用默认值，`config.RedisConfig` 不暴露前缀）：

| 常量 | 今日值 | 新值 | 用途 |
| --- | --- | --- | --- |
| `defaultStreamPrefix` | `ml:stream:` | `ml2:stream:` | 频道历史 stream；派生 `seq:`（C4 稠密 seq 计数）、`retained:`（first_retained 标记） |
| `defaultPubSubPrefix` | `ml:pubsub:` | `ml2:pubsub:` | 实时投递通道；派生控制频道 `__live__` |
| `defaultPresencePrefix` | `ml:presence:` | `ml2:presence:` | `idx:`(set)、`member:`(TTL string)、`occ:gen:`(INCR) |
| `defaultClusterPrefix` | `ml:cluster:` | `ml2:cluster:` | `node_epoch:`(C2 INCR)、`user:member:`、`user:sessions:` |
| `defaultClusterNodePrefix` | `ml:cluster:node:` | `ml2:cluster:node:` | 节点租约（SCAN 源，`ClusterNodePrefix+"*"`） |
| `defaultClusterSessionLeasePrefix` | `ml:cluster:session:lease:` | `ml2:cluster:session:lease:` | 会话租约 |
| `defaultClusterSessionSnapshotPrefix` | `ml:cluster:session:snapshot:` | `ml2:cluster:session:snapshot:` | 会话快照 |
| `defaultClusterChannelPrefix` | `ml:cluster:channel:` | `ml2:cluster:channel:` | `owner:` hash（SCAN `owner:*`） |
| `defaultEpochKey` | `ml:broker:epoch` | `ml2:broker:epoch` | broker epoch（UUID，SETNX） |

`pkg/redisbroker/cluster_command_bus.go:42-44`（3 个包级常量，不挂 Options）：

| 常量 | 今日值 | 新值 |
| --- | --- | --- |
| `clusterCommandStreamPrefix` | `ml:cluster:cmd:stream:` | `ml2:cluster:cmd:stream:` |
| `clusterCommandReplyPrefix` | `ml:cluster:cmd:reply:` | `ml2:cluster:cmd:reply:` |
| `clusterCommandStatePrefix` | `ml:cluster:cmd:state:` | `ml2:cluster:cmd:state:` |

### 3.2 测试中的硬编码字面量（必须同步）

- `cluster_command_bus_test.go:256,259`：`"ml:cluster:cmd:reply:test"` 断言
- `cluster_command_bus_test.go:972`：`PSubscribe "ml:cluster:cmd:*"`
- `cluster_command_bus_test.go:1046`：`PSubscribe "ml:cluster:cmd:*"`
- `cluster_command_bus_test.go:1250`：`PubSubChannels "ml:cluster:cmd:*"`

改为 `ml2:` 字面量，或更稳地用包级常量拼接（如 `clusterCommandReplyPrefix+"test"`、`clusterCommandStreamPrefix` 的公共祖先 pattern）。其余测试全部经 `opts.*`/`NewOptions` 取前缀，自动跟随，不应需要改。

### 3.3 必须保持的结构约束

- `node_epoch:` 键刻意落在 `node:` 前缀**外**（避免被节点租约 SCAN 误伤），换代后 `ml2:cluster:node_epoch:` vs `ml2:cluster:node:*` 的避撞结构保持原样；`TestRedisSessionDirectory_NodeEpochKeyEscapesNodeLeaseScan` 必须仍绿。
- C3 的 inbox stream 键同样刻意不在 `node:` 下。
- SCAN pattern（`ClusterNodePrefix+"*"`、`ClusterSessionLeasePrefix+"*"`、`ClusterChannelPrefix+"owner:*"`）都经 opts 拼接，自动跟随。

### 3.4 不会受影响的

memory broker、C1 sim、HMAC、SDK、`_examples/`：无 `ml:` 键。`docs/design`、`docs/review`、`docs/archive`、`docs/v2/tasks/` 既有规格是历史文档，保持 `ml:` 原样。

## 4. 实施

1. 改 §3.1 的 12 个常量值；同文件注释里的键形一并改。
2. 改 §3.2 的 4 处测试字面量（优先用常量拼接）。
3. grep 门禁（生产代码）：`grep -rn '"ml:' --include='*.go' .` 必须零命中（注释中的键形也算，一并改成 `ml2:`；历史文档目录除外）。
4. 文档：`docs/developer/01/02/03/04`、`docs/deployment.md` 中的 `ml:` 键形全部改 `ml2:`；`kernel-architecture.md`「舰队」一节把「建议换代」改为「已换代（C5）」。
5. 不新增配置项、不改键结构。

## 5. 必须存在的测试

1. **换代隔离**（新增）：真实 Redis 上，broker `Publish` + presence/cluster 键写入后，`SCAN ml:*`（旧代）无任何本进程写入的键，`SCAN ml2:*` 有对应键。注意测试共用一个 DB，断言应限定本测试唯一频道名/节点名，不断言全库为空。
2. **既有测试全绿**：前缀经 opts 自动跟随，不应有语义回归；§3.2 的 4 处更新后全绿。
3. `TestRedisSessionDirectory_NodeEpochKeyEscapesNodeLeaseScan` 仍绿（避撞结构保持）。
4. `TestClusterCommandBus_KeyNeverWrittenToRedis` / `SendCommandUsesStreamNotPublish` / `NoRequestPubSubSubscription` 仍绿（pattern 换代后仍匹配命令总线键族）。
5. `go test -count=1 ./pkg/redisbroker`；`go test -count=1 -run "TestSim_|TestClusterCommandBus" .`；`go test ./...`；`go test -race . ./pkg/redisbroker`。

禁止用固定长 `Sleep` 代替同步点。无 Redis 则 Skip 并写明。

## 6. 验收清单

1. 12 个前缀常量全部 `ml2:`；生产代码 grep 无 `ml:` 字面量（含注释键形）。
2. 键结构/数据类型/TTL/语义零变化；SCAN pattern 与避撞结构保持。
3. 配置 schema 未加字段；proto/SDK/memory/sim/HMAC 零改动。
4. 文档（developer/01/02/03/04、deployment）键形同步；历史文档目录未动。
5. §5 测试命令全绿（含新增的换代隔离测试）。

## 7. 完成报告

- 文件列表
- §6 逐条证据
- 测试命令与结果
- 偏离（应无；Redis 环境 skip 须写明）

## 8. 实现备注（完成后填写）

实现于 `v2` 分支（基线为 C5 规格提交 `4ffa4d7`，C4 tip `d99cd86` 之上）。真实 Redis（127.0.0.1:6379，测试 DB 14）全量实跑，无 skip。

- **常量换代**：`pkg/redisbroker/options.go` 9 个默认值常量（stream/pubsub/presence/cluster/node/session lease/session snapshot/channel/epoch）与 `pkg/redisbroker/cluster_command_bus.go` 3 个包级命令总线前缀常量（`cmd:stream:` / `cmd:reply:` / `cmd:state:`）全部由 `ml:` 改为 `ml2:`，仅改值；键结构、数据类型、TTL、SCAN 拼接方式、`node_epoch:` 避撞结构全部未动。命令总线前缀保持包级常量，未重构为派生自 `ClusterPrefix`。
- **注释键形**：`cluster_command_bus.go`（包注释 inbox 键形、`streamKey` 注释）、`cluster_directory.go`（`nodeEpochKey` / `ListSessionLeases` / `ListNodeLeases` 注释）、根包 `cluster_epoch.go`（`NodeEpochAllocator` 注释）、`cluster_epoch_test.go`（避撞测试注释）中的键形同步改为 `ml2:`。
- **测试字面量**：`cluster_command_bus_test.go` 4 处硬编码全部改为包级常量拼接——`CloneCommandMetadataIsIndependent` 用 `clusterCommandReplyPrefix+"test"`；`KeyNeverWrittenToRedis` / `SendCommandUsesStreamNotPublish` 的 `PSubscribe` 与 `NoRequestPubSubSubscription` 的 `PubSubChannels` 用 `clusterCommandReplyPrefix+"*"`（命令总线唯一的 Pub/Sub 通道族就是应答通道，覆盖等价且随常量自动跟随）。
- **换代隔离测试**（新增 `pkg/redisbroker/keyprefix_test.go`，`TestKeyPrefixGeneration_Ml2IsolatedFromLegacyMl`）：真实 Redis 上以 UUID 标记的唯一频道/节点名驱动 broker `Publish`（stream + seq + first_retained）、presence `Add`（idx/member）、目录 `NextNodeEpoch`（node_epoch），断言 `SCAN ml:*` 无任何带本测试标记的键、6 个预期键全部以 `ml2:` 前缀存在、且能被 `SCAN ml2:*<marker>*` 找到。旧代 pattern 以 `"ml"+":*"` 拼接，避免在 Go 源码中出现引号包裹的退役前缀字面量（门禁要求）。无固定长 Sleep。
- **文档**：`docs/developer/01-architecture.md`（3 行）、`02-configuration.md`（6 行）、`03-admin-api.md`（1 行）、`04-cluster.md`（27 行）、`docs/deployment.md`（2 行）中的键形全部换代（词边界替换，未误伤 `yaml:` 等）；`docs/v2/kernel-architecture.md` 仅「舰队」一节改为「Redis 键前缀已换代为 `ml2:`（PR-KA-C5）」。`docs/design`、`docs/review`、`docs/archive`、`docs/v2/tasks/` 既有规格保持原样。
- **验证**：门禁 `grep -rn '"ml:' --include='*.go' .` 零命中；`go test -count=1 ./pkg/redisbroker` ok（61.6s，实跑含新测试 0.05s PASS）；`go test -count=1 -run "TestSim_|TestClusterCommandBus" .` ok（0.77s）；`go test ./...` 全部 ok；`go test -race . ./pkg/redisbroker` ok（77.1s / 66.4s）；`TestRedisSessionDirectory_NodeEpochKeyEscapesNodeLeaseScan`、`KeyNeverWrittenToRedis`、`SendCommandUsesStreamNotPublish`、`NoRequestPubSubSubscription` 单独点名复跑均 PASS。配置 schema（`config.RedisConfig`）未加字段；proto/SDK/memory broker/C1 sim/HMAC/`client.go`/`hub.go`/`session.go` 零改动（git status 仅含规格允许的文件）。
- **偏离**：无。
