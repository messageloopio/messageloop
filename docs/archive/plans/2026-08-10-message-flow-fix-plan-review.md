# 消息数据流修复方案 — Task 0 全面复核报告(2026-08-10)

> 复核执行时间: 2026-08-10
> 复核基线 commit: `7e92f4b34d857b0f3b8377fa0010c29e57157e5d`(git rev-parse HEAD)
> 复核人: agentic worker(opencode)
> 依据文档: `docs/superpowers/plans/2026-08-10-message-flow-fix-plan.md`

## 1. 基线验证结果

| 命令 | 结果 |
|------|------|
| `go build ./...` | PASS(0 错误) |
| `go vet ./...` | PASS |
| `go test ./...` | PASS(全部包 ok: messageloop 38.2s、cmd/server、config、grpcstream、redisbroker、topics、websocket、proxy) |
| `cd sdks/ts && npm test` | PASS(2 suites / 24 tests) |
| `cd sdks/go && go test ./...` | PASS |
| Redis(127.0.0.1:6379) | 可用(`TcpTestSucceeded=True`),集成测试可跑 |

无已存在的失败;修复前基线与方案预期一致(全绿)。

## 2. 证据清单逐条复核(25 项)

行号以基线 commit 为准。每项给出 `确认` / `已漂移` / `推翻` 结论。

| # | 结论 | 复核结果 |
|---|------|----------|
| 0-1 | Redis 模式通配符订阅失效 | **确认**。`pkg/redisbroker/pubsub.go:55-60` 用 `b.subscribed[channelName]` 精确 map 匹配;`pkg/redisbroker/redis.go:56-61` Subscribe 仅写精确 channel;`node.go:289` 将通配符字面量(如 `forex.*`)直接传入 `broker.Subscribe`;`hub.go:79-84`(isWildcard 判定在 `hub.go:74-77`)。四段证据与描述一致。 |
| 0-2 | Redis History exclusive off-by-one | **确认**。`pkg/redisbroker/history.go:59-66` `streamStartID` 非零返回 `"(ts-seq"`(exclusive);`broker.go:49` 接口语义为 `offset >= sinceOffset`;`broker_memory.go:185` 内存实现按 `pub.Offset < sinceOffset` 跳过(即 `>=`);`client.go:604` `sinceOffset := sub.Offset + 1`。证据链完整。 |
| 0-3 | epoch 按节点 UUID | **确认**。`pkg/redisbroker/redis.go:35` `epoch: uuid.NewString()`;消费方 `client.go:562-566`(`interface{ Epoch() string }`)、`client.go:599-612`(比较与降级为 0);`client.go:621-625` `MaxRecoveredPublications` 截断。 |
| 0-4 | 恢复消息一律 Binary | **确认**。`client.go:626-630` 恢复路径构造 `Payload_Binary`;对照 `hub.go:431-438` 实时路径按 `pub.IsText` 区分 Text/Binary。 |
| 0-5 | TS JSON encode 丢 oneof | **确认(机制修正,非推翻)**。`sdks/ts/src/transport/codec/json.ts:150-165` encode 经 `transformInboundMessage` 输出 **camelCase** 字段(`clientId` 而非 `client_id`)。实测: 生成代码无实例 `toJson`(undefined),encode 走原始对象分支,输出 `{"connect":{"clientId":"c1",...}}`。服务端 `protocol/client/v1/service.proto:56-61` 对 `client_id/client_type/session_id` 显式 `[json_name = "snake_case"]`,覆盖默认 camelCase JSONName,而 `shared/marshaler.go:88-93` 的 protojson `DiscardUnknown: true` 导致 camelCase 字段被**静默丢弃**(Go 实测: `clientId` 无法解析出 `ClientId`,`client_id` 可以)。即: oneof 键(`connect`)不丢,但**内容字段全丢**,与服务端 wire 不兼容。方案测试断言 `encoded.connect.client_id` 可复现损坏。 |
| 0-6 | TS JSON decode 无 fromJson + 无 snake→camel | **确认**。`json.ts:167-170` decode 仅 `JSON.parse` + `transformOutboundMessage`,value 为原始 snake_case JSON(实测 `connected.session_id` 读不到 `sessionId`);`sdks/ts/src/client/client.ts:189-247`(实际 `src/client/client.ts`,非 `src/client.ts`)经 `parseOutboundMessage`(`src/message/converters.ts:227-262`)读取 camelCase 属性(`sessionId`、`messages`),与 decode 输出不匹配;服务端 `shared/marshaler.go:88-93` `UseProtoNames: true`。 |
| 0-7 | TS 映射表 `survey_reply` 误写 | **确认**。`sdks/ts/src/transport/codec/json.ts:9-21` 中写的是 `survey_response: "surveyResponse"`(第 20 行);proto 定义 `protocol/client/v1/service.proto:29,48` 为 `survey_reply`。实测 decode `{"survey_reply":...}` 的 case 为 undefined。 |
| 0-8 | Payload_Json 入 broker 坍塌为 text | **确认**。`client.go:924-930`(`Payload_Json` → `MarshalJSONStruct` + `isText = true`);`hub.go:309-324`(及 `hub.go:431-438`)重建时仅按 `IsText` 生成 `Payload_Text`/`Payload_Binary`;broker 模型仅 `([]byte, isText)`(`broker.go:7-14`)。 |
| 0-9 | Admin gRPC 无 auth_token 时无认证 | **确认**。`pkg/grpcstream/admin_server.go:10-14` 仅 `AdminAuthToken != ""` 时挂 `adminAuthInterceptor`;`config/config.go` `Validate()`(`config.go:143-197`)无任何强制;`cmd/server/runtime.go:56-61` 透传 `cfg.Server.GRPCAdmin.AuthToken`;`config.go:60-64` 字段路径为 `Server.GRPCAdmin.AuthToken`。 |
| 0-10 | Admin 操作绕过 ACL | **确认**。`pkg/grpcstream/api_handler.go:174-190`(Subscribe/Unsubscribe)经 `Node.SubscribeSession` → `cluster_commands.go:202-220`(实际 202-220)直调 `AddSubscription`,无 `checkSubscribeACL`;`api_handler.go:25-107`(Publish)同样无 publish ACL。 |
| 0-11 | 心跳 IdleTimeout 默认 300s 未生效 | **确认**。`node.go:82-90` 仅 `cfg.Heartbeat.IdleTimeout != ""` 时创建 `HeartbeatManager`;`defaults.go:11` `DefaultHeartbeatIdleTimeout = 300s` 无引用;`heartbeat.go:27-29` `IdleTimeout == 0` 时 `Start` 直接返回。另外 `pkg/websocket/handler.go:66` 消费 `GetHeartbeatIdleTimeout()`(未配置时为 0,WS read deadline 落回 60s;Task 6 修复后变 600s,见行为变更)。 |
| 0-12 | WS 默认无写超时 | **确认**。`pkg/websocket/server.go:32-37` `DefaultOptions()` 无 `WriteTimeout`;`pkg/websocket/transport.go:43-45` 已支持 `writeTimeout > 0` 时设 deadline;`cmd/server/main.go:196-200` 仅配置非空才覆盖。 |
| 0-13 | 除 Publish 外入站 handler 无认证检查 | **确认**。`client.go:954`(Subscribe)、`:733`(RPC)、`:1050`(Unsubscribe)、`:1086`(Ping)、`:1119`(SubRefresh)、`:1143`(Survey)、`:1194`(SurveyReply)均无认证检查;对照 `client.go:849`(Publish 有)。 |
| 0-14 | 匿名模式可凭 SessionId 接管会话 | **确认**。`client.go:378-382` 无条件将 `connect.SessionId` 写入 c.session;`client.go:460-494` resume/takeover 分支不受 `requireAuth` 保护。 |
| 0-15 | 本地 resume 致 ConnectionsTotal 泄漏 + 绕过 maxConnsPerUser | **确认**。`client.go:469-530`: 本地 resume 分支(469-493)复制状态 + `closeQuiet` + `ReplaceSession`;`client.go:521-530` resumedLocal 时**跳过** `AddClient`(不 Inc 计数),旧 client 经 `closeQuiet`(`client.go:228-243`)不 Dec → 净 +1 泄漏;`hub.go:669-727` `ReplaceSession` 无 limit 检查(对比 `hub.go:151-171` `addWithLimit`)。 |
| 0-16 | lease TTL 90s < idle 300s;续约仅 handlePing | **确认**。`cluster_state.go:16` `defaultClusterSessionLeaseTTL = 90s`;`client.go:1086-1102` handlePing 内 `syncClusterSessionState`(10s 节流 `client.go:358`);`heartbeat.go` 无续约。另注意 `client.go:528` 本地 resume 与订阅变化(`node.go:310`)也会续约。 |
| 0-17 | CompareAndSwapSessionLease 已实现但无生产调用 | **确认**。`pkg/redisbroker/cluster_directory.go:80-123`(WATCH + 版本比较,`clusterSessionLeaseEqual` 在 :192-200 只比较 SessionID/NodeID/IncarnationID/LeaseVersion);写入方 `cluster_state.go:211` 无条件 `PutSessionLease`。 |
| 0-18 | XADD 与 PUBLISH 非原子 | **确认**。`pkg/redisbroker/redis.go:85-108`: XADD 成功后 PUBLISH 失败时 stream 已落条目不回滚。 |
| 0-19 | Pub/Sub 断线重连无回补 | **确认**。`pkg/redisbroker/pubsub.go:13-33` `runPubSubWithRetry` 仅指数退避重连,重连后重新 PSubscribe,无历史回补。 |
| 0-20 | redisBroker 无 Ready() | **确认**。`pkg/redisbroker/redis.go:42-53` `Start` 无 Ready 信号;对照 `node.go:127-133`(已等待 `Ready()` 接口)与 `broker_memory.go:68-70`(已实现)。补充: `health.go:33-34` `healthReadyBroker` 已存在,Redis broker 加 `Ready()` 后健康检查自动生效(行为变化,见文档修订)。 |
| 0-21 | 远程恢复失败留僵尸会话 | **确认**。`client.go:521-535` 先 `AddClient` 再 `restoreSessionSubscriptions`;`cluster_resume.go:112-127` 失败仅 `rollbackRestoredSubscriptions`(132-139),不删 hub session、不清 lease/snapshot。 |
| 0-22 | 指标三处不对称 | **确认(表述微调)**。`node.go:239-242`: `AddClient` 成功路径 Inc;失败路径已 `RemoveSession` 且未 Inc——即"失败不增"现状已成立,Task 13c 的第一条测试属**回归保护**而非 TDD 失败测试(修订文档注明)。`cluster_resume.go:154-155`(`restoreLocalSubscription` 直调 `broker.Subscribe` 无 `ActiveChannels` 计数,仅 :163-165 Inc `SubscriptionsTotal`);`cluster_commands.go:156-181` `handleClusterPublishCommand` 的 `client.Send`(:175)无 `MessagesDelivered`/`DeliveryFailures` 计数(对照 `hub.go:349-351`)。 |
| 0-23 | hub.removeWildcardSub 恒返回 last=true | **确认**。`hub.go:110-122`: `:117` 与 `:121` 两处 return 的第一个值均为 true。 |
| 0-24 | survey responseCh 满不丢结果(map 兜底),原高危结论推翻 | **确认(推翻原结论)**。`survey.go:96-98` 先写 `responses` map;`survey.go:123-166` `Wait` 的 done 分支(:156-163)从 map 收集全部结果,responseCh 满不影响完整性(仅可能延迟)。 |
| 0-25 | 会话 snapshot 不存 Ephemeral、ChannelOffsets 未填充 | **确认**。`cluster_state.go:63` `ChannelOffsets` 字段存在但 `clusterSessionSnapshot`(`:274-309`)不填充;`:291` `ClusterSubscriptionSnapshot{Channel: channel}` 恒 `Ephemeral: false`;消费方 `cluster_resume.go:115` 用 `sub.Ephemeral`。注意: `ClusterSubscriptionSnapshot.Ephemeral` **字段已存在**(`cluster_state.go:49-52`),Task 13e 只需填充、无需加字段。 |

**25 项全部 `确认`,无 `已漂移`、无 `推翻`(0-5 机制描述、0-22 测试性质、0-25 字段存在性三处做了补充修正,见下)。**

## 3. Task 1-13 草案与真实代码一致性复核

### Task 1(TS JSON codec)
- `Codec` 接口(`sdks/ts/src/transport/codec/codec.ts:7-27`)签名与草案一致:`name()/encode(msg: object): Uint8Array|string/decode(data: Uint8Array|string): OutboundMessage/useBytes()`。
- 生成代码 **无实例 `toJson()`**(实测 undefined),顶层 `toJson`/`fromJson` 可用(bufbuild/protobuf v2,`package.json` 依赖 `^2.0.0`)。草案注释已预判此二选一 → **确定采用顶层函数**。
- 草案 `name()` 返回 `"json"` 与现状 `"messageloop+json"`(`json.ts:136`)不符。改名非修复所需且涉及 WS subprotocol 协商(`sdks/ts/src/transport/websocket.ts:98-99`;服务端 `pkg/websocket/handler.go:26-30` 声明 subprotocol 列表、`transport.go:13-18` 仅认 `messageloop+proto` 为 binary)→ **修订草案: 保留 `"messageloop+json"`**,既有断言 `codec.test.ts:12` 无需改动。
- 草案测试构造(`create(InboundMessageSchema, ...)`)与 `InboundMessageSchema/OutboundMessageSchema` 存在性确认(`service_pb.ts:99,194`)。
- 草案 Step 2 预期"encode 输出无 connect 键"与实测不符(实测有 `connect` 键但字段为 camelCase)→ **修订 Step 2 预期**: FAIL 点为 `encoded.connect.client_id` 为 undefined(camelCase 输出)与 decode 读不到 `sessionId`、`survey_reply` case undefined。
- 草案 Step 4/5 的 golden 对拍方向成立: 服务端 `shared/marshaler.go` 为 `UseProtoNames: true` 的 protojson。
- 草案删除五个手写转换、保留 `bigIntReplacer` 合理;encode 的 `"toJson" in msg` 分支可保留(未来兼容)或删除。

### Task 2(Redis History inclusive)
- 引用行号 `history.go:15-16, 27-28, 56-66` 准确;`history_test.go:50-83` 准确(`TestStreamStartID` 期望 `"(ts-seq"`(:59-61);`TestStreamOffsetFullRoundTrip` 以 `"("+id` 断言(:81),同样需改)。
- `client.go:604` 的 `sub.Offset + 1` 不动 ✓。
- **修订**: 集成测试 skip 模式参照 `pkg/redisbroker/cluster_command_bus_test.go:183-212` 的 `requireCommandBusRedis`(同包内可用),而非根目录 `cluster_redis_integration_test.go`(不同包,无法直接复用)。

### Task 3(通配符订阅 + 引用计数)
- `redis.go:18-69` 结构体/New/Subscribe/Unsubscribe 引用准确(`redisBroker` 结构体 :18-26;`New` :30-38;`Subscribe` :56-61;`Unsubscribe` :64-69)。
- `pubsub.go:50-60` 过滤逻辑引用准确。
- `pkg/topics/cstrie.go` 全文复核: **lock-free**(CAS + unsafe.Pointer),无内部互斥锁 → `interested()` 在 `subMu.RLock` 内调 `matcher.Lookup` **无锁序倒置风险**,确认。
- `isWildcard`(`hub.go:74-77`)为包内私有 → redisbroker 内写私有副本 ✓(草案已注明)。
- `matcher.Subscribe(ch, ch)` 以 pattern 字符串作 Subscriber: `topics.Subscription{Topic, Subscriber}` 接口值语义可行(`cstrie.go:171`)。
- **修订**: 单测 `newTestRedisBroker()` 需直接构造结构体(`&redisBroker{subscribed: make(map[string]int), wcCounts: ..., matcher: topics.NewCSTrieMatcher()}` 等),不连 Redis;集成测试用 `requireCommandBusRedis`。

### Task 4(集群级 epoch)
- `redis.go:28-53`(`New`/`Start`)、`Epoch()`(:138-141)、`options.go:30-52`(Options 结构)引用准确;`options.go:10-27` 常量区可加 `defaultEpochKey = "ml:broker:epoch"`。
- 时序确认(`node.go:112-135`): `Node.Run` 等待 `Ready()`;Task 4 的 `Start` 草案中 `initEpoch` 先于 `runPubSubWithRetry` 执行,而 Task 11b 的 `Ready()` 在 PSubscribe 确认后关闭 → **Ready 关闭时 epoch 必然已就绪,无需额外显式同步**。
- `client.go:605` 空 epoch 走全量恢复的防御语义确认(`client.go:605-612`)。
- 修订: 无。集成测试参照 `requireCommandBusRedis`;`docs/deployment.md` 是否存在需在执行时确认(不存在则跳过该文件或补运维注意事项到现文档)。

### Task 5(Admin 强制认证)
- `config/config.go:60-64` `GRPCAdmin.AuthToken` 字段路径确认;`Validate()`(:143-197)确认无强制。
- **修订**: `config/config_test.go` **无 `minimalValidConfig` helper**(:9-90 全部内联构造)→ 测试内联构造或新建 helper。
- `configs/test.yaml`(:1-26)无 `auth_token` 且 `server.grpc_admin.addr` 已配置 → Task 5 后需补 `auth_token` 或 `allow_insecure: true`。
- `cmd/server/runtime_test.go:40` 有 `GRPCAdmin{Addr: addr}` 但不调 `Validate()`(仅 `cmd/server/main.go:34` 调用)→ 不受影响。
- `pkg/grpcstream/admin_server.go:10-14` 与 `server.go:80-103`(interceptor)引用准确;WARN 日志点可放 `PrepareAdminServer`。

### Task 6(心跳默认值)
- `node.go:82-90` 引用准确;`heartbeat.go:19-23`/`:27-29` 确认 `IdleTimeout == 0` 时 manager 不启 goroutine → 默认值必须非零。
- `node_test.go` 无心跳相关既有测试(需新建);`GetHeartbeatIdleTimeout()`(`node.go:552-558`)可观测。
- **行为影响补充**: `pkg/websocket/handler.go:66-69` WS read deadline 未配置心跳时将从 60s 变为 600s(2×IdleTimeout)——写入行为变更清单。

### Task 7(WS 写超时)
- `server.go:32-37`、`transport.go:43-45`、`main.go:196-200` 引用准确。
- **修订**: `pkg/websocket/transport_test.go` 仅 2 个 close-code 测试(:10-17),无 conn mock → 需新建测试基础设施(或仅断言 `DefaultOptions().WriteTimeout == 10s`,阻塞写超时用例参考 `pkg/grpcstream/transport_test.go:192` 的模式评估可移植性;gorilla/websocket 的 net.Conn 需真实连接,若成本过高可只测默认值 + 集成测试)。
- 对齐参照 `pkg/grpcstream/transport.go:19` `defaultWriteTimeout = 10 * time.Second` 确认。
- main.go 修改点: 方案二选一已明确——`cfg.Transport.WebSocket.WriteTimeout == ""` 时保留 `DefaultOptions()` 默认值;`"0"` 显式关闭需文档说明。

### Task 8(入站认证守卫)
- `client.go:325-348` `handleMessage` 分发入口确认;`Authenticated()`(`client.go:727-731`)确认;匿名模式 Connect 成功后 `authenticated = true`(`client.go:504`)→ 不误伤 ✓。
- **受影响既有测试**: `TestClientSession_HandleMessage_Ping`(`client_test.go:333-359`)、`TestClientSession_HandleMessage_SubRefresh`(:741)、`TestClientSession_HandleMessage_RPC` 系列(:513 等)均为"未 Connect 直接发消息"→ 按方案 Step 3 修正(先 Connect)。`TestClientSession_HandleMessage_Unsupported`(:693)先 Connect 后 Unsubscribe,不受影响。
- 未知/空 envelope 的 `default` 分支: `handleMessage` 现无 default(静默返回 nil)→ 草案合理。

### Task 9(匿名接管禁止 + resume 指标)
- `client.go:378-416, 454-530`、`hub.go:681-690`(`ReplaceSession` :669-727)、`client.go:228-243`(`closeQuiet`)、`metricsCharged`(`client.go:103-105`)引用准确。
- 方案 A(metricsCharged 转移)可行: 旧 client 已 Inc(+1),转移后新 client close 时 Dec(-1),平衡。
- `ReplaceSession` 签名变更波及: `client.go:493` 与 `hub_test.go:874`(编译驱动可找齐)。
- `addWithLimit`/`maxConnsPerUser`(`hub.go:151-171`)确认。

### Task 10(lease TTL + CAS)
- `cluster_state.go:16`、`cluster_directory.go:80-123`、`cluster_resume.go:34-88`、`cluster.go:68`(`SessionDirectory` 接口)引用准确;`cluster_state.go:256-258`(leaseVersion 0→1 fallback)确认。
- `fakeSessionDirectory.CAS` 恒 true(`cluster_remote_test.go:29-31`)确认,按版本号模拟可行。
- **确认草案 Step 2.3**: 续约(`syncClusterSessionState`)为拥有者自身调用(handlePing/订阅变化),且 `PutSessionLease` 不递增版本号,与并发 resume 的 CAS 无版本冲突 → 保持无条件 Put ✓。
- CAS 失败断开 code: `DisconnectStale`(3502)确认合适。
- 修订: 无。文档 `docs/developer/04-cluster.md:324`("CAS 需要显式调用方使用")在 Task 10 后需更新(补充修订记录)。

### Task 11(Redis 可靠性三件套)
- `redis.go:73-129`(Publish/PublishTransient)、`pubsub.go:13-80` 引用准确。
- **11a 修订**: go.mod **无 miniredis**(违反"不引入新依赖"约束);`redisBroker.client` 为具体类型 `*redis.Client` 不可 stub → 注入 PUBLISH 失败改用 **go-redis Hook**(`client.AddHook`)在真实 Redis 上做集成测试(env 守卫;本机 Redis 可用)。XDEL 回滚断言可验证。
- **11b 确认**: go-redis v9.18.0,`pubsub.Receive(ctx)` 可阻塞等待 PSubscribe 订阅确认(返回 `*Subscription`),用于关闭 `Ready()`;`runPubSubWithRetry` 重连不重置 ready(readyOnce)✓;`node.go:127-133` 无需修改 ✓。
- **11b 补充行为**: `health.go:33-49` `healthReadyBroker` 对 Redis broker 将自动从"恒 not applicable"变为"ready 前 503"——行为变化,写入变更清单;`docs/developer/05-observability.md:24` 需更新。
- **11c 修订**: 回补仅覆盖精确 channel(通配符缺口)——方案已注明已知限制 ✓;`XRangeN` 与 `streamStartID` 复用确认。

### Task 12(Publication 模型扩展)
- `broker.go:7-14`(Publication)、`:40,47`(Publish/PublishTransient 签名)、`broker_memory.go:112-165`、`pkg/redisbroker/message.go:8-15`、`redis.go:73-129`、`history.go:36-52`、`pubsub.go:62-77`、`node.go:463-488`、`hub.go:309-324, 431-438`、`client.go:919-940, 626-642` 全部引用准确。
- `pkg/grpcstream/api_handler.go`: Publish 在 :25-107(草案引用 42-100)、GetHistory 在 :230-257(草案引用 230-256)——**行号轻微漂移,符号定位一致,标记已漂移(更新行号)**。
- `protocol/server/v1/api.proto:130-135` `HistoryPublication` 确认(offset=1, payload=2, is_text=3, time=4;新字段 id/metadata 取 5/6)。
- `sharedpb.Payload` 已有 `content_type`(`types.proto:11`);`clientpb.Message` 已有 `metadata`(`service.proto:116`)→ 草案第 4 点 `pub.Payload.ContentType` 引用成立。
- 所有 `Broker.Publish` 调用方清单确认: `client.go:940`、`api_handler.go:96`、`node.go:463-488`、`node.go:834-853`(presence);mock broker 清单: `client_test.go` `fakeHistoryBroker`(:1055)、`client_fix_test.go` `fakeEpochHistoryBroker`(:843)、`cluster_resume_test.go` `evictTestBroker`(:19)、`health_test.go` `fakeBrokerNoReady`(:19)、`api_handler_test.go`(Publish 相关 mock)、`pkg/grpcstream` 测试——编译驱动可找齐。
- 草案 `IsText` 保留/删除执行时决定;草案注明"最终字段集以 Task 0 复核结果微调"。
- **修订**: 无阻塞项。`shared/genproto` 为独立 module(需要确认根 module 的引用方式: `go.mod` 中 replace 或独立版本,执行 `task generate-protocol` 时确认;`shared/go.mod` 存在)。

### Task 13(P2 批量)
- 13a: `api_handler.go:150-220`(实际 Subscribe/Unsubscribe :174-208)、`cluster_commands.go:202-220`、`acl.go` 确认。**ACL principal 模型**: `ACLEngine.CanSubscribe/CanPublish(channel, userID)`(`acl.go:79,106`)以 userID 为粒度 → admin 操作使用固定 admin 身份(文档化)或在 Node 层复用 client 检查。
- 13b: `client.go:521-535`、`cluster_resume.go:112-127` 确认;补偿方法 `RemoveSession`(`hub.go:500-502`)与 `deleteClusterSessionState`(`cluster_state.go:217-242`)真实名称确认。
- 13c: 三处确认(见 0-22);第一条测试为回归保护(现状已"不增"),修订说明。
- 13d: `presence_redis.go:40-51`(index TTL = `PresenceTTL*2` vs member TTL = `PresenceTTL`)、`:54-60`(Remove 不清理空 index)、`cluster_projection_repair.go:108-117`(仅 `ReplaceNodeChannels`,无 `owner:*` 扫描)、`node.go:834-853`(`_ =` 忽略错误)确认。
- 13e: `cluster_state.go:274-309`、`cluster_resume.go:115` 确认。**修订**: `ClusterSubscriptionSnapshot.Ephemeral` 字段已存在(`cluster_state.go:49-52`),Task 13e 只需填充,草案"结构需加字段"表述修订。

## 4. 文档修订记录(对方案文档的修订,第二阶段按修订后执行)

| # | Task | 修订内容 |
|---|------|----------|
| R1 | 1 | `JSONCodec.name()` 保留 `"messageloop+json"`(草案 `"json"` 作废;subprotocol 协商与既有断言依赖);encode 采用顶层 `toJson(InboundMessageSchema, msg)`(生成代码无实例 toJson,草案注释二选一已确定);Step 2 预期修正: FAIL 点为 `client_id` 缺失/读不到 `sessionId`/`survey_reply` case undefined(encode 实际输出含 `connect` 键但字段为 camelCase)。 |
| R2 | 2 | 集成测试 skip 模式参照 `requireCommandBusRedis`(`pkg/redisbroker/cluster_command_bus_test.go:183`),非根目录 `cluster_redis_integration_test.go`;`TestStreamOffsetFullRoundTrip` 的 `"("+id` 断言(:81)一并改。 |
| R3 | 3 | `newTestRedisBroker()` 直接构造结构体(不连 Redis);CSTrieMatcher 无锁(lock-free),`subMu.RLock` 内调 `matcher.Lookup` 无锁序倒置;集成测试用 `requireCommandBusRedis`。 |
| R4 | 4 | 时序确认: `Start` 中 `initEpoch` 先于 `runPubSubWithRetry`,结合 Task 11b 的 Ready 语义,无需额外同步;`docs/deployment.md` 若不存在则不创建,运维注意事项并入 `docs/protocol.md` 或现有部署文档。 |
| R5 | 5 | `config_test.go` 无 `minimalValidConfig` helper,测试内联构造。 |
| R6 | 6 | 行为影响补充: WS read deadline 未配置心跳时由 60s 变 600s(`pkg/websocket/handler.go:66-69`),写入行为变更清单。 |
| R7 | 7 | `transport_test.go` 无 conn mock;若阻塞写超时用例成本过高,仅断言默认值(单元)+ 集成覆盖;`"0"` 显式关闭写超时需文档说明。 |
| R8 | 11a | 无 miniredis 依赖;PUBLISH 失败注入改用 go-redis `client.AddHook`(真实 Redis 集成测试,env 守卫;本机可用)。 |
| R9 | 11b | go-redis v9.18 `pubsub.Receive(ctx)` 确认订阅;补充: `health.go` 健康检查对 Redis broker 行为变化(ready 前 503),`docs/developer/05-observability.md:24` 更新。 |
| R10 | 12 | `api_handler.go` 行号漂移: Publish 在 :25-107、GetHistory 在 :230-257;`shared/` 为独立 go module,generate 后确认根 module 引用方式。 |
| R11 | 13a | ACL 以 userID 为粒度,admin 操作使用固定 admin 身份(文档化)。 |
| R12 | 13c | 第一条测试(AddClient sync 失败后 ConnectionsTotal 不增)现状已满足,属回归保护而非 TDD 失败测试。 |
| R13 | 13e | `ClusterSubscriptionSnapshot.Ephemeral` 字段已存在(`cluster_state.go:49-52`),只需填充,无需加字段。 |
| R14 | 10/11b 联动 | `docs/developer/04-cluster.md:324`(CAS 无调用方表述)与 `docs/developer/05-observability.md:24`(Redis broker 恒 not applicable)在对应 Task 落地后更新。 |

## 5. 阻塞项

**无阻塞项。** 25 项证据全部确认,无推翻;Task 1-13 草案的修订均为非阻塞的"行号/参照/表述"级调整(R1-R14),已在本报告第 4 节登记,第二阶段按修订后内容执行,并在执行报告的"与方案文档的偏差"一节引用本表。

复核结论: **通过,进入第二阶段(Task 1 起按序执行)。**
