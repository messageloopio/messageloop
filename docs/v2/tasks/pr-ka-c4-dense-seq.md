# PR-KA-C4 实现规格：History 稠密 seq 与真中洞检测（Q8）

| 字段 | 值 |
| --- | --- |
| 标题 | `broker: dense per-channel seq on history entries, true middle-gap detection` |
| 状态 | **Ready** |
| 依赖 | C3 已合（`d7b01d8`）。在 `v2` 分支上做 |
| 设计来源 | [kernel-architecture.md](../kernel-architecture.md) StreamLog / Gap 合同、KD-K12、KD-K14、Q8 |
| 验收人 | 主 agent |

## 1. 目标

A2 的 Gap 合同只承诺**可检测子集**：头裁（`head_trimmed`）、整段蒸发（`empty_expired`）、epoch 重置。中间被 `XDEL` 掉的单洞在 `ts<<20|seq` 编码上不可检（源码与架构文档均已承认）。本 PR 兑现架构文档「Stream 条目旁存稠密 seq，再承诺中洞」的后续里程碑：

1. 每条 history stream 条目携带**每频道稠密 seq**（1, 2, 3, …），发号与 `XADD` **原子**完成（Lua），崩溃不留「已发号但无条目」的假洞。
2. `History` 页内做**真中洞检测**：相邻条目稠密 seq 不连续 → `HistoryGapMiddle`（新），线上映射为新 proto 枚举 `GAP_REASON_MIDDLE`。
3. 重连 catch-up 用稠密 seq 检测中洞 → 现有 `catchUpGaps` 计数 + Warn（**仍**不下发客户端，client-facing live gap envelope 仍是 future work）。
4. Position offset 语义（`ts<<20|seq`）**不变**；Publish 成功合同（A2/KD-K14）**不变**；memory broker **不动**（其 offset 本身就是稠密 seq，环形缓冲无中洞）。

**不做：** 改 offset 编码 / `parseStreamOffset` / `streamStartID`；`ml2:` 键前缀换代（后续独立刀）；client-facing live gap 信封；C1 sim（进程内函数调用，无 Redis）；集群命令总线（C3 刚落）；SDK 业务逻辑；Occupancy / LiveBus。

## 2. 允许改动的文件

- `broker.go`：`HistoryGapMiddle` 常量、`Publication.Seq` 字段、Gap 合同注释更新
- `pkg/redisbroker/redis.go`：`Publish` 改 Lua 原子发号 + XADD；`redisMessage` 实时载荷带 seq；seq 键常量
- `pkg/redisbroker/message.go`：`redisMessage` 增加 `Seq`（`json:"seq,omitempty"`）
- `pkg/redisbroker/history.go`：读取条目 `s` 字段、页内中洞扫描
- `pkg/redisbroker/pubsub.go`：`deliverOnce` 记录 `lastSeqs`、`catchUpMissed` / `checkCatchUpGap` 中洞检测
- `pkg/redisbroker/options.go`：仅当要导出 seq 键前缀常量（可继续用包内常量）
- `recover.go`、`recover_test.go`：`gapReasonV2` 增加 Middle 映射
- `metrics.go` / `metrics_test.go`：仅当 `recovery_gap_total` 的 label 注册需要（动态 label 则不用改）
- `protocol/shared/v2/types.proto`：`GAP_REASON_MIDDLE = 5`；`task generate-protocol` 再生 `shared/genproto/**` 与 `sdks/ts/src/proto/**`（**只**再生，不动 SDK 业务代码）
- 测试：`pkg/redisbroker/history_test.go`、`pubsub_test.go`、`ready_test.go` 及因字段/行为变化的既有测试装配
- `docs/developer/01-architecture.md`（Broker/History/offset 段）、`docs/developer/04-cluster.md`（offset 语义段）
- `docs/v2/kernel-architecture.md`：**仅**「Gap 合同」一节（中洞从「不承诺」改为承诺，并加 `middle` 行）
- `docs/v2/tasks/pr-ka-c4-dense-seq.md`（完成备注）

禁止：改 `client.go` / `hub.go` / `session.go`；改 `pkg/redisbroker/cluster_command_bus.go`、`cluster_epoch.go`、`internal/cluster/*`；改 offset 编码与 `parseStreamOffset` / `streamStartID` 语义；改 memory broker 行为；改 SDK 业务代码；git commit/push。

## 3. 现状（动手前再读）

- `Publish`（`pkg/redisbroker/redis.go:282-353`）：`XADD`（`MAXLEN ~`）→ `Expire` → `offset = parseStreamOffset(id)`（`ts<<20|seq`，`history.go:120`）→ 二次序列化带 offset → `PUBLISH`。PUBLISH 失败只 Warn 不 `XDel`（KD-K14）。
- `redisMessage`（`pkg/redisbroker/message.go:23-35`）是 stream `data` 字段与实时 PUBLISH 载荷**共用**的 JSON 信封；`Offset` 在 XADD 后回填、二次序列化只进实时载荷（stream 里的 `data` 不含 offset）。
- `first_retained` 标记键 `ml:stream:retained:<ch>`（`redis.go:361-384`），TTL 与 stream 相同；头裁判定在 `history.go:92`。
- `History`（`history.go`）：`XRangeN(stream, streamStartID(sinceOffset), "+", limit)`，按 A2 §5 表填 `HistoryPage`；空批 + `since>0` 禁止报 `None`。
- catch-up（`pubsub.go:494-561`）：重连后按 `lastOffsets[ch]+1` `XRangeN` 重读；`checkCatchUpGap` 只能检**尾部截断**，注释明说「offsets are millisecond-based, so a normal pause is indistinguishable from missing entries」。
- 实时投递簿记：`deliverOnce`（`pubsub.go:593-600`）更新 `lastOffsets[ch]`；`Unsubscribe` 清理（`redis.go:247`）。
- memory broker：每频道 `nextOff` 从 1 起逐条 +1（`broker_memory.go:50,213-219`），已是稠密 seq；环形缓冲从头覆盖，无中洞。
- proto：`protocol/shared/v2/types.proto` `GapReason` 现有 0–4（UNSPECIFIED/NONE/HEAD_TRIMMED/EMPTY_EXPIRED/EPOCH_RESET）；`recover.go:390-398` `gapReasonV2` 映射；`buf generate` 同时再生 Go（`shared/genproto`）与 TS（`sdks/ts/src/proto`）。
- Go SDK 不引用 `GapReason`；TS SDK 只有生成代码引用。

## 4. 键、字段与发号

| 项 | 值 |
| --- | --- |
| seq 计数键 | `ml:stream:seq:<ch>`（`opts.StreamPrefix + "seq:" + ch`），`INCR` 发号，TTL 与 stream 相同（随每次发布刷新） |
| stream 条目 | 新增字段 `s` = 稠密 seq（十进制字符串）；`data` 字段不变（`data` JSON 内**不**含 seq） |
| 实时载荷 | `redisMessage` 新增 `Seq uint64 \`json:"seq,omitempty"\``；只进 PUBLISH 载荷（XADD 后回填，与 `Offset` 同一模式） |
| Position offset | **不变**：仍是 `ts<<20|seq`，来自 stream ID |
| `first_retained` | 机制不变 |
| `Publication` | 新增 `Seq uint64`（0 = 未知 / legacy / transient） |

- 禁止 Go 侧「先 `INCR` 再 `XADD`」两步：两步之间崩溃会留下已发号无条目的**假中洞**。发号必须Lua 脚本内原子完成。
- 每条 stream 只有一个 seq 序列；epoch 语义不变（C2 的 `node_epoch` 与本 seq 无关，不动）。
- seq 键随 stream 同 TTL 过期后，下一次发布从 1 重新发号：合法（旧游标由 head_trimmed / epoch 兜住），不算中洞。
- `PublishTransient`：不发号、不建 seq 键、不写 stream（不变）。

## 5. 算法

### 5.1 发送（`Publish`）

```
msg = redisMessage{...}                 // 不含 Seq
streamData = serialize(msg)             // 与今日相同
{seq, id} = EVAL(script, keys={seqKey, stream}, args={maxLen, streamData, ttlSeconds}):
    seq = INCR seqKey
    id  = XADD stream * MAXLEN ~ maxLen  s seq  data streamData
    EXPIRE seqKey ttl
    EXPIRE stream ttl
    return {seq, id}
offset = parseStreamOffset(id)          // 编码不变
msg.Offset = offset; msg.Seq = seq
pub.Offset = offset; pub.Seq = seq
updateFirstRetained(...)                // 不变
PUBLISH serialize(msg)                  // 失败只 Warn 不 XDel（不变）
```

- 现有两次独立 `Expire` 折进脚本（原子的副产品）；`Expire` 失败今日只 Warn，脚本化后同理失败即整脚本失败按 XADD 失败处理 `(0, err)`——可接受，发布即失败，无半状态。
- Lua 内 `INCR` 与 `XADD` 要么都成功要么都失败：崩溃不留假洞。

### 5.2 History 页内中洞检测（`history.go`）

- `XRangeN` 照旧；每条把 `s` 字段解析进 `Publication.Seq`（无 `s` → `Seq=0`，legacy/未知）。
- 填 `HistoryPage` 的既有优先级不变，中洞检测追加在最后：
  1. 空页 + `since>0` → `EmptyExpired`（不变）
  2. `FirstRetained > sinceOffset` → `HeadTrimmed`（不变）
  3. **页内相邻两条 `Seq` 均 >0 且 `next.Seq != cur.Seq+1`** → `Gap=true, GapReason=HistoryGapMiddle`
- 缺 `s` 的条目断开证据链：跨「未知对」不断言（宁可漏报不可诬报）。
- 单条目页、全部 legacy 页：不报 Middle。

### 5.3 catch-up 中洞检测（`pubsub.go`）

- `deliverOnce`：与 `lastOffsets[ch]` 并列维护 `lastSeqs[ch]`（`pub.Seq > 0` 才记；`Unsubscribe` 同步清理）。
- `catchUpMissed`：每条目解析 `s`；批内连续性 + 基线检查（`lastSeqs[ch]>0` 且首条 `s>0` 时，首条必须 `== lastSeq+1`）。发现洞：`catchUpGaps.Add(1)` + Warn（带 channel 与 seq 区间）。
- `checkCatchUpGap` 的尾截检测保留；「中洞不可检」注释改为中洞经稠密 seq 可检。**不**新增 client-facing 信封（future work 注释保留）。
- legacy 基线（`lastSeqs==0`）或 legacy 条目：跳过相应检查。

### 5.4 proto 与映射

- `protocol/shared/v2/types.proto`：`GAP_REASON_MIDDLE = 5;`（注释：页内/保留区中间缺条目，稠密 seq 不连续）。
- `task generate-protocol`；确认 `shared/genproto` 与 `sdks/ts/src/proto` 再生、diff 只有该枚举值。
- `broker.go`：`HistoryGapMiddle` 常量（iota 追加，不动现有值序）。
- `recover.go` `gapReasonV2`：`HistoryGapMiddle → GAP_REASON_MIDDLE`；`observeRecoveryGap` label 值 `middle`。

### 5.5 memory broker

不动。其 offset 即稠密 seq（1 起逐条 +1），环形缓冲只从头覆盖，中洞不可能出现；memory `History` 永不报 `HistoryGapMiddle`。

## 6. 接入

- `Broker` 接口签名不变（`Publication` 加字段，非破坏）。`cmd/server` 不动。
- 所有构造 `Publication` 字面量的 fake/测试若用字段名初始化则不受影响；逐一编译确认。
- epoch / Position / `publishAck` 语义不变；客户端协议字段号不变（只加枚举值）。
- Go SDK（独立模块 `sdks/go`）不引用 GapReason，`Set-Location sdks/go; go test` 应仍绿。

## 7. 必须存在的测试

1. **原子发号**：50 路并发 `Publish` 同一频道 → stream 内 `s` 字段恰好为 1..50 连续无重复；offset 仍单调；`data` JSON 不含 `seq` 键。
2. **History 中洞**：发 5 条，`XDEL` 第 3 条，`History(ch, 第1条offset, 0)` → `Gap=true`、`GapReason=HistoryGapMiddle`，`Publications` 仍含读到的 4 条。
3. **recover 映射**：fake History 返回 `HistoryGapMiddle` 页 → `RecoverComplete` `gap_reason=GAP_REASON_MIDDLE`，`recovery_gap_total{reason="middle"}` +1。
4. **legacy 容忍**：直接 `XADD` 一条无 `s` 字段的条目夹在有 seq 的条目中间 → 不报 Middle；全 legacy 流不报 Middle。
5. **旧语义回归**：既有 HeadTrimmed / EmptyExpired / NoGapAtHeadWithMarker / InclusiveSinceOffset 等测试不改语义全绿。
6. **catch-up 中洞**：建立 `lastSeqs` 基线后 `XDEL` 一条，触发 `catchUpMissed` → `catchUpGaps` 计数 +1（可用缩短/直接调用，禁止固定长 Sleep）。
7. **Publish 合同不变**：PUBLISH 失败条目保留且带 `s` 字段，`return (offset, nil)`（扩展现有 `KeepsStream` 测试）；`pub.Seq` 回填。
8. **seq 键卫生**：`Publish` 后 seq 键与 stream 均有 TTL；`PublishTransient` 后 seq 键不存在。
9. `go test -count=1 ./pkg/redisbroker`；`go test -count=1 -run "TestSim_|TestClusterCommandBus" .`；`go test ./...`；`go test -race . ./pkg/redisbroker`；`Set-Location sdks/go; go test`。

禁止用固定长 `Sleep` 代替同步点 / Eventually。无 Redis 则 Skip 并写明。

## 8. 验收清单

1. stream 条目带稠密 seq，发号与 XADD 原子（Lua）；Go 侧无「先 INCR 后 XADD」。
2. History 页内中洞 → `HistoryGapMiddle` → 线上 `GAP_REASON_MIDDLE` + 指标；空页/边界语义不变。
3. catch-up 能检中洞（计数 + Warn），仍不下发客户端。
4. offset 编码、`parseStreamOffset`/`streamStartID`、`first_retained`、Publish 成功合同全部不变。
5. memory broker 不动；C1 sim / C2 epoch / C3 命令总线零改动。
6. proto 只加 `GAP_REASON_MIDDLE = 5`，再生产物 diff 干净；SDK 业务代码零改动。
7. §7 测试命令全绿。

## 9. 完成报告

- 文件列表
- §8 逐条证据
- 测试命令与结果
- 偏离（应无；Redis 环境 skip 须写明）

## 10. 实现备注（完成后填写）

（空）
