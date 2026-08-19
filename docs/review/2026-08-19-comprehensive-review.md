# MessageLoop 全面评审报告（2026-08-19）

> 第二轮全面评审，基线为 D15 重构完成后（`77c8b78`，根包已清空、`internal/runtime` 已就位）。
> 方法：9 个并行评审代理按维度深入阅读源码，所有发现均基于实际代码核实。
> 严重级别沿用本目录约定：Critical（正确性/数据丢失/安全）/ Important（健壮性/并发/资源）/ Minor（可读性/一致性/小改进）。

## 评审维度

1. 架构与模块边界
2. 并发与正确性
3. 代码规范符合性（对照 AGENTS.md）
4. 错误处理与断开语义
5. 协议与序列化一致性
6. 测试质量与覆盖
7. 安全
8. 配置与部署
9. 性能热点

---

## Critical（4 项）

### C1. HTTP 代理 JSON 方言混用，认证路径静默丢字段

- **位置**：`proxy/http.go:162/198/234`
- **问题**：RPC 路径特意用 protojson（注释注明 encoding/json 无法 round-trip oneof），但 `Authenticate`/`SubscribeAcl`/`PublishAcl` 却用 `encoding/json.Unmarshal` 解析 protobuf 生成结构体。proto3 JSON 标准输出为 camelCase（`userInfo`），生成代码的 json tag 是 `user_info`，两者不匹配时 `json.Unmarshal` **不报错、静默丢字段**——认证成功但 `UserInfo` 为 nil，会话以空 user 身份通过认证。同一代理包内请求/响应还存在 snake_case（手写 map）与 protojson 两种线格式。
- **修复**：统一用 `protojson.UnmarshalOptions{DiscardUnknown: true}`，与 `doRequest` 错误路径一致。
- **置信度**：high

### C2. 广播路径每条消息重复序列化 N 次

- **位置**：`internal/session/hub.go:387-441` + `internal/session/session.go:682/693`
- **问题**：广播时所有订阅者共享同一个 `OutboundMessage`，但每个 `Send` 都走 `enqueue` 独立 `MarshalAppend`；`session.go:693` 又 `append([]byte(nil), ...)` 全新拷贝后立刻归还池化 buffer（`pool.go` 的 `sync.Pool` 只省下了 marshal 缓冲区）。N 个订阅者 = N 次序列化 + N 次堆分配。
- **修复**：按 marshaler 类型缓存一份字节，帧所有权移交 writer，写完后归还池。
- **置信度**：high

### C3. WS 关闭握手与读循环并发读同一连接

- **位置**：`pkg/transport/ws/transport.go:82-90` vs `pkg/transport/ws/handler.go:77`
- **问题**：`Close` 在发完 close 帧后用 `NextReader` 循环排空；而读循环仍阻塞在 `ReadMessage`。Close 可由心跳 goroutine（`heartbeat.go:160-168`）、writer 错误路径（`session.go:595-612`）、DrainAll 触发，均与读循环并发。gorilla/websocket 明确要求单 reader，两个 goroutine 并发解析帧是数据竞争（`-race` 可复现）。
- **修复**：Close 只做 `WriteControl` + `conn.Close()`，排空交给读循环。
- **置信度**：high

### C4. 广播写路径同步等待且 ctx 不可取消

- **位置**：`internal/session/hub.go:353/398/427` + `internal/session/session.go:707-712` + `internal/stream/broker_memory.go:234-248`
- **问题**：`client.Send` 最终阻塞在 `<-frame.done`，而 ctx 是 `context.Background()`。memory broker 下 handler 在发布者的读循环里同步执行，**最慢的订阅者决定发布者 ack 延迟**；redis broker 下 `dispatch` 阻塞式入队（`pkg/redisbroker/pubsub.go:337`），一个卡死的客户端占住一个 delivery worker，16 个全卡则全站实时投递停摆。WS/QUIC 允许 `WriteTimeout=0` 显式禁用写超时（`ws/server.go:34`、`ws/transport.go:44`），此时死连接可无限阻塞整条链。
- **修复**：广播用带超时的 ctx，或禁止将写超时配为 0。
- **置信度**：high

---

## 1. 架构与模块边界

依赖方向经 `go list` 验证无 import 环。叶子包（cluster/protocol/stream/topics）职责单一、分层正确；问题集中在编排层与会话层。

**Important**

- `internal/session/session.go:617-626`：`isPeerClosedError` 在号称 transport 无关的 session 核心中直接 import `gorilla/websocket` 和 `grpc/codes`/`grpc/status` 做错误嗅探，核心包编译期耦合两个具体 transport 实现。修复：由各 transport 的 Write 把"对端关闭"归一化为 sentinel error，session 只做 `errors.Is`。
- `internal/runtime/node.go:374` vs `:451` vs `:544`：同一 `syncClusterSessionState` 有三套上下文策略——`AddClient` 用无超时的 `context.Background()`（Redis 黑峰会无限阻塞连接接入）、`AddSubscription` 透传调用方 ctx、`RemoveSubscription` 用 `clusterStepTimeout`。修复：统一为带超时的独立 ctx。
- `internal/runtime` 是事实上的 God 包：`Node`（node.go:24-53）同时持有 hub、broker、presence、cluster、`proxy.Router`、heartbeat、survey registry、occupancy 去重表、metrics；node.go 1555 行外加 10+ 个 cluster_*.go。`aliases_local.go` 近 300 行从 8 个 internal 包再导出约 150 个符号——"平移式重构"的未完工状态。建议按既定 PR 序列继续把 proxy/survey/health 移出 Node。
- `internal/session/runtime.go:97-151` 的 `Runtime` 接口：30+ 方法的胖缝，涵盖 proxy、survey、cluster resume、recovery 编排。依赖反转做到了，但缝太宽，session 与 runtime 实为双向强耦合。建议按场景拆成若干小接口。

**Minor**

- `internal/runtime/node.go:136` + `cmd/server/main.go:71-75`：`NewNode` 无条件构造 `MemoryBroker` 和内存 PresenceStore，main.go 随后一律覆盖，Redis 模式下首个实例即垃圾。
- `internal/session/runtime.go:4`：包注释仍写 "until D15"，D15 已完成，注释过时。
- 命名三胞胎：`session.go:139` `type Client = Session` 同包别名，runtime 又同时别名两者，node.go 用 `Client`、Runtime 接口用 `Session`，检索成本高。建议收敛为 `Session`。
- `internal/cluster/sim` 向上 import `internal/runtime`，`runtime/cluster_sim.go` 在生产包导出 `Sim*` 测试钩子，cluster 子树反向依赖编排层。
- `internal/admin/admin_server.go:12` import `pkg/transport/grpc` 复用 PrepareServer，internal 反向依赖 pkg，注释承认是过渡产物。
- 双重别名层：runtime 的 aliases_local.go 与 session/runtime.go:30-87 对 protocol/authz/channel/occupancy/stream 的别名大面积重复，两处需手工同步。

**值得肯定**：`Broker` 接口契约（`internal/stream/broker.go:168-238`）文档质量高；订阅 saga 回滚设计严谨；`cmd/server` 装配清晰、失败即回收。

## 2. 并发与正确性

锁分层设计严谨（`h.mu → connShard`、`deliverMu → subMu` 顺序一致且有注释佐证），close 幂等、resume takeover 的 `RemoveSessionIfMatches`/`PrepareSessionUser` 防误删到位；redis broker 背压、去重（deliverOnce）与断连重建周密。除 C3/C4 外：

**Important**（无）

**Minor**

- `pkg/redisbroker/pubsub.go:92-99`：`Subscribe` 等待 Redis ack 最长 5s（`liveOpAckTimeout`），位于 `handleSubscribe → AddSubscription → broker.Subscribe` 的读循环上，Redis 抖动时每个新频道订阅 stall 读循环 5s。建议 ack 确认异步化。
- `internal/session/client.go:427-459`：本地 takeover 不等待旧读循环退出，旧连接读循环若正在处理已读入的帧，会与新读循环并发调用同一 `handleMessage`。各操作有锁兜底不损坏状态，但消息处理顺序不再保证。
- `internal/session/session.go:468-472`：Close 先快照 channels，worker 再查 ephemeral；若订阅恰被并发移除则默认 `ephemeral=false`，可能对 ephemeral 订阅误发 presence leave。
- `client.go:281/297/382/1252` 直接读 `c.session`/`c.user` 而写在 `c.mu` 下，当前同 goroutine 实际安全但模式脆弱，建议统一走访问器。
- `session.go:191` `sendQueue.notFull` 实际用作"非空"唤醒信号，命名与语义相反。

## 3. 代码规范符合性

**Important（系统性）**

- Imports 未按三组分组：多数核心文件把第三方与本项目包混排——`internal/session/client.go:13-23`、`hub.go:11-16`、`internal/runtime/node.go:12-22`、`pkg/transport/grpc/client_server.go:4-7`、`pkg/transport/ws/handler.go:7-14`；`cmd/server/main.go:12-28` 组内也未排序。正面例子：`internal/stream/broker_memory.go:3-16`。修复：全仓 `goimports -local github.com/messageloopio/messageloop` 并纳入 CI。
- 导出符号缺 doc comment：`client.go:26 NewClient`、`:72 WithProtocol`、`:142 Send`、`:147 HandleMessage`、`:827 MakeOutboundMessage`；`hub.go:60 NewHub`、`:79 AddSub`、`:329 BroadcastPublication`；`ws/handler.go:16 Handler` 等。
- 超长函数：`client.go:244-487 handleConnect` 约 243 行、`:494-710 finishConnect` 约 216 行、`hub.go:329-450 BroadcastPublication` 约 120 行。建议按阶段拆分（认证、resume、订阅处理）。

**Minor**

- `hub.go:265-275` 残留整段被注释掉的死代码 `pubToProto`；`hub.go:59` 注释写 `newHub` 实为导出的 `NewHub`；`session.go:70` 字段注释为中文，与全仓英文注释约定不一致。
- 日志参数风格混用约 18 处（`log.ErrorContext(ctx, "...", err)` 位置参数 vs `"error", err` 键值对）。
- genproto 别名不统一：`ws/handler.go:13` 用 `sharedpb`，`client.go:19` 用 `sharedv2`。
- `pkg/topics/cstrie.go:237/317/396` 对内部不变量违反直接 `panic`（沿袭 centrifuge），对服务器进程会打挂整个节点，建议评估改为错误返回或至少注释说明有意为之。

## 4. 错误处理与断开语义

**Important**

- `pkg/transport/grpc/handler.go:51-53`：非 Disconnect 错误处理与 WS/QUIC 不一致。`HandleMessage` 对非 Disconnect 错误已在 `client.go:183-192` 发送 INTERNAL_ERROR 信封并应"软失败"，WS（`ws/handler.go:104`）和 QUIC（`quic/handler.go:109`）均记录后继续读循环；唯独 gRPC 直接 `return err` 终止整条流，且原始错误文本作为 gRPC status message 泄漏给客户端。修复：与 WS/QUIC 对齐，记录后 `continue`。
- `internal/session/client.go:878/1009`：客户端帧缺字段（"missing channel in RPC/publish"）被报为 `INTERNAL_ERROR/server_error` 且 Error 级日志。既有 3501 `DisconnectBadRequest` 和 BAD_REQUEST/client_error 语义，应使用后者。

**Minor**

- `internal/runtime/node.go:151`：`authorizer, _ = NewAuthorizer(空配置)` 二次吞错，若失败 `node.authorizer` 为 nil，后续 `Decide` panic。修复：失败时返回构造错误。
- `node.go:442/456/467/535/551/564`：saga 回滚错误静默吞掉无日志，留下 hub/broker/metrics 不一致且无迹可查。修复：rollback 内至少 Warn 级日志。
- `node.go:193-219`：broker 启动错误的异步上报对无 `Ready()` 的 broker 失效，注释承诺与实现不符。
- `internal/protocol/disconnect.go` 3506-3509 四个码定义并导出但生产代码从未使用（如 3509 TooManyErrors 本可用于"错误过多即断开"）。要么接入要么删除。
- `session.go:707-712`：enqueue 在 `ctx.Done()` 分支放弃等待但帧仍留队列稍后写出，调用方观察到失败而客户端实际收到帧，语义含糊。
- `client.go:958/1046` 等把 `err.Error()` 直接塞进对客户端的 Error.Message，`HTTPStatusError.Error()` 会带上后端原始响应体，可能泄漏后端内部细节。

## 5. 协议与序列化一致性

**Important**

- `docs/protocol.md:17`：WS 子协议表称 `messageloop` 为 "Protobuf binary (default)"，**实现恰好相反**——`ws/handler.go:117-124` 与 `transport.go:14-19` 将 `messageloop`（含无子协议）映射为 protojson 文本帧，`integration_test.go:159-166` 明确测试该行为；同文档 QUIC 表（:34）又写 `messageloop` 是 JSON alias。文档自相矛盾且与代码相反。修复：WS 表改为 "JSON (default)"，注明仅 `messageloop+proto` 走二进制。

**Minor**

- `docs/protocol.md:64-95`：Inbound 表缺 `presence_query`（`protocol/client/v2/service.proto:32`），Outbound 表缺 `presence`（field 15）与 `presence_event`（field 16）。
- `pkg/transport/grpc/transport.go:147`、`transport_test.go:346`：注释写断开码区间 "3500-3512"，码表已到 3514。
- `docs/protocol.md:525`：`disconnect.go` 已迁至 `internal/protocol/disconnect.go`。
- ALPN/子协议→Marshaler 映射存在三份（`shared/streamframe.go:39` 的 `MarshalerForALPN` 实际无人调用，属死代码；`quic/handler.go:37-43` 内联重复；`ws/handler.go:117`）。建议 QUIC handler 改用该函数或删除。
- `heartbeatReadTimeout` 在 `ws/handler.go:136` 与 `quic/handler.go:122` 逐字重复。

**验证为正确**：streamframe 帧读写（4 字节大端前缀、短写处理）、12 个 inbound oneof 全覆盖、版本门 fail-closed 且先于认证、断开码三传输送达机制、protojson `UseProtoNames` 与文档一致。协议层无行为性 bug，债务集中在文档同步。

## 6. 测试质量与覆盖

全仓约 100 个 `_test.go`，`internal/runtime` 30 个测试文件 318 个测试函数。关键路径（路由、pub/sub、matcher、重连、集群 fencing/CAS）都有实质覆盖，并发测试、panic 容忍、错误注入等硬场景也没回避。

**Important**

- 测试"层级错位"：`internal/session`（client.go 1731 行核心路由）、`internal/occupancy`、`internal/survey` 包内零测试，全靠 `internal/runtime` 层经 `nodeRuntime` 适配器间接覆盖。重构 `session.Runtime` 接口时这些测试无法随迁，覆盖会悄悄蒸发。
- `pkg/redisbroker` 约一半测试（history、catchup gap、presence Lua 脚本）经 `requireCommandBusRedis` 连接 `127.0.0.1:6379`，不可用时 `t.Skip` **静默跳过**——CI 无 Redis 则这些关键路径零执行。修复：CI 加 Redis service，或对纯 bookkeeping 逻辑引入 fake。

**Minor**

- `sdks/go` 全部 10 个测试文件（其 go.mod 未依赖 testify）、`shared/marshaler_test.go`、`internal/protocol/disconnect_test.go` 等用裸 `t.Fatal/t.Error`，与 AGENTS.md 的 testify 约定不一致。
- Benchmark 分布不均：`Hub.BroadcastPublication` 大扇出、ws/grpc 传输层、redisbroker 发布路径均无基准。
- `internal/session/heartbeat.go:30` 的 `defaultJitter` 真实抖动分布永不执行（测试用 `SetJitterForTest` 固定）。

## 7. 安全

设计成熟度高：集群命令总线 HMAC ≥32 字节 fail-closed、deny-precedence 鉴权、`subtle.ConstantTimeCompare`、三传输统一速率/订阅数/连接数/帧大小限制、survey 响应按 expected session 防伪。

**Important**

- `cmd/server/main.go:349-357`：admin HTTP（`/metrics`、`/health`）完全无认证、无 TLS、无超时配置。默认绑 `127.0.0.1:8080` 是缓解，但配成 `0.0.0.0` 时无任何警告。修复：非 loopback 地址时启动告警，或支持可选 bearer token。
- `internal/session/client.go:376/402`：会话接管防御缺口——auth proxy 认证成功但返回空 `UserInfo.ID` 时，`resumeAllowed` 仍为 true，跨用户检查 `authUser != "" && authUser != existing.UserID()` 被跳过，`existing.user = authUser` 直接清空会话身份。知道 sessionID 即可接管任意会话。修复：`resumeAllowed = p != nil && authUser != ""`。
- `proxy/http.go:407`：`io.ReadAll(resp.Body)` 无大小上限，恶意或被攻破的后端可返回无限 body 造成内存耗尽。修复：`io.LimitReader` 加上限。

**Minor**

- `internal/admin/api_handler.go:176`：admin Survey 的 `req.TimeoutMs` 未按 channel policy 的 `MaxSurveyTimeout` 钳制（客户端路径 `client.go:1428-1430` 有钳制）。
- `pkg/transport/quic/tls.go:62`：insecure 模式自签证书有效期仅 24h，进程运行超 24h 后新连接握手必失败。仅限 dev，但行为隐性。
- `internal/session/client.go:1007-1014` + `internal/authz/authorizer.go:278-290`：publish 路径在 ACL `Decide` 之前不做 `ValidateTopic`（Recover/Presence 分支有 `isWildcard` 检查，Publish 没有），发布到字面 `a.*` 会命中通配订阅者。
- admin gRPC bearer token 可在无 TLS 明文上传输（`config.go:428` 只强制 token 存在）；admin gRPC 未设 `MaxRecvMsgSize`；proxy 转发客户端 token 给 backend 允许 `http://` 明文；Redis 连接无 TLS 选项。

## 8. 配置与部署

**Important**

- `docs/deployment.md:227` vs `cmd/server/main.go:132`：文档称优雅停机可配置默认 10s，实际硬编码 `lynx.WithShutdownTimeout(30*time.Second)`，既不可配置默认值也是 30s。
- `docs/deployment.md:129-134` 多节点集群 YAML 示例缺 `hmac_key`/`hmac_key_file`，按 `config.go:464-473` 校验规则照抄即启动失败。
- `config/config.go:450-452`：`stream_approximate` 校验自相矛盾——注释承认"未设置与显式 false 无法区分"，但错误信息写 "remove the field or set it to true"（删掉字段同样报错），且 `pkg/redisbroker/options.go:70` 运行时默认本就是 true，导致任何不显式写 `stream_approximate: true` 的 redis 配置都无法通过校验。修复：改 `*bool` 区分"未设置"，或修正错误信息。

**Minor**

- `config.go:428` vs `pkg/transport/grpc/server.go:35-37`：Validate 把 `grpc_admin.addr` 当可选，但 `validateOptions` 硬性要求非空，错误延迟到 cluster/broker 初始化后才暴露。
- `config.go:458-460` + `main.go:154-179`：`cluster.backend` 取值不校验，任意值（如 `"etcd"`）会静默构造一个无任何依赖的"启用"集群。
- `config-example.yaml:25` 注释称 idle_timeout "parse failure = 300s"，但 Validate 会拒绝无法解析的时长，该回退路径经 YAML 不可达。
- `config-node1.yaml`/`config-node2.yaml` 名为双节点示例却无 `cluster` 块；`config-example.yaml:81` 默认 `allow_all_origins: true` 与生产清单相悖；`require_auth` 未在任何示例 YAML 中展示。

## 9. 性能热点

除 C2/C4 外：

**Important**

- `internal/stream/broker.go:69-83`：JSON payload 双重转换——JSON 字节 `json.Unmarshal → map → structpb.NewStruct`，随后每个 JSON 订阅者的 protojson 又把 Struct 序列化回 JSON。对纯 JSON 部署是每次 publish 的纯开销。修复：JSON kind 且 marshaler 为 JSON 时直通原始字节。
- `internal/stream/broker_memory.go:188`：发布路径持全局写锁仅为查/建 history map 条目，所有 channel 的 Publish 在此串行并挡住 `interested()` 的 RLock。修复：先 RLock 查，未命中再升级写锁。

**Minor**

- 一次 publish 两次 `matcher.Lookup`（`broker_memory.go:173` + `hub.go:337`），且 cstrie Lookup 每层递归都 `make(map)` + 切片分配（`cstrie.go:361-390`）。修复：无通配订阅时短路，或把 Lookup 结果随 handler 传下去。
- `hub.go:93-107`：`wcSubsMu` 单锁包住整个 `matcher.Subscribe`，cstrie 的无锁 CAS 在锁内失去意义。
- cstrie 的 copy-on-write Subscribe/Unsubscribe 每次拷贝整层 map，高订阅 churn 场景成本高（持久化无锁结构的固有权衡，建议基准量化后再定夺）。
- 广播并行分支每条消息最多起 64 个 goroutine（`hub.go:414-440`），大 fan-out 下 goroutine churn 可观。
- AGENTS.md "订阅锁 16384 分片"实际指 `node.go:56` 的 saga subLocks，hub 的 subShards 只有 64，文档易误导。

---

## 总结

- **发现分布**：Critical 4 项、Important 约 15 项、Minor 约 30 项。
- **项目底色很好**：协议/实现一致性、并发锁设计、安全 fail-closed 默认值、测试覆盖面在同规模 Go 服务中属上乘。
- **最优先三件事**：① 修 C1（proxy JSON 方言混用，安全+正确性双重问题）；② 修 C2（广播序列化去重）；③ 修 C3（WS Close 并发读）。
- **两条主线债务**：① 文档/注释与代码漂移（protocol.md、deployment.md、断开码区间）；② "平移式重构"未完工（双重别名层、`session.Runtime` 胖接口、测试层级错位）——建议按既定 PR 序列推进，不与本次修复混杂。

---

## 附录：C4 设计评审——广播 `frame.done` 同步等待语义（2026-08-19 复审）

> 针对 C4 条目的深入设计评审。只做分析与建议，本附录不改代码。
> 注意：自 cca9308（memory broker 异步分片投递）起，原条目"handler 在发布者读循环里同步执行"已过时——memory broker 的广播现运行在分片投递 goroutine 上，发布者 ack 不再被订阅者拖住；redis broker 侧描述（delivery worker 占用）仍然成立。

### 现状链路

1. `Hub.BroadcastPublication`（`internal/session/hub.go:340`）对每个订阅者 `sendFrame → enqueueBytes`（`session.go:713`）：`tryEnqueue` 入队（Control 32 / Data 256 两条有界 lane）后阻塞在 `<-frame.done`，ctx 为硬编码的 `context.Background()`，本层无超时。
2. 唯一写 goroutine `writerLoop`（`session.go:576`）逐帧 `Transport.Write` 后以结果信号 `frame.done`。
3. 超时边界在传输层：WS 默认 10s 写截止时间（`DefaultWSWriteTimeout`），gRPC `sendWithBudget` 入队 10s + 送达 ack 10s。两者都可配，WS 允许显式置 0 禁用。
4. 失败路径收敛：lane 满 → `Close(DisconnectSlowConsumer)`；写错误 → writerLoop 退出并按 §7 表关会话；队列关闭时所有挂起帧立即 `done <- ErrSessionNotAttached`。因此 **`frame.done` 保证最终必然触发**，不存在真正的永久阻塞/goroutine 泄漏——前提是会话最终会被关闭（心跳兜底）。

### 为什么同步等待不能简单删掉

`frame.done` 的等待结果喂给两个正确性消费者：

- **`recordDeliveredOffsets`（hub.go:528）**：只把"确认写上线"的 offset 记入每订阅的 last-delivered 簿记，resume/recovery 依此决定恢复起点。fire-and-forget 会把未上线的 offset 记为已送达 → 客户端 resume 时跳过恢复 → **丢消息**。这是该语义的核心理由。
- **metrics**：`MessagesDelivered`/`DeliveryFailures` 反映真实写线结果而非入队受理。

### 残留的代价

- 小扇出（≤8）走串行分支，延迟是各订阅者写耗时**累加**，一个 10s 慢写拖住整条广播。
- 慢而未死的订阅者（TCP 背压但未触发 lane 满）占住其分片/worker 直到写超时触发：memory broker 下是同 shard 其他频道延迟，redis broker 下是 hash 到同一 worker 的频道延迟（有界，≤写超时×队列深度）。
- WS `WriteTimeout: 0` 时单帧等待无传输层上界，仅靠心跳判死后 `conn.Close` 中断写。

### 方案对比

| 方案 | 语义变化 | 改动面 | 风险 |
|---|---|---|---|
| A. 维持现状 | 无 | 无 | 慢订阅者长尾 stall 持续存在 |
| B. 等待加 ctx 超时（对齐写超时），超时按"未送达"处理 | 超时帧不记 delivered、记 DeliveryFailures；会话由慢消费者机制自行清理 | hub.go 一处 + 测试 | 超时误伤"慢但健康"的客户端 → offset 簿记偏旧 → resume 多恢复（有稳定消息 ID 去重 + C6 gap 检测兜底，**宁可多恢复的方向是安全的**） |
| C. fire-and-forget + 异步完成回调更新簿记 | delivered 簿记变最终一致，需按订阅者单调取 max-offset 更新；metrics 归属移到 writer 侧 | hub/session 边界重设计 | 回调乱序、簿记一致性、Attach 换队列后旧帧归属——复杂度高 |
| D. 禁止 `WriteTimeout: 0`（配置校验拒绝） | 无 | config 校验一行 | 消除唯一无界路径；但 sync 等待本身保留 |

### 建议

- **短期**：B + D。B 把单订阅者最坏 stall 从"写超时×队列深度"压到一次广播预算内，且失败方向安全（多恢复、不丢消息）；D 消灭唯一无界配置。两者都是小改动。
- **中期**：小扇出串行分支可无条件走并行发送（`broadcastParallelLimit` 已有界），消除 ≤8 扇出的延迟累加。
- **长期**：若 profile 显示广播等待仍是投递延迟主因，再上 C（异步簿记），届时需先补 `recordDeliveredOffsets` 的乱序/单调性测试。
