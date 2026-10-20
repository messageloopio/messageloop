# 广播攒批(broadcast batching)实现方案

日期:2026-10-20
状态:implemented(2026-10-20 落地,偏差见 §10)
关联:单房间 10 万人游戏房容量评估;`internal/stream/broker.go` 顺序契约;`internal/session/hub.go` 扇出路径

## 1. 背景与目标

### 1.1 问题

投递路径目前没有应用层攒批:每条 publication 对每个订阅者都是独立一帧、独立一次写确认
(`internal/session/hub.go:418` 单条信封;`internal/runtime/recover.go:533` 注释
"one message per frame")。单频道扇出受两个乘法约束:

- 同频道在一个节点上串行投递(顺序契约,`internal/stream/broker.go:181-189`);
- 每次 `BroadcastPublication` 内并发发送封顶 `broadcastParallelLimit = 64`
  (`hub.go:28`),且每次发送等待该帧写上 socket 的确认(`enqueueBytes` 等
  `frame.done`)。

大房间(本地订阅者数万)的房间级消息率因此被压在每节点几十条/秒的量级。把 M 条消息
合成一帧,入队与写确认次数从 N×M 降到 N,是收益最大的单项优化。

### 1.2 目标

- 同频道多条 publication 在一个可配置的收集窗口内合成一个多消息帧投递。
- 小频道零行为变化:不满足条件的频道走现有立即扇出路径,逐条不攒。
- 内存有界:攒批层只引入有界缓冲,"满 = 阻塞生产者",沿 Redis broker 既有反压链
  传回,由 Redis `client-output-buffer-limit` 掐订户 + history 追平这个已存在的
  泄压阀兜底;攒批层自己永不丢弃、永不无界攒。
- 协议与 SDK 零改动:`clientpb.Publication.Messages` 本来就是 `repeated`,
  客户端按 list 解析,服务端单方面开启对客户端透明。

### 1.3 非目标

- 不攒 transient 事件(presence/occupancy join/leave,offset=0):低延迟语义,
  且不是主要压力源。
- 不改恢复重放(recover)路径:B3 已刻意改为逐条流式(`session.go:636`),
  连接期语义不同。
- 不做消息速率自适应触发:每频道滑动窗口状态引入滞后与抖动,订阅者数阈值已覆盖
  主要场景;压测后确有需要再议。
- 不做跨频道攒批(会破坏按频道分帧的语义)。

## 2. 总体设计

```
broker handler (每频道串行, node.go:224)
      │
      ▼
publicationBatcher.Deliver(ch, pub)          [internal/runtime/broadcast_batcher.go 新增]
      │
      ├─ pub.Offset == 0(transient)────────────► hub.BroadcastPublication(立即)
      ├─ 频道策略关闭(enabled=false)──────────► hub.BroadcastPublication(立即)
      ├─ 本地订阅者数 < min_subscribers ────────► hub.BroadcastPublication(立即)
      └─ 其余 ──► channelAccumulator(每频道一个)
                        │  append(满则阻塞 = 背压)
                        ▼
                  flusher goroutine(每 accumulator 一个)
                        │  触发:max_delay / max_messages / max_batch_bytes
                        ▼
                  hub.BroadcastPublications(ch, pubs)   [hub.go 新增]
```

关键决策:**flush 在独立的 flusher goroutine 上执行,不在 delivery worker 上**。
收益:flush 期间(最坏 10s 写预算)worker 不被占住,同 hash 桶的其它频道照常投递
——比今天(慢广播占住 worker 整段广播时长)的头阻塞还更好。正确性:每频道一个
flusher,串行 flush,顺序契约保持;append 与 flush 用互斥锁同步。

## 3. 配置

### 3.1 全局配置(config.go 新增顶层段)

```yaml
broadcast:
  batching:
    enabled: false          # 总闸,默认关(v1.0 RC 稳定期,零行为变化;现网 game 栈配置显式开)
    max_delay: 8ms          # 收集窗口上限
    max_messages: 32        # 批条数上限
    max_batch_bytes: 16384  # 批内原始载荷字节预算(见 §6.2 ×2 通胀规则)
    min_subscribers: 256    # 生效阈值:频道本地订阅者数(NumSubscribers)达到才攒
    idle_reap: 30s          # accumulator 闲置回收 TTL
```

`Config` 增加顶层 `Broadcast BroadcastConfig`。默认关的理由:仓库先例(Survey
默认关,KD-6)+ RC 阶段行为变更必须 opt-in;开启动作是部署配置一行,不是代码。

### 3.2 Validate 规则(config.Validate)

- `enabled=true` 时:`0 < max_delay ≤ 500ms`;`max_messages ≥ 2`;
  `1024 ≤ max_batch_bytes`。
- **帧尺寸约束**:`2 × max_batch_bytes ≤ transport.MaxMessageSize 的有效值`
  (未配置时有效值 64KB,`config.go:217`)。×2 是 JSON wire 通胀安全系数
  (binary 载荷 base64 ≈ ×1.37 + protojson 转义 + 信封开销)。配置过小的
  `max_message_size` 而不调低 `max_batch_bytes` 时拒绝启动。

### 3.3 频道策略覆盖

沿用 `ChannelPolicySpec` 指针字段惯例(`config.go:181`):

- `config.ChannelPolicySpec` 增 `Batching *bool`(yaml `batching`);
- `channel.CompiledPolicySpec` 增 `batching *bool`,`CompilePolicySpec`/`Overlay`
  随动;
- `channel.ChannelPolicy` 增 `Batching *bool`(nil = 未覆盖,回落全局
  `enabled`)。注意:这是该结构体第一个指针字段,偏离现有全值字段风格——
  因为默认值取决于全局配置,policy 层看不到;review 时如更倾向
  `BatchingSet bool + Batching bool` 双字段,语义等价。

配置表达:游戏房 pattern 开、告警类 pattern 关:

```yaml
server:
  authorizer:
    - channel_pattern: "game.*"
      effects: { batching: true }
    - channel_pattern: "notify.critical.*"
      effects: { batching: false }
```

## 4. 组件设计

### 4.1 publicationBatcher(internal/runtime/broadcast_batcher.go,新文件)

```go
type publicationBatcher struct {
    cfg      config.BatchingConfig
    policy   func(ch string) ChannelPolicy   // n.ChannelPolicy
    hub      *session.Hub
    subs     func(ch string) int             // h.NumSubscribers
    metrics  *metrics.Metrics

    mu   sync.Mutex
    accs map[string]*channelAccumulator  // 惰性创建,闲置回收
}

func (b *publicationBatcher) Deliver(ch string, pub *stream.Publication) error
func (b *publicationBatcher) Close()   // 逐 accumulator 最终 flush(短超时)
```

**Deliver 判定顺序**(每条 publication,O(1) 快路径优先):

1. `pub.Offset == 0` → 立即(transient);
2. 全局或频道策略关 → 立即;
3. `subs(ch) < min_subscribers` → 立即;
4. 否则取/建 accumulator 并 append。

阈值只在 append 时评估;订阅者数跌破阈值后 accumulator 继续用到闲置回收,
下一条 publication 重新判定——天然的迟滞,无状态抖动。

**channelAccumulator**:

```go
type channelAccumulator struct {
    mu    sync.Mutex
    cond  *sync.Cond          // "有空间"信号,append 满时阻塞 delivery worker(背压)
    buf   []*Publication      // 攒批缓冲(有界:max_messages / 预算字节)
    bytes int
    notify chan struct{}      // cap 1,阈值触发信号
    // flusher goroutine 私有:swap 出的在途批、timer、lastAppend
}
```

- **append**:加锁;若 `len(buf) ≥ max_messages` 或 `bytes + pubSize > 预算`
  → `cond.Wait()`(阻塞生产者,背压起效点);入队后若达任一阈值 → 非阻塞发
  notify;解锁。**攒批期间不拷贝载荷**:Publication 在 broker 盖完 offset 后
  不可变,只存指针。
- **超大单条旁路**:`pubSize > 预算` 的单条不进缓冲——先
  `flushAndWait()`(通知 flusher 并等 buf 排空,保证不越过更早的消息),再直接
  `hub.BroadcastPublication(ch, pub)` 独发。直接走立即路径会越过缓冲中的更早
  消息,破坏顺序,必须先排空。
- **flusher 循环**:`select { case <-notify; case <-timer.C }`;flush 时在锁内
  swap 出整个 buf(与后续 append 并行,双缓冲,最坏 2×预算/频道),解锁后调
  `hub.BroadcastPublications`;重置 timer = `max_delay`。
- **闲置回收**:flush 后 `time.Since(lastAppend) > idle_reap` → 从 map 删除、
  goroutine 退出。短命频道不泄漏 map 条目;accumulator 种群天然有界
  (只有过阈值的频道才建)。
- **Close(ctx)**:向所有 flusher 发最终 flush 信号,等待带短超时(如 2s);
  超时未 flush 的内容随进程退出丢弃——客户端靠 history/catch-up 恢复,语义
  与今天重启一致。

### 4.2 Hub.BroadcastPublications(internal/session/hub.go)

新方法,与 `BroadcastPublication` 同构,差异点:

```go
func (h *Hub) BroadcastPublications(ch string, pubs []*Publication) error
```

- 一次性 `mergeMatchingSubscribers`(整批同一批订阅者);
- `msgs` 按 pubs 逐条构造(Id 用既有 `publicationMessageID` 规则:offset>0 用
  `channel-offset`,批内恒成立;Position 逐条 `shared.PositionFrom`);
- 每编码一次 `MarshalAppend`、共享帧字节的机制**原样复用**(整批一个信封,
  N 个订阅者仍只 1~2 次序列化);
- `broadcastWriteBudget` 语义不变:整帧一次确认——这正是 N×M → N 的收益来源;
- `recordDeliveredOffsets(ch, maxOffset(pubs), clients, delivered)`:同一帧原子
  送达,批内消息同生共死,记最大 offset 即正确;
- `delivered[]`/慢写者预算/3512 路径不变;`TestHub_BroadcastPublication_SlowWriterHitsBudget`
  增加批量版断言。

并发说明:flusher 并发数 = 批量频道数(小),叠加 worker 立即路径(16)后,
节点内并发广播调用 ≈ 16 + K,每次内部仍受 `broadcastParallelLimit=64` 约束。
上限略高于今天的 16×64,量级不变;如压测显示需要,后续加节点级发送信号量,
本方案不含。

### 4.3 JSON 多消息 splice(hub.go 既有优化的批量扩展)

现状:单条 JSON object 载荷走 `spliceRawJSONPayload` 原文拼接,避免
protojson round trip 丢失 >2^53 整数精度(`hub.go:393-401`)。批量帧中
`"json":{}` 占位点出现 M 次:

- **全批可拼**(每条都是 JSON object)→ 扩展 `spliceRawJSONPayload` 为多点
  拼接:`bytes.Index` 从后往前逐个替换,占位点顺序即 msgs 顺序;
- 存在任何不可拼消息(非 object、marshal 失败)→ 整帧回退 protojson 路径
  (全拼或全不拼,不做混合——混合的帧形状审查成本高于收益)。

### 4.4 接线(internal/runtime/node.go)

`node.go:224` 的 handler 闭包改为 batcher 分发:

```go
if n.batcher != nil {
    n.broker.Start(ctx, n.batcher.Deliver)
} else {
    n.broker.Start(ctx, func(ch string, pub *Publication) error {
        return n.hub.BroadcastPublication(ch, pub)
    })
}
```

`enabled=false` 时 batcher 不构造,路径与今天逐字节相同。Node 关停序列中在
DrainAll 之前调 `batcher.Close()`(先清积压再断客户端)。

### 4.5 metrics(internal/metrics)

| 指标 | 类型 | 标签 | 用途 |
|---|---|---|---|
| `messageloop_broadcast_batch_messages` | Histogram | trigger=delay/messages/bytes/shutdown | 批大小分布,收益直接证据 |
| `messageloop_broadcast_batch_flush_seconds` | Histogram | - | 单次 flush 广播耗时,stall 可见性 |
| `messageloop_broadcast_batch_pending_bytes` | Gauge | - | 攒批在途字节;**长期贴顶 = 反压在工作/容量不足** |
| `messageloop_broadcast_batch_backpressure_total` | Counter | - | append 阻塞次数 |

## 5. 边界与风险

| 风险 | 处置 |
|---|---|
| 内存爆炸 | §6 四件套:有界缓冲 + 满则阻塞 + 字节封顶 + 阈值限种群;泄压阀沿用 Redis output buffer 掐订户 + history 追平 |
| 顺序破坏 | flusher 每频道串行;超大单条先 `flushAndWait` 再独发;offset 连续性由 broker 保证 |
| JSON 大整数精度 | §4.3 全拼或全不拼 |
| delivered-offset 语义 | 批记 max offset;跨节点 resume 快照路径(`LookupSubscriber`)无需改动 |
| `ErrOutboundTooLarge` | 靠 §3.2 Validate 的 ×2 约束前置排除;残余风险(极端转义膨胀)表现为该编码订阅者整帧失败 → 重连 catch-up,与今天单条超限同型 |
| flusher 慢导致同频道积压 | append 阻塞 → worker 阻塞 → worker 队列(256)满 → 消费循环阻塞 → Redis 反压;行为与今天慢广播占住 worker 同构,爆炸半径不变(仅该频道,16 worker 隔离其它频道) |
| flush 期间订阅者增减 | swap 后按快照扇出;新订阅者靠 catch-up 补,与今天单条广播窗口内的一致性相同 |
| shutdown 丢缓冲 | `Close` 最终 flush + 短超时;残余靠 history 恢复,与今天进程退出语义一致 |

## 6. 内存界限(回答"如何防内存爆炸"的设计落点)

1. **有界**:每频道至多 缓冲(≤ 预算字节/条数)+ 一个在途批 ≈ 2 × max_batch_bytes;
2. **满则阻塞**:append 在 `cond.Wait()` 上阻塞 delivery worker,反压沿
   memory broker(dispatch shard 满阻塞发布方,`broker_memory.go:26`)或
   Redis broker(worker 队列 256 满 → 消费循环阻塞,`pubsub.go:29`)既有链路
   传回;**绝不 drop**(顺序契约 + dense seq 不丢语义),**绝不无界攒**;
3. **字节封顶**:阈值双计数(条数 + 原始载荷字节),时间窗 max_delay 是延迟
   参数不是内存参数;
4. **种群有界**:只有订阅者过阈值的频道才有 accumulator,闲置 idle_reap 回收。

## 7. 测试计划

| 层 | 文件 | 用例 |
|---|---|---|
| accumulator 单测 | `broadcast_batcher_test.go` | 阈值触发(delay/messages/bytes);满则阻塞(生产者 goroutine 卡住 → flush 后恢复);超大单条旁路且不越过缓冲消息;闲置回收;Close 最终 flush |
| Hub 批量 | `hub_test.go` 增 | 批量帧多消息有序到达;每编码一次序列化(现有断言迁移);`recordDeliveredOffsets` 记 max;慢写者触发预算(批量版);多点 JSON splice(含混合类型回退);`MaxMessageSize` 前置拆批验证 |
| batcher 集成 | `broadcast_batcher_test.go` | transient 不攒;策略关不攒;订阅者阈值下不攒;阈值迟滞(跌回不立刻拆);与 fake broker 串行投递组合下顺序保持 |
| 接线 | `runtime` 包既有测试增 | enabled=false 路径与今天一致(行为回归);enabled=true 端到端:客户端收到含 N 条消息的 Publication 帧且 offset 连续 |
| config | `config` 包 Validate 测试 | §3.2 全部规则矩阵 |
| SDK 冒烟 | `sdks-go` e2e / `sdks/ts` | 多消息 Publication 帧解析(协议本就 repeated,验证即可) |
| 基准 | `hub_test.go` 增 | `BenchmarkBroadcastPublications` batch=1/8/32 × 订阅者 1k/10k,对照现有单条基准 |

## 8. 发布与灰度

1. 合入默认关;附本文档 + README 配置参考。
2. game 栈(fleetly 配置)显式开启:`enabled: true, min_subscribers: 256`;
   小频道零变化。
3. 观察 `batch_messages` 分布(收益)、`pending_bytes`/`backpressure_total`
   (健康)、`flush_seconds` P99(stall)。预期:10k 本地订阅者、房间 100 msg/s
   场景下,广播批大小向 max_messages 靠拢,单节点房间吞吐按批大小近似线性提升。
4. 参数调优顺序:先 max_delay(延迟预算),再 max_messages(帧大小),最后
   max_batch_bytes。

## 9. 工作量分解

| # | 任务 | 文件 | 规模 |
|---|---|---|---|
| 1 | BroadcastConfig + Validate | `internal/config/config.go` | 小 |
| 2 | ChannelPolicySpec/CompiledPolicy/ChannelPolicy.Batching | `internal/config/config.go`、`internal/channel/channel_policy.go` | 小 |
| 3 | publicationBatcher + accumulator | `internal/runtime/broadcast_batcher.go`(新) | 中,核心 |
| 4 | Hub.BroadcastPublications + 多点 splice + 记账 | `internal/session/hub.go` | 中 |
| 5 | node.go 接线 + Close 顺序 | `internal/runtime/node.go` | 小 |
| 6 | metrics 四项 | `internal/metrics` | 小 |
| 7 | 测试矩阵(§7) | 各对应 `_test.go` | 大头 |
| 8 | 文档 | README 配置参考、`configs/` 示例、本文档状态更新 | 小 |

任务 1/2 无依赖可先行;3/4 可并行(接口先行:`Deliver` 与
`BroadcastPublications` 签名定稿);5/6 依赖 3;7 随各任务同行;8 收尾。
不碰:传输层、协议 proto、recover、occupancy、cluster 命令总线路径
(`cluster_commands.go:218` 自建信封,与本方案无关)。

## 10. 实现记录(与方案的偏差)

实现按本方案落地(`internal/runtime/broadcast_batcher.go`、
`internal/session/hub.go BroadcastPublications`、`internal/config`、
`internal/channel`、`internal/metrics`、`internal/runtime/node.go` 接线),
以下为评审中确定的偏差与补充:

1. **配置路径**:`server.broadcast`(不是顶层 `broadcast:`)——`NewNode` 只收
   `*config.Server`,顶层段到不了运行时;批量配置因此成为 `server:` 的子块,
   `nil` 段 = 关闭。
2. **flush 触发标签**:histogram 标签为 `delay / messages / explicit /
   shutdown`。字节上限不作为触发器(append 在触及预算前就会阻塞,无法"攒满
   字节再 flush"),`explicit` 是方案 §4.1 超大单条旁路的 drain。
3. **inFlight 排水(方案遗漏的正确性补充)**:oversize 旁路的
   `flushAndWait` 必须等被排空批的**广播返回**(`inFlight` 计数),而不是只等
   swap 完成——否则立即发送(delivery worker)与被排空批的扇出(flusher)
   并发进 hub,同频道线序被破坏。batcher 单测
   `TestBatcher_OversizeBypassPreservesOrder` 钉死该行为。
4. **Close 扫尾**:`Close` 在等待 flusher 退出后对残余缓冲补一次 flush,
   堵住"append 恰在 flusher 处理 done 之前入队"的竞态窗口;
   `closeOnce` 保证幂等。
5. **JSON 多点拼接保留前缀**:与单条实现一致,占位点的 `"json":` 键保留、
   只替换 `{}`,因此拼接结果形如 `"json":{"n":...}`;载荷内部出现的
   `"json":{}` 字面量天然存活(搜索从上一处插入点之后继续)。
6. **批量帧记 delivered-offset 记批内最大值**:同帧原子送达,与方案 §4.2
   一致;`recordDeliveredOffsets` 调用点随委托化合并。
