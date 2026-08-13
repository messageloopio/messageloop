# 消息数据流修复方案 — 执行报告(2026-08-10)

> 执行基线: `7e92f4b34d857b0f3b8377fa0010c29e57157e5d`(Task 0 复核时记录)
> 执行者: agentic worker(opencode)
> 前置: Task 0 全面复核通过(复核报告 `2026-08-10-message-flow-fix-plan-review.md`,无阻塞项,含修订 R1-R14 已同步进方案文档)

## 1. 每个 Task 的状态与 commit

| Task | 状态 | Commit |
|------|------|--------|
| Task 0 全面复核 | 完成(25 项证据全部确认,无推翻;R1-R14 修订方案) | `72e4e91` docs(plan): complete Task 0 review and revise plan per findings |
| Task 1 TS SDK JSON codec | 完成 | `01a2d71` fix(ts-sdk): rewrite JSON codec with bufbuild fromJson/toJson |
| Task 2 Redis History inclusive | 完成(含真实 Redis 集成测试) | `2509096` fix(redisbroker): make History sinceOffset inclusive per Broker contract |
| Task 3 通配符订阅 + 引用计数 | 完成(含真实 Redis 集成测试) | `f83568b` fix(redisbroker): support wildcard subscription interest with refcounting |
| Task 4 集群级 epoch | 完成(含并发竞争测试;部署文档补充运维注意) | `6add022` fix(redisbroker): use cluster-wide epoch stored in Redis |
| Task 5 Admin gRPC 强制认证 | 完成 | `089fee8` feat(config): require admin gRPC auth token unless allow_insecure |
| Task 6 心跳 IdleTimeout 默认值 | 完成 | `71f518c` fix(node): apply default heartbeat idle timeout when unconfigured |
| Task 7 WS 默认写超时 | 完成(含真实 TCP 阻塞写测试) | `37720c5` fix(websocket): default 10s write timeout to protect broadcast from slow consumers |
| Task 8 入站消息统一认证守卫 | 完成(5 个新用例 + 2 个既有用例修正) | `45a4634` fix(client): require authentication for all non-connect inbound messages |
| Task 9 匿名接管禁止 + resume 指标/限流 | 完成(3 个新用例;3 个既有测试改认证模式) | `0f5d56a` fix(client): disable anonymous session takeover; balance metrics and conn limit on resume |
| Task 10 lease TTL 600s + CAS 抢占 | 完成(2 个新用例;fake 目录按版本号模拟) | `27c8146` fix(cluster): extend session lease TTL and use CAS for cross-node resume |
| Task 11 可靠性三件套(11a/11b/11c) | 完成 | `4a9d9b2` fix(redisbroker): roll back stream on pubsub failure; add Ready signal and reconnect catch-up |
| Task 12 Publication 模型扩展(破坏性) | 完成(编译驱动找齐全部调用方;4 个新用例;proto 重新生成) | `9bbb555` feat(broker)!: preserve payload kind/content_type/id/metadata through Publication model |
| Task 13a Admin ACL | 完成 | `9baeb53` fix(admin): enforce ACL on admin subscribe/publish operations |
| Task 13b 僵尸会话回滚 | 完成 | `3007476` fix(cluster): roll back session when remote subscription restore fails |
| Task 13c 指标对称 | 完成(2 条真实修复 + 1 条回归保护) | `abe61bf` fix(metrics): balance connections/channels/delivery counters on failure and cluster paths |
| Task 13d presence/projection 杂项 | 完成 | `364eee5` fix(cluster): align presence index TTL, reap dead owner projections, log presence publish errors |
| Task 13e snapshot 补全 Ephemeral | 完成 | `0e08398` fix(cluster): preserve ephemeral flag in session snapshots |

全部 13 个 Task 完成,无跳过。commit 共 19 个(含 Task 0 的文档 commit)。

## 2. 全量验证命令输出摘要(收尾)

| 命令 | 结果 |
|------|------|
| `go build ./...` | PASS |
| `go vet ./...` | PASS |
| `go test -race ./...` | PASS(8 个包全绿) |
| `MESSAGELOOP_TEST_REDIS_ADDR=127.0.0.1:6379 go test -race ./...` | PASS(本机 Redis 可用,集成测试全部实跑,无 skip) |
| `cd sdks/ts && npm test` | PASS(2 suites / 29 tests) |
| `cd sdks/go && go test ./...` | PASS |

Redis 集成测试全部实跑(R2/R3 修订后均使用 `requireCommandBusRedis` 守卫;本机 `127.0.0.1:6379` 可用): Task 2 `TestRedisBroker_History_InclusiveSinceOffset`、Task 3 `TestRedisBroker_WildcardReceivesPublication_Redis`、Task 4 `TestRedisBroker_Epoch_*`(3 个)、Task 11 `TestRedisBroker_Publish_PubSubFailureRollsBackStream` / `TestRedisBroker_Ready_ClosesAfterSubscribe` / `TestRedisBroker_Reconnect_CatchesUpMissedMessages` / `TestClusterRedis_NodeRun_WaitsForBrokerReady`、Task 13d `TestRedisPresenceStore_IndexTTLAndCleanup` 全部通过。

## 3. 与方案文档的偏差及理由

方案文档在 Task 0 已按复核报告修订(R1-R14,见复核报告第 4 节),执行阶段与修订后文档一致。执行中的额外偏差:

| # | 偏差 | 理由 |
|---|------|------|
| D1 | Task 1: `JSONCodec.encode` 对非 bufbuild 普通对象走原样 `JSON.stringify`(草案为纯 `toJson`)| 既有测试 `should return string from encode` 传普通对象;`Codec` 接口输入为 `object`,兼容两种形态 |
| D2 | Task 1: `decode` 使用 `fromJson(..., { ignoreUnknownFields: true })` | 与服务端 protojson `DiscardUnknown: true`(`shared/marshaler.go:93`)对齐;否则 `survey_reply` 测试 wire 中的未知键报错 |
| D3 | Task 4: epoch 用 `atomic.Value` 存储(Task 0 未预判) | `initEpoch`(Start goroutine)与 `Epoch()`/`Publish` 并发读写,`-race` 检测到数据竞争;原子存储是最小修复 |
| D4 | Task 7: 阻塞写超时测试用真实 TCP + gorilla 握手(httptest),非 net.Pipe mock | `pkg/websocket/transport_test.go` 原无 conn mock;真实 TCP 最接近生产路径且无外部依赖 |
| D5 | Task 8: `ForceTestIDs` 同时设置 `authenticated = true` | 多个测试(集群集成、survey 等)用 `ForceTestIDs + AddClient` 直连客户端,统一认证守卫后必须标记已认证;符合该方法"testing purposes"语义 |
| D6 | Task 9: `TestClusterRedis_RemoteResumeTakeover` 等 3 个既有测试改为认证模式(`require_auth: true` + 认证 proxy stub) | Task 9 语义(仅认证连接可 resume)使匿名跨节点 resume 测试失效;集群部署本应开启 `require_auth`;`newClusterRedisTestNode` 统一启用 `RequireAuth` |
| D7 | Task 9: 本地 resume 采用方案 A(metricsCharged 转移),`ReplaceSession` 签名增加 error 返回 | 方案 A 是推荐项;签名变更经编译驱动确认仅 2 处调用点(client.go + hub_test.go) |
| D8 | Task 10: resumeRemoteSession 中 CAS 在 takeover 命令之前执行 | 方案 Step 2 顺序隐含 CAS 优先(冲突则不发 takeover);实现按此落地 |
| D9 | Task 11a: 测试用 go-redis `client.AddHook` 注入 PUBLISH 失败(修订 R8 已预判);实际按 `cmd.Name() == "publish"` 匹配 | go-redis v9 无 `PublishCmd` 类型(`Publish` 返回 `*IntCmd`) |
| D10 | Task 11b: `Node.Run` 阻塞语义测试改为"Run 不早于 Ready 返回"不变式 | 真实 Redis 上 PSubscribe+Receive <150ms,固定时长断言不可靠 |
| D11 | Task 11c: `runPubSub` 暴露 `activePubSub` 字段供测试模拟断线(`pubsub.Close()`) | 无该注入点无法在真实 Redis 上模拟 pub/sub 断开;字段仅测试读取 |
| D12 | Task 12: 删除 `Publication.IsText`(草案倾向删除) | 全量改调用方成本可控(编译驱动),避免双字段不一致 |
| D13 | Task 12: `PublishTransient` 签名改为 `error`(不再返回 offset) | 草案如此;全部调用方确认无消费 offset |
| D14 | Task 12: presence join/leave 的 WARN 日志(13d 内容)提前在 Task 12 落地;13d 补充了 `PresencePublishFailures` 指标 | 修改 `PublishTransient` 调用时自然处理,避免二次改动 |
| D15 | Task 13a: ACL principal 用固定 `"admin"` 常量;`SubscribeSession/UnsubscribeSession` 返回 error,`api_handler.Subscribe` 按既有语义记录 `results[ch]=false`(不返回 RPC error) | 与 api_handler 既有的每 channel 结果语义一致 |
| D16 | Task 13c: 第一条测试(AddClient 失败不增)为回归保护(修订 R12 已注明,现状已满足) | 与方案一致 |
| D17 | Task 13d: `ClusterQueryStore` 接口新增 `ListNodeProjections`/`DeleteNodeProjection` 两方法(波及所有实现与 fake) | 方案要求 repairer 扫描 owner 投影;通过接口抽象而非直接访问 Redis |
| D18 | Task 13e: 快照 Ephemeral 通过 `hub.LookupSubscriber` 读取 | `Client` 自身不保存 Ephemeral 标志,需从 hub 查询 |

## 4. 行为变更清单

方案文档"行为变更清单"13 条全部生效;执行中补充/确认的额外条目:

| # | 变更 | 来源 |
|---|------|------|
| B1 | `PublishTransient` 返回类型由 `(uint64, error)` 改为 `error`(破坏性) | Task 12 |
| B2 | `ReplaceSession` 返回 error;本地 resume 超 `maxConnsPerUser` 时新连接被断开(`DisconnectConnectionLimit`) | Task 9 |
| B3 | 本地 resume 不再走 `AddClient`,`ConnectionsTotal` 不再泄漏(此前每次本地 resume +1) | Task 9 |
| B4 | 跨节点 resume 通过 CAS 抢占 lease,冲突时新连接以 `DisconnectStale`(3502)断开 | Task 10 |
| B5 | Redis broker 实现 `Ready()`:健康检查在订阅就绪前返回 503(此前恒 not applicable);`Node.Run` 等待订阅就绪 | Task 11b |
| B6 | 断线重连后精确 channel 自动回补丢失消息;通配符 pattern 不回补(已知限制) | Task 11c |
| B7 | presence index TTL 由 120s 改为 60s(与 member 对齐);空 index 立即删除 | Task 13d |
| B8 | projection repair 主动清理 lease 已失效的 `owner:*` 投影(此前等 10min TTL) | Task 13d |
| B9 | 新指标 `messageloop_presence_publish_failures_total` | Task 13d |
| B10 | admin Subscribe/Unsubscribe/Publish 受内置 ACL 约束(固定 `"admin"` principal) | Task 13a |
| B11 | 远程恢复订阅失败时会话完整回滚(此前残留僵尸会话) | Task 13b |
| B12 | 快照跨节点恢复保持订阅的 ephemeral 标志(此前全部变永久) | Task 13e |
| B13 | 未配置心跳时 WS read deadline 由 60s 变 600s(2×默认 idle timeout) | Task 6 |

方案"已知限制"7 条保持不变(JSON 大整数精度、通配符回补缺口、PublishTransient 不可回补、SDK 独立项、`__presence` 无消费者[已在 `docs/protocol.md` 说明现状]、WS/gRPC 错误处理不一致、`removeWildcardSub` 计数语义)。

## 5. 备注

- 方案文档 88 个 checkbox 已全部勾选。
- `docs/protocol.md`、`docs/deployment.md`、`docs/developer/04-cluster.md`、`docs/developer/05-observability.md`、`config-example.yaml`、`configs/test.yaml` 已同步更新。
- 生成产物(`shared/genproto/server/v1` + TS `api_pb.ts`)随 Task 12 提交;`task generate-protocol` 对未修改的 client/proxy 产物的行尾噪音已还原。
- 全部 commit 通过 `--no-verify` 提交(仓库无 pre-commit hook 配置,verify 默认即通过)。
