# PR-KA-D4 实现规格：LiveBus 缓冲满语义（occupancy 优先丢 + 降级标记）

| 字段 | 值 |
| --- | --- |
| 标题 | `redisbroker: drop occupancy first under delivery pressure and mark degraded channels` |
| 状态 | **Ready**（待实现） |
| 依赖 | D3 已合（`daf22a8`,`live_drop_total` 已由 seq 跳变检测覆盖 publication)。在 `v2` 分支上做 |
| 设计来源 | [kernel-architecture.md](../kernel-architecture.md) LiveBus 缓冲满合同（:409)；转正评审架构覆盖路 |
| 验收人 | 主 agent |

## 1. 目标

落地架构 :409 缓冲满合同的剩余三项（publication 的「禁止静默丢而不计数」已由 D3 的 seq 跳变检测满足）:

1. **满时优先丢 Occupancy**:`dispatchOccupancy`(`pkg/redisbroker/pubsub.go:339-345`）从阻塞发送改为**非阻塞**:worker 队列（`deliveryQueueSize=256`）满时丢弃该 occupancy 事件，`live_drop_total.Add(1)`，记 Warn 日志，并把该频道标为降级。occupancy 状态由下一快照/事件修复（B2 既有语义，合同原文「可靠下一快照补」)，所以丢它是安全的；occupancy 按 gen 单调，丢中间事件不影响最终收敛。
2. **业务 Publication 保持反压**:`dispatch`(:328-334）阻塞发送**不变**——队列满时反压到消费循环，最终由 go-redis 缓冲溢出兜底、D3 seq 跳变检测计数。投递失败不否定 Append(:360 / KD-K14 / A2 既有语义，本 PR 不动）。
3. **频道降级标记**:broker 新增 `degraded` 集合。置位时机：(a) occupancy 因队列满被丢；(b) `noteLiveSeqGap` 检出 publication seq 跳变（缓冲溢出证据）。清除时机：该频道下一次**成功入队**（occupancy 非阻塞发送成功或 publication 阻塞发送完成）。重连（`setActivePubSub`,:789-796）清空全部。可观性：新增 Gauge `live_degraded_channels`（当前降级频道数，转换时同步），状态转换记 Info/Debug 日志。

**不做：** 改 go-redis 缓冲大小/换自家 socket 读取层；publication 在 dispatch 点的主动丢弃（保持反压）；降级标记的任何**消费方**（本 PR 标记只用于指标与日志，不反向影响 Interest/发布判定——合同只要求「标降级」);memory broker（无缓冲满语义）;C3 命令总线；proto/SDK。

## 2. 允许改动的文件

- `pkg/redisbroker/pubsub.go`:`dispatchOccupancy` 非阻塞化、降级集合的置位/清除、`noteLiveSeqGap` 置位降级、`setActivePubSub` 清空
- `pkg/redisbroker/redis.go`:`degraded` 字段（连同锁——优先复用既有互斥，若新加锁须论证无锁序倒置）
- `metrics.go`:`LiveDegradedChannels` Gauge + 注册；`metrics_test.go`
- `pkg/redisbroker/pubsub_test.go`（新测试）
- `docs/developer/05-observability.md`:`live_degraded_channels` 条目 + 缓冲满语义一段话
- `docs/v2/kernel-architecture.md`::409 缓冲满行改已落地表述（参照 C5/D3 落地注记方式）+ Document History 追加一行
- `docs/v2/tasks/pr-ka-d4-buffer-full.md`(§8 实现备注）

禁止：改 publication 投递/反压语义；改 occupancy 的 gen 判定（`node.go` 侧零改动）；改 `catchUpMissed`/C6 GapNotice 路径；proto、SDK、根包其他文件；`docs/v2/README.md` 与增量表（主 agent 负责）;git commit / tag / push。

## 3. 现状（动手前再读）

### 3.1 投递路径

消费循环 `runPubSub`(:375 起）从 go-redis `ChannelWithSubscriptions(WithChannelSize(1024))`(:407）读消息：publication → `noteLiveSeqGap`(D3)→ `deliverOnce`（去重+推进 lastSeqs)→ `dispatch` → **阻塞**发送到按频道哈希的 worker 队列（16 workers × 256 深，:291-334);occupancy → `dispatchOccupancy`(:339-345）同样**阻塞**发送。worker 队列满 → dispatch 阻塞 → 消费循环停读 → go-redis 1024 缓冲满 → go-redis 静默丢（D3 起由 seq 跳变计数，仅限 publication)。

### 3.2 关键约束

- `deliverOnce` 的临界区锁序是 `deliverMu → subMu`(:693-716),`noteLiveSeqGap` 同序（:665-686)。降级集合若用 `subMu` 或新锁，必须保持既有锁序，不得引入 `subMu → deliverMu` 反向路径。
- `dispatch`/`dispatchOccupancy` 在 `deliveryActive=false`（未 Start 的单测）时内联执行（:332-334,:343-344)——非阻塞化只影响 worker 池路径，内联路径语义不变。
- occupancy 与 publication 共享同一 per-channel worker 队列，保持同频道顺序；丢 occupancy 不打乱 publication 顺序，后续 occupancy 事件（更大 gen）照常投递——node 侧 `gen <= last_applied` 丢弃（`occupancy_gen_discard_total`）语义不受影响。
- `live_drop_total` 已在 D3 注册为无 label Counter;occupancy 丢弃与 publication seq 跳变共用此计数器，**无双计**(occupancy 无 seq 永不触发跳变检测，publication 永不在 dispatch 点被丢）。

### 3.3 降级集合的生命周期参照

`setActivePubSub`(:789-796）每连接重置 `liveActive`；降级集合照此在同一处置空。`liveControlChannel = "__live__"` 永不进降级集合（没有 occupancy/publication 流量）。

## 4. 测试

| 测试 | 内容 |
| --- | --- |
| occupancy 满则丢 | 构造 deliveryActive 且目标 worker 队列灌满的 broker，发 occupancy：立即返回（不阻塞）、`live_drop_total` +1、频道入降级集合、gauge==1、有 Warn；再发一条同频道 publication 确认仍走阻塞语义（goroutine + 短超时断言未立即完成；队列腾空后完成） |
| 降级清除 | 降级频道下一次成功入队（occupancy 或 publication）后集合清除、gauge 归 0 |
| seq 跳变置位 | 扩 D3 的 `noteLiveSeqGap` 单测：检出跳变时频道入降级集合 |
| 重连清空 | `setActivePubSub` 后降级集合为空 |
| gauge 注册 | `metrics_test.go` 既有模式 |
| 回归 | D3 的 live drop 测试（含真实 Redis）与 C6 gap 测试全绿，语义不变 |

测试禁止固定长 Sleep（用 Eventually/轮询/ channel 同步）；真实 Redis 沿用 `requireCommandBusRedis` guard(DB 14)。

## 5. 验证

```bash
go build ./...
go test -count=1 ./pkg/redisbroker
go test -count=1 -run "TestSim_|TestClusterCommandBus|TestMetrics" .
go test -count=1 ./...          # 串行
grep -n "live_degraded_channels" metrics.go docs/developer/05-observability.md
grep -n "缓冲满" docs/v2/kernel-architecture.md   # :409 行已是落地表述
```

## 6. 验收清单

1. `dispatchOccupancy` 非阻塞：满则丢 + `live_drop_total` +1 + Warn + 降级置位；`dispatch` 阻塞语义零改动。
2. 降级集合置位/清除/重连清空时机与 §1.3 一致；`live_degraded_channels` gauge 同步正确。
3. publication 侧无新丢弃点、无 `live_drop_total` 双计；`noteLiveSeqGap` 检出跳变时置位降级。
4. 锁序与既有 `deliverMu → subMu` 一致，无新倒置；内联（未 Start）路径语义不变。
5. §4 测试全覆盖且绿；无固定长 Sleep。
6. 文档同步（05-observability.md、:409 落地表述、Document History)；未碰 §2 禁止项；无格式 churn；无 git 操作。

## 7. 完成报告

- 改动文件列表
- §6 每条 过/失败 + 证据
- 测试命令与真实输出
- 偏离（应无）

## 8. 实现备注（实现方填）

（空）
