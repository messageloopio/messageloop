# 方案执行交付说明(Handoff)——给原方案 agent

> 目的: 向方案文档 `2026-08-10-message-flow-fix-plan.md` 的原作者交付执行结果,并附一份可直接运行的验收 prompt。
> 执行结果详见 `2026-08-10-message-flow-fix-execution-report.md`(状态/commit/验证/偏差/行为变更)与 `2026-08-10-message-flow-fix-plan-review.md`(Task 0 复核)。

## 1. 实现汇总(19 个 commit,基线 7e92f4b)

| Task | Commit | 实现要点 | 关键测试 |
|------|--------|----------|----------|
| 0 复核 | `72e4e91` | 25 项证据全确认,修订 R1-R14 | 复核报告 |
| 1 TS codec | `01a2d71` | 重写 json.ts:顶层 `toJson/fromJson`;保留 name "messageloop+json";decode `ignoreUnknownFields` 对齐服务端 DiscardUnknown | `sdks/ts/test/codec.test.ts`(14 用例,含服务端 golden 对拍) |
| 2 History inclusive | `2509096` | `streamStartID` 去 `"("`;`client.go:604` 的 +1 未动 | `TestStreamStartID`/`TestStreamOffsetFullRoundTrip`/`TestRedisBroker_History_InclusiveSinceOffset` |
| 3 通配符订阅 | `f83568b` | redisBroker 精确+通配符引用计数;`interested()` 用 CSTrieMatcher(lock-free) | `TestRedisBroker_Interested_Wildcard`/`_Unsubscribe_RefCount`/`_WildcardReceivesPublication_Redis` |
| 4 集群 epoch | `6add022` | Redis 固定 key `ml:broker:epoch`(SET NX);epoch 用 `atomic.Value`;部署文档补充 | `TestRedisBroker_Epoch_SharedAcrossNodes`/`_PersistedAcrossRestart`/`_ConcurrentInit` |
| 5 Admin 认证 | `089fee8` | `GRPCAdmin.AllowInsecure` + Validate 强制;WARN 日志;示例/测试配置补 token | `TestValidate_AdminRequiresAuthToken` |
| 6 心跳默认值 | `71f518c` | 未配置回落 `DefaultHeartbeatIdleTimeout`(300s) | `TestNewNode_HeartbeatDefaultIdleTimeout` |
| 7 WS 写超时 | `37720c5` | `DefaultOptions().WriteTimeout=10s`;main.go 空配置保留默认;真实 TCP 阻塞写测试 | `TestDefaultOptions_WriteTimeout`/`TestTransport_WriteTimesOutWhenPeerStopsReading` |
| 8 入站认证守卫 | `45a4634` | `handleMessage` switch 前统一守卫 + default 分支 `DisconnectBadRequest`;`ForceTestIDs` 标记已认证 | 5 个 `*_BeforeAuth` 用例 |
| 9 匿名接管禁止 | `0f5d56a` | `resumeAllowed = requireAuth && proxy`;匿名忽略 SessionId;metricsCharged 转移;`ReplaceSession` 返回 error + limit 检查 | `TestClientSession_AnonymousResumeRejected`/`_LocalResume_MetricsBalanced`/`TestHub_ReplaceSession_EnforcesMaxConnsPerUser` |
| 10 lease TTL + CAS | `27c8146` | TTL 90s→600s;`resumeRemoteSession` 先 CAS(期望=旧版本,desired=+1)再 takeover;冲突 `DisconnectStale` | `TestResumeRemoteSession_UsesCAS`/`_CASConflictAborts` |
| 11 可靠性三件套 | `4a9d9b2` | 11a PUBLISH 失败 XDEL 回滚(go-redis Hook 注入);11b `Ready()`(PSubscribe Receive 确认);11c `lastOffsets` + 断线回补(XRangeN)+ 去重 | `TestRedisBroker_Publish_PubSubFailureRollsBackStream`/`_Ready_ClosesAfterSubscribe`/`_Reconnect_CatchesUpMissedMessages`/`TestClusterRedis_NodeRun_WaitsForBrokerReady` |
| 12 Publication 模型 | `9bbb555` | `Publication` 增加 Kind/ContentType/Id/Metadata,删除 IsText;`Publish(ch, *Publication)`、`PublishTransient` 返回 error;`PayloadProto()` 共享重建;redisMessage 加字段+旧格式兼容推断;api.proto `HistoryPublication` id=5/metadata=6 并重新生成 | `TestMemoryBroker_Publish_PreservesKindAndMetadata`/`TestRedisBroker_Message_BackwardCompat`/`TestClient_Recovery_PreservesPayloadType`/`TestAPIServiceHandler_GetHistory_ReturnsContentTypeAndId` |
| 13a Admin ACL | `9baeb53` | `adminPrincipal="admin"`;SubscribeSession/UnsubscribeSession + admin Publish 入口 ACL 检查;集群命令路径不重复校验 | `TestAPIServiceHandler_(Subscribe\|Publish)_ACLDenied` |
| 13b 僵尸会话回滚 | `3007476` | 恢复订阅失败 → RemoveSession + deleteClusterSessionState + `DisconnectStale` | `TestClient_RemoteResume_RestoreFailureRollsBackSession` |
| 13c 指标对称 | `abe61bf` | restoreLocalSubscription/removeLocalSubscriptionOnly 对称维护 ActiveChannels;handleClusterPublishCommand 计数 MessagesDelivered/DeliveryFailures | `TestNode_RestoreLocalSubscription_ActiveChannelsMetric`/`_ClusterPublishCommand_MessagesDeliveredMetric`/`_AddClient_ClusterSyncFailure_NoGaugeIncrease`(回归保护) |
| 13d presence/projection | `364eee5` | presence index TTL=member TTL;Remove 清空空 index;repairer 新增 `ListNodeProjections`/`DeleteNodeProjection` 扫 owner 投影(lease 失效即删);`PresencePublishFailures` 指标;`__presence` 现状写入 protocol.md | `TestRedisPresenceStore_IndexTTLAndCleanup`/`TestClusterProjectionRepairer_ReapsDeadOwnerProjections` |
| 13e snapshot Ephemeral | `0e08398` | `clusterSessionSnapshot` 经 `hub.LookupSubscriber` 填充 Ephemeral | `TestNode_ClusterSessionSnapshot_PreservesEphemeral` |
| 收尾 | `9b3ce31` | 方案文档 checkbox 全勾选 + 执行报告 | 执行报告 |

## 2. 验收 prompt(复制给原方案 agent)

````markdown
你是 MessageLoop 消息数据流修复方案(2026-08-10-message-flow-fix-plan.md)的原作者。执行者已按方案完成 Task 0-13 并逐任务提交。现在请你**验收执行结果**。

## 依据文档(按序阅读)
1. `docs/superpowers/plans/2026-08-10-message-flow-fix-plan.md`(方案,含 Task 0 修订 R1-R14,88 个 checkbox 已勾选)
2. `docs/superpowers/plans/2026-08-10-message-flow-fix-plan-review.md`(Task 0 复核报告:25 项证据全确认)
3. `docs/superpowers/plans/2026-08-10-message-flow-fix-execution-report.md`(执行报告:commit 列表、验证摘要、偏差 D1-D18、行为变更 B1-B13)

## 验收环境
- 仓库根目录 D:/Codes/qiulin/messageloop,Go 1.26(toolchain 自动)
- Redis 127.0.0.1:6379 可用(集成测试实跑;若不可用,`t.Skipf` 不算失败,报告中注明)
- 基线 commit: 7e92f4b;执行 commit 序列见执行报告第 1 节(19 个 commit)

## 验收步骤

### Step 1: 提交序列核对
```bash
git log --oneline 7e92f4b..HEAD
```
核对: 19 个 commit,message 与执行报告第 1 节一致,顺序为 Task 1→13。

### Step 2: 全量验证(必须全绿)
```bash
go build ./...
go vet ./...
go test -race ./...
MESSAGELOOP_TEST_REDIS_ADDR=127.0.0.1:6379 go test -race ./...
cd sdks/ts && npm test
cd sdks/go && go test ./...
```

### Step 3: 逐 Task 抽查(每个 Task 至少跑指定测试并人工核对一处实现)

| Task | 验证命令 | 人工核对点 |
|------|----------|-----------|
| 1 | `cd sdks/ts && npx jest test/codec.test.ts` | json.ts 用顶层 `toJson/fromJson`;encode 输出 snake_case(`client_id`);decode 能解析服务端 golden(connected/publication) |
| 2 | `go test ./pkg/redisbroker/ -run 'TestStreamStartID\|TestStreamOffsetFullRoundTrip\|TestRedisBroker_History_InclusiveSinceOffset' -v` | `streamStartID` 非零返回 `"ts-seq"`(无 `(`);`git show 2509096` 确认 `client.go:604` 的 `sub.Offset + 1` 未改动 |
| 3 | `go test -race ./pkg/redisbroker/ -run TestRedisBroker -v` | Subscribe/Unsubscribe 引用计数;`interested()` 同时匹配精确与通配符 |
| 4 | `go test -race ./pkg/redisbroker/ -run TestRedisBroker_Epoch -v` | `New()` 不再生成 UUID;`initEpoch` SET NX + Get;`Epoch()` 原子读 |
| 5 | `go test ./config/ -run TestValidate_AdminRequiresAuthToken -v` | Validate 规则:Addr 非空 + token 空 + !allow_insecure → error;`configs/test.yaml` 已补 token |
| 6 | `go test . -run TestNewNode_HeartbeatDefaultIdleTimeout -v` | node.go 未配置心跳回落 `DefaultHeartbeatIdleTimeout` |
| 7 | `go test ./pkg/websocket/ -run 'TestDefaultOptions_WriteTimeout\|TestTransport_WriteTimesOut' -v` | `DefaultOptions().WriteTimeout == 10s`;main.go 空配置保留默认 |
| 8 | `go test . -run 'TestClientSession_HandleMessage_(Subscribe\|RPC\|Unsubscribe\|Ping\|SubRefresh)_BeforeAuth' -v` | `handleMessage` 统一守卫(Connect 除外);default 分支 `DisconnectBadRequest` |
| 9 | `go test . -run 'TestClientSession_AnonymousResumeRejected\|TestClientSession_LocalResume_MetricsBalanced\|TestHub_ReplaceSession_EnforcesMaxConnsPerUser' -v` | `resumeAllowed` 守卫;metricsCharged 转移;`ReplaceSession` limit 检查 |
| 10 | `go test . -run TestResumeRemoteSession -v` | CAS expected=旧版本、desired=+1;冲突返回 `DisconnectStale`;`cluster_state.go` TTL=600s;续约仍无条件 Put |
| 11 | `go test -race ./pkg/redisbroker/ -run 'TestRedisBroker_Publish_PubSubFailureRollsBackStream\|TestRedisBroker_Ready\|TestRedisBroker_Reconnect' -v` + `go test -race . -run TestClusterRedis_NodeRun_WaitsForBrokerReady -v` | PUBLISH 失败 XDEL 回滚;`Ready()` 在 Receive 确认后 close;重连回补精确 channel + offset 去重 |
| 12 | `go test . -run 'TestMemoryBroker_Publish_PreservesKindAndMetadata\|TestClient_Recovery_PreservesPayloadType' -v` + `go test ./pkg/redisbroker/ -run TestRedisBroker_Message_BackwardCompat -v` + `go test ./pkg/grpcstream/ -run TestAPIServiceHandler_GetHistory_ReturnsContentTypeAndId -v` | broker.go 新 `Publication`/`PayloadKind`;`PayloadProto()` 按 Kind 重建;api.proto `HistoryPublication` id=5/metadata=6 且 `shared/genproto/server/v1` 已提交生成产物;旧格式 stream 数据兼容推断 |
| 13a | `go test ./pkg/grpcstream/ -run 'TestAPIServiceHandler_(Subscribe\|Publish)_ACLDenied' -v` | admin 固定 `"admin"` principal;集群命令路径不重复校验 |
| 13b | `go test . -run TestClient_RemoteResume_RestoreFailureRollsBackSession -v` | 恢复失败 → RemoveSession + deleteClusterSessionState + 断开 |
| 13c | `go test . -run 'TestNode_AddClient_ClusterSyncFailure_NoGaugeIncrease\|TestNode_RestoreLocalSubscription_ActiveChannelsMetric\|TestNode_ClusterPublishCommand_MessagesDeliveredMetric' -v` | ActiveChannels 对称;PublishToSession 计数 |
| 13d | `go test ./pkg/redisbroker/ -run TestRedisPresenceStore_IndexTTLAndCleanup -v` + `go test . -run TestClusterProjectionRepairer_ReapsDeadOwnerProjections -v` | index TTL=60s 且空 index 删除;repairer 删无 lease 的 owner 投影、跳过自己 |
| 13e | `go test . -run TestNode_ClusterSessionSnapshot_PreservesEphemeral -v` | snapshot 填充 Ephemeral 并经 `LookupSubscriber` 读取 |

### Step 4: 行为变更与偏差核对
- 方案"行为变更清单"13 条 + 执行报告第 4 节 B1-B13,逐条确认代码行为与描述一致(可抽查 2-3 条关键项: 如 #12 Payload_Json 全链路保留、#9 匿名接管禁止、B4 CAS 冲突断开)。
- 执行报告第 3 节 D1-D18 偏差: 逐条判断"合理/不可接受"并给出理由。

### Step 5: 输出验收报告
格式: 每 Task 一行结论(`通过` / `不通过` + 证据: 测试名/输出/代码位置),全量验证结果表,偏差核对结论,最终验收结论(`验收通过` 或列出阻塞问题)。报告写入 `docs/superpowers/plans/2026-08-10-message-flow-fix-acceptance.md`(若已存在则覆盖)。发现问题时不要直接改代码——先列出问题清单,由执行者修复。
````
