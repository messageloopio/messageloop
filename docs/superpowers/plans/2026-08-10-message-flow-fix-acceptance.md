# 消息数据流修复方案 — 验收报告(2026-08-10)

> 验收人: 方案原作者
> 验收基线: `7e92f4b` → `e1900fc`(19 个 commit)
> 验收依据: 方案文档(含 R1-R14 修订)、Task 0 复核报告、执行报告(偏差 D1-D18、行为变更 B1-B13)

## 最终结论: **有条件不通过 — 2 个需修复项 + 1 个建议项,修复后复审**

整体质量高: 提交序列与方案一致、全量验证全绿、17/18 个 Task 抽查通过、偏差 D1-D18 全部判定合理。但存在 1 个功能不完整项(13d 指标未接线)和 1 个偶发测试失败(重连回补重复投递),按"发现问题不直接改代码、列出问题清单由执行者修复"的原则,验收暂不通过。

---

## Step 1: 提交序列核对 — 通过

`git log --oneline 7e92f4b..HEAD` 共 19 个 commit,与执行报告第 1 节一致:
- `72e4e91` Task 0 复核 + 方案修订(文档)
- Task 1→13e 按序各 1 个 commit: `01a2d71` `2509096` `f83568b` `6add022` `089fee8` `71f518c` `37720c5` `45a4634` `0f5d56a` `27c8146` `4a9d9b2` `9bbb555` `9baeb53` `3007476` `abe61bf` `364eee5` `0e08398`
- `9b3ce31` `e1900fc` 收尾文档 2 个
- 工作区干净(`git status` 无未提交变更)

## Step 2: 全量验证 — 全部通过

| 命令 | 结果 |
|------|------|
| `go build ./...` | PASS |
| `go vet ./...` | PASS |
| `go test -race ./...` | PASS(9 个包全绿) |
| `MESSAGELOOP_TEST_REDIS_ADDR=127.0.0.1:6379 go test -race ./...` | PASS(Redis 实跑,无 skip) |
| `cd sdks/go && go test ./...` | PASS |
| `cd sdks/ts && npm test` | PASS(2 suites / 29 tests) |

## Step 3: 逐 Task 抽查结论

| Task | 结论 | 证据 |
|------|------|------|
| 1 TS JSON codec | 通过 | 14 tests PASS;`json.ts:32,40` 用顶层 `toJson/fromJson`;手写映射表全删;`bigIntReplacer` 保留 |
| 2 History inclusive | 通过 | 3 用例 PASS(含真实 Redis);`history.go:70-77` 非零返回 `"ts-seq"`;`git show 2509096` 确认 `client.go` 未动,`sub.Offset + 1` 保持 |
| 3 通配符+引用计数 | 通过 | 5 用例 PASS(含真实 Redis 1.8s);`redis.go:126-181` 精确/通配符双计数 + matcher;`pubsub.go:74` 走 `interested()` 无残留精确布尔判断 |
| 4 集群 epoch | 通过 | 3 用例 PASS;`redis.go:102-114` SET NX+Get;`Epoch()` atomic.Value;`options.go` EpochKey 默认 `ml:broker:epoch` |
| 5 Admin 强制认证 | 通过 | `config.go:184-189` 规则正确;`configs/test.yaml` 已补 token;allow_insecure WARN 日志在 `admin_server.go:15-19` |
| 6 心跳默认值 | 通过 | `node.go:82-95` 回落 300s;B13 的 600s 来源确认(`pkg/websocket/handler.go:64-73`: read deadline = 2×idleTimeout) |
| 7 WS 写超时 | 通过 | `server.go:35` 默认 10s;`main.go:188-203` 空配置保留默认;阻塞写测试用真实 TCP(httptest) |
| 8 入站认证守卫 | 通过 | 5 用例 PASS;`client.go:325-355` 统一守卫 + default `DisconnectBadRequest`;匿名模式 Connect 后 `authenticated=true` 不误伤;`_BeforeAuth` 测试不走 `ForceTestIDs`,D5 不掩盖守卫 |
| 9 匿名接管+指标/限流 | 通过 | 3 用例 PASS;`client.go:463-474` `resumeAllowed` 守卫;metricsCharged 转移;`hub.go:663-680` ReplaceSession 返回 error + limit 检查;D6 改动仅限测试认证方式 |
| 10 lease TTL+CAS | 通过 | `cluster_state.go:20` TTL=600s;`cluster_resume.go:34-78` CAS 先于 takeover、冲突 `DisconnectStale`;续约仍无条件 Put(符合方案) |
| 11 Redis 可靠性 | **通过(附条件)** | 3 用例 + NodeRun 等待 Ready 均 PASS;XDEL 回滚、Receive 确认后 close readyCh、catchUpMissed 回补 + offsetDelivered 去重均已核对;**但见问题 P2(偶发失败)** |
| 12 Publication 模型 | 通过 | 4 用例 PASS;`broker.go:25-35` 新字段;`PayloadProto()` 四处复用(`hub.go:310,424`、`client.go:670`、`api_handler.go:255`);api.proto id=5/metadata=6 生成产物已提交;旧格式按 isText 回退;**命名残留见问题 P3** |
| 13a Admin ACL | 通过 | 2 用例 PASS;固定 `"admin"` principal(`cluster_commands.go:59`);拒绝记 `results[ch]=false`;集群命令路径不重复校验 |
| 13b 僵尸会话回滚 | 通过 | `client.go:561-572` RemoveSession + deleteClusterSessionState + DisconnectStale |
| 13c 指标对称 | 通过 | 3 用例 PASS;AddClient 改为成功后才 Inc(无需 Dec);`cluster_resume.go:165-224` Inc/Dec 对称;PublishToSession 成败均计数 |
| 13d presence/projection | **不通过** | 测试 PASS 且 TTL/清理/reaper 均正确;**但 `PresencePublishFailures` 指标只定义未接线(问题 P1)** |
| 13e snapshot Ephemeral | 通过 | `cluster_state.go:286-325` 经 `LookupSubscriber` 填充;`cluster_resume.go:136-151` 恢复时使用该标志 |

## Step 4: 偏差 D1-D18 核对结论

全部判定 **合理**,要点:
- D1/D2(encode 兼容普通对象、decode ignoreUnknownFields): 与 `Codec` 接口及服务端 `DiscardUnknown: true` 对齐,合理。
- D3(epoch atomic.Value): -race 驱动的必要修复,合理。
- D5(ForceTestIDs 置 authenticated): 已核实 `_BeforeAuth` 守卫测试不经过该方法,无掩盖,合理。
- D6(3 个集群测试改认证模式): Task 9 语义的必然结果,改动经 `--stat` 核实仅限认证方式,合理。
- D10/D11(Run 不早于 Ready 不变式、activePubSub 测试注入点): 工程上稳妥,合理。
- D12/D13(删 IsText、PublishTransient 返回 error): 消除双字段不一致,编译驱动确认无遗漏,合理。
- D14: 方向合理但**执行不完整**——WARN 日志已落地,"13d 补充了 PresencePublishFailures 指标"只完成了定义,未接线(见 P1)。
- D17(ListNodeProjections/DeleteNodeProjection 接口化): 符合抽象边界,合理。

行为变更抽查: #12(`TestClient_Recovery_PreservesPayloadType` PASS)、#9(`TestClientSession_AnonymousResumeRejected` PASS)、B4(`TestResumeRemoteSession_CASConflictAborts` PASS)均与描述一致。

## 问题清单(交执行者修复)

### P1【需修复】Task 13d: `PresencePublishFailures` 指标为死指标
- **证据**: 全仓库 grep 仅命中 `metrics.go:20,93,113`(定义、构造、注册),**无任何 `.Inc()` 调用**;`node.go:839-866` `PublishPresenceJoin`/`PublishPresenceLeave` 失败路径只有 WARN 日志。
- **影响**: 执行报告 B9 声称的新指标 `messageloop_presence_publish_failures_total` 恒为 0,可观测性承诺未兑现。
- **修复要求**: 在 presence 发布失败路径(与 WARN 日志同处)递增该指标;补一个失败注入用例断言计数增加。

### P2【需修复】`TestRedisBroker_Reconnect_CatchesUpMissedMessages` 偶发失败(疑似回补重复投递)
- **证据**: `-race` 下首次运行 FAIL(`ready_test.go:176` 期望 5 条实收 6 条),随后连跑 5 次 PASS——flaky。
- **影响**: 多收的 1 条指向重连瞬间 live pubsub 与 catchUpMissed 回补的重叠窗口(`offsetDelivered` 去重可能存在先投递后记录的时序窗)。若属实,这是 Task 11c 的核心语义缺陷(恰好是"去重"这一卖点)。
- **修复要求**: 定位重复来源(建议:投递前先记录 offset,或回补与 live 投递串行化/加锁);修复后以 `-race -count=20` 压测该用例稳定通过。

### P3【建议】Task 12 命名残留
- `broker_memory_test.go:197` 测试仍名 `TestMemoryBroker_Publish_IsText`(函数体已改用 Kind),建议重命名为 `TestMemoryBroker_Publish_Kind` 类名,避免误导。

## 复审条件

P1、P2 修复并提交(P3 可同批),`go test -race ./...`(含 Redis)+ `-count=20` 压测 P2 用例全绿后,本验收转为 **通过**。

---

## 修复回执(2026-08-10,执行者追加)

按问题清单完成 P1/P2/P3 修复,3 个 commit:

| 项 | Commit | 状态 |
|----|--------|------|
| P1 | `da0e0c8` fix(metrics): wire PresencePublishFailures on presence publish errors | 完成 |
| P2 | `fe0c054` fix(redisbroker): serialize delivery dedup to prevent double delivery on reconnect | 完成 |
| P3 | `e82ea5e` test(broker): rename IsText test to match Kind semantics | 完成 |

### P1: PresencePublishFailures 指标接线

- **根因**: Task 13d 执行时用脚本替换 `node.go` 的 presence 发布失败路径,替换 pattern 未命中,导致只有 WARN 日志落地、`PresencePublishFailures.Inc()` 缺失(验收报告 D14 判断准确)。
- **改动**: `node.go:848-851, 866-869`(`PublishPresenceJoin`/`PublishPresenceLeave` 失败分支,与 WARN 日志同处,`n.metrics != nil` 判空与同文件其他指标一致);新增 `TestNode_PublishPresenceFailure_IncrementsMetric`(`node_test.go`,注入 `failTransientBroker` 使 `PublishTransient` 失败,断言 join+leave 各计 1 次、成功路径不计数)。
- **验证**: `go test -race . -v -run Presence` → 3 个用例 PASS(含 2 个既有用例)。

### P2: 重连回补偶发重复投递

- **根因分析**: 原实现为"检查 `offsetDelivered` → 投递 → 记录 `lastOffsets`"(`pubsub.go`),检查与记录之间存在非原子窗口;且测试在 `activePubSub.Close()` 后立即发布断线消息,旧连接的 teardown(go-redis receive goroutine 退出、channel 关闭)是异步的,存在"旧连接 live 投递"与"重连后 catchUpMissed 回补"交错的可能性——一旦某条消息被两条路径各自投递一次且第二次检查发生在第一次记录之前,即出现 6 条。
- **修复**: 新增 `deliverOnce(channel, pub)`(`pkg/redisbroker/pubsub.go:186-218`):同一 channel 的"查重 → 记录 lastOffsets → 调 handler"放入同一临界区(`deliverMu`,新字段 `pkg/redisbroker/redis.go:33`),记录先行——任何交错下第二条投递路径必然观察到 offset 已记录而跳过;live 循环与 `catchUpMissed` 统一走 `deliverOnce`,删除原 `offsetDelivered`/`recordDeliveredOffset` 两个拆分方法。transient(offset=0)消息不查重直接投递(维持既有语义)。
- **测试加固**(不放宽断言): `ready_test.go:161-173` 发布断线消息前先 `require.Eventually` 等待 `activePubSub == nil`(断开真正生效),使回补路径确定。
- **验证**: `MESSAGELOOP_TEST_REDIS_ADDR=127.0.0.1:6379 go test -race -count=20 ./pkg/redisbroker/ -run TestRedisBroker_Reconnect_CatchesUpMissedMessages` → 20/20 PASS(28.5s);追加 `-count=50` → 50/50 PASS(57.0s)。断言保持"恰好 5 条、严格等于发布序列"。

### P3: 测试命名残留

- `broker_memory_test.go:197` `TestMemoryBroker_Publish_IsText` → `TestMemoryBroker_Publish_Kind`(函数体已按 `Kind` 断言)。

### 全量验证(修复后)

| 命令 | 结果 |
|------|------|
| `go build ./...` / `go vet ./...` | PASS |
| `go test -race ./...` | PASS(8 包全绿) |
| `MESSAGELOOP_TEST_REDIS_ADDR=127.0.0.1:6379 go test -race ./...` | PASS(Redis 实跑,无 skip) |

请按"复审条件"复核: 全量验证 + `-count=20` 压测均已全绿,本验收可转为 **通过**。

---

## 复审结论(2026-08-10,验收人追加)

**最终结论: 验收通过。**

对修复回执逐项独立复核:

| 项 | 复核结果 |
|----|----------|
| P1 指标接线 | 通过。`node.go:850,869` 失败路径递增指标且 `n.metrics != nil` 判空;新用例 `TestNode_PublishPresenceFailure_IncrementsMetric`(注入失败断言 join/leave 各计 1、成功不计)实测 PASS。 |
| P2 重复投递 | 通过。根因分析成立(查重→投递→记录的非原子窗口 + 旧连接异步 teardown 的交错);修复 `deliverOnce`(`pkg/redisbroker/pubsub.go:186-218`)将"查重→记录→投递"收敛进 `deliverMu` 单一临界区且记录先行,任何交错下第二条路径必然观察到已记录而跳过;live 与回补统一入口;transient(offset=0)不查重维持原语义;测试加固用 `require.Eventually` 等断开生效,断言未放宽(仍"恰好 5 条")。压测 `-race -count=20` 由验收人独立重跑: **20/20 PASS(23.4s)**。 |
| P3 命名残留 | 通过。`broker_memory_test.go:197` 已更名 `TestMemoryBroker_Publish_Kind`。 |

全量验证(验收人独立重跑,`-count=1` 强制非缓存): `go build` / `go vet` / `MESSAGELOOP_TEST_REDIS_ADDR=127.0.0.1:6379 go test -race -count=1 ./...` **8 包全绿**。

非阻塞观察(不修复,仅记录): `deliverOnce` 在 `deliverMu` 临界区内调用 handler,全局串行化所有 Redis 侧投递——换来的是 per-channel 严格顺序与无条件去重,语义正确;若未来 Redis 侧吞吐成为瓶颈,可评估"锁内记录、锁外投递"的优化(去重仍成立,仅极端场景顺序性略降)。
