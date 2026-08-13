# MessageLoop 评审修改方案（核实后终稿）

> 来源：`docs/review/summary.md`（8 个模块评审报告）。
> 核实方式：主 agent 派 4 个独立核实 agent 逐条对照源码验证全部 Critical/Important 条目，抽查 Minor 条目，并对关键冲突点（行尾、TLS ServerName 等）做了复核。
> 核实结论：约 100 条 findings 中，**无一条被完全推翻**；4 条部分修正定性；2 条评审阶段已被评审 agent 自行推翻的线索不列入。核实中新发现 8 条遗漏问题，已并入。
> 每条目注明状态：✅ 确认 / 🔶 部分确认（定性修正见备注）。

---

## P0 — Critical（正确性/崩溃/数据丢失，立即修复）

| # | 问题 | 位置 | 状态 | 修复要点 |
|---|---|---|---|---|
| P0-1 | TS SDK：pong 超时调 `close()` 永久杀死客户端，重连被自身取消 | `sdks/ts/src/client/client.ts:468-471` | ✅ | 改走 `handleDisconnect()`（保留 `isClosedFlag=false`），一行级修复 |
| P0-2 | Go SDK：`handleConnected` 与 `Close()` 竞态 → `close(nil)` panic，进程崩溃 | `sdks/go/client.go:296-308,830-838` | ✅ | close 前判 nil，或 Close 后置已关闭哨兵 channel |
| P0-3 | TS SDK：`recv()` 的 close/error 无法可靠传播：onclose 不 resolve 挂起 resolver（永久挂起+监听器泄漏），error 靠"yield null → 解析崩溃"偶然传播 | `sdks/ts/src/transport/websocket.ts:172-214` | ✅ | close 时 reject/throw 结束迭代器；errorHandler 检查 `done` 并抛真实错误 |
| P0-4 | TS SDK：error 信封不路由 pendingRPC（干等 30s），且 connected 状态下任何 error 信封（ACL 拒绝/RPC 超时）都触发整连接断开重连；与 Go SDK 行为分歧 | `sdks/ts/src/client/client.ts:276-303` | ✅ | 按 id reject pendingRPC；已连接时仅调 errorHandler，不重连 |
| P0-5 | TS SDK：`Connected.publications`（离线恢复消息）被静默丢弃且不更新 offset，下次重连以旧 offset+1 恢复 → 永久跳过消息 | `sdks/ts/src/client/client.ts:189-214` | ✅ | 对齐 Go SDK `client.go:319-333`：投递恢复消息并更新 `channelOffsets` |
| P0-6 | TS SDK：浏览器下 protobuf 完全不可用（未设 `binaryType="arraybuffer"`，收到 Blob 无法 decode）；Node 侧 `ws` 默认值掩盖了该问题 | `sdks/ts/src/transport/websocket.ts:34-43` | ✅ | dial 后按环境设置 binaryType；decode 支持 Blob 分支 |
| P0-7 | 服务端：ephemeral 订阅仍登记 presence 并发布 join/leave——文档与协议承诺"不登记"（`docs/protocol.md:166,365`），属于文档承诺但代码未实现 | `client.go:626-633,1050-1056`；`cluster_resume.go:143` | ✅ | connect/subscribe/restore 三处跳过 ephemeral 的 `presence.Add` 与事件发布，补测试 |

## P1 — Important（健壮性/并发/契约，本迭代修复）

### A. 服务端核心（根包）

| # | 问题 | 位置 | 状态 | 修复要点 |
|---|---|---|---|---|
| P1-A1 | connect 路径普通错误（`AddClient`/`syncClusterSessionState`/`resumeRemoteSession`）导致半打开连接：只发 INTERNAL_ERROR 不断连，连接处于半注册态且无法再 Connect | `client.go:526-559,305-321` | 🔶 | 失败统一包装为 `Disconnect` 或显式 `c.close(...)`。备注：remote resume 失败路径 `authenticated` 尚未置 true，但 `c.session` 已被改写，定性仍为半打开 |
| P1-A2 | resume 失败僵尸会话：`closeQuiet(old)` 后 `ReplaceSession` 失败直接返回，旧会话永久残留 hub（订阅照收广播、投递报错累加 `DeliveryFailures`） | `client.go:517-522`；`hub.go:501-519,673-682` | 🔶 | 失败分支回滚：`RemoveSessionIfMatches(sessionID, oldSession)` + 清理 cluster 状态。备注：`syncClusterSessionState` 失败产生的是"新连接半打开"而非僵尸（ReplaceSession 已成功），并入 A1 修复 |
| P1-A3 | `close()` 与并发 `Subscribe` 的订阅泄漏窗口（核实新发现）：锁外拷贝频道列表后 16-worker 才移除，期间新增订阅不被清理 | `client.go:147-169` | ✅（新） | `handleSubscribe` 加 status 检查，或 close 清理期间阻塞新增订阅 |
| P1-A4 | `ClientInfo()` 无锁读取 `c.client/c.session/c.user`，`-race` 可复现（当前无生产调用方，潜伏） | `client.go:754-763` | 🔶 | 加 `c.mu.RLock()` 快照或复用三个加锁 getter。备注：`connectedAt` 不可变，非竞态字段 |
| P1-A5 | close/handleUnsubscribe 按频道各起一个无上限 presence goroutine（单连接 10k 订阅断开 = 10k goroutine + Redis 往返，DoS 面） | `client.go:183-189,1117-1120` | ✅ | 复用 16-worker 模式或信号量 |
| P1-A6 | broker 启动失败 goroutine 内 `panic(err)`：Redis 连接失败 → `Run()` 已返回后进程崩溃，lynx 无法感知（核实后由 Minor 升级） | `node.go:124-130` | ✅ | 错误经 ready 通道并入 `Run` 返回路径，goroutine 内只记日志 |

### B. Proxy 与传输

| # | 问题 | 位置 | 状态 | 修复要点 |
|---|---|---|---|---|
| P1-B1 | HTTP proxy payload 编解码实质损坏：oneof 经 `encoding/json` 永不还原，RPC 载荷全丢（功能残废，现有测试只断言非 nil 被绕过） | `proxy/http.go:87-92,103-107` | ✅（实证） | 改用 `protojson.Marshal/Unmarshal`；补 `TestHTTPProxy_RPC_PayloadRoundTrip` |
| P1-B2 | WS 子协议协商：marshaler 由 offer 列表 `strings.Contains` 决定，帧类型由协商结果决定——两者可不一致（文本帧载 protobuf 字节），连接不可用 | `pkg/websocket/handler.go:46-48,111-119` | ✅ | 用 `conn.Subprotocol()` 精确 switch，删除子串匹配；补协商矩阵测试 |
| P1-B3 | gRPC `sendWithTimeout` 共享 timer：enqueue 逼近 deadline 时第二个 select 立即假超时 → 健康连接被误判慢消费者断开 | `pkg/grpcstream/transport.go:63-82` | ✅ | enqueue 成功后重置 timer 或为 ack 阶段独立计时 |
| P1-B4 | gRPC `Close` 断连帧竞态：sendCh 满时 Close 阻塞 10s 且断连帧未入队；worker 在 sendCh/closeCh 同就绪时可丢弃 DISCONNECT_ERROR | `pkg/grpcstream/transport.go:84-104,139-148` | ✅ | worker 退出前排空 sendCh；writeError 失败降级为直接关闭 |
| P1-B5 | HTTP proxy 非 200 只把 body 文本拼进 error，后端无法用结构化 `sharedpb.Error` 表达 HTTP 级错误；另 RPC 请求 `Meta` 被丢弃（核实新发现） | `proxy/http.go:392-395,87-92` | ✅ | 非 200 先尝试解析结构化 error；请求体补 `metadata` 透传 |
| P1-B6 | WS Transport `Close` 在 WriteControl/SetReadDeadline 失败时跳过 `conn.Close()`，对端 RST 时 fd 泄漏 | `pkg/websocket/transport.go:56-91` | ✅ | `defer t.conn.Close()` |

### C. Broker 与集群

| # | 问题 | 位置 | 状态 | 修复要点 |
|---|---|---|---|---|
| P1-C1 | 集群命令总线断线不重连：pubsub 断开后 reader 静默退出，节点永久停止处理集群命令直至重启，无日志无指标 | `pkg/redisbroker/cluster_command_bus.go:129-140` | ✅ | 仿 `runPubSubWithRetry` 加退避重连；断线打 Warn + 计数 |
| P1-C2 | `deliverOnce` 全局 `deliverMu` 把 handler（含客户端网络写）关进临界区：一个慢消费者阻塞所有频道投递与 catch-up | `pkg/redisbroker/pubsub.go:163-190` | ✅ | handler 移出临界区（check+record 在锁内），或按频道分片锁 |
| P1-C3 | Redis broker handler 错误 `_ =` 吞掉且 panic 无 recover（内存 broker 会传播错误，两实现契约不一致；panic 会炸掉 pubsub 协程） | `pkg/redisbroker/pubsub.go:167-169,187-189` | ✅ | 记指标/日志 + recover；接口注释明确异步投递契约 |
| P1-C4 | 断线 catch-up 存在无提示消息缺口：XRangeN 上限截尾 + go-redis 100 条缓冲满静默丢弃，客户端无 gap 感知 | `pkg/redisbroker/pubsub.go:107-156` | ✅ | catch-up 后校验断层并显式通知（gap 信封） |
| P1-C5 | `waitForReply` 截止时刻 `ctx.Done()` 与 reply 通道关闭竞争，随机返回硬错误（实测偶发测试失败） | `pkg/redisbroker/cluster_command_bus.go:232-257` | ✅ | deadline 时优先走 `resolveTimedOutCommand`；测试放宽 |
| P1-C6 | takeover 驱逐用无条件 `RemoveSession` 而非 `RemoveSessionIfMatches`，存在误删新会话窗口 | `cluster_resume.go:275-277` | ✅ | 改为 `RemoveSessionIfMatches(sessionID, client)` |

### D. Topics 与协议

| # | 问题 | 位置 | 状态 | 修复要点 |
|---|---|---|---|---|
| P1-D1 | optimized bitmap `Unsubscribe` 不清理尾部 `empty` constituent 位，pos 复用后产生误投递（探针已复现 `Lookup("b.c")` 误中） | `pkg/topics/optimized_inverted_bitmap.go:110-124` | ✅（复现） | 清理循环扩展到 `maxConstituents`，区分 constituent 与 empty |
| P1-D2 | 位图 matcher 重复 `Unsubscribe` 同一 ID → pos 别名：两个订阅共享 ID、互相覆盖（公共 API 潜伏炸弹；当前调用方靠引用计数规避） | `pkg/topics/inverted_bitmap.go:69-77`；`optimized_inverted_bitmap.go:110-124` | ✅（复现） | 回收前检查 `subscribers[sub.ID]` 存在；接口文档约定幂等性 |
| P1-D3 | 空分段/空 topic 语义不一致：optimized 拒绝其余 4 种接受的输入，且注释声称"为一致而拒绝"（事实相反，测试固化了不一致） | `pkg/topics/optimized_inverted_bitmap.go:73-80,132-138` | ✅ | 统一五实现语义（建议统一拒绝空分段），修正注释与测试 |
| P1-D4 | 重复订阅语义不一致：位图为多重订阅，其余三种按 Subscriber 幂等；`Subscription.ID` 仅位图使用，差异隐身于公共接口 | `pkg/topics/inverted_bitmap.go:30-67` 等 | ✅ | `Matcher` 接口文档明确语义；或位图按 (topic, subscriber) 去重 |
| P1-D5 | 4 个 `.proto` 的 `go_package` 缺 `/shared/`：当前靠 `paths=source_relative` 碰巧能工作，未来任何跨 proto import 必然编译失败 | `protocol/{client,server,proxy,event}/v1/*.proto` | ✅ | 改为 `.../shared/genproto/<pkg>/v1;<alias>`，`task generate-protocol` 重新生成 |

### E. 配置、启动与可观测性

| # | 问题 | 位置 | 状态 | 修复要点 |
|---|---|---|---|---|
| P1-E1 | 默认配置无法启动 + 文档命令错误（合并 05-3/05-4/08-F5/F19）：`config.yaml`/`config-node1/2.yaml` 均缺 `grpc_admin` 段必启动失败；AGENTS.md/deployment.md 的单文件 `go run cmd/server/main.go` 编译失败；`Validate()` 允许仅配 gRPC 但 WS 服务器无条件构造（空 addr 落 80 端口、空 path panic） | `config.yaml` 等；`cmd/server/main.go:65-70`；`config/config.go:147-150` | 🔶 | ① 三份配置补 `grpc_admin`；② 文档改 `go run ./cmd/server`；③ `Validate()` 加 WS addr/path 联动校验与 gRPC 必填校验。备注：空 addr 绑 80 在多数环境因权限失败而非"静默成功"；config.yaml 还默认依赖 Redis |
| P1-E2 | `transport.websocket.read_timeout` 死字段：声明+校验+文档俱全，`newWebSocketServer` 从未赋值，显式配置完全无效 | `config/config.go:87,159`；`cmd/server/main.go:189-195` | ✅ | 装配时解析赋值（沿用 WriteTimeout 模式），补生效性测试 |
| P1-E3 | 默认 `config.yaml` 安全姿态（核实后由 Minor 升级）：无鉴权 `/health`/`/metrics` 绑 `:8080` 全接口、Redis 明文密码、废弃的 `check_origin: true` | `config.yaml:3,12,21` | ✅ | 回环绑定、密码占位符、改 `allowed_origins` |
| P1-E4 | CI 缺口：无 `buf generate` 产物一致性校验（proto 改了不重新生成也能过 CI）；golangci-lint 用 `latest` 不可复现 | `.github/workflows/ci.yml` | ✅ | 加 `buf generate && git diff --exit-code`；固定 lint 版本 |

### F. Go SDK

| # | 问题 | 位置 | 状态 | 修复要点 |
|---|---|---|---|---|
| P1-F1 | `PingTimeout` 字段存在但零实现（`handlePong` 空、WS 无 read deadline）：半开连接永不发现、自动重连永不触发 | `sdks/go/options.go:154-158`；`client.go:1074-1078` | ✅ | 实现 pong 超时：超时主动 Close transport 触发重连；先写失败测试驱动 |
| P1-F2 | 未实现 SubRefresh/Survey：收到 `SurveyRequest` 静默丢弃，客户端无法参与 cluster survey；服务端已完整实现 | `sdks/go/client.go:249-281` | ✅ | 至少提供 Survey 应答回调 + `SendSurveyReply`；SubRefresh 可后置 |
| P1-F3 | ephemeral 完全不支持（6 处硬编码 `Ephemeral:false`），协议与服务端均支持 | `sdks/go/client.go:187-191,437-443,470-476` | ✅ | 加 `SubscribeWith(channel, opts)` 变体 |
| P1-F4 | `Connect()` 失败/重试不清理：不 close transport、不停 receiveLoop；重试复用同 transport 产生双 gen-0 receiveLoop | `sdks/go/client.go:198-215` | 🔶 | 失败统一 close + 停 loop；重试推进 generation 换新 transport。备注："第二个 Connect 被服务端判 BadRequest"仅在首次已认证后成立 |
| P1-F5 | 自动重连后手动 `Connect()` 必然挂起：gen=0 启动而 generation 已 >0，Connected 被永久丢弃，阻塞至 30s 超时 | `sdks/go/client.go:203,724,288-290` | ✅ | `Connect()` 同样 `generation.Add(1)` + 新 transport |
| P1-F6 | `HandlerImpl.RPC/Authenticate` 对 handler 返回 `(nil,nil)` 无防护 → gRPC handler panic → proxy 进程崩溃 | `sdks/go/proxy.go:246,296-298` | ✅ | nil 判定时返回 `codes.Internal` |
| P1-F7 | `resumed=true` 时不回写服务端下发的订阅列表：集群快照恢复的频道不在本地 map，下次重连丢失 | `sdks/go/client.go:310-317` | ✅ | 无条件以服务端列表为准写回 |
| P1-F8 | `OnSubscribed/OnUnsubscribed` 只传 ctx，丢弃 proto 中的 `session_id/channel/username`，生命周期钩子失去意义 | `sdks/go/proxy.go:88-98,348-369` | ✅ | 接口加参数（breaking change，同步示例/测试） |
| P1-F9 | `Unsubscribe` 不清 `channelOffsets`：退订重订后重连按旧 offset 恢复 → 收到退订期间的历史消息 | `sdks/go/client.go:356-362` | ✅ | 退订时删除 `channelOffsets[ch]` |

### G. TS SDK（P0 之外）

| # | 问题 | 位置 | 状态 | 修复要点 |
|---|---|---|---|---|
| P1-G1 | `reconnect()` 发 Connect 后不等待 Connected：握手成功但服务端不回复时永久卡 "connecting" | `sdks/ts/src/client/client.ts:379-415` | ✅ | 复用 `waitForConnection` 的等待+超时逻辑 |
| P1-G2 | `close()` 固定 `setTimeout(100)` 不等真实 close 事件，且不 flush sendQueue（挂起的 publish 永不 settle）；另 close 与 in-flight reconnect 竞态可"复活"已关闭客户端 | `sdks/ts/src/transport/websocket.ts:216-225`；`client.ts:399-407` | ✅ | close 事件驱动 + 超时兜底；dial 后回查 `isClosedFlag` |
| P1-G3 | 认证失败 UX：connecting 状态收到 error 信封不 reject `waitForConnection`，真实原因（InvalidToken）被 30s 笼统超时吞掉 | `sdks/ts/src/client/client.ts:160-180` | ✅ | connecting 阶段 error 信封 reject 并携带 code |
| P1-G4 | 依赖与默认值：`ws` 在 devDependencies（Node 18/20 必缺）；`@grpc/grpc-js` 幽灵 peer dep；`headers` 选项是死代码；autoReconnect/connectTimeout 默认值与 Go SDK 漂移 | `sdks/ts/package.json:42-54`；`websocket.ts:101-107` | ✅ | ws 移 dependencies 或 peer+optional；删 grpc-js；headers 走构造参数；对齐默认值 |
| P1-G5 | TS 重连后不同步服务端 `subscriptions` 列表、退订不清 `channelOffsets`（核实新发现，与 Go F7/F9 同根）；会话恢复条件与 Go 分歧（`recover` 仅在 epoch 非空时置位） | `sdks/ts/src/client/client.ts:188-214,500-503,572-580` | ✅（部分新） | 三项对齐 Go SDK 语义 |
| P1-G6 | 类型/API 一致性：`IClient.publish` 缺 `transient`；无 `setReconnectBackoff` multiplier；`sendQueue` 递归无背压；双套 handler API 并存重复投递 | `types.ts:26`；`options.ts:178-183`；`websocket.ts:144-162`；`client.ts:41-51` | ✅ | 接口补齐 + `implements IClient`；while 循环 + 队列上限；统一单套 handler API |

---

## P2 — Minor（排期批量处理）

### 服务端核心
- `statusConnected` 死常量（生产只有"0→closed"两态）：补赋值或删常量+改文档（`client.go:128-134`）✅
- `subShard.broadcastPublication` 仅测试调用的 ~80 行重复实现，且无 exact+wildcard 去重：删除，测试改走 Hub 层（`hub.go:291-375`）✅
- ACL `path.Match` 语义：`chat.**` 无特殊语义、`*` 跨点匹配与 CSTrie 单段语义不一致、管不到 `*/__presence`：统一为分段通配或修正文档+补测试（`acl.go:84,111`）✅
- `GetActiveChannels` 把通配模式当频道列出并重复计数（`hub.go:624-657`）✅
- `ReplaceSession` 连接数检查 TOCTOU（`hub.go:673-698`）✅
- `PresenceInfo.ClientID` 实为 session ID，命名误导（`client.go:628-632`；文档已如实记录）✅
- `Survey.Close` 单条 drain + channel 永不关闭 + "channel full" 假警报（`survey.go:174-181,107-108`）✅
- 订阅上限检查：重复频道/ACL 拒绝频道虚增计数（`client.go:591-598,1012-1019`）✅
- 未认证 connect 把客户端伪造的 session ID 传给认证代理（`client.go:387-391,431`）✅
- 单客户端并发 survey 响应经 `lastSurveyRequestID` 单槽回退串路由（`client.go:1274-1279`）✅
- `hub.broadcastPublication` 串行分支缺 panic recover，与并行分支不一致（核实新发现，`hub.go:447-458`）✅

### Broker 与集群
- `runPubSub`/`catchUpMissed` 重复反序列化代码块：抽 `messageToPublication`（`pubsub.go:78-97,130-153`）✅
- `lastOffsets` 退订不清理，按频道名无限增长（`pubsub.go:184`）✅
- 内存 broker history map 按频道名无限增长（设计意图已确认，建议文档化差异）（`broker_memory.go:112-118`）✅
- `executeHandlerBounded` 超时 handler 继续占信号量槽（已知权衡，文档注明 handler 必须响应 ctx）（`cluster_command_bus.go:603-619`）✅
- `clusterSessionSnapshot.ChannelOffsets/BrokerEpoch` 从未填充——跨节点精确续读未完成功能（`cluster_state.go:286-325`）✅
- presence `Remove` 的 SCard→DEL 与并发 Add 竞态（"在线但不可见"幽灵窗口）：Lua 原子化（`presence_redis.go:57-72`）✅
- `SendCommand` 不预检目标节点租约，目标已死白等 5s（`cluster_command_bus.go:222-224`）✅

### Proxy 与传输
- `Router.Close` 只保留最后 error → `errors.Join`（`proxy/router.go:84-96`）✅
- `Router.AddFromConfig` 失败留下半初始化路由表（`proxy/router.go:74-81`）✅
- admin Publish 与 `handlePublish` 的 payload→Publication 转换重复（含 `payloadBytes`）：根包抽共享函数（`api_handler.go:35-59,283-297` vs `client.go:956-976,1209-1222`）✅
- WS handler 升级失败/NewClient 失败后重复 `WriteHeader(500)`（`pkg/websocket/handler.go:39-55`）✅
- `GRPCProxyConfig` 缺 `ServerName/InsecureSkipVerify` 字段（注：原线索"不校验 ServerName"已被推翻——grpc-go 自动从 authority 派生校验；残余仅为配置缺口）（`proxy/grpc.go:44-46`）🔶

### Topics 与协议
- cs-trie CAS 重试无界递归（ABA 不成立、概率极低）：改有界循环（`cstrie.go:168,231,302`）✅
- `cleanParent` 重试参数错位、重试恒 no-op（惰性清理兜底，不影响正确性）（`cstrie.go:421-423`）✅
- `MarshalTypeError`/`UnmarshalTypeError` 错误串完全相同；`ProtoJSONMarshaler.Name()` 返回 `"json"` 冲突且不在 `Marshalers` 列表（`shared/marshaler.go:126-152`）✅
- `naive.Unsubscribe` for-range-continue 绕路写法 + 空 topic map 不清理；`topicMatches`/`matchCriteria` 重复（`naive.go:30-43,70-87`）✅
- `Error` 消息无错误码枚举，`code`/`type` 职责重叠（`protocol/shared/v1/errors.proto`）✅
- `json_name` 显式注解在 `UseProtoNames:true` 下全部失效（死注解）（`protocol/client/v1/service.proto`）✅
- `Subscriber` 空接口无 comparable 约束，传不可比较值运行时 panic（核实新发现，5 实现同病）✅
- `optimized_inverted_bitmap.Unsubscribe(nil)` nil 解引用（核实新发现）✅

### 配置与可观测性
- `runNodeWithPreflight`/`preparedGRPCServers.Close()` 死代码（无实际泄漏，纯残留）（`cmd/server/runtime.go:17-39,83-90`）✅
- `setupCluster` 双重 `NewCluster` + `SetPresenceStore` 副作用（`cmd/server/main.go:101-143`）✅
- `ToProxyConfig` 丢失 `Timeout`，解析逻辑分散两处（`config/config.go:115-123`）✅
- `consumer_group` 死字段；`stream_approximate:false` 被静默忽略（均已文档化，建议消费或删除）（`config/config.go:143`；`options.go:111-113`）✅
- 指标全无 label（无法按节点/频道细分）+ registry 不含 `go_*`/`process_*`（`metrics.go:26-98`；`cmd/server/main.go:39`）✅

### Go SDK（Minor）
- `Build*` 构造器吞 `ToPayload` 错误（`client.go:933,947,1008`）✅
- 回调 handler 字段无锁读写竞态（`client.go:79-83,609-631`）✅
- `RPC` 无默认超时，连接死亡时挂起（`client.go:575-605`）✅
- `ReceivedMessage` 死代码，`OnMessage` 与 TS 语义不一致（`message.go:315-325`）✅
- `NewProxyServer` 零值 `Insecure=false` 与服务端 TLS 拨号不匹配（`proxy.go:397-409`）✅
- 订阅/发布级 `token`、`PublishAck` 未暴露（`client.go:510-519,274-276`）✅

---

## P3 — 文档修正批次（纯文档/卫生，成本低，可一次性完成）

1. **心跳**：`02-configuration.md:44,103,357` + `defaults.go` ——"空=禁用心跳"改为"回退 300s；`0s` 才禁用（未文档化行为）"✅
2. **History**：`04-cluster.md:295,356` + `03-admin-api.md:311-317,554` —— exclusive 改 inclusive（代码两实现已一致）✅
3. **epoch**：`04-cluster.md:236-238` + `01-architecture.md:177` —— 改为"Redis epoch 集群共享、跨重启持久"；区分内存/Redis 实现 ✅
4. **TTL**：`04-cluster.md:201,308` 会话租约 90s→600s；`:281` presence 索引 120s→60s ✅
5. **admin 鉴权**：`README.md:44-61` 补 auth_token；`02-configuration.md` 补 `allow_insecure` 字段与第 6 条校验规则；`03-admin-api.md:35` "空=不鉴权"已过期 ✅
6. **deployment.md**：`:172` 断连码 DisconnectStale→DisconnectIdleTimeout(3511)；`:18-23` 监听器"默认值"表仅 HTTP 有默认，其余必填（`06-development.md:208` 同病）✅
7. **断连码标注**：`protocol.md:341-355` 为 3502/3506-3509 加"当前未产生/语义迁移"标注；`05-observability.md:154` 的 3502 改为"是（集群恢复失败）"✅
8. **protocol.md**：`:62-76` OutboundMessage 表补 `sub_refresh_ack` 行 ✅
9. **gRPC 断连码**：`01-architecture.md:397`/`05-observability.md:145` 声称 code 完整传递，实际数值码丢失（`pkg/grpcstream/transport.go:106-121`）——建议改代码把数值码编入错误信封 ✅
10. **read_timeout**：`02-configuration.md:150` "心跳禁用默认 60s"分支不可达，实际默认 2×idle=600s ✅
11. **TS SDK 版本号**：`08-sdk-ts.md:5`、`06-development.md:242` 的 1.0.5→1.1.0 ✅
12. **03-admin-api.md:565**："Go 与 TS SDK 的后端集成"——TS 无管理 API 客户端，改为仅 Go ✅
13. **信任边界**：`deployment.md` Multi-Node 章节补"Redis 网络隔离是集群安全前提"✅
14. **归档**：`docs/fix-plan.md`、`RPC_TIMEOUT.md`、`docs/superpowers/plans/` 移入 `docs/archive/` 或加"已完成"横幅 ✅
15. **删除**：`sdks/ts/src/proto/v1/service_pb.ts`（旧布局残留）、`server.exe`、`nul`（工作区残留，未追踪）✅
16. **行尾**：经复核，仓库内 128 个 `.go` 文件 **全部以 CRLF 存储、无混用**（模块 02 报告"混用"与模块 08"统一 LF"均不准确）；无 `.gitattributes`，建议添加以锁定归一化策略 ✅（已复核）
17. **TS lint**：`npm run lint` 指向未安装的 eslint；`dist/` 有旧构建残留 ✅

---

## 测试补充清单（按投入产出排序）

1. **HTTP proxy payload 往返测试**（P1-B1 回归，首个应补）：断言 `resp.Payload.GetData()` 非 nil
2. **TS SDK 真实 WebSocket 集成测试**：当前 30 个用例无一真实 socket，覆盖 dial→Connected→pub/sub 闭环、断线检测、重连状态机
3. **WS 子协议协商矩阵测试**（P1-B2 回归）：offer 顺序/未知协议/无协议 × marshaler 与帧类型一致
4. **位图 matcher 回归**：stale-empty 位复现序列（P1-D1）；重复卸载 pos 别名（P1-D2）；五实现差分测试纳入空分段/卸载阶段
5. **connect 失败路径**：ReplaceSession 失败无僵尸（P1-A2）；AddClient/cluster sync 失败必断连（P1-A1）
6. **Go SDK pong 超时**（先写失败测试驱动 P1-F1 实现）；`handleConnected`×`Close` 并发（P0-2 回归）
7. **命令总线重连测试**（P1-C1 修复后）；`waitForReply` 截止竞争（P1-C5）
8. **deliverOnce/catchUpMissed/redisClusterQueryStore/clusterNodeLeaseManager 单元测试**（当前零覆盖）
9. **配置-启动一致性测试**：逐份 yaml 断言 `Validate()` 通过且可预绑定，防"默认配置无法启动"复发
10. **ephemeral presence 测试**（P0-7 回归）：ephemeral 订阅不登记 presence、无 join/leave 事件
11. `heartbeat_test.go`、`subscription_saga_test.go`、`shared/marshaler` 单测（该 module 零测试文件）、`Node.Shutdown/DrainAll`

---

## 核实阶段的定性修正记录（供追溯）

| 原条目 | 修正 |
|---|---|
| 03-06 gRPC TLS ServerName | 推翻"不校验 ServerName"：grpc-go 从 authority 自动派生校验；残余仅为 `GRPCProxyConfig` 配置缺口（降 Minor） |
| 01-ClientInfo 竞态 | `connectedAt` 不可变非竞态；当前无生产调用方，属潜伏 API 缺陷 |
| 01-僵尸会话 | `syncClusterSessionState` 失败子路径定性为"新连接半打开"（并入 P1-A1），非僵尸 |
| 06-发现 B | "第二个 Connect 被服务端判 BadRequest" 仅在首次已认证后成立；核心危害是双 receiveLoop 泄漏 |
| 02-行尾混用 / 08-统一 LF | 均不准确：repo 内 .go 全部 CRLF 存储、无混用（主 agent 复核） |
| 04-线索 3/5（cs-trie 语义、naive 基准误用） | 评审阶段已自行推翻，不列入方案 |
| 05-13 broker panic、05-11 config.yaml 安全姿态 | 由 Minor 升级为 Important（P1-A6/P1-E3） |
| 08-分片常量线索 | 不成立：64/16384 与 AGENTS.md 一致，文档准确 |

**核实新发现并已并入方案的 8 条**：close()×Subscribe 订阅泄漏窗口（P1-A3）；hub 串行广播无 recover（P2）；HTTP proxy 丢弃 Meta（P1-B5）；WS NewClient 失败写 500（P2）；`Subscriber` comparable 约束（P2）；`Unsubscribe(nil)` panic（P2）；TS 不同步 subscriptions + 不清 channelOffsets（P1-G5）；`05-observability.md`/`06-development.md` 两处文档同病（P3-6/7）。
