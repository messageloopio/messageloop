# 内核架构重设 · 独立评审

| 字段 | 值 |
| --- | --- |
| 被评文档 | [`kernel-architecture.md`](kernel-architecture.md) |
| 日期 | 2026-08-16 |
| 状态 | Review（原文已按本评审于同日修订；下列条目保留为审计轨迹） |
| 评审方式 | 两路独立：设计完备性 + 集群/源码核验；本文件为合并结论 |
| 裁决 | **major concerns**（针对 08-15 初稿）。08-16 修订已吸收下表 critical / major；是否批准实施另议。 |

## 08-16 修订对照

| 评审 | 修订落点 |
| --- | --- |
| C1 Detached 未钉 | 状态机表 + 事件×状态；Detached 仅本机交接；被抢只准 Fence→Closed |
| C2 跨节点零拷贝 | SessionDoc 水化；零拷贝限本机 |
| C3 盲写 Put | 续约 = same-fence Bind；A1 先行；验收「B CAS 后 A ping 不得抢回」 |
| C4 Occupancy 投递「或」 | KD-K9b：只走 LiveBus |
| C5 `**` / Cluster | 编译规则 + 拒绝不可路由 pattern；Cluster 非目标（KD-K13b） |
| C6 gap 不可诚实 | 可检测子集；停 XDel-on-fail；中洞不承诺 |
| C7 deny 集 | 语言包含 + 表驱动最小集 |
| M1 fencing 第 8 刀 | 增量改为 A1 在现行类型上先做 |
| M2 混部 / proto | A0 字段 + cap 表 + schema 门闩 |
| M3 Q 与 KD 打架 | Q1–Q5 关闭为 KD-K21–K26 |
| M4 Diagnosis 过时 | 结构 / 行为两栏 |
| M5 HMAC 口号 | 密钥在配置、签字段、TTL、与 Directory Redis ACL 分工 |
| M6 三钟发号 | 专表：OccupancyGen / node_epoch INCR、version 仅抢权 +1 |
| M7 Publish 合同 | 四条验收测试 |
| M8 写队列无数字 | 深度 / 超时 / 错误映射表 |
| M9 OnLeave 无推 | SCAN+jitter 第一适配器 |
| M10 Capability | 闭集 8 位 + YAML 兼容期 |
| M11 与 v1.0 并行 | RC 前只 A0/A1；拆对象在 B1 |

---

## Summary

词汇表、四问四集合、十二条宪法是对的北极星。现行树上的结构裂缝（三种 close、盲写 Put、`PSubscribe *`、memory 不看 Interest、CAS 后不回滚）与源码一致。

靶心还不能实施。工程师会在这些地方各自发明语义：Detached 是否仍占 Directory、「零拷贝跨节点」、Occupancy 怎么投到只有通配兴趣的节点、History 如何在 `ts<<20|seq` 上报告 gap、通配如何「不得越过 deny 集」、LiveBus 的 `**` 在 Redis Cluster 上怎么订。增量路径把 fencing 放在最后，而 fencing 才是唯一硬不变量的修复。

---

## 站得住的部分

- **一词一义 / 四问四集合 / 三把时钟** 对准真实耦合：Hub matcher、`redisBroker.interested`、`PresenceStore`、`SessionDirectory` 已经分开，且经常被拿去回答错误的问句。
- **Session 单激活是唯一硬不变量**，其余派生。现行 `PutSessionLease` 盲写与 CAS 后 takeover 不回滚（`cluster_resume.go`）确实会留下指向空节点的 lease。
- **Broker → StreamLog + LiveBus**、Interest 自己计数、禁止通配谎称 last：`addWildcardSub` 恒 `true`，Redis 用 `wcCounts` 补锅，memory `Publish` 不看订阅。
- **控制事件退出 Publication / `cluster_emit`** 是对 v1.0 两阶段门闩的诚实升级。
- **失败两层、Admin = 同核 + Capability、单节点 ≅ 集群、不拆微服务** 与产品定位匹配。
- 诊断在源码里对得上：`ReplaceSession` 扫 64 shard、`close` / `closeQuiet` / `evictSessionForTakeover`、`PSubscribe(prefix*)`、`syncClusterSessionState` → 无条件 SET、命令总线无签名。

---

## Critical（挡住「可实施」）

### C1. Detached 对 Directory「仍占或不占」未钉死

- **Section**: Session 状态表；三种关闭；Bind-then-Evict
- **问题**: 状态表写「仍占 Directory（或不占，见下）」，下文没有「下」。Detach 定义为交接中仍占；Bind 第 1 步已让新 fencing 成为唯一 owner。本机接管、跨节点、真走、被 Fence 四条路径没有 (事件 × 状态 → 下一状态) 表。
- **改**: 删掉「或不占」。Detached **只**表示：本进程仍持有 Session 对象、Directory 仍认本 fencing、附件已撕。被抢节点只准 Fence → Closed，不准再 Detach。

### C2. 「订阅表跟着 Session 零拷贝跨节点」物理上不成立

- **Section**: Session / Channel / Bind 步骤 4
- **问题**: Session 是进程内对象。Bind 成功时对象仍在旧进程。跨节点永远是 snapshot 水化；今天就是 `ClusterSessionSnapshot` + `restoreLocalSubscription`。同进程 Attach/Detach 才是指针稳定。文档同时拒绝频道有家、又不给 Session wire schema。
- **改**: 「零拷贝」仅限本机接管。跨节点：Directory 只存 fencing；Session 文档（Subs + Position + ephemeral + token）可水化；新节点一次登记 Coverage/Interest；写队列跨节点丢弃并 replay。

### C3. Bind-then-Evict 在盲写 Put 仍在时关不掉双活

- **Section**: Cluster；心跳续约；增量第 8 刀；KD-K4/K5
- **问题**: ping/pong → `throttledClusterRefresh` → `PutSessionLease` 无条件 SET，且不升 version。B 已 CAS 抢走后，A 下一次 ping 把所有权写回 A。Evict 走希望型 pubsub，丢了加上持续 Put 就是抢回。心跳节还要求 Pong 续 Directory——按今天实现就是这条盲写。
- **改**: 「删除刷新路径上的 `PutSessionLease`」是 KD-K5 的硬前提，不是括号。续约 = `Bind(session, sameFencing)` 或 no-op；返回 fence 则本地 Fence 附件。验收：B CAS 之后 A 的 `syncClusterSessionState` 不得移动 lease。

### C4. Occupancy 控制通道 vs Stream 无家：通配者在别的节点没有合法投递面

- **Section**: Occupancy；KD-K7 / K9 / K13
- **问题**: 产品要通配者收精确频道 join/leave。今天靠 `PSubscribe *` 或 `PublishTransient` 精确频道。设计同时禁止全网 `*`、不为 Occupancy Bind 频道、控制事件不进 Stream、通道写成「`ml:ctrl:presence:{ch}` **或** NodeRPC」。节点 A 上 C 的 join，节点 B 只有 `im.**`：A 不知道发给谁。兴趣目录会变成被赶走的投影权威；广播违反缩放目标。
- **改**: 选定一条投递面，禁止「或」。倾向：Occupancy 事件走 **LiveBus**（不是 Stream，不是 Publication 信封），key 为精确频道，投递给对该频道有 Interest（含 pattern）的节点。这把 C4 绑死在 C5。

### C5. LiveBus「按兴趣订」对后缀 `**` / Redis Cluster 不可按字面实现

- **Section**: 数据面缩放；KD-K13
- **问题**: 精确频道 `SUBSCRIBE`/`SSUBSCRIBE` 可以。Redis glob 的 `*` 跨点，不是段匹配；`**` 不是 Redis 算子。`chat.**` → `PSubscribe chat.*` 过匹配且漏掉 `chat` 本身。Redis 7 sharded PUB/SUB **没有** pattern subscribe。现行适配器是 standalone `redis.NewClient`。`*.room` / `im.*.tick` / 裸 `**` 没有前缀。
- **改**: 拆合同：精确 Interest → 精确/分片订阅；`字面前缀 + 末尾 **` 可走前缀订阅；其余 pattern 在 SetInterest **拒绝**，或显式降级为「收 shard + 本地过滤」并打指标。Redis Cluster 列为非目标或单独写 expansion 索引的主人。不要声称 KD-K13 已被「前缀/分片订阅」关闭。

### C6. History gap 在 `ts<<20|seq` 上无法诚实报告

- **Section**: StreamLog；KD-K12；`recover.go` / `history.go`
- **问题**: 仓库自己写着：毫秒编码下，裁掉的头与正常静默无法区分。memory 环覆盖可用 `since < oldest`；Redis 不能用同一谓词。空批可以是追上、从未发布、或 TTL 蒸发。
- **改**: 先改 Position 合同再谈 gap。(1) Stream 旁存单调 seq，gap = 缺号 / 低于 first_retained；或 (2) 只承诺头裁与「from 非 unset 且空流」两种 gap，宣布中洞不可检。同步停止 PUBLISH 失败后 `XDel`（那与「Publish 成功 = 日志已接受」相反）。

### C7. 「通配不得越过 deny 集」没有可计算定义

- **Section**: Authorizer SubscribePattern
- **问题**: Deny 是 pattern × principal 语言，不是有限精确频道表。默认投递还允许。未定义：静态语言包含还是「此刻已存在的频道」、与 Proxy 动态拒绝的关系、`**` 对任意 deny 是否必拒、规则热更新后已订 pattern 如何收敛。
- **改**: `Decide(SubscribePattern, p)` = Allow iff `L(p) ⊆ AllowLang(principal)`。给出 `*` / 末尾 `**` 求交伪代码和表驱动用例。SubRefresh：整条 pattern Unsubscribe，不按精确频道拆。

---

## Major

### M1. 增量路径顺序错误：fencing 不应是第 8 刀

双活不依赖 Session 拆对象。步骤 1–7 合并期间 `syncClusterSessionState` 仍会 Put。应拆成：

- **先（现行类型上即可）**: 删刷新 Put；续约 same-fence Bind；CAS 失败回滚；ping 不得抢回。
- **后**: Stream 命令总线、repair 合一、迁 `internal/cluster`。

「第一刀 Session/Attachment」是模块化论证，不是正确性论证。

### M2. 没有混部 / proto / 与 v1.0 的衔接

`cluster_emit` 的教训写在 `NewNode` 里，增量第 5/8 刀完成标准却是「去掉旧路径」，没有 min-version、dual-stack、flag。v1.0 刚把 `RecoverResult` + Ack 内嵌 publications 写成权威；内核把旧字段留空，旧 SDK 会认为已追上——正好是 KD-K12 要消灭的谎。缺少 cap 表（`recover.v2` 等）和字段号。

### M3. Open Questions 与已拍 KD 打架

Q4 重新打开已裁定的 KD-K6。Q1 倾向已是现行行为却不是 KD。Q3 挡住宪法 4/11/12。应：Q1 → KD（未订阅可发）；Q3 → 字段先加、流式等写队列；删 Q4；补 Detached、Session 文档、Occupancy 路由、gap 算法、deny 包含。

### M4. Diagnosis 把 v1.0 已补的行为裂缝写成仍在

结构裂缝仍在。行为上 `recoverSubscription` 已统一 Connect/Subscribe，`ChannelOffsets` 已填充，一等 `presence_event`、客户端 Survey、`sessionCoversChannel` 已在树上。应改成两栏：结构仍在 / 行为 v1.0 已补。否则实现者会重复做 PR-03。

### M5. HMAC 只是口号

未写密钥放哪（不得放 Redis）、签哪些字段、`issued_at` 窗口、`command_id` TTL ≥ handler。能写 Redis 的人仍能盲写 Directory Bind。信任边界没有真的从 Redis 前移，除非 Directory 写入也有节点身份。

### M6. 三把时钟的发号规则没写

OccupancyGen 谁 INCR、fencing `node_epoch` 的两个「或」会撞、Position 在 `uint64 offset` 上无法表达 unset。

### M7. memory ≡ Redis 的 Publish 合同与现状相反且无迁移

memory 同步回传 handler 错误；Redis PUBLISH 失败则 XDel。第 3 刀只提 interested，没提「Publish 成功 = 日志已接受」的验收测试。

### M8. 写队列「同一张表」没有数字

缺队列深度、frame 上限、RST/EOF/Canceled → `peer_closed | slow_consumer` 映射。单流无法字节插队，只能「下一帧选 Control」。

### M9. Membership `OnLeave` 在 Redis 上没有推送机制

Keyspace notify 不可靠。应允许带抖动的 SCAN 作为第一适配器，并定义：OnLeave → 作废该 incarnation 名下 lease，不必等 600s。

### M10. Authorizer 与现行 ACL/Policy YAML 未闭合；Capability 位没有闭集

Admin GetHistory 的主语是 Capability + 频道，不走 Coverage。旧两张表如何映射到一条 Decide+Effects，要有兼容期。

### M11. 与 v1.0 并行策略缺失

ROADMAP 是 1 人 14 周收 v1.0。八刀都是史诗。需要写死：RC 前内核只改文档 + reserved 字段；或 0–2 刀（gap / Interest / Authorizer）进 v1.x 且不碰 Session 对象。

---

## Minor / Nit

- 关键机制缺公平备选表（Occupancy 投递三选一、Position 编码、第 1 刀是否先改指针不改类型名）。
- `Publish` 在 `history=false` 时「拒绝或上层改 Live」两说；Survey 人数门的集合来源未钉；跨区域 Directory Bind 是否直接禁止未写。
- 第 1 刀完成标准写 Occupancy 分叉测试，控制通道却在第 5 刀。「或不占，见下」悬空。

诊断补注（非否决）：CAS 后若旧 **node lease 已无**，现行代码允许继续——死节点旁路应保留，不要「修掉」。`ReplaceSession` 失败回滚是第四条关闭路径，目标三个名字仍应把它收进去。

---

## 建议的文档修订顺序（先改方案，再谈批准）

1. 钉死 C1 状态机表 + C2 Session 文档 / 取消跨节点零拷贝措辞。
2. 把删盲写 Put 提升为独立的第 0/1b 刀（M1 + C3）。
3. 选定 Occupancy 走 LiveBus，并写清 pattern → Redis 订阅编译（C4 + C5）。
4. 定义 gap 的可检测子集或改 Position 编码（C6）。
5. 写出 deny 语言包含算法（C7）。
6. 混部矩阵 + proto 字段号 + cap 名（M2）；Q 表与 KD 对齐（M3）。
7. Diagnosis 拆成结构 / 行为两栏（M4）。

修订完成前，本文档保持 **Draft + 不可实施**。宪法章节可以先当 v1.x PR 的门禁清单使用。
