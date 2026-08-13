# MessageLoop 评审汇总（原始报告集）

> 评审日期：2026-08-12。8 个子代理并行评审，报告原样搬运，未做删减、改写或二次判断。核实与汇总由下游主 agent 负责。

## 基线状态

- 模块01: `go build ./...` OK / `go test ./...` OK（含 `-race` 全部通过；`TestNode_SetupProxy_*` 两用例显式 `t.Skip`）
- 模块02: `go build ./...` OK / `go test ./...` OK（本机有 Redis，集成测试真实执行；首次并行跑套件时 `TestClusterCommandBus_ReturnsUnknownFinalStateAfterTimeout` 偶发失败一次，隔离复跑 5 轮全通过）
- 模块03: `go build ./...` OK / `go test ./proxy/... ./pkg/websocket/... ./pkg/grpcstream/...` OK（`go vet` 干净，transport 测试 `-race -count=3` 通过）
- 模块04: `go build ./...` OK / `go test ./pkg/topics/...` OK（含 `-race`）；`shared` 为独立 module，无任何测试文件；全部基准 `-benchtime=10x` 无 panic
- 模块05: `go build`/`go vet`/`go test ./config/... ./cmd/... .`（含 `-race`）OK；但实测文档给出的 `go run cmd/server/main.go --config ./config.yaml` 编译失败（`undefined: prepareGRPCServers`），`go run ./cmd/server --config config.yaml` 启动失败（`grpc-admin-server addr is required`）
- 模块06: `cd sdks/go && go build ./... && go test ./...` OK（含 `go test -race ./...`，2.3s）
- 模块07: `cd sdks/ts && npm test` OK（2 套件 30 用例）；`npm run build`（ESM+CJS+types）通过；`npm run lint` 不可用（eslint 未声明为 devDependency）
- 模块08: `go build ./...` 与 `go test ./...` 均通过

---

## 模块 01：核心会话层

# 评审报告：MessageLoop 核心会话层（评审任务 01）

## 基线测试结果

- `go build ./...`：通过，无错误。
- `go test ./...`：全部包通过（无 Redis 集成测试失败，`TestNode_SetupProxy_*` 两个用例为显式 `t.Skip`，需真实服务器，非回归）。
- `go test -race ./...`：全部通过（含根包 41.7s、pkg/topics 39s 的锁无关并发测试），现有测试未触发任何数据竞争。

## 总体评价

代码整体质量较高：订阅变更用 saga 逆序回滚、`close` 的 16-worker 并发清理、心跳/慢消费/广播的 goroutine 都有界且 `-race` 干净，说明锁覆盖在已测路径上是完整的。主要问题集中在**连接建立（handleConnect）的失败路径**：resume 失败会留下"僵尸会话"（hub 残留已关闭的 client）、connect 路径的普通错误返回会让连接停留在半打开状态；其次是若干"死代码/误导性命名/文档与实现不一致"类问题（`statusConnected` 从未使用、`subShard.broadcastPublication` 仅测试调用、ACL 的 `**` 语义与订阅匹配器不一致、presence 的 `ClientID` 实为 session ID）。现有测试对成功路径覆盖充分，但对失败路径（ReplaceSession 失败、AddClient 失败、心跳超时、限流边界、ACL `**`）覆盖薄弱。

## Findings

---

**[Important] ClientInfo() 无锁读取存在数据竞争（线索 1：确认）**
[位置] client.go:754-763
[问题] `ClientInfo()` 直接读取 `c.client/c.session/c.user/c.connectedAt`，而这些字段在 `handleConnect`（client.go:388-391、504-514、532-548）与 `cluster_resume.go:92-109` 中均在 `c.mu` 保护下写入。
[证据]
```go
func (c *Client) ClientInfo() *ClientInfo {
	return &ClientInfo{
		ClientID:    c.client,      // 无锁读
		SessionID:   c.session,     // handleConnect 锁内写
		...
```
[修复建议] 加 `c.mu.RLock()` 快照各字段；或改为复用 `SessionID()/UserID()/ClientID()` 三个已有加锁 getter。
[置信度] high（竞态真实存在；当前无生产调用方——grep 仅 client_test.go:247 调用——故 `-race` 未触发，属于潜伏问题）

---

**[Important] Resume 失败后旧会话成为僵尸（线索 5：确认）**
[位置] client.go:517-522（配合 client.go:194-200、hub.go:501-519）
[问题] `handleConnect` 本地 resume 路径先 `oldSession.closeQuiet()`（关闭旧 transport、置 `statusClosed`），随后 `ReplaceSession` 失败（如 `DisconnectConnectionLimit`，hub.go:673-682）时直接 `return err`。新连接随后被 `HandleMessage` 关闭，其 `close()` 调 `RemoveSessionIfMatches(sessionID, c)`，但 sessions 表中仍是旧 client（`session != c`，hub.go:510-512 返回 false），**旧会话永久残留在 `sessions` map + connShards + subShards 中**：transport 已关但订阅仍被广播，每次投递报错并累加 `DeliveryFailures`，直到节点重启或下一次成功 resume 才自愈。`syncClusterSessionState` 失败路径（client.go:557-559）同样产生该僵尸。
[证据]
```go
oldSession.closeQuiet()
if err := c.node.hub.ReplaceSession(connect.SessionId, c); err != nil {
	return err   // ← 旧会话已关但仍在 hub
}
```
[修复建议] `ReplaceSession` 失败时回滚：删除 hub 中旧会话条目（`RemoveSessionIfMatches(sessionID, oldSession)`）并清理其 cluster 状态；或在失败分支显式恢复。
[置信度] high

---

**[Important] connect 路径的普通错误返回导致半打开连接（新发现）**
[位置] client.go:551-553、557-559、526-528；配合 client.go:305-321
[问题] `handleConnect` 中 `AddClient`、`syncClusterSessionState`、`resumeRemoteSession` 返回的是 `fmt.Errorf` 包装的普通错误而非 `Disconnect`。`HandleMessage` 只对 `Disconnect` 错误调 `close()`（client.go:306-310），普通错误仅发 `INTERNAL_ERROR` 帧并返回 err，**连接保持打开**。此时 `c.authenticated` 已置 true（client.go:533）、`c.session` 可能已被改写为远端 session（cluster_resume.go:93），客户端处于"已认证但未注册/半注册"状态，且无法再 Connect（会被判 `DisconnectBadRequest`）。
[证据]
```go
if !resumed || !resumedLocal {
	if err := c.node.AddClient(c); err != nil {
		return err   // 普通错误 → 连接不关闭
	}
```
[修复建议] 这些失败统一包装为 `Disconnect` 错误（或显式 `c.close(...)`），保证 connect 失败必断连。
[置信度] high

---

**[Minor] statusConnected 从未使用（线索 6：确认）**
[位置] client.go:128-134；对照 docs/developer/01-architecture.md:122
[问题] 生产代码只写 `statusClosed`（client.go:142/234/232），`status` 零值为 0（连 `statusConnecting` 都没赋过），`statusConnecting/statusConnected` 两个常量是死代码。文档声称的三态机（connecting → connected → closed）实际只有"0 → closed"两态。
[证据] `statusConnected` 全部引用仅 client_test.go:1428/1444 与架构文档；client.go 中无任何赋值。
[修复建议] 要么在 connect 成功后置 `statusConnected` 并让 `ClientInfo/心跳` 使用，要么删除死常量并修正文档。
[置信度] high

---

**[Minor] subShard.broadcastPublication 是仅测试调用的重复实现（线索 7：确认）**
[位置] hub.go:291-375；对照 hub.go:396-489
[问题] 生产广播只走 `Hub.broadcastPublication`（node.go:126），`subShard.broadcastPublication` 仅在 hub_test.go:425/449 被调用。两者是 ~80 行几乎完全重复的实现，且 subShard 版本没有 exact+wildcard 去重逻辑（按 session 合并），容易在将来被误接回生产路径造成重复投递（正是 fix-plan.md:182 记录过的历史 bug）。
[修复建议] 删除 subShard 版本，测试改为走 `Hub` 层。
[置信度] high

---

**[Minor] ACL 用 path.Match：`chat.**` 无特殊语义，且与订阅匹配器语义不一致（线索 2：确认但需澄清）**
[位置] acl.go:84、111；acl.go:10 注释声称支持 `chat.**`
[问题] 实测 `path.Match` 行为（已用 Go 程序验证）：`*` 可跨 `.` 匹配任意非 `/` 字符，因此 `chat.**`/`chat.*` 都能匹配 `chat.a.b`（`**` 只是两个叠加的 `*`，与常见 glob 的目录穿透语义无关），但**都不能**匹配含 `/` 的频道（如 presence 伴生频道 `chat.a/__presence`）。关键不一致：ACL 的 `*` 跨点匹配，而 hub 通配订阅（CSTrieMatcher，delimiter="."，pkg/topics/matcher.go:4-5）的 `*` 只匹配单段——ACL 允许订阅 `chat.*` 的用户能订阅 `chat.a.b`，但用 `chat.*` 作订阅模式却收不到 `chat.a.b` 的消息；同理 ACL 规则永远管不到 `*/__presence` 频道。
[修复建议] 明确文档语义并统一：将 ACL 匹配改为与 matcher 相同的分段通配，或至少修正注释与文档（去掉 `chat.**` 示例），并补充 `**` 与 `/` 频道的测试。
[置信度] high

---

**[Minor] GetActiveChannels 把通配模式当频道列出并重复计数（线索 3：确认）**
[位置] hub.go:624-657
[问题] 精确订阅按频道统计订阅者数（626-635），随后把每条通配订阅的**模式本身**（如 `chat.*`）以 `counts[sub.Topic]++` 计入（637-644）。结果：① 通配模式被当成真实频道返回给管理 API；② 同一会话同时精确+通配订阅同一频道时被计 2 次（与 `broadcastPublication`/`GetMatchingSubscribers` 的按 session 去重语义矛盾）。
[证据]
```go
h.wcSubsMu.Lock()
for _, sub := range h.wcSubs {
	counts[sub.Topic]++   // 模式本身 +1
}
```
[修复建议] 通配订阅不计入频道列表（或按"该模式实际匹配到的频道"投影），并按 session 去重。
[置信度] high

---

**[Minor] ReplaceSession 连接数检查存在 TOCTOU（线索 4：确认，窗口较窄）**
[位置] hub.go:673-698
[问题] 不同用户替换时，限额检查在 `h.mu` 内读 `shard.users`（673-682），但 connShard 的实际写入发生在 `h.mu.Unlock()` 之后（684-698）。同一用户两个不同 session 并发 resume/添加时，两个检查可能都看到旧计数而双双通过，导致超限。
[证据]
```go
if userConns >= h.maxConnsPerUser { h.mu.Unlock(); return DisconnectConnectionLimit }
h.sessions[sessionID] = newClient
h.mu.Unlock()
h.connShards[index(...)].mu.Lock()   // 写入发生在锁释放后
```
[修复建议] 将 connShard 写入移入 `h.mu` 保护区间，或改用 per-shard 的原子 check-and-add。
[置信度] medium（逻辑窗口真实存在，但需并发不同会话同用户才触发）

---

**[Minor] PresenceInfo.ClientID 实为 session ID（线索 8：确认）**
[位置] client.go:628-632、1051-1055、1355-1359；node.go:206-208
[问题] `PresenceInfo.ClientID` 与 presence 存储键均写入 `c.session`，与 SDK 语义的 `client_id`（`connect.ClientId`）不同。管理 API `GetPresence`（pkg/grpcstream/api_handler.go:227-232）把该值原样暴露为 `client_id`。文档 03-admin-api.md:271 已如实记录"当前实现中即会话 ID"，属已知怪癖，但字段名对使用者有误导性，且 resume 时 presence 记录跟随 session 存活——若未来语义改为真 clientID 会破坏该行为。
[修复建议] 改名（如 `SessionID`）或补充文档强调；保持键=session 的语义不变。
[置信度] high

---

**[Minor] Survey.Close 的 drain 是单条且 channel 从不关闭（线索 9：确认但危害为良性）**
[位置] survey.go:174-181
[问题] `Close()` 仅非阻塞 drain 一条消息，`responseCh` 从未 `close()`。但不会泄漏 goroutine：`AddResponse` 的发送全部非阻塞（select+default，101-113）、`Wait` 靠 `done` 退出并从 map 取结果（156-164）、超时 goroutine 由 `closeOnce` 保证只关一次。真正的问题是两个：① `Close` 的"清空"语义误导（清不掉已缓冲的其余 99 条）；② 107-108 行 "channel full, response may be dropped" 的告警是**假警报**——响应已先写入 map（96-98），channel 满不影响结果。
[修复建议] `Close` 改为循环 drain 或干脆删除；修正告警文案。
[置信度] high

---

**[Minor] 订阅上限检查重复计数、未剔除 ACL 拒绝的频道（新发现）**
[位置] client.go:591-598、1012-1019
[问题] `handleConnect` 的上限检查用 `inheritedCount + len(subs)`，`handleSubscribe` 用 `currentCount + len(sub.Subscriptions)`：① 若 connect 携带的订阅与已继承频道重复、或 subscribe 批内频道重复，会虚增计数导致**误拒绝**；② ACL 拒绝的频道（随后 `continue` 跳过）仍计入上限。
[证据]
```go
if currentCount+len(sub.Subscriptions) > limit {   // 重复频道被计两次
	return DisconnectChannelLimit
```
[修复建议] 按去重后的新增频道数检查，且先做 ACL 过滤再计数。
[置信度] medium（语义缺陷明确，触发需重复订阅）

---

**[Minor] 未认证 connect 把客户端提供的 session ID 传给认证代理（新发现）**
[位置] client.go:387-391、427-433、467-474
[问题] `connect.SessionId` 在认证**之前**被写入 `c.session`（387-391），随后作为 `authReq.SessionID` 传给代理（431）。匿名模式/拒绝 resume 时该值虽在 468-474 被回滚，但伪造的 session ID 已经暴露给认证代理（代理可能据此做会话级授权）。
[修复建议] 认证请求使用原始 session 或显式标记"未验证"的 session ID。
[置信度] medium

---

**[Minor] close()/handleUnsubscribe 按频道各起一个 presence goroutine（新发现）**
[位置] client.go:183-189、1117-1120
[问题] 每个订阅频道 `go c.node.PublishPresenceLeave(...)` 无并发上限：单连接订阅 10k 频道断开时会瞬间产生 10k 个 goroutine（每个内含一次 `PublishTransient`，Redis 模式下是一次网络往返）。与同函数中订阅移除的 16-worker 有界设计不一致。
[修复建议] 复用 16-worker 模式或信号量限制 presence 事件发布。
[置信度] medium

---

**[Minor] 单客户端并发 survey 响应会串路由（新发现）**
[位置] client.go:1274-1279
[问题] `handleSurveyReply` 在请求无 `RequestId` 时回退到 `lastSurveyRequestID`（单槽位）。同一客户端同时收到多个 survey 时，早期 survey 的响应会被误路由到最新 survey，且该响应会被 `IsExpectedSession` 判为"伪造"丢弃或污染最新 survey 的结果。
[修复建议] 移除回退逻辑，强制客户端带 `RequestId`。
[置信度] medium

---

## 建议补充的测试

1. `TestHub_ReplaceSession_FailureLeavesNoZombie`：构造超限场景使 ReplaceSession 返回 `DisconnectConnectionLimit`，断言旧会话从 `sessions` map、connShard、subShard 全部清除、旧 transport 已关且无残留订阅（回归线索 5）。
2. `TestClient_Connect_AddClientFailureDisconnects`：注入 cluster sync 失败，断言连接被关闭而非停留半打开（回归 AddClient/syncClusterSessionState 普通错误路径）。
3. `TestClient_Connect_RemoteResumeErrorClosesConnection`：`resumeRemoteSession` 返回 Redis 错误时连接必须关闭。
4. `TestClient_ClientInfo_RaceFree`：并发 `HandleMessage(Connect)` 与 `ClientInfo()` 循环，-race 下运行（回归线索 1）。
5. `TestClient_SubscribeLimit_DuplicateChannels`：重复频道/ACL 拒绝频道不应虚增订阅计数。
6. `TestClient_Heartbeat_IdleDisconnect`：`HeartbeatManager` + 短 IdleTimeout，验证无消息时以 3511 断开、有消息时不误杀（当前无 heartbeat_test.go）。
7. `TestSurvey_Close_DrainsAllBuffered`：填满 responseCh 后 Close，断言 Wait/Results 语义（回归线索 9）。
8. `TestHub_GetActiveChannels_NoWildcardPatterns`：通配订阅不应把模式当频道列出、exact+wildcard 不重复计数（回归线索 3）。
9. `TestACLEngine_DoubleStarSemantics`：`chat.**` 对 `chat.a.b`、`chat/a/b`、`chat.a/__presence` 的匹配断言，锁定 path.Match 的真实语义（回归线索 2）。
10. `TestHub_ReplaceSession_ConcurrentLimit`：并发替换/添加压 `maxConnsPerUser` 边界，断言不超限（回归线索 4）。
11. `TestNode_Shutdown_DrainsAll`：多个已连接 client 下调用 `Shutdown`，断言全部以 `DisconnectForceNoReconnect` 关闭且不超时（当前仅集成测试间接覆盖）。
12. `TestClient_HandleMessage_ConcurrentConnect`：两个 goroutine 并发 Connect，断言状态一致、无竞态（当前并发测试只打 Ping）。

---

## 模块 02：Broker 与集群层

# 评审报告：Broker 与集群层（MessageLoop）

## 基线测试结果与总体评价

- `go build ./...` 通过；`go test ./...` 通过。本机 `127.0.0.1:6379` 有可用 Redis，**Redis 集成测试全部真实执行**（非跳过），包括根包的 `TestClusterRedis_*`（DB 15）与 redisbroker 包（DB 14）。
- 首次并行跑两套测试时 `TestClusterCommandBus_ReturnsUnknownFinalStateAfterTimeout` 失败一次，隔离复跑 5 轮全套件均通过——为偶发（机理见 Finding 6）。
- 总体评价：代码质量较高——命令去重/claim 租约/租约续期/panic 恢复/回滚路径设计完整且有测试背书；Redis 侧 `deliverOnce` 去重设计（锁内 check+record+deliver）正确解决了重连双投问题。主要问题集中在：命令总线**没有断线重连**（静默失能）、`deliverOnce` 全局锁把 handler 关进临界区（多频道吞吐瓶颈）、以及 **04-cluster.md 多处与代码漂移**（History 语义、会话租约 TTL、presence 索引 TTL、epoch 语义）。

## Findings

```
[级别] Important
[位置] pkg/redisbroker/cluster_command_bus.go:129-140（Start 的 reader goroutine）
[问题] 命令总线断线后不重连：pubsub 连接一旦断开，`pubsub.Channel()` 关闭、reader 循环静默退出，该节点从此不再处理任何集群命令，直到进程重启。
[证据] Start 只 `Subscribe` 一次后 `for message := range pubsub.Channel()`；全文件无任何重连/退避逻辑（对比 broker 侧 pubsub.go:14-34 有 runPubSubWithRetry 1s→30s 退避）。go-redis 对 Channel() 模式不自动重连。失效后无日志、无指标，命令在 Redis 侧无订阅者被丢弃，发送方只能等到 5s 超时返回 UnknownFinalState——静默降级。
[修复建议] 仿照 runPubSubWithRetry 给 reader 循环加退避重连；Shutdown 用 stop 标志区分主动关闭与意外断线；断线时打 Warn 日志并计数（可复用 ClusterCommandTimeouts 指标）。
[置信度] high
```

```
[级别] Important
[位置] pkg/redisbroker/pubsub.go:163-190（deliverOnce）
[问题] 单把全局 `deliverMu` 串行化所有频道的投递去重，且 handler（= node 的 hub.broadcastPublication，含客户端网络写）在临界区内执行——任意频道一个慢消费者会阻塞所有频道的实时投递与断线 catch-up（队头阻塞），多频道吞吐受限。已知线索 2 确认，且比线索所述更严重。
[证据] `b.deliverMu.Lock(); defer b.deliverMu.Unlock()`（173-174 行）；`b.handler(channel, pub)` 在锁内（187-189 行）。对照：内存 broker 在 handler 调用前已释放 h.mu（broker_memory.go:139-144）。
[修复建议] 至少把 handler 调用移出临界区（check+record 在锁内，投递在锁外，代价是极端交叠下可能重复投递一次，可在 handler 层以 client 端 offset 幂等兜底）；或按频道 hash 分片多把锁（对齐 hub 的 16384 subLock 分片模式）。
[置信度] high
```

```
[级别] Important
[位置] pkg/redisbroker/pubsub.go:168、188（`_ = b.handler(channel, pub)`）
[问题] handler 错误被吞掉：Redis broker 的投递错误对发布者不可见，而内存 broker 把 handler 错误原样返回给 `Publish` 调用者（broker_memory.go:141-144，且 TestMemoryBroker_Publish_HandlerError 断言此行为）。同一 `Broker` 接口下两实现行为不一致；此外 Redis 侧 handler panic 无 recover，会直接炸掉 pubsub 协程（对比 hub.broadcastPublication 并行分支有 recover，hub.go:468-474）。已知线索 3 确认。
[证据] 内存：`return offset, (*h)(ch, &stored)`（broker_memory.go:142）；Redis：`if b.handler != nil { _ = b.handler(channel, pub) }`（pubsub.go:167-169）。两处（实时路径 + 去重路径）均为 `_ =` 吞错。
[修复建议] 至少记录指标/日志而非静默丢弃；为 deliverOnce 增加 panic recover 转错误日志；在接口注释中明确"异步投递实现不向 Publish 传播投递错误"的契约差异。
[置信度] high
```

```
[级别] Important
[位置] docs/developer/04-cluster.md:295、356；docs/developer/03-admin-api.md:311-317、554
[问题] 文档与代码漂移，已知线索 1 确认且方向明确：**代码全部一致（inclusive），错的是文档**。同一文档还有 3 处独立漂移：会话租约 TTL、presence 索引 TTL、epoch 语义。
[证据] 代码：接口契约 `offset >= sinceOffset`（broker.go:105-108）；内存实现 `pub.Offset < sinceOffset` 跳过（broker_memory.go:180）；Redis `streamStartID` 非零返回 `"ts-seq"`（history.go:70-77，提交 2509096 修复）。文档 04-cluster.md:295 仍称 `"(ts-seq"` 排他；:356 与 03-admin-api.md:316、554 仍称 Redis exclusive。另：会话租约文档称 90s（04-cluster.md:201、308），代码为 600s（cluster_state.go:20）；presence 索引文档称 `PresenceTTL * 2`=120s（:281），代码为 PresenceTTL=60s（presence_redis.go:50，提交 364eee5 已改）；4.4 节称 epoch 为"每 broker 进程随机 UUID"（:236-238），代码为 SET NX 共享集群 epoch（redis.go:107-119，epoch_test.go 的 SharedAcrossNodes 断言为证）。
[修复建议] 同步更新 04-cluster.md 与 03-admin-api.md 四处表述；建议在 04-cluster.md 维护一张"最近行为变更"表或标注提交号，避免再次漂移。
[置信度] high
```

```
[级别] Important
[位置] pkg/redisbroker/pubsub.go:107-156（catchUpMissed）
[问题] 断线 catch-up 存在无提示的消息缺口，三层叠加：① XRangeN 数量上限 = StreamMaxLength（125 行），Approx 修剪下流内条目可略超上限，超出的尾部（最新消息）被截掉；② catch-up 执行期间新到消息进入 pubsub 缓冲区（go-redis 默认 100 条），缓冲满时 go-redis 静默丢弃，丢弃的消息不在 XRange 快照内 → 永久丢失；③ 缺口无任何水位/标记通知客户端（对比内存 broker 环形缓冲同样覆盖，但 Redis 侧本可检测 `last+1` 已不在流内）。
[证据] `XRangeN(ctx, ..., int64(b.opts.StreamMaxLength))`（125 行）；catch-up 先于 `pubsub.Channel()` 消费开始（58-60 行）；go-redis `Channel()` 满时丢弃（非阻塞发送）。
[修复建议] catch-up 后校验最新流 ID 与 lastOffsets 之间是否有断层，有则向客户端发送显式 gap 提示（如复用 RecoveryTruncated 类信封）；将 pubsub 消费缓冲调大或 catch-up 期间同时消费 channel。
[置信度] medium
```

```
[级别] Important（测试偶发性 + 产品小瑕疵）
[位置] pkg/redisbroker/cluster_command_bus.go:232-257（waitForReply）；cluster_command_bus_test.go:103-127
[问题] 超时瞬间 `ctx.Done()` 与 reply 通道关闭（go-redis 在 deadline 时关闭订阅连接）两个 select 分支同时就绪，随机选中 `!ok` 分支会返回硬错误 "cluster command reply channel closed"，而测试期望 UnknownFinalState——首次全套件并行运行时观察到该测试失败（0.20s），隔离复跑 5 轮通过。
[证据] waitForReply 两个 case：`case <-ctx.Done(): ... resolveTimedOutCommand` 与 `case reply, ok := <-replies: if !ok { return nil, fmt.Errorf(...) }`；测试用 100ms 超时且断言 `require.NoError`。产品侧瑕疵：deadline 时应优先走 resolveTimedOutCommand 而非报错。
[修复建议] 产品：`ctx.Done()` 就绪时优先返回 UnknownFinalState（如先非阻塞检查 ctx.Err()）；测试：拉长超时到 500ms+ 或对两种结果都接受。
[置信度] medium
```

```
[级别] Minor
[位置] pkg/redisbroker/pubsub.go:78-97 与 130-153
[问题] runPubSub 与 catchUpMissed 存在几乎相同的"反序列化 + 构造 Publication + Time 兜底"代码块（已知线索 4 确认）。
[证据] 两处均为 `deserializeMessage → redisMsg.Type != messageTypePublication → &Publication{...} → if pub.Time == 0` 结构，仅 Offset 来源不同（redisMsg.Offset vs parseStreamOffset(m.ID)）。
[修复建议] 抽取 `messageToPublication(channelName string, redisMsg *redisMessage, offset uint64) *Publication` 辅助函数。
[置信度] high
```

```
[级别] Minor
[位置] pkg/redisbroker/redis.go:48（lastOffsets 字段）、pubsub.go:184
[问题] `lastOffsets` 只在投递时写入、`Unsubscribe` 从不清理——长时间运行下按频道名无限增长（每次投递一次 map 写入，频道退订后条目残留）。
[证据] 全文件搜索 lastOffsets：写入点仅 pubsub.go:184，无删除点；与订阅簿记（subscribed/wcCounts 会 delete）不一致。
[修复建议] Unsubscribe 归零时删除对应 lastOffsets 条目。
[置信度] high
```

```
[级别] Minor
[位置] cluster_resume.go:275-277（evictSessionForTakeover）
[问题] takeover 驱逐用无条件 `n.hub.RemoveSession(sessionID)`，而非仓库为 stale 保护专门设计的 `RemoveSessionIfMatches`（hub.go:501）：LookupSession 与 RemoveSession 之间若同 session 的新本地连接完成 ReplaceSession，会误删新会话注册。LeaseVersion 校验（cluster_commands.go:291-296）收窄了窗口但不能完全关闭（租约版本经 PUT 同步可能漂移）。
[证据] `if sessionID != "" { n.hub.RemoveSession(sessionID) }`（275-277 行）；`RemoveSessionIfMatches` 注释明确"prevents a failed or stale connection from evicting a session that a newer client has taken over"（hub.go:496-500）。
[修复建议] 改为 `n.hub.RemoveSessionIfMatches(sessionID, client)`。
[置信度] medium
```

```
[级别] Minor
[位置] broker_memory.go:112-118（Publish 无条件创建 channelHistory）
[问题] 内存 broker 的 history map 按频道名无限增长：任意频道被发布一次即分配 256 槽环形缓冲且永不回收（仅空条目在退订时回收），与 Redis broker 的 HistoryTTL 过期语义不对称；单节点长跑 + 高基数频道名是增长向量。
[证据] Publish 对不存在的频道直接 `b.history[ch] = h`；Unsubscribe 仅当 `h.count == 0` 才 delete（98-107 行）；测试 TestMemoryBroker_History_RetainedAfterLastUnsubscribe 确认"有内容即保留"是设计意图。
[修复建议] 文档化该差异；或为单节点模式引入空转回收/上限策略。
[置信度] high
```

```
[级别] Minor
[位置] pkg/redisbroker/cluster_command_bus.go:603-619（executeHandlerBounded）
[问题] 超过 10s 的 handler 被"放弃"而非终止：goroutine 继续运行并占用信号量槽（`<-sem` 在 defer 里，直到 handler 返回），claim 租约停止续期后到期，重复命令可能被二次 claim 并发执行同一命令；卡死 handler 数量上限即全部 128 槽被占。属已知设计权衡，但值得记录风险。
[证据] `done := make(chan outcome, 1); go func(){ ... done <- ... }()`；超时分支直接 return，goroutine 无取消手段。
[修复建议] 文档注明"handler 必须响应 ctx 取消，否则会占用并发槽"；可考虑给槽释放挂独立超时。
[置信度] high
```

```
[级别] Minor
[位置] cluster_state.go:286-325（clusterSessionSnapshot）
[问题] 快照的 `ChannelOffsets` 与 `BrokerEpoch` 字段从未填充——跨节点恢复无法按 offset 精确续读，远程 resume 后只能靠客户端自带的 offset 续读（client.go:642 `sub.Offset + 1`）；新客户端未带 offset 时退化为全量恢复。04-cluster.md:202、238 已承认此缺口，此处仅确认其为"未完成功能"而非文档错误。
[证据] clusterSessionSnapshot 只填 Subscriptions/AuthContext/身份字段；结构体声明了 ChannelOffsets/BrokerEpoch（cluster_state.go:67-68）。
[修复建议] 填充 ChannelOffsets（每频道最后投递 offset，数据源即 hub 订阅簿记）与 BrokerEpoch，实现跨节点精确续读。
[置信度] high
```

```
[级别] Minor
[位置] pkg/redisbroker/presence_redis.go:57-72（Remove）
[问题] Remove 的 `SCard==0 → DEL index` 与并发 Add 存在竞态：Add 的 SADD 在 Remove 的 SCard 与 DEL 之间执行时，索引被删而成员键仍在——Get 返回空，出现"在线但不可见"的幽灵窗口。
[证据] Remove：pipeline(DEL+SREM) → SCard → 若 0 则 DEL index；Add：pipeline(SET+SADD+EXPIRE)。两步非原子。
[修复建议] 用 Lua 脚本原子化（SCard 后判断 + DEL），或删除时以 EXPIRE 0 替代。
[置信度] medium
```

```
[级别] Minor
[位置] pkg/redisbroker/cluster_command_bus.go:222-224（SendCommand）
[问题] 发送命令前不检查目标节点租约是否存活：目标已宕机时 PUBLISH 到无订阅者频道成功返回，发送方白等满 5s 默认超时。纯效率问题，非正确性。
[证据] Publish 后直接 waitForReply；对照 BroadcastCommand 有 scanKeys+Get 租约预检（274-297 行）。
[修复建议] SendCommand 前顺带 GetNodeLease 快速失败。
[置信度] high
```

```
[级别] Minor
[位置] pkg/redisbroker/cluster_command_bus.go:1-11（包注释信任边界）；docs/deployment.md
[问题] 已知线索 6 部分确认：信任边界（无签名/认证、依赖 Redis 网络隔离）在 04-cluster.md:180 有明确说明，但部署文档 deployment.md 的 "Redis Broker / Multi-Node Cluster" 章节（95-147 行）只提共享实例/数据库与 epoch 维护，未提及该信任边界与 Redis 隔离要求；集群命令可注入 disconnect/takeover/publish 属高影响面。
[证据] 04-cluster.md:180 完整描述信任边界；deployment.md 全文无"信任/签名/隔离"表述。
[修复建议] deployment.md 增加"Redis 网络隔离是安全前提"的部署项。
[置信度] high
```

```
[级别] Minor
[位置] 仓库行尾（已知线索 7，方向反转）
[问题] 线索称 cluster_query_store.go 用 CRLF "与仓库其余 LF 不一致"——实际相反：约 90% 的 .go 文件为全 CRLF，约 40 个文件为 LF（broker.go、pubsub.go、message.go、history_test.go 等），cluster_query_store.go（201/202）、node_test.go、cluster_redis_integration_test.go、api_handler_test.go 等为**同一文件内混用**。仓库无 .gitattributes，混行尾会造成 diff 噪音。
[证据] 全仓扫描：CRLF 全量文件 74+ 个，LF 文件 40 个，混合 5 个。
[修复建议] 统一为 LF（或 CRLF），加 .gitattributes 归一化。
[置信度] high
```

```
[级别] Minor（测试缺口，已知线索 5 确认）
[位置] pkg/redisbroker/cluster_query_store.go；cluster_state.go:352-434；pubsub.go:107-156
[问题] 三个单元级缺口均确认：① redisClusterQueryStore 全部方法（Lua 脚本、ReplaceNodeChannels、ListChannels、ListNodeProjections）无独立单测，仅集成路径覆盖；② clusterNodeLeaseManager 无单独测试（仅集成测试中随 Cluster deps 启动，失败路径完全未覆盖）；③ catchUpMissed/deliverOnce 无独立单测，仅 TestRedisBroker_Reconnect_CatchesUpMissedMessages（依赖真实 Redis）。
[证据] 全仓 grep：无任何测试文件直接引用 redisClusterQueryStore、clusterNodeLeaseManager、catchUpMissed、deliverOnce；redisbroker 包内也无 benchmark（对比内存 broker 有 2 个）。
[修复建议] 见下文"建议补充的测试"。
[置信度] high
```

## 建议补充的测试

1. **deliverOnce 单元测试（miniredis 或内存 fake client）**：同 offset 双投（实时与 catch-up 交叠）、offset 0 瞬时消息不去重、handler 错误/panic 时锁释放与状态一致性、跨频道并发投递不互相阻塞（回归 deliverMu 改造）。
2. **catchUpMissed 单测**：last=0 跳过、XRange 失败仅 Warn 继续、流内条目被修剪后 last+1 起点行为、断线期间新订阅（无基线）不补。
3. **redisClusterQueryStore 单测**：Lua 脚本加减/归零 HDEL/空 hash DEL、ReplaceNodeChannels 空 map 时删键、ListChannels 聚合与排序、ListNodeProjections/DeleteNodeProjection 解析（含 nodeID 含冒号的边界）。
4. **clusterNodeLeaseManager 测试**：renewOnce 失败时 Start 返回错误且可重试、Shutdown 提前返回、续租失败仅 Warn 不退出循环。
5. **命令总线重连测试**（修复后）：模拟 pubsub 断线 → reader 重连 → 命令仍被执行；断线期间命令超时返回 UnknownFinalState。
6. **waitForReply 截止时刻竞争测试**：deadline 与通道关闭同时就绪时必走 resolveTimedOutCommand 分支。
7. **presence Remove/Add 竞态测试**：并发 Add+Remove 后 Get 不丢在线成员（Lua 原子化后回归）。
8. **Redis broker benchmark**：`BenchmarkRedisBroker_Publish`、`BenchmarkDeliverOnce_MultiChannel`（量化 deliverMu 瓶颈）。
9. **evictSessionForTakeover stale 保护测试**：LookupSession 返回新 client 时 RemoveSessionIfMatches 不误删新会话。

另注：`SRem(ctx, key, []string)` 传切片实测会被 go-redis 扁平化展开（n=3 全部移除），presence_redis.go:117 非 bug；`pkg/redisbroker/epoch.go` 文件不存在（initEpoch 在 redis.go），任务清单中的文件名略有不符，无实际影响。

---

## 模块 03：Proxy 与传输层

# 评审报告：Proxy 与传输层（Task 03）

## 基线测试与总体评价

`go build ./...` 通过；`go test ./proxy/... ./pkg/websocket/... ./pkg/grpcstream/...` 全部通过（cached），`go vet` 干净，`pkg/grpcstream` 的 transport 测试加 `-race -count=3` 通过。总体架构良好：`Transport` 抽象干净、gRPC 单 worker 串行写 + 入队前拷贝、`closeOnce` 一次性关闭、admin 拦截器常量时间比较等设计扎实，测试对并发写/关闭竞态覆盖不错。但存在三处实质缺陷：**HTTP proxy 的 payload JSON 编解码实际是坏的（oneof 经 `encoding/json` 永不还原）**、**WebSocket 子协议协商可能产生"marshaler 与帧类型"不一致**、**`sendWithTimeout` 共享 timer 会在负载下误报超时**；另有断连帧竞态与若干测试缺口。

## Findings

```
[级别] Important
[位置] proxy/http.go:87-92（请求侧）、proxy/http.go:103-107 及全部 doRequest 解析回调（响应侧）
[问题] HTTP proxy 的 payload 编解码实质损坏：请求 payload 不是 proto3 JSON 映射，响应 payload 的 oneof（Data）永远无法还原，客户端经 HTTP proxy 的 RPC 拿不到任何实际载荷数据。
[证据] 实测（probe 程序，读操作不落库）：
  json.Marshal(Payload{Json:...}) → {"content_type":"application/json","Data":{"Json":{...}}}
  json.Unmarshal(`{"payload":{"json":{...}}}`, &RPCResponse) → resp.Payload.GetData() == nil
  根因：shared/genproto/shared/v1/types.pb.go:34 的 Data 是 isPayload_Data 接口字段且无 json tag，
  encoding/json 无法写入 oneof 包装；协议要求的 {"data":{"json":...}} 包装也不存在。
  现有 TestHTTPProxy_RPC（transport_test.go:93-96）只断言 resp.Payload 非 nil，掩盖了 Data==nil。
[修复建议] 用 protojson.Marshal/Unmarshal 收发（与 gRPC/文档的 proto3 JSON 契约一致），或用
  MarshalAppend 序列化整条 proxypb.RPCRequest 后仅 JSON 化外层信封。
[置信度] high（实证）
```

```
[级别] Important
[位置] pkg/websocket/handler.go:46-48, 111-119
[问题] 已知线索 6 确认且更严重：marshaler 由"客户端请求的"子协议列表经 strings.Contains 决定，
      而帧类型（msgType）由"协商后的" conn.Subprotocol() 决定，两者可能不一致，导致文本帧携带
      protobuf 字节或反之，客户端无法解码。
[证据] gorilla 在客户端 offer 列表中取第一个服务端支持的子协议：客户端 offer
  ["messageloop", "messageloop+proto"] 时协商结果为 "messageloop"（文本帧），
  但 marshaler 循环命中 "messageloop+proto" 包含 "proto" → ProtobufMarshaler。
  任意未知子协议名（如 "xproto"）也会因 Contains 命中 ProtobufMarshaler 而帧类型是 Text。
[修复建议] 用协商结果 conn.Subprotocol() 做精确 switch 映射，删除 Contains 子串匹配。
[置信度] high
```

```
[级别] Important
[位置] pkg/grpcstream/transport.go:63-82
[问题] 已知线索 1 确认：sendWithTimeout 复用同一个 timer，先后在两个 select 里读 timer.C。
[证据] 若 enqueue（第一个 select）耗时接近 timeout（sendCh 繁忙），timer 在两次 select 之间已触发，
  第二个 select 立即命中 timer.C 返回 "write timeout"，而帧此刻已成功入队且会被 worker 投递——
  调用方（核心层 write 路径 client.go:1099）据此把健康连接判为慢消费者断连。若第一个 select 在
  timer.C 与 sendCh 同时就绪时随机选中 sendCh，同样误报。反过来第二个 select 也消耗了
  enqueue 剩下的预算，实际留给"等待投递确认"的时间趋近 0。
[修复建议] 第一个 select 成功后调用 timer.Reset(剩余或完整 timeout) 再进入第二个 select；
  或第二个 select 单独用 time.After。语义上应保证"enqueue+ack 各占独立预算"或"总预算内不误报"。
[置信度] high（竞态存在性由代码确定；实际触发概率随负载升高）
```

```
[级别] Important
[位置] pkg/grpcstream/transport.go:84-104, 139-148
[问题] 已知线索 2 确认：Close 先置 closed=true 再经 sendWithTimeout 投递断连帧，存在两条退化路径。
[证据] ① sendCh 满（64 帧积压、worker 卡在 SendMsg）时，writeError 的 enqueue 阻塞满
  writeTimeout（默认 10s），Close 阻塞 10s 且断连帧根本没入队；② 即使入队成功，worker 的
  select 在 sendCh 与 closeCh 同时就绪时随机二选一，可能直接退出而丢弃断连帧，此时 Close 还会
  在第二个 select 里白等 errCh 直到超时。客户端两种情况下都收不到 DISCONNECT_ERROR 帧。
[修复建议] worker 退出前先排空 sendCh（对每个遗留 req 回填 errCh 后退出）；Close 的 writeError
  失败时降级为直接关闭（不阻塞 10s），并考虑关闭顺序：先确认断连帧被 worker 取走再 close(closeCh)。
[置信度] medium-high
```

```
[级别] Important
[位置] proxy/http.go:392-395
[问题] 已知线索 3 确认：doRequest 仅接受 200，非 200 把原始 body 文本拼进 error 字符串。
[证据] `if resp.StatusCode != http.StatusOK { return nil, fmt.Errorf("proxy returned status %d: %s", ...) }`
  后端无法用结构化 sharedpb.Error 表达 HTTP 级错误（503/429/网关错误等），错误到客户端时只剩
  handleRPC 的 PROXY_ERROR 文本信封（client.go:851-859）；与 gRPC 代理路径也不一致
  （gRPC 侧同样把 status 错误拍平成文本，未做 codes → sharedpb.Error 映射）。
[修复建议] 非 200 时先尝试解析 body 中的结构化 error 字段（如 notificationErrorResponse 模式），
  解析成功则作为 sharedpb.Error 返回、失败才回退文本；必要时映射常见状态码（429→RATE_LIMITED 等）。
[置信度] high
```

```
[级别] Minor
[位置] proxy/grpc.go:44-46
[问题] 已知线索 4 部分推翻：`credentials.NewTLS(&tls.Config{})` 不会导致"不校验 ServerName"。
[证据] grpc-go v1.79.1 credentials/tls.go:114-121：ClientHandshake 在 Config.ServerName 为空时
  从 dial authority 解析 host 作为 TLS ServerName，证书主机名校验正常执行。残余问题仅是配置缺口：
  GRPCProxyConfig 只有 Insecure 一个字段，无法像 HTTP 侧 TLSConfig 那样设置 ServerName/
  InsecureSkipVerify——endpoint 配成 IP、证书绑域名时无法覆盖验证名，也无法跳过校验。
[修复建议] 给 GRPCProxyConfig 增加 ServerName/InsecureSkipVerify 字段并传入 tls.Config。
[置信度] high（源码核实）
```

```
[级别] Minor
[位置] proxy/router.go:84-96
[问题] 已知线索 5 确认：Router.Close 只保留最后一个代理的 error，前面的失败被覆盖。
[证据] `var lastErr error; for ... { if err := rt.proxy.Close(); err != nil { lastErr = err } }`
[修复建议] 用 errors.Join 聚合全部错误。
[置信度] high
```

```
[级别] Minor
[位置] pkg/grpcstream/api_handler.go:35-59 与 client.go:956-976（另 payloadBytes api_handler.go:283-297
      vs client.go:1209-1222）
[问题] 已知线索 7 确认：Publish 的 sharedpb.Payload oneof → Publication 转换与根包 handlePublish 重复，
      两处各自处理 nil/ContentType/Kind，已有细微差异（handlePublish 未设置 ContentType 的等价物…实际
      两处都保留 ContentType，但 json 序列化失败的错误语义不同），属典型"复制粘贴漂移"风险点。
[证据] api_handler.go:35-59 与 client.go:956-976 结构逐行对应。
[修复建议] 在根包提取共享转换函数（如 PayloadToPublication(p *sharedpb.Payload, id string)）供两处调用。
[置信度] high
```

```
[级别] Minor
[位置] pkg/websocket/transport.go:56-91
[问题] Close 在 WriteControl 或 SetReadDeadline 失败时直接 return，跳过 t.conn.Close()；
      对端已 RST（如 SO_LINGER 0 断开、网络错误）时底层 fd 不被关闭，逐连接泄漏 fd。
[证据] `if err != nil { return err }` × 2 均位于 conn.Close() 之前。
[修复建议] 改为 `defer t.conn.Close()` 或所有失败路径统一关闭。
[置信度] medium
```

```
[级别] Minor
[位置] proxy/router.go:74-81
[问题] AddFromConfig 逐条 Add，第 k 条失败时前 k-1 条路由已生效，返回 error 后留下半初始化状态
      （node.SetupProxy 会中止，但 Router 已被污染，若上层忽略错误则路由不完整）。
[证据] `for _, routeCfg := range cfg.Routes { if err := r.Add(...); err != nil { return err } }`
[修复建议] 失败时回滚已添加的路由，或先全部编译再一次性提交。
[置信度] high
```

```
[级别] Minor
[位置] pkg/websocket/handler.go:39-44, 51-55
[问题] 升级失败/NewClient 失败路径在升级可能已部分写入响应后调用 rw.WriteHeader(500)，
      产生 superfluous/multiple WriteHeader 噪音；upgrade 失败后连接状态不可靠，500 也无意义。
[证据] `if err != nil { log...; rw.WriteHeader(http.StatusInternalServerError); return }`
[修复建议] 升级失败直接 return（gorilla 已写握手错误响应）；NewClient 失败路径先关闭已升级连接再返回。
[置信度] medium
```

## 已核实为"非问题"的线索

- 线索 4 详见上（grpc-go 自动派生 ServerName，仅剩配置缺口）。
- 维度 5 契约一致性核查通过：核心层 `write()` 错误 → `go c.close(DisconnectSlowConsumer)`（client.go:1099），两个传输的写超时均能触发该路径；`client.close`/`closeQuiet` 保证 transport.Close 每连接至少调用一次且核心层 status 保护避免重复，因此 gRPC closeOnce 与 WS 传输本身不幂等（WS 无 closeOnce）均未造成实际重复关闭；handler defer closeFn 保证 worker goroutine 一定收到 closeCh，未发现 goroutine 泄漏路径。

## 建议补充的测试

- **proxy**：
  - `TestHTTPProxy_RPC_PayloadRoundTrip`：用 protojson 期望格式做端到端往返，断言 `resp.Payload.GetData()` 非 nil（当前断言会被绕过）——这是首个应补的测试。
  - `TestHTTPProxy_Non200_ReturnsStructuredError`（503 + JSON body）与 `TestHTTPProxy_NetworkError`（连接拒绝/中途断开）。
  - `TestHTTPProxy_RPC_MetadataForwarded`：验证 Meta 经 HTTP 透传（当前实现会丢弃，测试会先红）。
  - `TestGRPCProxy_NotificationMethods`：OnConnected/OnSubscribed/OnUnsubscribed/OnDisconnected 走真实 gRPC 服务器（当前 mock 全部 not implemented）。
  - `TestNewGRPCProxy_TLS`：TLS 端点 + ServerName 校验、错误 ServerName 拒绝。
- **pkg/websocket**：
  - 子协议协商矩阵：offer 顺序 ["messageloop","messageloop+proto"]、未知子协议 "xproto"、无子协议，断言 marshaler 与帧类型一致（当前会红）。
  - `TestHandler_MaxMessageSize`：超过 SetReadLimit 的帧触发断连（畸形帧路径）。
  - 畸形二进制帧（随机字节作为 protobuf）→ 服务端回 BAD_REQUEST 且连接存活。
  - 压缩开关（Compression: true）下的收发与 close 握手。
  - `TestTransport_Close_WriteControlFailure`：对端 RST 后 Close 仍关闭底层连接（fd 泄漏回归）。
- **pkg/grpcstream**：
  - `sendWithTimeout` 回归：慢队列下 enqueue 逼近 deadline 不误报超时（可注入可控时延的 fake stream）。
  - `TestTransport_Close_SendChFull`：sendCh 满时 Close 的耗时与断连帧投递行为（文档化或修复后断言）。
  - admin 端口端到端鉴权：正确 token 成功、错误 token Unauthenticated（当前仅单测 interceptor，无端到端）。
  - `TestAPIServiceHandler_Survey`：admin Survey 路径（有订阅者收集应答、无订阅者空结果、超时），当前完全无覆盖。
  - `MaxRecvMsgSize` 生效验证：超限帧被拒绝。
  - 断连帧顺序：多条 WriteMany + Close 时 DISCONNECT_ERROR 必须是最后一条。

**补充说明**：`api_handler.go:111-113` 的部分成功语义与文档一致（含"会话不存在不计失败"），实现正确；未发现 Publish 语义本身的缺陷，但调用方无法获知哪条失败，属文档已声明的协议限制，不做单独 finding。

---

## 模块 04：Topic 匹配与协议层

# MessageLoop 评审报告 — Topic 匹配与协议层

## 基线结果

- `go build ./...`：通过
- `go test ./pkg/topics/...`：全部通过（14.8s，含 TestThroughput）；`go test -race ./pkg/topics/`：通过
- `go test ./shared/...`：**shared 是独立 module**，需在 `shared/` 目录下运行；该 module 无任何测试文件
- 全部基准 `-benchtime=10x`：无 panic

总体评价：5 种 matcher 在常规路径上语义基本一致，`TestThroughput` 以 naive 为参考的采样校验有效且实际有断言；cs-trie 是经典的 CAS 字典树移植，在 32 线程压测下未现丢失/挂死，结构合理。但**optimized inverted bitmap 的 `Unsubscribe` 存在已实证的残留位 bug（误投递）**，两个位图 matcher 对重复/滥用 `Unsubscribe` 会损坏内部状态；协议层 4 个 `.proto` 的 `go_package` 与真实路径不符（当前潜伏，未来生成必炸）；`shared` 的 Marshaler 错误不可区分、`Name()` 冲突且完全无测试。测试覆盖明显偏向位图实现，其余 matcher 无并发正确性断言。

---

## Findings

### [Important] optimized inverted bitmap `Unsubscribe` 未清理尾部 `empty` constituent 的索引，pos 复用后产生误匹配
[位置] `pkg/topics/optimized_inverted_bitmap.go:110-124`（关键在 113-120 行 `break` 于 `i == len(constituents)`）
[问题] 订阅 "a"（maxConstituents=3）时会在 `bitmaps[1][""]`、`bitmaps[2][""]` 填充 pos；`Unsubscribe` 只清理实际 constituent（索引 0），尾部 `empty` 位图残留 pos。单独看无碍（被 Lookup 首段的 early-exit 掩盖），但**一旦 pos 被回收给更长的订阅**，残留位会与 padding 语义叠加，产生误匹配。
[证据] 独立程序实证：`Subscribe("a") → Unsubscribe → Subscribe("b.c.d")（复用 pos 0）→ Lookup("b.c")` 返回 `[alice]`，而 "b.c.d" 不应匹配 2 段的 "b.c"。现有 `TestOptimizedInvertedBitmapMatcherPaddingSemantics`（78-99 行）恰好因 early-exit 掩盖而通过。
[修复建议] `Unsubscribe` 循环到 `maxConstituents`：对 `i < len(constituents)` 清理 `bitmaps[i][constituents[i]]`，对 `i >= len(constituents)` 清理 `bitmaps[i][empty]`。
[置信度] high（已复现）

### [Important] 位图 matcher 重复 `Unsubscribe` 同一 ID 导致 pos 别名：两个订阅共享一个 ID，订阅互相覆盖
[位置] `pkg/topics/inverted_bitmap.go:69-77`、`pkg/topics/optimized_inverted_bitmap.go:110-124`
[问题] `Unsubscribe` 无条件把 `sub.ID` append 进 `deletedPositions`，不校验 `subscribers[ID]` 是否存在。同一 Subscription 卸载两次 → 同一 pos 入队两次 → 后续两次 `Subscribe` 拿到**同一个 ID**，第二个订阅覆盖 `subscribers[pos]`，第一个订阅彻底丢失、且按位图出现在对方 topic 下。
[证据] 独立程序实证：`subA=0, subB=1`；双次 unsub(subA) 后 C、D 两个新订阅均拿到 ID 0，`Lookup("y")` 返回 `[d b]`（x 的订阅者 d 出现在 y 下），`Lookup("x")` 只剩 `[d]`。
[修复建议] Unsubscribe 前检查 `subscribers[sub.ID]` 存在再回收；或把 `deletedPositions` 改为集合语义。当前 Hub（hub.go:110-122）与 redis broker（redis.go:152-166）均用引用计数规避了该路径，但 `Matcher` 接口未约定幂等性，属公共 API 的潜伏炸弹。
[置信度] high（已复现）

### [Important] 空分段/空 topic 语义不一致：optimized matcher 拒绝其余 4 种实现接受的输入
[位置] `pkg/topics/optimized_inverted_bitmap.go:73-80`（Subscribe 拒绝）、`132-138`（Lookup 返回 nil）
[问题] 注释声称 "Reject explicit empty segments ... to stay consistent with the other matchers"，但事实相反：naive 把 `"a."` 当作字面 key、trie/cs-trie 建 `""` 分支、`""` 空 topic 四种实现都可订阅可匹配；optimized 全部拒绝并返回 `ErrBadTopic`/nil。`TestOptimizedInvertedBitmapMatcherRejectsEmptySegments`（63-76 行）只是把这种不一致固化成了测试。
[证据] trie.go:44-57 对 `"a."` 正常建链；naive.go:19-27 直接存 key；cs-trie 分支 map 允许 `""` 键；optimized_inverted_bitmap.go:74 `if constituent == empty { return nil, ErrBadTopic }`。
[修复建议] 要么四种实现统一拒绝空分段（含 trie/cs-trie 的 `""` 分支路径），要么 optimized 允许空分段（把它当普通 constituent 索引）；并修正该误导性注释。
[置信度] high

### [Important] 重复订阅语义：位图 matcher 是"多重订阅"，其余三种是"按 Subscriber 幂等"
[位置] `pkg/topics/inverted_bitmap.go:30-67`（每次 Subscribe 分配新 pos）对比 `trie.go:57`、`naive.go:24`、`cstrie.go:206-209`
[问题] 已知线索 3 声称"cs-trie 重复订阅返回 true 但不更新，与 trie/naive 语义不一致"——**该说法不成立**：cs-trie 的 fast-path（`br.subs[sub]` 存在即返回）与 trie/naive 的集合重写最终状态完全一致（Subscriber 出现一次），压测也证实行为等价。真正的不一致在位图 matcher：同一 Subscriber 订阅两次 = 两个独立 ID，`Unsubscribe` 一个后另一个仍生效；而其余三种按 Subscriber 身份整体移除。`Subscription.ID` 仅位图使用（其余恒为 0），使该差异不可见地被写入公共接口。
[证据] cstrie.go:206-209 重复路径不执行 CAS 即返回；trie.go:57 `curr.subs[sub] = struct{}{}` 幂等覆盖；inverted_bitmap.go:64 `b.subscribers[pos] = sub` 每调用分配新 pos。
[修复建议] 在 `Matcher` 接口文档中明确重复订阅语义；若要统一，位图实现需按 (topic, subscriber) 去重。
[置信度] high

### [Important] 4 个 `.proto` 的 `go_package` 缺 `/shared/`，与真实生成路径不符（当前潜伏）
[位置] `protocol/client/v1/service.proto:8`、`protocol/server/v1/api.proto:10`、`protocol/proxy/v1/proxy.proto:9`、`protocol/event/v1/events.proto:6`（均为 `.../messageloop/genproto/...`），对比 `protocol/shared/v1/*.proto:5,8`（正确带 `/shared/`）
[问题] 实际生成目录是 `shared/genproto/<pkg>/v1`（buf.gen.yaml:5 `out: shared/genproto` + `paths=source_relative`），全部 Go 代码按文件位置导入 `.../shared/genproto/...`。`go_package` 选项值被嵌入生成的 descriptor（`service.pb.go` 中可 grep 到 `messageloop/genproto/client/v1`），但不影响当前生成物位置，故 `buf generate` 不会报错、build 正常——**潜伏破坏点**：将来任何 proto 若 import client/server/proxy/event 的 proto，protoc-gen-go 会按该错误选项生成 `.../messageloop/genproto/client/v1` 的导入，而该包不存在，必然编译失败。
[证据] `shared/genproto/client/v1/service.pb.go` 同时含 `messageloop/genproto/client/v1`（descriptor）与 `messageloop/shared/genproto/shared/v1`（真实导入）。
[修复建议] 将四处 `go_package` 改为 `github.com/messageloopio/messageloop/shared/genproto/<pkg>/v1;<alias>`。
[置信度] high

### [Minor] cs-trie 的 CAS 失败重试为无界递归（栈溢出/活锁风险存在但概率极低）
[位置] `pkg/topics/cstrie.go:168-170`（Subscribe）、`231-233`（Unsubscribe）、`302-304`（Lookup）
[问题] 已知线索 2 属实（结构性）：CAS 失败即递归自调用，重试深度无上限，每层重试还会再次按 topic 深度递归 `iinsert/iremove/ilookup`。理论极端竞争下可无限加深栈。但注意三点：(a) Go GC + 节点不可变（copy-on-write）保证同一指针值不会被回收重用，**ABA 不成立**；(b) 每次重试都是新的独立竞争，连续失败呈指数衰减，需约百万次连续失败才可能溢出——32 线程 × 3 轮压测未复现任何卡死/崩溃；(c) 该设计源自经典 ctrie 移植，属继承性风险。
[证据] cstrie.go:168-169 `if !c.iinsert(root, nil, words, sub) { return c.Subscribe(topic, sub) }`。
[修复建议] 将重试改为有界循环（如 1000 次后 `runtime.Gosched()` 或返回错误），消除理论上限。
[置信度] medium（结构确认，未复现）

### [Minor] `cleanParent` 的失败重试参数错位，实际是永久 no-op，收缩不再重试
[位置] `pkg/topics/cstrie.go:421-423`
[问题] `cleanParent(i, parent, parentsParent, ...)` 在 `contract` 失败后以 `cleanParent(parentsParent, parent, i, ...)` 重试，参数旋转后 i'=祖父、parentsParent'=墓穴节点；在良构 trie 中 `pMain.cNode.branches[word].iNode != i'` 恒成立，重试立即返回。效果：CAS 失败的收缩永不被本线程重试，tombstone 只能等后续操作经 `clean`/`toCompressed` 惰性剪除。不影响正确性（ctrie 本身就是惰性清理），但若与参考实现比对，此处疑似移植偏差（原仓库已不可考，无法定论）。
[证据] cstrie.go:422 `cleanParent(parentsParent, parent, i, c, word)` — 与调用处 279 行的 `cleanParent(i, parent, parentsParent, c, ...)` 参数序相反。
[修复建议] 重试应保持 `cleanParent(i, parent, parentsParent, ...)` 原参数序；或确认与上游一致后删除该无效重试。
[置信度] medium（重试无效为确定结论；是否偏离上游无法验证）

### [Minor] `MarshalTypeError` 与 `UnmarshalTypeError` 错误字符串完全相同；`ProtoJSONMarshaler.Name()` 与 `JSONMarshaler` 冲突
[位置] `shared/marshaler.go:141-143`、`150-152`、`126-128`、`51-53`
[问题] 已知线索 7 属实。两个错误类型 `Error()` 都返回 `"message is not a proto.Message"`，`Type` 字段从不格式化，调用方无法区分是 Marshal 还是 Unmarshal 失败、也无法得知具体类型（对比 `pkg/grpcstream/codec.go:20` 用 `%T` 的同类错误）。`ProtoJSONMarshaler.Name()` 返回 `"json"` 与 `JSONMarshaler` 重复，而 `Marshalers` 列表（131-134 行）又不含 ProtoJSONMarshaler——在 `pkg/websocket/handler.go:111-119` 的 subProtocol 选择逻辑中，"json" 只能选中 JSONMarshaler，"protojson" 类协议名也会因 `strings.Contains(subProtocol, "json")` 误配到 JSONMarshaler，且 ProtoJSONMarshaler 永远只能靠 default 命中。
[证据] marshaler.go:141-143 与 150-152 逐字相同；handler.go:113-119 的 `strings.Contains(subProtocol, marshaler.Name())` 匹配机制。
[修复建议] 错误信息包含 `%T`；`ProtoJSONMarshaler.Name()` 改为 `"protojson"` 并加入 `Marshalers`。
[置信度] high

### [Minor] `naive.Unsubscribe` 的 for-range-continue 删除与 topic 空 map 残留；`topicMatches` 与 `matchCriteria` 逻辑重复
[位置] `pkg/topics/naive.go:30-43`、`70-87`；`pkg/topics/inverted_bitmap.go:103-119`
[问题] 已知线索 4 属实。`for existing := range subscribers { if existing != sub.Subscriber { continue } ... }` 完全是 `delete(n.subs[sub.Topic], sub.Subscriber)` 的绕路写法；且从不清理空的 topic key（naive 的 map 永久增长，Lookup 全扫描持续为空 topic 付出代价，trie/cs-trie 都会剪枝）。`topicMatches` 与 `matchCriteria` 参数名不同、逻辑逐行相同，属复制粘贴。
[证据] naive.go:33-40 的循环体仅一条 delete 语句；naive.go:70-87 与 inverted_bitmap.go:103-119 结构完全一致。
[修复建议] 直接 delete 并顺带删除空 map；抽一个共享的 `match(sub, topic string) bool`。
[置信度] high

### [Minor] `Error` 消息无错误码枚举，`code`/`type` 双字符串字段职责重叠
[位置] `protocol/shared/v1/errors.proto:10-15`
[问题] `code`、`type` 均为自由字符串，无 enum、无 lint 约束。AGENTS.md 规定 Disconnect 码为 3000-3512 的数值域，但线上 `Error.code` 是 string；handler.go:95 用 `Type: "client_error"` 表达错误类别——"code 该填什么"完全靠各实现自觉，客户端无法类型化 switch。
[证据] errors.proto 无 enum；`pkg/websocket/handler.go:95` 设置 `Type` 而非 `code`。
[修复建议] 定义 `enum ErrorCode`（含 Disconnect 域）并约束 `code` 字段；或删去 `type` 字段合并进 code。
[置信度] high

### [Minor] `json_name` 显式注解在 `UseProtoNames: true` 下全部失效，且与 server/proxy proto 的默认风格不一致
[位置] `protocol/client/v1/service.proto:26-29,40-48,56,61,134`；`shared/marshaler.go:88-90`
[问题] `ProtoJSONMarshaler` 开启 `UseProtoNames`，protojson 输出一律用 proto 字段名（snake_case），显式 `[json_name = "rpc_request"]` 等注解是死代码。server/proxy proto 未加注解，但因 `UseProtoNames` 全局生效，两个面最终都是 snake_case——当前行为一致，但任何一处改动 `UseProtoNames` 或误删注解都会出现跨面不一致，缺乏单一事实来源。
[证据] marshaler.go:89 `UseProtoNames: true`（文档明确：为 true 时忽略 json_name 使用字段名）。
[修复建议] 统一删除冗余 `json_name` 注解，或删除 `UseProtoNames` 并统一走 json_name。
[置信度] high

### [Minor] 测试质量：并发正确性断言仅覆盖位图 matcher；`TestThroughput` 采样空间窄
[位置] `pkg/topics/utils_test.go:19-70`（`testMatcherConcurrentSubscribe` 仅被 `inverted_bitmap_test.go:15`、`optimized_inverted_bitmap_test.go:15` 调用）
[问题] 已知线索 8 属实：cs-trie/trie/naive 只有基准（BenchmarkMultithreaded*），无任何并发断言；位图并发测试使用的 topic 恰为 `"i.i.i"`（3 段 = maxConstituents），天然避开 padding 残留路径，故未暴露本报告首个 bug。`TestThroughput` 的采样只覆盖 3 位数字+通配符空间，且只测 Subscribe+Lookup，不测 Unsubscribe（stale-bit 类 bug 无法被其捕获）。
[证据] cstrie_test.go/trie_test.go/naive_test.go 无 `testMatcherConcurrentSubscribe` 调用；throughput_test.go:40-66 无 Unsubscribe 阶段。
[置信度] high

### 已推翻的线索（附证据）
- **线索 3**（cs-trie 重复订阅语义不一致）：推翻。cs-trie 的 no-op fast-path 与 trie/naive 幂等写入终态一致（Subscriber 集合各出现一次），压测一致。真正的不一致在位图（见上）。
- **线索 5**：全部推翻。`naive_test.go:133-227` 各多线程基准均正确使用 `NewNaiveMatcher()`（无 `NewTrieMatcher` 误用）；Unsubscribe 基准每次迭代新建 Subscription（`id, _ := m.Subscribe(...); m.Unsubscribe(id)`），不存在重复卸载同一 ID、`deletedPositions` 不会无限增长；`TestThroughput` **有断言**（`throughput_test.go:95-97` 的 `t.Fatalf`）。但注意：位图实现确实存在"重复卸载同一 ID"状态损坏的真实 bug（已实证，见上），只是基准没踩到。
- **线索 1/2/4/6/7/8**：确认（分别见上）。

### 其他新发现
- `sdks/ts/src/proto/v1/service_pb.ts` 是旧布局残留物（现布局为 `client/v1/`），会造成 TS 端重复/过时类型。
- `optimized_inverted_bitmap.go:111-112`：`Subscribe` 失败（含 nil）后若调用方未检查 error 直接 `Unsubscribe(nil)` 会 nil 解引用（接口未约定）。
- `Subscriber` 空接口无 comparable 约束：`map[Subscriber]struct{}` 对 slice 等不可比较值会在运行时 panic（5 种实现同病，建议文档注明或 `Subscribe` 内 `reflect.TypeOf(sub).Comparable()` 校验）。

---

## 建议补充的测试

1. **optimized 位图 stale-empty 回归测试**：`Subscribe("a") → Unsubscribe → Subscribe("b.c.d") → 断言 Lookup("b.c")/Lookup("b")/Lookup("b.c.e") 为空`（本报告实证 bug 的精确复现）。
2. **位图重复卸载测试**：同一 `Subscription` 卸载两次后再订阅两人，断言 ID 唯一、无订阅被覆盖（扩展 `assertSubscriptionIDsUnique` 场景）。
3. **5 种 matcher 语义一致性差分测试**：扩展 `TestThroughput` 的采样空间，纳入 `""`、`"a."`、`".a"`、`"a..b"`、纯 `"*"`、重复订阅 + 单次卸载、短 topic 后接长 topic 等边界，全部以 naive 为基准比对。
4. **cs-trie/trie/naive 并发正确性**：复用 `testMatcherConcurrentSubscribe`（当前仅位图有），并增加"订阅-取消-再订阅不同 topic"的并发阶段（现并发测试只回放同 topic）。
5. **`shared/marshaler` 单元测试**（当前 zero test files）：三实现 × `Marshal/MarshalAppend/Unmarshal/Name`，含非 proto.Message 输入、`MarshalTypeError`/`UnmarshalTypeError` 可区分性、`MarshalAppend` 追加语义、三种实现的 Name 唯一性。
6. **Unsubscribe 阶段纳入吞吐/差分测试**：在 `assertLookupConsistency` 前插入"部分订阅-卸载-重订阅"阶段，可捕获 stale-bit 类状态残留（现测试全生命周期无卸载）。
7. **cs-trie 有界重试保护**：可选——对同一节点做高并发 Subscribe/Unsubscribe 对抗压测并设 watchdog（本报告压测方案），防止未来重构引入活锁。

---

## 模块 05：配置、启动与可观测性

# 评审报告 05：配置、启动与可观测性

## 基线

- `go build ./...`、`go vet ./...`、`go test ./config/... ./cmd/... .`、`go test -race ./config/... ./cmd/...` 全部通过（缓存命中，无失败）。
- 实测额外验证：`go run cmd/server/main.go --config ./config.yaml`（AGENTS.md 文档给出的启动命令）**编译失败**；`go run ./cmd/server --config config.yaml` 启动失败，报 `grpc-admin-server addr is required`。

## 总体评价

代码主体质量高：装配顺序（broker → 预绑定 gRPC → 服务注册 → node.Run）与关闭顺序（停监听 → 排空 → 集群关停）正确，`Cluster.Start` 有回滚逻辑，gRPC 预绑定失败有内部清理且有测试覆盖。但配置层存在系统性文实不符：`Validate()` 与真实启动要求脱节（默认 `config.yaml` 根本无法启动、仅配 gRPC 时会意外监听 80 端口）、`transport.websocket.read_timeout` 是"声明、校验、文档俱全但从未被消费"的死字段、心跳禁用语义三处文档与代码矛盾。可观测性（指标无标签、registry 不含 Go runtime 指标）是文档承认的有意取舍，但对生产排障构成实际缺口。全部 8 条已知线索均已核实，其中 7 条确认、1 条（端口泄漏）部分确认——泄漏场景在当前代码路径下不成立，但相关清理代码是死代码。

---

## Findings

### 1. 心跳：文档与代码矛盾，"空 = 禁用心跳"不成立
[级别] Important
[位置] `docs/developer/02-configuration.md:44,103,357` vs `node.go:82-95`
[问题] 文档三处声称 `idle_timeout` 为空则"完全禁用心跳"（"不创建 HeartbeatManager"），但代码无条件创建 `HeartbeatManager`，空字段回退 300s 默认值。按文档配置的实际效果：空闲 300s 被断开（3511），而非禁用心跳。
[证据] `node.go:85-95`：
```go
idleTimeout := DefaultHeartbeatIdleTimeout
if cfg != nil && cfg.Heartbeat.IdleTimeout != "" { ... }
node.heartbeatManager = NewHeartbeatManager(HeartbeatConfig{IdleTimeout: idleTimeout})
```
`node_test.go:42-43` 已锁定此行为（`TestNewNode_HeartbeatDefaultIdleTimeout`）。文档 `02-configuration.md:103` 却写"字段为空 = 完全禁用心跳（node.go:82-90，不创建 HeartbeatManager）"。
[修复建议] 改文档三处；补充说明唯一禁用方式为 `idle_timeout: "0s"`（`heartbeat.go:27-29` 的 `Start` 对 0 直接返回，属未文档化行为）。
[置信度] high

### 2. `transport.websocket.read_timeout` 是死配置字段（声明、校验、文档俱全，从未消费）
[级别] Important
[位置] `config/config.go:87,159`、`cmd/server/main.go:188-217`、`pkg/websocket/handler.go:70`
[问题] `Validate()` 校验该字段、文档 `02-configuration.md:150` 详细描述了"显式配置 > 心跳联动 > 60s"的优先级规则，但 `newWebSocketServer` 构造 `websocket.Options` 时**从未赋值 `ReadTimeout`**。显式配置的 `read_timeout` 完全无效；二进制路径下实际读截止时间恒为 `2 × idle_timeout`（默认 600s），`handler.go:70-71` 的显式分支不可达。
[证据] `cmd/server/main.go:189-195`：`wsOpts` 仅设置 Addr/WsPath/TLSCertFile/TLSKeyFile/Compression/WriteTimeout/CheckOrigin，无 `ReadTimeout:` 赋值；而 `handler.go:70` 的 `if h.opt.ReadTimeout > 0` 依赖该字段。
[修复建议] 在 `newWebSocketServer` 中解析 `cfg.Transport.WebSocket.ReadTimeout` 并赋值（沿用 `WriteTimeout` 的解析模式）；或删除字段并改正文档。
[置信度] high

### 3. 默认配置 `config.yaml` 无法启动 + 文档化的构建/运行命令编译失败
[级别] Important
[位置] `config.yaml`（缺 `server.grpc_admin` 段）、`AGENTS.md`（`go run cmd/server/main.go --config ./config.yaml`）、`docs/deployment.md:10`（`go build -o messageloop cmd/server/main.go`）
[问题] `prepareGRPCServers` 无条件预绑定两个 gRPC 监听器，`grpcstream.validateOptions`（`pkg/grpcstream/server.go:34-42`）要求 `server.grpc_admin.addr` 非空；仓库内四个 yaml 均未配置 `grpc_admin`。实测 `go run ./cmd/server --config config.yaml` 启动即报 `grpc-admin-server addr is required`。同时 `main.go` 引用 `runtime.go` 中的 `prepareGRPCServers`，单文件构建/运行命令（AGENTS.md、deployment.md 文档）编译失败：`main.go:65:23: undefined: prepareGRPCServers`。
[证据] `cmd/server/main.go:65-68` 无条件调用；实测输出 `{"level":"info",... "msg":"grpc-admin-server addr is required"}` 与 `undefined: prepareGRPCServers`。
[修复建议] 默认配置文件补齐 `grpc_admin.addr`（可给注释引导）；构建/运行文档改为 `go build ./cmd/server` / `go run ./cmd/server`。
[置信度] high

### 4. `Validate()` 与实际启动契约脱节：仅配 gRPC 时 WS 服务器意外监听 80 端口
[级别] Important
[位置] `config/config.go:147-150`、`cmd/server/main.go:70,189-191`、`pkg/websocket/server.go:49-51,65-67`
[问题] `Validate()` 允许"至少一个传输"，但实际启动要求两处：仅配 WS 时启动即失败（`grpc-client-server addr is required`，文档 `02-configuration.md:186-188` 已注明）；**仅配 gRPC 时反向灾难**——`newWebSocketServer` 无条件构造，`Addr` 为空时 `http.Server.ListenAndServe` 落到 `:http`（80 端口）静默监听；若 `path` 也为空则 `mux.HandleFunc("")` 在构造期直接 panic。
[证据] `pkg/websocket/server.go:51`：`mux.HandleFunc(opts.WsPath, handler.ServeHTTP)`（空 pattern panic）；`server.go:66` `Addr: s.opts.Addr` 空值传给 `http.Server`（Go 标准库空 Addr 绑定 80 端口）。WS 空 path panic 已被文档 `02-configuration.md:149` 承认，但 `Validate()` 未做任何防护。
[修复建议] `Validate()` 增加：WS addr 非空则 path 必须非空；仅配 gRPC 时显式报错或禁止构造 WS 服务器；为空 addr 拒绝而非放任 80 端口。
[置信度] high

### 5. 可观测性缺口：指标全部无标签、registry 不含 Go runtime/process 指标
[级别] Minor（文档承认的设计取舍，但对生产排障是实际缺口）
[位置] `metrics.go:26-98`、`cmd/server/main.go:39`、`docs/developer/05-observability.md:62-64,190`
[问题] 14 个指标均无 label：多节点部署无法按节点/频道拆分连接数、发布量；直方图无 label 无法按频道聚合 P99；`prometheus.NewRegistry()` 裸建 registry，`/metrics` 不暴露 `go_*`/`process_*`。文档自述"如需 GC、协程数请另行采集"。
[证据] `metrics.go:26-30`：`ConnectionsTotal` 仅 Namespace+Name，无 `ConstLabels`/`VariableLabels`；`main.go:39`：`reg := prometheus.NewRegistry()`。
[修复建议] 至少为 cluster 指标加 `node_id`/`incarnation_id` label；注册 `collectors.NewGoCollector()` 与 `NewProcessCollector()`；连接数加 `transport`（ws/grpc）label。
[置信度] high

### 6. `runNodeWithPreflight` 与 `preparedGRPCServers.Close()` 均为死代码；"端口泄漏"场景在当前代码路径下不成立
[级别] Minor
[位置] `cmd/server/runtime.go:17-39,83-90`
[问题] 线索 5 部分成立：`Close()` 定义后从未被调用（仅 `runtime_test.go` 经错误路径直接调用 `clientServer.Close()`）；`runNodeWithPreflight`/`nodeRunner` 仅被自身测试引用，`main.go:73` 直接用 `app.OnStart(node.Run)`。但**不存在实际泄漏**：`prepareGRPCServers` 之后到注册完成之间无失败路径（`newWebSocketServer`/`newAdminServer` 不返回错误），且 `grpcstream.Server` 本身是 lynx.Service，正常关停会经 `Stop` 关闭监听器（`pkg/grpcstream/server.go:144-167`）。两段死代码纯属残留。
[证据] `runtime.go:29-39` 的 `Close()` 全仓库无调用点；grep `runNodeWithPreflight` 仅命中 `runtime_test.go:25`。
[修复建议] 删除 `runNodeWithPreflight`、`nodeRunner`、`preparedGRPCServers.Close()`（或把 Close 挂进 `main.go` 的 OnStop 以保留防御语义）。
[置信度] high

### 7. `setupCluster` 双重 `NewCluster` + `SetPresenceStore` 副作用
[级别] Minor
[位置] `cmd/server/main.go:101-143`
[问题] 首次 `NewCluster(ClusterDependencies{})` 仅用于 normalize/校验并获取 `NodeID`/`IncarnationID`，随后带完整 deps 二次构造；首次实例被丢弃。`node.SetPresenceStore` 作为构造副作用埋在装配函数内，与 `setupCluster` 命名职责不符。
[证据] `main.go:102-106` 空 deps 构造；`main.go:133-138` 二次构造；`main.go:131` 副作用。
[修复建议] 拆出 `normalizeClusterOptions(cfg)` 返回 normalized options，一次构造；`SetPresenceStore` 移回 main 装配区。
[置信度] high

### 8. `ToProxyConfig` 丢失 `Timeout`，超时解析逻辑分散两处
[级别] Minor
[位置] `config/config.go:115-123`、`cmd/server/main.go:168-179`
[问题] `ToProxyConfig` 转换时未拷贝 `Timeout`（string 字段），`setupProxy` 另行解析 `p.Timeout` 后手工赋值 `pc.Timeout`。功能正确（`proxy/proxy.go:106` 有 `Timeout time.Duration`），但转换契约不完整，未来调用者易踩坑。
[证据] `config.go:116-122` 返回结构无 Timeout；`main.go:172-178` 补解析。
[修复建议] `ToProxyConfig` 内部完成解析（返回 error 已有），删除 `setupProxy` 中的重复解析。
[置信度] high

### 9. 死配置字段与失效配置项
[级别] Minor
[位置] `config/config.go:143`（`ConsumerGroup`）、`pkg/redisbroker/options.go:111-113`
[问题] 线索 7 核实：`broker.redis.consumer_group` 全仓库无读取点（仅声明+文档标注）；`stream_approximate: false` 被静默忽略（仅 true 时覆盖默认 true）。两项均已被文档 `02-configuration.md:228,230` 如实记载，属于文档化陷阱而非未发现缺陷。
[证据] grep `ConsumerGroup` 仅命中 `config.go:143` 与文档；`options.go:111-113`：`if cfg.StreamApproximate { opts.StreamApproximate = cfg.StreamApproximate }`。
[修复建议] 消费或删除 `consumer_group`；`stream_approximate` 改为显式三态或在 `Validate()` 拒绝 false 并提示。
[置信度] high

### 10. deployment.md 监听面默认值与断连码错误
[级别] Minor
[位置] `docs/deployment.md:18-23,172`
[问题] 四个监听面"默认值"表与实际不符：仅 `server.http.addr` 有回退默认（`127.0.0.1:8080`，`main.go:222-224`）；WS `:9080`、gRPC `:9090`、admin gRPC `127.0.0.1:9091` 均为**必填**，无默认值。且 `deployment.md:172` 称空闲断开返回 `DisconnectStale`，实际为 `DisconnectIdleTimeout`（3511，`heartbeat.go:54`）。
[证据] `pkg/grpcstream/server.go:35-37`（`addr is required`）；`heartbeat.go:54`（`client.close(DisconnectIdleTimeout)`）。
[修复建议] 修正文档表与断连码。
[置信度] high

### 11. 默认运行配置的安全姿态
[级别] Minor
[位置] `config.yaml:3,12,21`
[问题] 作为默认启动配置与部署文档 Dockerfile 的默认拷贝对象（`deployment.md:237`），`config.yaml` 将**无鉴权**的 `/health`/`/metrics` 绑定到 `:8080`（全接口，示例文件为 `127.0.0.1`）、提交 Redis 明文密码、使用废弃的 `check_origin: true` 放开跨域。若用户照文档直接发布镜像，指标与健康状态对外暴露。
[证据] `config.yaml:3` `addr: ":8080"`（`config-example.yaml:3` 为 `"127.0.0.1:8080"`）；`main.go:226-227` `/metrics`、`/health` 无鉴权。
[修复建议] 默认配置收敛为回环绑定；密码改用占位符；删除 `check_origin` 改用 `allowed_origins`。
[置信度] high

### 12. CI 覆盖缺口
[级别] Minor
[位置] `.github/workflows/ci.yml`、`Taskfile.yml`
[问题] 覆盖 build/vet/race-test/golangci-lint，但：无 `buf generate` 产物一致性校验（proto 变更后未重新生成会静默通过）；coverage 仅上传 artifact，无阈值与 PR 注释；golangci-lint 用 `version: latest`（不可复现）；Taskfile 有 `test`/`vet`/`lint` 任务但 CI 未复用（重复定义）。
[证据] `ci.yml:22-36` 无 buf 步骤；`ci.yml:50` `version: latest`；coverage artifact 无后续消费步骤。
[修复建议] 加 `buf breaking`/`buf generate && git diff --exit-code` 步骤；固定 golangci-lint 版本；接入 codecov 或阈值检查。
[置信度] high

### 13. broker 启动失败走 panic
[级别] Minor
[位置] `node.go:124-130`
[问题] `Node.Run` 中 broker goroutine 出错时 `panic(err)`，进程崩溃无恢复路径。Redis broker 的 `Start` 若在连接阶段失败（Ping 超时 5s），panic 发生在 goroutine 内，`Run` 已提前返回 nil，lynx 侧无法感知启动失败。
[证据] `node.go:124-130`：`go func(){ if err := n.broker.Start(...); err != nil { ...; panic(err) } }()`。
[修复建议] 将 broker 启动失败并入 `Run` 的错误返回路径（如 ready 通道携带 error），goroutine 内仅记录日志。
[置信度] medium

---

## 建议补充的测试

1. **装配路径（cmd/server）**：`newWebSocketServer` 的 origin 三态（`allow_all_origins` / 废弃 `check_origin` / `allowed_origins` 精确匹配与无 Origin 拒绝）；`newBroker` 空 type 默认 memory、未知 type 报错；`newAdminServer` 空 addr 回退 `127.0.0.1:8080`；`setupCluster` 三态（禁用 / redis 带 deps / memory backend 不带 deps）且断言 `SetPresenceStore` 副作用仅在 redis 分支发生。
2. **配置边界（config_test.go）**：`proxy[].timeout` 非法时长；`stream_approximate: false` 的忽略语义；`server.heartbeat.idle_timeout: "0s"` 合法且禁用心跳；仅配 gRPC addr（WS 为空）应被拒绝——先修 `Validate()` 再加测试。
3. **死字段回归**：`read_timeout` 生效性测试（断言 `websocket.Options.ReadTimeout` 被赋值）；`ConsumerGroup` 读取点测试。
4. **TLS 加载**：`prepareGRPCServers` 指向不存在证书文件时错误路径清理（仿 `TestPrepareGRPCServers_CleansUpClientListenerOnAdminFailure`，但走 TLS 分支）。
5. **metrics.go**：独立测试注册后 `Gather` 出的指标集合恰为 14 个且无 `go_*`/`process_*`；`NewMetrics` 重复注册同一 registry 的 panic 语义。
6. **配置-启动一致性**：逐份解析 `config.yaml`/`config-node1.yaml`/`config-node2.yaml`/`configs/test.yaml` 断言 `Validate()` 通过且 `prepareGRPCServers` 可预绑定（用临时端口改写后跑通），防止再次出现"默认配置无法启动"。
7. **`Node.Run` 失败路径**：cluster.Start 返回错误时 `Run` 传播错误；broker 启动失败不 panic（若采纳修复 13）。

---

## 模块 06：Go SDK

# 评审报告：Go SDK（`sdks/go/`）

## 基线结果与总体评价

`cd sdks/go && go build ./... && go test ./...` 全部通过（含 `go test -race ./...`，2.3s）。SDK 核心架构（transport 抽象、generation 过滤、pendingRPC 的 channel+`sync.Once`、HandlerImpl 覆盖模式）设计清晰，已针对 P0-4/P1-10/P1-11/P2-9 等历史缺陷做了专项修复并带回归测试，并发主线（transport 热切换、RPC 关闭竞态）可信。但协议对等性上有系统性缺口：ephemeral、ping/pong 超时、SubRefresh/Survey 三个协议特性完全未实现，proxy 生命周期钩子丢参数；初始 `Connect()` 的失败/重试语义有真实缺陷（泄漏、重复 receiveLoop、generation 不匹配导致挂起）。测试全部基于 `fakeTransport`，无任何真实传输集成测试，重连成功路径与会话恢复从未被验证。文档（07-sdk-go.md）对 `PingTimeout` 未实施、`handlePong` 空实现如实披露，这点值得肯定。

---

## 已知线索核实

### 线索 1：`resumed=true` 时跳过 `Connected.Subscriptions` 写回 —— 确认（行为属实，影响有限）
[级别] Important
[位置] sdks/go/client.go:310-317
[问题] `handleConnected` 在 `resumed=true` 时跳过把服务端下发的订阅列表写回本地 `subscriptions` 映射。服务端无条件返回完整列表（`Subscriptions: c.subscriptionList()`，根 client.go:684-693），SDK 却选择性忽略。
[证据] SDK 端 `if !resumed { for ... c.subscriptions[sub.GetChannel()] = true }`；服务端 `Connected{ ... Subscriptions: c.subscriptionList() }` 无任何 resumed 分支。
[修复建议] 无条件写回（`resumed` 分支也应执行写回，因为服务端列表才是权威）；或在注释中明确服务端保证列表与本地一致的前提。
[置信度] high（行为）；影响范围：单进程重连场景下本地 map 与重发列表本就一致，实际不产生错误；跨进程复用同一 session id 恢复（集群快照恢复的频道不在本进程 map 中）时本地状态与服务端静默分叉，下次重连丢失这些频道。建议按 cluster 场景补充测试。

### 线索 2：Subscribe 永远 `Ephemeral=false` —— 确认
[级别] Important
[位置] sdks/go/client.go:437-443（Subscribe）、187-191（Connect 自动订阅）、470-476（Unsubscribe）、870-918（Build* 系列）
[问题] Go SDK 无任何途径设置 ephemeral，协议字段（service.proto:74 `Subscription.ephemeral`）与服务端均支持（根 client.go:619 `NewSubscriber(c, sub.Ephemeral)`、根 client.go:1035），TS SDK 通过 `options.ephemeral` 暴露（ts/client.ts:425, 498）。
[修复建议] 增加 `WithEphemeral(bool)` 选项并透传到所有构造 `Subscription` 的位置，或提供 `SubscribeWith(channel string, ephemeral bool)` 变体。
[置信度] high

### 线索 3：Build* 消息构造函数吞掉 `ToPayload` 错误 —— 确认
[级别] Minor
[位置] sdks/go/client.go:933、947、1008
[问题] `payload, _ := msg.ToPayload()` 静默吞错。转换失败（如 `json.Number` 等 structpb 不支持的载荷，proxy_test.go:86 的 stub 正是用此触发错误）时产出 payload 为 nil 的 Publish/RPC 消息，数据静默丢失。
[证据] 注释为 "Ignore error for backward compatibility"；主路径 `Publish`/`RPC` 方法（client.go:505-508, 538-541）均正确返回错误，行为不对称。
[修复建议] 无法改签名（public API）的话，至少在文档中标注该限制，或在生成消息时记录日志。
[置信度] high

### 线索 4：有 `PingInterval` 无 `PingTimeout` 生效逻辑 —— 确认
[级别] Important
[位置] sdks/go/options.go:57-60、88、154-158（`PingTimeout` 字段、默认值 10s、setter 均存在但全仓库无任何读取点）；client.go:1074-1078（`handlePong` 空实现）；client.go:1065-1069（pingLoop Send 失败仅 `continue` 吞掉）
[问题] 半开连接（NAT 超时、防火墙丢包但 TCP 不断）下：WS 读线程阻塞在 `ReadMessage`（websocket.go:97），`SetReadDeadline` 从未被调用，无 pong 超时触发关闭 → `receiveLoop` 永不退出 → 自动重连永不触发。对比 TS SDK 完整实现（client.ts:467-471 发送后设 `pingTimeoutTimer`，超时 `handleError` + `close`；client.ts:268-274 Pong 清除计时器）。
[证据] `func (c *client) handlePong() { // Pong received - the connection is alive ... }`——全空；`PingTimeout` 仅出现在 options.go。
[修复建议] 实现 pong 超时：pingLoop 记录最后发送时间，`handlePong` 更新时间；超时后主动 `Close()` transport 触发重连。文档 07-sdk-go.md:124、231 已如实披露该缺口。
[置信度] high

### 线索 5：未实现 SubRefresh / SurveyRequest / SurveyReply —— 确认
[级别] Important
[位置] sdks/go/client.go:249-281（`handleMessage` switch 缺少 `SubRefreshAck`/`SurveyRequest`/`SurveyReply` 三个 case）
[问题] 服务端实现完整（根 client.go:346-351、1175-1195 handleSubRefresh、1197-1287 handleSurvey/handleSurveyReply），协议定义在 service.proto:142-159。Go SDK 收到 `SurveyRequest` 会被静默丢弃——客户端永远无法应答 survey，服务端 `AddSurveyResponse` 会一直等不到响应；SubRefresh（ACL 重新校验）也无从发起。
[修复建议] 至少为 `OutboundMessage_SurveyRequest` 提供回调 + `SendSurveyReply` API；`SubRefresh` 可后置。
[置信度] high

### 线索 6：`OnSubscribed/OnUnsubscribed` 只传 `ctx` —— 确认
[级别] Important
[位置] sdks/go/proxy.go:88-98（接口定义）、142-148（默认实现）、348-369（`HandlerImpl.OnSubscribed/OnUnsubscribed` 直接丢弃 `req` 字段）
[问题] proxy.proto:102-116 明确定义 `OnSubscribedRequest{session_id, channel, username}`，服务端也真实填充（根 client.go:1064-1071、1123-1131），但 SDK 层全部丢弃——后端无法知道是谁、在哪个频道、什么用户订阅/退订，生命周期钩子基本失去意义（OnConnected/OnDisconnected 有参数，唯独这两个没有）。
[修复建议] 接口改为 `OnSubscribed(ctx, sessionID, channel, username string)`，同步修改示例与测试。
[置信度] high

### 线索 7：测试缺口 —— 确认（详见文末清单）
[级别] Important
[位置] sdks/go/client_test.go:15-24（`fakeTransport`）、500 行全部测试
[证据] 测试仅覆盖：transient 转发、RPC 关闭竞态/收包/错误信封路由、重连失败清理、stale Connected 丢弃、transport 热切换竞态。无真实 WebSocket/gRPC 集成测试；无重连**成功**路径（`TestClientTransportSwapRace` 中每次 reconnect 都失败）；无 resumed 会话、offset/epoch 重建、ping/pong、SubscribeAck/UnsubscribeAck/PublishAck 处理测试。
[置信度] high

---

## 新发现问题

### 发现 A：`handleConnected` 与 `Close()` 竞态可致 `close of nil channel` panic（进程崩溃）
[级别] Critical
[位置] sdks/go/client.go:296-308
[问题] `handleConnected` 在 `c.mu` 之外持有 `ch`，随后 `select { case <-ch: default: close(ch) }`。`Close()`（client.go:837-838）在置 nil 后若此时收到 Connected（已从 socket 读出的在途消息），`ch == nil` → default 分支执行 `close(nil)` → panic，直接崩溃整个进程。generation 检查（client.go:288-290）无法拦截：`Close()` 不推进 generation，同代消息仍会进入。
[证据] `c.mu.Unlock()` 后 `select { case <-ch: default: close(ch) }`，而 `Close()` 中 `c.connectedCh = nil`。
[修复建议] 加 nil 判断：`if ch != nil { ... }`；或在 Close 后置一个已关闭的哨兵 channel 而非 nil。
[置信度] high（代码路径确定；窗口小，需并发触发）

### 发现 B：`Connect()` 失败/重试路径不清理旧状态
[级别] Important
[位置] sdks/go/client.go:198-215
[问题] (1) 发送 Connect 失败、超时、ctx 取消三条路径都直接 return，不关闭 transport、不停止已启动的 receiveLoop（client.go:203 `go c.receiveLoop(trans, 0)`），依赖调用方自觉 Close；超时后服务端可能稍后返回 Connected，客户端变成"僵尸连接"（回调照常触发）。(2) 重试 `Connect()` 复用同一 transport（client.go:195-197 读 `c.transport`），对同一条连接发送第二个 Connect 信封（服务端根 client.go:378-380 视为 BadRequest），且第二个 receiveLoop 与第一个同为 generation 0——两个 gen-0 循环并存，重复投递消息。对比 `reconnect()` 每个失败分支都正确 `trans.Close()`（client.go:797-801）。
[修复建议] Connect 失败时统一关闭 transport 并终止对应 receiveLoop；重试前推进 generation 并用新 transport。
[置信度] high

### 发现 C：自动重连之后手动调用 `Connect()` 必然挂起
[级别] Important
[位置] sdgs/go/client.go:203 vs client.go:724；拦截点 client.go:288-290
[问题] `Connect()` 固定以 gen=0 启动 receiveLoop，而 `reconnect()` 每次 `c.generation.Add(1)`（client.go:724）。一旦发生过任意一次自动重连（generation > 0），后续手动 `Connect()` 收到的 Connected 因 `c.generation.Load() != gen` 被永久丢弃，阻塞至 30s 超时返回 "connection timeout"。`ReconnectMaxAttempts` 用尽后用户自然的补救动作（重新 Connect）恰好必然失败。
[修复建议] `Connect()` 也应 `gen := c.generation.Add(1)` 并用新 transport 重拨；或文档明确禁止重连后手动 Connect。
[置信度] high

### 发现 D：回调 handler 字段存在数据竞争
[级别] Minor
[位置] sdgs/go/client.go:79-83（字段）、609-631（setter 无锁）、330/375/415-417（receiveLoop 路径无锁读）
[问题] `OnMessage/OnError/OnConnected` 等 setter 无锁写，`handlePublication/handleError/handleConnected` 无锁读。连接期间从用户 goroutine 改回调（如中途换 OnError）即触发 race（`-race` 下可检出；现有测试未覆盖并发改回调）。
[修复建议] setter 与读取处加 `c.mu`（或独立 handlerMu）。
[置信度] high（竞态存在）；medium（实际影响，amd64 指针读写通常不撕裂）

### 发现 E：`HandlerImpl.RPC/Authenticate` 对自定义 handler 返回 `(nil, nil)` 无防护，直接 panic
[级别] Important
[位置] sdks/go/proxy.go:246、296-298
[问题] `resp.Error` 在 `resp == nil` 时解引用 panic。gRPC handler 内 panic 无 recover 时**整个 proxy 进程崩溃**（gRPC 不自动恢复 handler panic）。`Authenticate` 同理（`resp.UserInfo.ToProto()`）。示例里的 recoveryMiddleware 只保护了 RPCMux 链路，绕不开 HandlerImpl 这层。
[修复建议] 对 `resp == nil` 显式返回 `status.Error(codes.Internal, "handler returned nil response")`。
[置信度] high

### 发现 F：`Unsubscribe` 不清除 `channelOffsets`
[级别] Minor
[位置] sdks/go/client.go:356-362（handleUnsubscribeAck 只删 subscriptions）、93-94（channelOffsets 无删除点）
[问题] 退订再订阅同一频道后，下次重连携带旧 offset + `Recover: true`（client.go:748-763 遍历所有已订阅频道），服务端按 `sub.Offset+1`（根 client.go:642）恢复——用户会收到退订期间的历史消息（重复投递）。
[修复建议] 退订时同步删除 `channelOffsets[ch]`。
[置信度] high

### 发现 G：`RPC` 无默认超时，连接死亡时挂起 RPC 永不返回
[级别] Minor
[位置] sdks/go/client.go:575-605
[问题] 请求发出后连接断开（AutoReconnect 关闭时），pendingRPC 无人裁决，只能等调用方 ctx。TS SDK 有默认 `rpcTimeout: 30000`（ts/client.ts:762, 609）。连接存活时服务端 RPC_TIMEOUT（根 client.go:813-829）会兜底，故影响面有限；示例代码用 `context.Background()` 调用 RPC（example/basicgrpc/main.go:68），恰好是教科书式的挂起写法。
[修复建议] 增加 `WithRPCTimeout` 选项，默认对 RPC 施加超时 ctx。
[置信度] high

### 发现 H：`ReceivedMessage` 是死代码，收消息 API 与 TS SDK 语义不一致
[级别] Minor
[位置] sdgs/go/message.go:315-325（定义）、327-345（`wrapPublicationToMessages` 返回 `[]*Message`，channel/offset 塞进 metadata）
[问题] `ReceivedMessage{ID, Channel, Offset, Message}` 全仓库无引用；TS SDK 回调是 `ReceivedMessage[]`（ts/client.ts:42, 225-230）。文档 07-sdk-go.md:201 已如实说明。
[修复建议] 按 TS 对齐：`OnMessage(func([]*ReceivedMessage))`，或删除死代码。
[置信度] high

### 发现 I：`NewProxyServer` 的 `Insecure=false`（零值）与服务端拨号方式不匹配
[级别] Minor
[位置] sdks/go/proxy.go:397-409（不设 creds → 明文服务端）；proxy/grpc.go:41-46（服务端 Insecure=false 时用系统 CA TLS 拨号）
[问题] 零值 `ProxyServerOptions{Addr}` 得到明文监听，而 MessageLoop 服务端侧 Insecure=false 走 TLS → 握手必然失败，代理全部 RPC 在运行时才暴露。文档说 "Insecure 为 true 时使用明文凭据（开发默认）"，但 Go 零值恰恰是 false。
[修复建议] 默认 Insecure=true 或在 Insecure=false 且未配置 TLS 时启动即报错。
[置信度] medium

### 发现 J：协议字段 token/metadata 未暴露；`PublishAck` 被忽略
[级别] Minor
[位置] sdks/go/client.go:510-519（Publish 不设 `token`/`metadata`，协议 service.proto:102-104 支持）、445-452（Subscribe 不设 `Subscription.Token`）、274-276（PublishAck case 为空）
[问题] 需要频道级 token（如私密频道的发布/订阅凭证）的场景无法用 Go SDK 表达；Publish 无 offset 反馈。TS SDK 同样忽略 PublishAck，token 也未暴露——与 TS 对等，但相对协议仍是缺口。
[修复建议] 在 `Message.Metadata` 已存在的前提下补 Publish.Metadata 透传；token 可通过 Option 或显式 API 提供。
[置信度] high

---

## 建议补充的测试

1. **真实传输集成测试**：起本地 `httptest` + gorilla/websocket echo 服务（或直接用根包 `pkg/grpcstream` 的服务端组件）跑通 Dial→Connect→Subscribe→Publish→收消息闭环，JSON 与 Protobuf 两种编码各一条。
2. **重连成功路径**：fakeTransport 注入"第一次 Recv 失败、第二次 Connected"，断言 `OnReconnecting`/`OnReconnected` 顺序、`sessionID` 延续、`connected` 标志位翻转。
3. **会话恢复**：断言 reconnect 发出的 Connect 携带 `SessionId`/`Epoch`，每个已订阅频道 `Recover=true` + 正确 offset；resumed=true 的 Connected 后本地 subscriptions/offset 与服务端列表一致（覆盖线索 1）；epoch 变化（服务端重启）时 offset 失效重发（`Epoch` 旧值 → 期望 from-beginning）。
4. **ping/pong**：发送 Ping 后收到 Pong 的闭环；实现 pong 超时后补超时触发重连的测试（当前无实现，先写会失败的测试驱动实现）。
5. **Subscribe/Unsubscribe 完整流程**：SubscribeAck/UnsubscribeAck 到达前后 `subscriptions` 映射与重连重发列表的一致性；订阅部分被 ACL 拒绝时本地状态只含成功项。
6. **PublishAck 处理**：发布后收到带 offset 的 PublishAck，验证事件可观测（至少在测试中验证消息被正确消费、不 panic）。
7. **竞态补测**：连接建立瞬间并发 `Close()`（复现发现 A 的 close(nil) panic）；连接中并发调用 `OnError`（复现发现 D）；`Connect()` 失败后立即重试（复现发现 B 的双 receiveLoop）。
8. **proxy 防护**：自定义 handler 返回 `(nil, nil)` 时 `HandlerImpl.RPC/Authenticate` 不 panic 的测试。
9. **协议未覆盖项**：收到 `SurveyRequest`/`SubRefreshAck` 信封不崩溃（当前静默丢弃），为后续实现留回归基线。

---

## 模块 07：TypeScript SDK

# TypeScript SDK 评审报告（任务 07）

## 基线测试结果与总体评价

`cd sdks/ts && npm test`：**2 个测试套件、30 个用例全部通过**（仅覆盖 `client/options` 构造与 codec 编解码）；`npm run build`（ESM + CJS + types）编译通过；`npm run lint` **不可用**（eslint 未声明为 devDependency，命令报 `'eslint' is not recognized`，且无配置文件）。

总体评价：SDK 结构清晰、分层合理（client/transport/codec/message），JSON codec 与协议 wire format 对齐较好（golden 测试正确），但**断线检测与错误路由存在系统性缺陷**——传输层的 close/error 事件无法可靠驱动客户端状态机，错误信封被当作连接级错误触发整条连接重连（与 Go SDK 行为分歧）；浏览器下 protobuf 编码完全不可用（`binaryType` 未设置）；`Connected.publications`（离线恢复消息）被静默丢弃。6 条已知线索中 5 条确认属实、1 条（SubRefresh/Survey）需修正措辞——服务端已实现该协议，Go/TS 两个 SDK 均未对齐。测试覆盖与代码量严重不成比例，`client.ts`（776 行）几乎无测试。

---

## Findings

```
[级别] Critical
[位置] src/client/client.ts:468-471
[问题] pong 超时路径调用 close() 导致客户端永久关闭，自动重连永远不会发生；一次网络抖动（ping 发出但 pong 丢失）即杀死客户端。
[证据] sendPing 中：
  this.pingTimeoutTimer = setTimeout(() => {
    this.handleError(new Error("Pong timeout"));
    this.close();        // close() 置 isClosedFlag = true
  }, this.options.pingTimeout);
  handleError 先调度了重连（attemptReconnect），随后 close() 将 isReconnecting 置 false 并 clearTimeout(reconnectTimer)，重连被取消。而 close() 之后 handleError 的重连条件 `!this.isClosedFlag` 也永久不满足。
[修复建议] pong 超时应走 handleDisconnect()（保留 isClosedFlag=false，进入重连流程），而不是 close()。对比：Go SDK 完全没有 pong 超时（handlePong 为空实现），TS 是唯一实现方且行为破坏性。
[置信度] high
```

```
[级别] Critical
[位置] src/transport/websocket.ts:34-43（构造器未设置 binaryType）
[问题] 浏览器下 protobuf 编码完全不可用：浏览器原生 WebSocket 二进制帧默认 `binaryType = "blob"`，onmessage 收到的是 Blob，codec.decode 只接受 Uint8Array|string，`new TextDecoder().decode(blob)` 抛 TypeError。
[证据] 构造器只注册 onmessage/onerror/onclose/onopen，从未设置 `socket.binaryType = "arraybuffer"`；json.ts:37 与 protobuf.ts:23 均只处理 Uint8Array|string。Node 侧 ws 默认 'nodebuffer' 恰好可用，掩盖了该问题。codec.useBytes() 返回 true 但 transport 从未依据它设置 binaryType。
[修复建议] 在 dial 成功后设置 binaryType（浏览器环境置 "arraybuffer"，ws 保持 'nodebuffer'）；decode 增加 Blob 分支（await blob.arrayBuffer()）。
[置信度] high
```

```
[级别] Critical
[位置] src/transport/websocket.ts:172-214（recv 实现）
[问题] 传输层 close/error 事件无法可靠通知客户端，断线检测依赖 30s 后的 ping 失败（ping 被禁用时永不检测）；onerror 通过"产出 null 消息→parseOutboundMessage 崩溃→TypeError"的意外路径传播，真实错误信息丢失且时序相关。
[证据] ① onclose 只置 _connected=false 并触发 closeListeners，从不 resolve 挂起的 Promise resolver → recv() 在 `yield await new Promise(...)` 处永久挂起，finally 清理不执行，监听器泄漏；客户端 receive() 的 for-await 永不退出，不抛错。② errorHandler 将 resolver 解析为 `{done:true, value:null}`，但生成器忽略 done 标记，直接把 null 当作消息 yield 出去；handleMessage(null) 在 parseOutboundMessage 中访问 msg.envelope 抛 TypeError 才被 receive() 的 catch 捕获 → 错误传播是靠崩溃偶然实现的，且当生成器不在 await 挂起点（正在消费队列）时 resolver 为 null，错误被完全吞掉。③ 客户端从未调用 transport.onClose，MessageLoopClient 对 onclose 一无所知。
[修复建议] recv() 改为：close 时以 throw/reject 结束迭代器（向客户端抛"连接已关闭"错误）；errorHandler 检查 result.done 并 throw 真实错误；onclose 也解析挂起 resolver。
[置信度] high
```

```
[级别] Critical
[位置] src/client/client.ts:276-303、189-214；服务端 client.go:821-857（RPC 错误以 Error 信封返回）
[问题] 协议级错误信封处理与 Go SDK 分歧，且 RPC 错误不路由到 pending 请求：
  ① 服务端 RPC 失败（RPC_TIMEOUT/PROXY_ERROR/ACL 拒绝/限流）以 OutboundMessage_Error 返回且 Id=请求 Id（MakeOutboundMessage client.go:737 复制 in.Id）。Go SDK deliverPending 会把该错误快速投递到对应 RPC（sdks/go/client.go:253-260）；TS 只处理 rpcReply.error，pending RPC 只能干等 rpcTimeout（30s）。
  ② TS 对任何 error 信封在 connected 状态都执行 handleError → handleDisconnect → 重连。Go SDK 仅 `isConnError = !connected` 时才重连。因此一次"发布被 ACL 拒绝"或"RPC 超时"（服务端错误信封）就会导致 TS 客户端断开整条连接并重连。
[证据] client.ts:277-281：case "error" 直接 this.handleError(error)；handleError（client.ts:295-302）在 connectionState==="connected" 时无条件 handleDisconnect。对比 sdks/go/client.go:258-260：错误信封仅在 !connected 时通知 connectErrCh，已连接时只调 errorHandler。
[修复建议] error 信封处理：先按 id 匹配 pendingRPC 并 reject；无匹配且已连接时仅调 errorHandler，不触发重连。
[置信度] high
```

```
[级别] Critical
[位置] src/client/client.ts:189-214（"connected" 分支）
[问题] Connected.publications（服务端离线恢复消息）被静默丢弃，且 channelOffsets 未按恢复消息更新；结合重连时以旧 offset+1 恢复（服务端 client.go:642 sinceOffset=sub.Offset+1），后续重连会**永久跳过**错过的消息。Go SDK 在 handleConnected 中明确消费 publications 并更新 offset（sdks/go/client.go:319-333）。
[证据] 服务端在 Connect 恢复时把历史消息放进 Connected.Publications（client.go:684-694）；TS "connected" 分支只读 sessionId/epoch/resumed，parsed.data.publications 从未被访问。parseOutboundMessage 返回的 data 里 publications 字段明明存在。
[修复建议] "connected" 分支遍历 parsed.data.publications：投递消息给 handlers 并更新 channelOffsets（与 Go SDK 对齐）；同时补集成测试验证恢复链路。
[置信度] high
```

```
[级别] Important
[位置] src/client/client.ts:379-415（reconnect）
[问题] reconnect() 发送 Connect 后不等待 Connected（dial() 有 waitForConnection，reconnect() 没有）：若新连接握手成功但服务端不回复 Connected（认证挂起/网络半开），客户端永久卡在 "connecting" 状态，无超时、无重试。Go SDK 的 reconnect 用 connectedCh/connectErrCh + connectionTimeout 等待（sdks/go/client.go:787-802）。
[证据] reconnect() 结尾仅 `await this.connect();` 即返回；catch 只在 dial/send 抛错时调度下一轮 attemptReconnect。且 dial() 失败路径（client.ts:124-131）在抛异常前就 attemptReconnect()——用户拿到异常后，无引用的幽灵客户端仍会在后台无限重连并占用会话。
[修复建议] reconnect() 复用 waitForConnection 的等待逻辑（含超时与失败重试）；dial 失败时若 autoReconnect 开启可重连但应由调用方持有客户端（或直接不重连、仅抛错）。
[置信度] high
```

```
[级别] Important
[位置] src/client/types.ts:26（线索 2，确认）
[问题] IClient.publish 签名缺 transient 参数，与类实现 `publish(channel, msg, transient = false)` 不一致；MessageLoopClient 未声明 implements IClient，接口漂移无法被编译器发现。文档 08-sdk-ts.md 亦称"实现了 IClient 接口"，属双重误导。
[证据] types.ts:26 `publish(channel: string, msg: Message): Promise<void>`；client.ts:586-590 带第三个参数。测试 client.test.ts:29 专门验证了 transient 穿透，但类型接口未同步。
[修复建议] 接口补 `transient?: boolean`，并让 MessageLoopClient implements IClient 以强制一致。
[置信度] high
```

```
[级别] Important
[位置] src/client/options.ts:178-183（线索 3，确认）
[问题] setReconnectDelay 只设 initial/max，无 multiplier setter；ClientOptions.reconnectBackoffMultiplier 只能通过 buildClientOptions 手工构造。Go SDK 的 WithReconnectBackoff(initial, max, factor) 三参数齐全（sdks/go/options.go:168-174），TS 是 API 倒退。文档 08-sdk-ts.md:218 承认了该缺口，但仍属公开 API 不完整。
[证据] options.ts:178 `setReconnectDelay(initial: number, max: number)`；client.ts:768 有默认 multiplier=2。
[修复建议] 新增 setReconnectBackoff(initial, max, multiplier) 或给 setReconnectDelay 加第三参数。
[置信度] high
```

```
[级别] Important
[位置] src/transport/websocket.ts:216-225（线索 4，确认）
[问题] close() 用固定 `setTimeout(resolve, 100)` 等待关闭：不等待真实 close 事件，慢网络上 socket 可能仍未关闭就返回；且未 flush sendQueue 中挂起的 send Promise（close 后队列项永不 settle，调用方 await publish 永久挂起）。
[证据] websocket.ts:218-224：无 close 事件监听，纯 100ms 定时。对比 Go 侧 ws transport Close 会真正等待。
[修复建议] 用 Promise + onclose 事件驱动（附超时兜底），并拒绝/清空 sendQueue 中剩余项。
[置信度] high
```

```
[级别] Important
[位置] src/transport/websocket.ts:144-162（processSendQueue）
[问题] 发送队列递归实现且无背压：processSendQueue 递归调用自身，突发大批量 publish（数万条）时同步递归深度等于队列长度，存在栈溢出风险；队列无上限，背压缺失（bufferedAmount 未参考）。事件监听器数组（messageListeners/errorListeners/closeListeners）只增不减（onMessage/onError 无 remove 方法），recv() 每次调用注册的监听器在 close 挂死路径下不清理，多次重连后监听器与旧 transport 累积。
[证据] websocket.ts:160-161 `this.isSending = false; this.processSendQueue();` 直接递归；send() 无队列长度检查。
[修复建议] 改为 while 循环 + async 微任务让出；增加 maxQueueSize 拒绝/丢弃策略；提供 removeListener API。
[置信度] medium
```

```
[级别] Important
[位置] src/transport/websocket.ts:101-107、src/client/options.ts（默认值）
[问题] ① Node.js 运行期依赖 ws 但 ws 仅在 devDependencies：engines 声明 node>=18，而全局 WebSocket 是 Node 21+ 才稳定可用，Node 18/20 消费者必然命中 `await import("ws")` 且 module not found；浏览器打包器（Vite/webpack）解析该动态 import 时也会因 ws 未安装而报错。② headers 选项是死代码：ws 的 headers 需在构造函数 options 中传入，运行时给 socket.additionalHeaders 赋值无效。③ 默认值两处漂移：autoReconnect 默认 true（Go SDK 默认 false）、connectTimeout 30s（Go DialTimeout 10s）。④ @grpc/grpc-js peer 依赖（线索 1，确认）：无任何 gRPC 传输实现，npm 7+ 仍会自动为消费者安装无用包，应删除或加 peerDependenciesMeta 标记 optional。
[证据] package.json:42-44 peerDependencies；:45-54 devDependencies 含 ws；websocket.ts:91 动态 import("ws")；docs 08-sdk-ts.md:33 自认"Node 18-20 需自行安装 ws"。
[修复建议] ws 移入 dependencies（或 peerDependencies+meta optional 并在缺失时报可读错误）；headers 改为通过构造参数透传；对齐默认值；移除 @grpc/grpc-js。
[置信度] high
```

```
[级别] Important
[位置] src/client/client.ts:160-180（waitForConnection）
[问题] 认证失败路径 UX 极差：服务端对非法 token 先发 Error 信封再断开（client.go:403-412），但 connecting 状态下 error 信封既不 reject waitForConnection、也不 fail dial；onclose 又不驱动 recv 报错 → dial 只能等 connectTimeout（默认 30s）后报笼统的 "Connection timeout"，真实原因（AUTH_REQUIRED/InvalidToken）被吞。Go SDK 通过 connectErrCh 快速失败。
[证据] waitForConnection 仅轮询 isConnectedFlag/isClosedFlag，30s 超时；error 信封在 connecting 状态下只调 errorHandler（无人监听）即返回。
[修复建议] connecting 阶段收到的 error 信封应 reject waitForConnection（携带 code/type），并透传真实错误；或对 onclose 事件在未连接完成时直接 reject。
[置信度] high
```

```
[级别] Important
[位置] src/client/client.ts:500-503；sdks/go/client.go:748-765
[问题] 会话恢复条件分歧：TS 仅在 `isReconnecting && epoch !== ""` 时设 recover:true，Go SDK 重连恒定 Recover:true + Epoch。服务端对无 epoch 的恢复请求按"从最早开始恢复"处理（client.go:643-649），TS 在服务端未启用 epoch（如内存 broker 无 Epoch 实现）时直接不恢复，行为与 Go SDK 不一致。
[证据] client.ts:500 `recover: this.isReconnecting && this.epoch !== ""`；sdks/go/client.go:753 `Recover: true`。
[修复建议] 与 Go 对齐：重连恒为 recover:true（epoch 为空时服务端自会兜底）。
[置信度] medium
```

```
[级别] Important
[位置] src/transport/websocket.ts:26（线索 5，确认，但影响低）
[问题] `(OutboundMessageSchema as any).fromBinary` 与 `(msg as any).toBinary()` 绕过类型：@bufbuild/protobuf v2 的 GenMessage 类型本就在 schema 上提供 fromBinary、在消息实例上提供 toBinary（`Message<Desc>` 的 `toBinary` 与 `Desc` 的 `fromBinary` 是官方 API），类型断言纯属多余，且掩盖了真正的类型错误（如跨 schema 调用）。
[证据] protobuf.ts:16-19、26。
[修复建议] 去掉 any，直接用 OutboundMessageSchema.fromBinary(bytes)（类型上需要将 schema 作为 MessageDesc 传入）。
[置信度] high
```

```
[级别] Minor
[位置] src/client/client.ts:188-214、src/message/converters.ts:191-202（线索 6，需修正措辞）
[问题] SubRefresh/Survey 并非"协议整体未对齐"：服务端已完整实现 handleSubRefresh（client.go:1175）与 handleSurvey/handleSurveyReply（client.go:1197-1281），且 cluster 命令 Survey 依赖客户端回复。TS SDK 有 createSubRefreshMessage（导出但 MessageLoopClient 从不使用、也不处理 subRefreshAck），Go SDK 两者皆无。双方 SDK 均无法参与 cluster survey、无法被服务端刷新订阅 token——属 SDK 缺失的协议能力而非协议未实现。
[证据] client.go:346-351 分发 InboundMessage_SubRefresh/SurveyRequest/SurveyReply；sdks/go 无任何 SubRefresh/survey 引用；client.ts 的 handleMessage switch 对 subscribeAck/unsubscribeAck/publishAck/subRefreshAck/surveyRequest/surveyReply 全部无 case。
[修复建议] 至少补齐 SubRefresh（SDK 已有构造器）与 surveyRequest 的应答回调，或在文档中明确为不支持项及影响（cluster survey 不可用）。
[置信度] high
```

```
[级别] Minor
[位置] src/client/client.ts:41-51、src/client/types.ts:21-58
[问题] 双套 handler API（legacy 单处理器 + Set 多处理器）并存导致语义不一致：onMessage 覆盖、addMessageHandler 追加，两者同时触发；消息优先投 legacy 再投 Set。IClient 接口同时暴露两套，且与类实现、文档表格（08-sdk-ts.md:165-175）三方不完全对齐。
[证据] client.ts:234-246 publication 分支先调 messageHandler 再遍历 messageHandlers。
[修复建议] 统一为单套 API（保留 onXxx 为 addXxx 的别名），避免重复投递。
[置信度] medium
```

```
[级别] Minor
[位置] src/client/client.ts:520-554、379-415
[问题] close() 与进行中的 reconnect() 存在竞态：close() 置 isClosedFlag 并 await transport.close()，但 reconnect() 在 `await WebSocketTransport.dial(...)` 之后不回查 isClosedFlag，会重新赋值 this.transport、startMessageLoop 并向已关闭的客户端发 Connect——"已关闭"的客户端被复活。另外 close() 期间 reconnectTimer 已清、reconnect() 的 catch 分支还会再 attemptReconnect（此时 isClosedFlag 已 true，会被拦截，行为恰好正确但不明显）。
[证据] client.ts:399-407 reconnect 在 await 后无 isClosedFlag 检查。
[修复建议] dial 完成后检查 isClosedFlag，若已关闭则立即关闭新 transport 并 return。
[置信度] medium
```

```
[级别] Minor
[位置] package.json:28、sdks/ts 无 eslint 配置
[问题] `npm run lint` 脚本指向未安装的 eslint（devDependencies 无 eslint、无 .eslintrc），README/docs（08-sdk-ts.md:331）宣称可用 lint 命令实测即崩。另 dist/ 为 gitignored 残留物：本次 build 前 dist/cjs/event/event.js、converters.js 等文件存在但 src/ 已无 event/ 模块（旧构建产物），本地易误导；docs 08-sdk-ts.md:5 声称版本 1.0.5，package.json 已是 1.1.0。
[证据] `npm run lint` → "'eslint' is not recognized"；build 前 dist 存在 src 不存在的 event/ 目录。
[修复建议] 补 eslint+config 或删除 lint 脚本；发布任务用 rimraf 已能覆盖（确认 task release-sdk-ts 先清理）；同步文档版本号。
[置信度] high
```

---

## 建议补充的测试

1. **真实 WebSocket 集成测试**：起 `ws` 服务端（或对接仓库内 Go 测试服务），验证 dial→Connected 全链路、JSON/proto 双编码收发；这是当前最大空白（全部现有测试无真实 socket）。
2. **浏览器 protobuf 回归**：模拟 `binaryType="blob"` 路径（传入 Blob 给 decode），断言当前缺陷并防止回退。
3. **断线检测与重连**：服务端主动 close（无 error 信封）→ 验证 recv 终止、ping 失败路径触发 handleDisconnect；onclose 挂起 resolver 的回归测试。
4. **pong 超时行为**：不回复 pong → 断言走重连而非 close() 永久关闭（修复后）。
5. **RPC 错误路由**：服务端返回 Error 信封（携带请求 id）→ 断言 pending RPC 快速 reject 且**连接不重连**；rpcReply.error、RPC 超时两条路径。
6. **error 信封处理**：connected 状态下收到 ACL/限流错误 → 仅 errorHandler、不断开。
7. **会话恢复**：Connected.publications 恢复消息被投递 + channelOffsets 更新；epoch 不匹配时 offset 重置。
8. **重连状态机**：reconnect() 后无 Connected 回复 → 超时重试而非卡死；close() 与 in-flight reconnect 竞态。
9. **state change / multi-handler**：addStateChangeHandler 事件序列（disconnected→connecting→connected→reconnecting…）、handler 移除函数生效、legacy+Set 双投递语义。
10. **protobuf 端到端 round-trip**：`encode → ws → decode` 全类型载荷（json/binary/text）Golden 对比；sendQueue 背压与 close 时队列清理。
11. **选项与默认值**：setReconnectBackoff（新增后）与 Go SDK 参数等价性、autoReconnect 默认值一致性测试。

---

## 模块 08：跨模块一致性与文档

# MessageLoop 跨模块一致性与文档评审报告

## 总体评价

文档体系整体质量较高：`docs/developer/01~08` 与代码的吻合度明显优于旧文档（`protocol.md`/`deployment.md`），断连码、配置默认值、集群组件描述大多有源码行号支撑且正确。但存在三类系统性问题：① 若干"实现演进后文档未跟上"的陈旧论断（Redis History 的 exclusive 语义、heartbeat 可禁用、Redis epoch 按实例随机），恰好集中在最容易被 SDK/客户端依赖的语义上；② 一条**文档承诺但代码未实现**的语义（ephemeral 订阅不登记 presence），且两个 SDK 的 ephemeral 支持不对称；③ 仓库卫生与生命周期管理问题（`fix-plan.md`/`RPC_TIMEOUT.md` 已完成却未归档、`server.exe`/`nul` 残留、TS SDK 过时生成文件与版本号陈旧）。无编译/测试基线问题。

## 1. 协议特性覆盖矩阵

特性列取自 `protocol/client/v1/service.proto`；"服务端"指根包 `client.go` + `pkg/grpcstream`。

| 特性 | 服务端 | Go SDK (`sdks/go`) | TS SDK (`sdks/ts`) |
| --- | --- | --- | --- |
| Connect | ✅ | ✅ | ✅ |
| Session resume (`session_id`) | ✅ | ✅（仅重连路径） | ✅（仅重连路径） |
| Subscribe | ✅ | ✅ | ✅ |
| Unsubscribe | ✅ | ✅ | ✅ |
| Publish | ✅ | ✅ | ✅ |
| Publish transient | ✅ | ✅ `Publish(ch,msg,true)` | ✅ `publish(ch,msg,true)` |
| Subscription.offset/recover/epoch | ✅（`client.go:637-650`，offset+1 续读 + epoch 校验） | ✅（仅重连时发送） | ✅（仅重连时发送） |
| Subscription.ephemeral | ⚠️ 接受字段，但 presence 语义未实现（见 F1） | ❌ 恒写 `Ephemeral:false`（`sdks/go/client.go:189,441,474,872,897,916`） | ⚠️ 仅全局 `setEphemeral`（`client.ts:425,498,560`），非逐订阅 |
| Subscription.token | ✅（`client.go:702-706` 透传代理 ACL） | ❌ | ❌ |
| RPC request/reply | ✅ | ✅ | ✅ |
| SubRefresh | ✅（`client.go:1175-1195`，ACL 复核+撤销） | ❌ 无任何 API | ⚠️ 仅导出 `createSubRefreshMessage` 信封构造器（`converters.ts:191`），客户端无方法 |
| Survey request/reply | ✅（echo 应答 + registry，`client.go:1199-1287`） | ❌ | ❌（`parseOutboundMessage` 能识别，客户端忽略） |
| Ping/Pong | ✅ | ✅ | ✅ |
| SubscribeAck/UnsubscribeAck/PublishAck/SubRefreshAck 消费 | ✅ 发送 | ✅ 处理前三种（PublishAck 忽略） | ⚠️ 解析出类型但 `handleMessage` 不处理（乐观跟踪订阅集合） |

**不对等格子的结论**：Go SDK 是唯一完全没有 ephemeral/SubRefresh/Survey 的层；TS SDK 有 ephemeral 但无逐订阅粒度、无 Survey；两个 SDK 都没有订阅级 token；服务端侧 ephemeral 的 presence 语义（F1）与文档不符。

## 2. Findings

```
[级别] Critical
[位置] docs/protocol.md:365、docs/developer/03-admin-api.md:277；代码：client.go:626-633（connect 订阅）、client.go:1050-1056（handleSubscribe）、cluster_resume.go:143
[问题] 文档承诺"ephemeral 订阅不登记在线状态"，实现无条件登记 presence 并发布 join/leave 事件。protocol.md 表（:166）也写明 ephemeral "not tracked for presence"，但 presence.Add 前没有任何 ephemeral 判断。
[证据] client.go:1050-1056 仅判 !alreadySubscribed 即 presence.Add；client.go:626-633 同理；hub.go:210 的 Subscriber.Ephemeral 字段被存储（:217、cluster_resume.go:139）却从不参与 presence 决策；现有测试（client_test.go:577-578）只断言 ack，未覆盖 presence。
[修复建议] 改代码：在 handleConnect/handleSubscribe/restoreSessionSubscriptions 中跳过 ephemeral 订阅的 presence.Add 与 join/leave 事件发布，并补测试。改文档是掩盖缺陷。
[置信度] high
```

```
[级别] Important
[位置] docs/developer/02-configuration.md:80、:103、:44、:357（"字段为空 = 完全禁用心跳"）；代码：node.go:82-95
[问题] 已知线索 2 证实：实现中 idle_timeout 为空时回退 300s 默认，心跳永远开启（node.go 注释明确 "Idle timeout detection is always on"）；docs 02 的三处（yaml 注释、字段表、defaults.go 表）与 config-example.yaml 走查均称"空 = 禁用"。
[证据] node.go:85 `idleTimeout := DefaultHeartbeatIdleTimeout`；cfg.Heartbeat.IdleTimeout == "" 时直接用该值创建 HeartbeatManager；heartbeat.go:27 仅在 IdleTimeout==0 时跳过，而该值不可能为 0。
[修复建议] 改文档：02-configuration.md:80/103/357 与 defaults.go 表中"解析失败时兜底"的表述统一改为"为空或解析失败均回退 300s；无法禁用心跳"。若确有禁用需求则改代码并加配置项。
[置信度] high
```

```
[级别] Important
[位置] docs/developer/04-cluster.md:295、:356；docs/developer/03-admin-api.md:316、:554；代码：pkg/redisbroker/history.go:28-32、:66-77
[问题] 已知线索 1 证实：两份文档称 Redis broker 的 since_offset 为 exclusive（"(ts-seq" 排他起始），实现是 inclusive——history.go 明确注释 "matching the Broker.History contract: offset >= sinceOffset"，streamStartID 用包含形式 "ts-seq"。内存 broker 也是 inclusive（broker_memory.go:180）。两个实现语义已一致，文档却声称不一致。
[证据] history.go:30 `start := streamStartID(sinceOffset)` → :70-77 返回 "ts-seq"（无 "(" 前缀）；broker.go:105 接口契约 "offset >= sinceOffset"。
[修复建议] 改文档：04-cluster.md §8 与 §11、03-admin-api.md GetHistory 节删除"Redis 为 exclusive"表述，统一为 inclusive；同时 04-cluster.md:295 中"内存 broker 是 inclusive"保留但注明两者一致。
[置信度] high
```

```
[级别] Important
[位置] docs/developer/04-cluster.md:236-238、01-architecture.md:177；代码：pkg/redisbroker/redis.go:103-117、options.go:18
[问题] 文档称 Redis broker 的 Epoch() 是"每个 broker 进程实例启动时生成的随机 UUID"，并据此推导"跨节点恢复必因 epoch 不匹配而从历史开头恢复"。实际 Redis epoch 是存于 `ml:broker:epoch` 键的**集群共享、跨重启持久**值（SETNX 首写，epoch_test.go 有 SharedAcrossNodes/PersistedAcrossRestart 两组测试）。文档 §4.4 的核心结论（跨节点精确续读不可行）已不成立，且 01-architecture.md:177"epoch 是 broker 实例的随机标识"对 Redis 实现是错的。deployment.md:121 的描述反而是正确的。
[证据] redis.go:110 SETNX 固定键；options.go:18 defaultEpochKey = "ml:broker:epoch"；epoch_test.go:38-39 断言两节点 epoch 相等。
[修复建议] 改文档：04-cluster.md §4.4 重写为"Redis epoch 集群共享且跨重启稳定，跨节点恢复的 epoch 校验可通过"；01-architecture.md §3.4 区分内存（随机）与 Redis（共享）两种实现。
[置信度] high
```

```
[级别] Important
[位置] docs/developer/04-cluster.md:56-58、config-node1.yaml、config-node2.yaml；代码：cmd/server/runtime.go:68-81、pkg/grpcstream/server.go:34-37
[问题] 文档称 config-node1/2.yaml 是"双节点演示的基础配置"，但两份文件都没有 `server.grpc_admin` 段。当前二进制无条件预绑定两个 gRPC 监听器，管理监听器地址为空时 `prepareGRPCServers` 直接报 "grpc-admin-server addr is required" 启动失败——这两份配置按现状**无法启动**。
[证据] runtime.go:74-78 newGRPCAdminServer(opts.Addr="") → server.go:45 validateOptions 报错；config-node1.yaml 全文无 grpc_admin。
[修复建议] 改文件：为两份配置补 `grpc_admin.addr`（如 127.0.0.1:19091 / :29091）+ auth_token；同时更新 04-cluster.md §2.3 说明。或改代码支持空管理地址跳过预绑定（需同步改 02-configuration.md:186-188 的"启动要求"）。
[置信度] high
```

```
[级别] Important
[位置] README.md:44-61（Quick Start 配置）、docs/developer/02-configuration.md:74-75、:102、docs/developer/03-admin-api.md:35；代码：config/config.go:184-189、:67
[问题] config.go 新增 `AllowInsecure` 与"grpc_admin.addr 非空时必须提供 auth_token 或 allow_insecure:true"的校验（config.go:184-189），但：① README 快速开始配置只有 addr 无 auth_token → 按文档操作必然启动失败；② 02-configuration.md 整节未收录 `allow_insecure` 字段，其示例与表格仍写"auth_token 留空 = 不鉴权"（现在是启动错误）；③ 03-admin-api.md:35"为空则不启用鉴权"已过期。config-example.yaml 已更新（auth_token: "change-me" + allow_insecure 注释），文档未跟上。
[证据] config.go:187 `if addr != "" && AuthToken == "" && !AllowInsecure { return error }`；config-example.yaml:6-7。
[修复建议] 改文档：README Quick Start 补 auth_token（或 allow_insecure）；02-configuration.md 补 allow_insecure 字段行、修正"空=不鉴权"表述并补 Validate() 第 6 条规则（:15-35 只列了 5 条）。
[置信度] high
```

```
[级别] Important
[位置] protocol/client/v1/service.proto:8、protocol/server/v1/api.proto:10、protocol/proxy/v1/proxy.proto:9、protocol/event/v1/events.proto:6；实际路径：shared/genproto/...（buf.gen.yaml:5 out: shared/genproto）
[问题] 已知线索 5 证实：4 个 proto 的 go_package 为 `github.com/messageloopio/messageloop/genproto/...`，与实际生成/导入目录 `shared/genproto/...` 不一致（shared 下两个 proto 是正确的 `shared/genproto/shared/v1`）。当前因 `paths=source_relative` + `;alias` 包名后缀而"碰巧能工作"，但 go_package 即文档，任何依赖 go_package 的工具链或第三方生成（如 TS/其他语言插件）都会得到错误路径。
[证据] 生成文件内嵌 go_package 字符串（service.pb.go:1683 `...genproto/client/v1;clientpb`）；全仓库导入均为 `.../shared/genproto/...`（client.go:17 等 80 处）。
[修复建议] 改 4 个 proto 的 go_package 为 `.../shared/genproto/<pkg>/v1;<alias>` 并重新生成（`task generate-protocol`），保持与共享模块路径一致。
[置信度] high
```

```
[级别] Important
[位置] sdks/go/client.go:431-462（Subscribe）；sdks/ts/src/client/client.ts:556-567 + options.ts（setEphemeral）；sdks/ts/package.json:42-44
[问题] 已知线索 3 证实：Go SDK Subscribe/Unsubscribe/自动订阅 6 处均硬编码 `Ephemeral:false`，且无 token/offset/recover 逐订阅参数；TS SDK 仅支持全局 ephemeral（一个选项影响所有订阅，粒度与协议逐订阅字段不对等）。TS SDK 声明 `@grpc/grpc-js` peerDependency 但无任何 gRPC 传输代码（08-sdk-ts.md:32 已如实说明，但 npm 安装仍会强制解析该 peer）。两 SDK 均无 SubRefresh/Survey 客户端方法（TS 仅导出信封构造器）。
[证据] sdks/go/client.go:441 `Ephemeral: false`；client.ts:425/498/560 全部走 this.options.ephemeral；package.json:43 peerDependencies。
[修复建议] 改代码（Go SDK）：Subscribe 增加可变参数（如 SubscribeWithOptions 携带 Ephemeral/Token）；TS SDK 提供逐订阅 ephemeral 或移除全局选项；TS SDK 删除 @grpc/grpc-js peerDependency（无消费者）。文档 08-sdk-ts.md:32 与 07-sdk-go.md 已诚实标注现状，属可选再更新。
[置信度] high
```

```
[级别] Minor
[位置] docs/developer/08-sdk-ts.md:5、docs/developer/06-development.md:242；sdks/ts/package.json:3
[问题] 两文档称 TS SDK 版本为 1.0.5；实际 package.json 为 1.1.0（且最近提交 "chore(sdk-ts): release v1.1.0"）。
[证据] package.json:3 `"version": "1.1.0"`。
[修复建议] 改文档两处版本号。
[置信度] high
```

```
[级别] Minor
[位置] sdks/ts/src/proto/v1/service_pb.ts（全文件，package messageloop.v1 旧布局）
[问题] 一份与当前 client/v1 布局重复的过时生成文件（go_package 为 `.../genproto/go/client/v1`，来源是更早的 buf 配置），全部源码从 `client/v1/service_pb` 导入，此文件无引用。06-development.md:143 已注明"可忽略或删除"，但文件仍留在仓库。
[证据] 文件头 "source: v1/service.proto (package messageloop.v1)"；src 内所有 import 均为 ../proto/client/v1/service_pb。
[修复建议] 删除该文件；它是 git 追踪的（含 go_package 中的 "go/" 路径，不可能由现行 buf.gen.yaml 再生），删除后不会复现。
[置信度] high
```

```
[级别] Minor
[位置] docs/deployment.md:172（"disconnected with a DisconnectStale error code"）；代码：heartbeat.go:54
[问题] 心跳超时断连码是 3511 DisconnectIdleTimeout，不是 3502 DisconnectStale。此外 deployment.md:18-23 的 Listener Model 表把所有监听器标了"Default"（:9080/:9090/127.0.0.1:9091/127.0.0.1:8080），但代码只有 server.http.addr 有回退默认（main.go:222-224），其余地址为空即启动失败（尤其 grpc_admin，见 F5）；06-development.md:208 同样把 127.0.0.1:9091 标为默认值。
[证据] heartbeat.go:54 `client.close(DisconnectIdleTimeout)`；pkg/grpcstream/server.go:34-37。
[修复建议] 改文档：deployment.md:172 改 3511；两张表的 "Default" 列改为"示例值/无默认"并注明 grpc_admin 必填。
[置信度] high
```

```
[级别] Minor
[位置] docs/developer/01-architecture.md:397、docs/developer/05-observability.md:145；代码：pkg/grpcstream/transport.go:106-121
[问题] 文档称传输层"把 Code 与 Reason 交给客户端（gRPC 用 DISCONNECT_ERROR 错误信封）"。gRPC 路径的 DISCONNECT_ERROR 信封只携带 code 字符串 "DISCONNECT_ERROR" 与 reason（放入 message 字段），**数值断连码被丢弃**——gRPC 客户端无法区分 3503/3500/3511。文档措辞暗示 code 完整传递。
[证据] transport.go:110-114 Error{Code:"DISCONNECT_ERROR", Message: reason}，int32(disconnect.Code) 仅用于构造该固定串。
[修复建议] 改代码（将数值码编码进错误信封，如 message 或 metadata），或改文档明示"gRPC 端仅收到原因文本，数字码丢失"。建议前者，否则 gRPC 客户端断连语义残缺。
[置信度] high
```

```
[级别] Minor
[位置] docs/protocol.md:341-355（Disconnect Codes 表）、:346；docs/developer/05-observability.md:154；代码：cluster_resume.go:77、disconnect.go:45-50
[问题] 3502 Stale 的文档语义（"did not authenticate within the configured interval"）与实际唯一触发点不符——实际仅出现在跨节点恢复失败回滚时（cluster_resume.go:77），不存在"鉴权超时窗口"机制；3506-3509 四个码全库无生产触发点，protocol.md 未标注（05-observability.md 已正确标注"否（保留定义）"，但其 3502 行也标"否"与实际不符）。
[证据] 全库 grep：DisconnectStale 仅 client.go:571（恢复快照失败回滚路径）与 cluster_resume.go:77。
[修复建议] 改文档：protocol.md 为 3502/3506-3509 加"当前未产生/语义迁移"标注；05-observability.md:154 将 3502 改"是（集群恢复失败）"。
[置信度] high
```

```
[级别] Minor
[位置] docs/developer/03-admin-api.md:565；sdks/ts/src（无任何管理 API 客户端）
[问题] 03-admin-api.md 称"Go 与 TypeScript SDK 的后端集成均通过本管理 API 与服务端通信"——TS SDK 没有管理 API 客户端、没有代理后端（proxy）支持，只有 Go SDK 的 proxy.go 走 ProxyService。TS 侧的 server/proxy/event proto 文件仅因 buf 模块级生成而存在，无任何引用。
[证据] sdks/ts/src 中唯一引用 api_pb/proxy_pb 的是它们自身；README.md:282-283 也仅声明 TS SDK 为 WebSocket 客户端。
[修复建议] 改文档：03-admin-api.md:565 改为"Go SDK 的后端集成…"。
[置信度] high
```

```
[级别] Minor
[位置] docs/protocol.md:62-76（OutboundMessage 信封表）
[问题] 信封表缺 `sub_refresh_ack` 一行（proto service.proto:46 有该信封，服务端 handleSubRefresh 会返回），表内已列 survey_request/survey_reply 等 10 种但漏此一种。
[证据] protocol/client/v1/service.proto:46；client.go:1190-1194。
[修复建议] 改文档补一行。
[置信度] high
```

```
[级别] Minor
[位置] 已知线索 4、7 的核实结论：
[问题] ① server.exe（32,424,960 字节）与 nul（51 字节，内容为 "dir: cannot access '/b'..." 的误重定向产物）都在磁盘上，但**均未被 git 追踪**，且被 .gitignore 覆盖（*.exe、/nul）——线索 4 部分成立：无入库风险，但工作区残留应清理。② 64/16384 分片常量核实仍为 numHubShards=64（hub.go:19）、numSubLocks=16384（node.go:40），AGENTS.md/CLAUDE.md 所述一致——线索 7 不成立（文档准确）。③ CRLF 无问题：库内统一 LF（i/lf），工作区按 autocrlf=true 检出 CRLF，无混用。
[证据] git ls-files 无 server.exe/nul；git check-ignore -v 命中 .gitignore:34:/nul；nul 内容如上。
[修复建议] 删除工作区 server.exe 与 nul；无需改 .gitignore。
[置信度] high
```

```
[级别] Minor
[位置] docs/fix-plan.md（全文）；docs/superpowers/plans/2026-08-10-*.md、RPC_TIMEOUT.md
[问题] 已知线索 6 证实：fix-plan.md 是已完成并"验证结果全部通过"的修复计划（P0-1~P2-27），抽查 P0-3（MarshalJSONStruct 已抽取，client.go:120）、P1-4（Node.Publish 返回 offset）、P1-6（requireAuth 拒绝无代理 token）、P2-8（未认证 Publish 已改 DisconnectInvalidToken）、P2-19（PublishTransient）均已在代码中落地。RPC_TIMEOUT.md 是更早的实现记录，其中函数名已过时（onRPC()/getRPCTimeout()，现为 handleRPC/GetRPCTimeout）。两者继续留在 docs/ 会误导"当前行为说明"的读者。
[证据] fix-plan.md:419-433 全部勾选；client.go:120/642/982 与计划一致。
[修复建议] 归档：将 docs/fix-plan.md、RPC_TIMEOUT.md、docs/superpowers/plans/ 移入 docs/archive/ 或在文件头加"历史记录，已于 2026-08-10 完成"横幅；README 的 Repository Guide 中对 RPC_TIMEOUT.md 的引用同步更新。
[置信度] high
```

```
[级别] Minor
[位置] docs/developer/02-configuration.md:150、:186-188（WS read_timeout 与"心跳禁用"分支、gRPC 预绑定）；代码：pkg/websocket/handler.go:65-69、node.go:85
[问题] ① read_timeout 说明中的"未配置且心跳禁用时默认 60s"分支实际不可达（心跳永开，见 F2），默认生效值是 2×idle_timeout=600s，文档未给出该实际默认数字；② "启动要求"一节已如实描述双监听器无条件预绑定，但与 F5 的 node 配置矛盾未指出现状后果。
[证据] handler.go:65-69：idleTimeout>0 恒成立 → readTimeout=2×300s=600s。
[修复建议] 改文档：将 read_timeout 说明改为"未配置时取 2×idle_timeout（默认 600s）"。
[置信度] high
```

```
[级别] Minor
[位置] config.yaml（根目录，gitignored）；docs/developer/06-development.md:213
[问题] 文档称 config.yaml 为"默认开发配置"，但该文件同样缺少 grpc_admin 段（且含已废弃的 check_origin: true），按当前二进制无法启动（同 F5）。
[证据] config.yaml:1-26 无 grpc_admin。
[修复建议] 本地文件补充 grpc_admin（它不入库，无需改仓库文档，但可在 06-development.md 加注）。
[置信度] high
```

## 3. 建议归档/删除的文档与文件

| 项目 | 处置 | 理由 |
| --- | --- | --- |
| `docs/fix-plan.md` | 归档或加"已完成"横幅 | 计划已全部实施并验证（fix-plan.md:419-433），作为历史记录误导读者 |
| `RPC_TIMEOUT.md` | 归档 | 实现记录，函数名已过时；内容已并入 docs/developer/02 与 01 的三层超时 |
| `docs/superpowers/plans/2026-08-10-message-flow-fix-*.md` | 归档 | 同一批修复的工作记录，与 fix-plan.md 重复 |
| `sdks/ts/src/proto/v1/service_pb.ts` | 删除 | 旧布局重复生成文件，无引用，重新生成也不会更新（06-development.md:143 已承认） |
| `server.exe`（32MB） | 删除（工作区） | 未被追踪，gitignore 已覆盖，纯构建残留 |
| `nul`（51B） | 删除（工作区） | 误重定向产物，未追踪，`.gitignore` 中 `/nul` 可保留作防御 |
| `docs/review/` | 保留（未追踪） | 当前评审工作区 |

**文档与代码一致、无需改动的确认项**：64/16384 分片常量（AGENTS.md/CLAUDE.md 准确）；`consumer_group` 字段未消费（02-configuration.md:230 准确）；`stream_approximate:false` 被忽略（02-configuration.md:228 准确）；`ml:broker:epoch` 共享 epoch 描述（deployment.md:121 准确）；05-observability.md 的断连码"是否触发"标注整体准确（除 3502 一处）；Go SDK 重连/恢复行为描述（07-sdk-go.md）与实现一致。

## 未完成模块

无（模块 06 首次返回空报告，已按流程用相同 prompt 重试成功，重试报告见上）。
