# MessageLoop 全面评审报告（2026-08-21）

> 第三轮全面评审，基线为 2026-08-19 评审后的修复链顶端（`254eceb`，即 C1–C3 修复、cca9308/f374126/e471dee 性能提交与 C4 深度分析之后）。
> 方法：9 个维度并行深查（8 个评审代理 + 错误处理维度由主评审补位），所有发现均基于实际代码核实并复核上一轮 65 项发现的修复状态；另跑全量构建与测试基线。
> 严重级别沿用本目录约定：Critical（正确性/数据丢失/安全）/ Important（健壮性/并发/资源）/ Minor（可读性/一致性/小改进）。

## 0. 基线

- `go build ./...`、`go vet ./...`：通过。
- `go test ./... -count=1`：**唯一失败**为 `TestRepositoryConfigsValidateAndPrebind/../../config.yaml`——本机存在一份面向旧树、未被 git 跟踪的本地开发 `config.yaml`，被 v2 收紧后的校验拒绝（`server.grpc_admin` 需 `auth_token` 或显式 `allow_insecure: true`）。属环境性失败，非 v2 代码缺陷；其余全部包绿。本地 Redis 在运行：`pkg/redisbroker` 全套集成测试真实执行（12.5s，零跳过）。
- 附带发现（Minor）：上述测试会读取未跟踪的本地文件，任何机器上残留的过期本地配置都会让 `go test ./...` 变红。建议只校验 git 跟踪的示例配置，未跟踪文件降级为 skip（`cmd/server/config_consistency_test.go:32-38` 已有 not-exist skip，但存在即校验）。

## 1. 上一轮发现复核总览（65 项）

| 维度 | 项数 | FIXED | PARTIAL | OPEN |
|---|---|---|---|---|
| 1 架构与模块边界 | 10 | 1 | 0 | 9 |
| 2 并发与正确性（含 C3/C4） | 7 | 1 | 1 | 5 |
| 3 代码规范 | 7 | 1 | 2 | 4 |
| 4 错误处理与断开语义 | 8 | 2 | 2 | 4 |
| 5 协议与序列化 | 6 | 4 | 0 | 2 |
| 6 测试质量与覆盖 | 5 | 0 | 2 | 3 |
| 7 安全（含 C1） | 8 | 3 | 1 | 4 |
| 8 配置与部署 | 7 | 4 | 2 | 1 |
| 9 性能热点（含 C2） | 7 | 2 | 1 | 4 |
| **合计** | **65** | **18** | **11** | **36** |

四个 Critical 的处置：**C1 FIXED**（proxy 全路径 protojson+DiscardUnknown，camelCase 回归测试在位）；**C2 FIXED**（per-encoding marshal-once + enqueueBytes 共享字节，所有权契约成文）；**C3 FIXED**（WS Close 只 WriteControl+conn.Close，排空删除，-race 有测试钉住）；**C4 PARTIAL**——memory 侧经 cca9308 缓解（发布者 ack 不再被订阅者直接拖住），但 `frame.done` 同步等待、广播 ctx=Background（`internal/session/hub.go:364`）、redis delivery worker 被慢订阅者占用、WS `WriteTimeout=0` 四要素全部在位，附录建议的 B+D 未实施（`internal/session/session.go:744-749`、`pkg/redisbroker/pubsub.go:335-342`、`pkg/transport/ws/server.go:33-36`）。

修复主线的战果是真实的：16 个修复提交没有引入越界 import（`go list` 逐行比对）、没有破坏协议冻结（`git diff d21139d..HEAD -- protocol/` 仅新增 `gap_notice=19` 与两个枚举值）、HMAC/deny 优先/版本门经复核完好。遗留集中在：架构债整体冻结（9/10 OPEN）、错误文本外泄、admin HTTP、若干文档面。

---

## 2. 新发现 — Critical（1 项）

### S1. 跨用户 resume 即接管：sessionID 对任意已认证用户等价于跨用户凭证

- **位置**：`internal/session/client.go:442-462`（resume 门只要求"有非空认证身份"）+ `internal/session/hub.go:860-914`（PrepareSessionUser）+ `internal/runtime/cluster_resume.go:56-67`（远端路径 CAS 不校验 UserID）
- **问题**：ae41013 修复了"空 user 接管"，但**不同非空用户之间**的 resume 仍被放行：用户 B 持有效 token + 知道用户 A 的 sessionID，即可越过唯一防线（per-user 连接限额），`existing.user = authUser`、Detach A 的连接、继承 A 的全部订阅（**不做订阅 ACL 重评**）与恢复游标；A 被静默断开。跨节点路径同样不比对 authUser。sessionID 并非强保密能力——presence 快照/事件与 admin GetPresence 都携带它（`client.go:738-744`、`node.go:1385`、`api_handler.go:392`）。该行为被 `client_fix_test.go:1800-1812` 固化为设计（B1 §6.5 只考虑限额），但多租户 + per-user `allow_subscribe` 部署下构成订阅级越权。
- **修复**：默认拒绝跨用户 resume（`authUser != existing.UserID()` → 断开 3512/stale），或引入 resume token；至少在接管时对继承订阅重跑 `Decide`。
- **置信度**：机制 high（代码+测试证实）；风险取决于是否将 sessionID 视为保密能力。

---

## 3. 新发现 — Important

### 并发与正确性

**E1. e471dee 的 JSON splice 在生产路径是死分支（三个维度独立确认）**
- 位置：`internal/session/hub.go:428`
- 问题：命中条件 `m.Name() == (JSONMarshaler{}).Name()`（`"json"`），但生产三条传输的 JSON 会话全部挂 `shared.ProtoJSONMarshaler`（Name=`"protojson"`；`ws/handler.go:130`、`quic/handler.go:43`）；`JSONMarshaler{}` 仅 `internal/cluster/sim` 与测试在用。后果：①上一轮 §9"JSON payload 双重转换"（`json.Unmarshal→structpb→protojson`，`hub.go:369` + `internal/stream/broker.go:70-79`）在生产**依旧存在**，大整数精度/键序保真承诺未兑现；②配套测试（`hub_test.go:502/559/612`）全用 `JSONMarshaler{}`，恰好测在死分支同侧，全部绿灯。
- 修复：条件改为按实例/编码家族匹配（`m == shared.ProtoJSONMarshaler || m == (shared.JSONMarshaler{})`）；测试改用生产 marshaler。
- 置信度：high

**E2. splice 防御回退路径别名池化 buffer（潜伏 use-after-put + 静默空 payload）**
- 位置：`internal/session/hub.go:440,452-459` + `:552-556`
- 问题：`spliceRawJSONPayload` 找不到 `"json":{}` 占位符时**原样返回入参**（池化 buffer 别名）；`spliced=true` 分支不拷贝直接共享，随后 `putBuffer` 归还池——下一个取用者覆写同一底层数组时，这些字节可能仍被排队帧引用（数据竞争/帧损坏），且订阅者会静默收到空 `{}` payload。当前因 protojson 恒出占位符而不可达，但一旦 protojson 渲染变化即触发。
- 修复：回退分支返回深拷贝并按失败处理（放弃 splice、走正常 marshal），绝不把别名缓冲区当共享帧。
- 置信度：路径 high / 触发概率 low

**E3. cca9308 同频道投递存在乱序竞态，且时序契约未写入 Broker 接口**
- 位置：`internal/stream/broker_memory.go:306-328`（offset 在 `h.mu` 内分配，enqueue 在锁外）；注释承诺 `:21-28,37-43,257-265`；接口文档缺口 `internal/stream/broker.go:93-95,169-186`
- 问题：两个并发发布者向同一频道发布时，offset 分配顺序与进入 shard 队列的顺序可颠倒（单 worker 按队列序投递）→ 订阅者可能先收 offset 2 再收 offset 1。竞态在旧同步版即存在，但新注释把"same channel → in-order"写成硬保证；`hub.go:594-599` 的 max-guard 也自认乱序可能。同时该"同频道串行、跨频道并发"的契约只在两个实现里成文，Hub 广播静默依赖它——任何新 Broker 实现若并发投递同频道将无声打乱客户端顺序。
- 修复：`enqueue` 移入 `h.mu` 临界区（或降级注释为"单发布者有序"）；在 `Broker` 接口文档显式钉住 handler 并发/定序契约。
- 置信度：high

**E4. 本地 takeover 写 `existing.ctx` 与多处无锁读构成数据竞争**
- 位置：写 `internal/session/client.go:453`；无锁读 `:209,1376,1543/1557,1789`、`session.go:314/413`
- 问题：接管替换 `existing.ctx` 时，旧读循环（takeover 不等其退出的窗口内）及心跳/survey goroutine 可能正无锁读；context 是双字 interface，并发读写是真实数据竞争（-race 可报）。
- 修复：读侧统一锁下取快照，或令 ctx 在 NewClient 后不可变。
- 置信度：模式 high / 影响 medium

### 安全

**E5. 集群命令 HMAC 规范化编码存在字段歧义，Channel/Metadata 不在签名内**
- 位置：`internal/cluster/hmac/hmac.go:127-154`（`\n` 行拼接、无长度前缀/转义）；`internal/cluster/contracts.go`（Channel、Metadata 被注释明确排除）
- 问题：①SessionID/TargetIncarnationID 相邻变长字段以 `\n` 分隔，SessionID 含换行时不同字段组合可产生相同规范字节——签名对重写后的字段仍有效；`Connect.session_id` 完全未校验字符集（`client.go:319-322`），自建含换行 sessionID 可使合法签名的 takeover 命令字段重排后指向另一会话。②subscribe/disconnect 的 Channel 与断开 code/reason 不签名，能写 Redis 者可改写不破签。实际可利用性被 command_id 去重 + 流内顺序 + LeaseVersion 围栏三重抑制，属对"写 Redis ≠ 注入"声明的边界削弱。
- 修复：规范字节加长度前缀或转义；Channel/Metadata（或其哈希）入签；session id 字符集/长度校验。
- 置信度：机制 high / 可利用性 low-medium

**E6. 旧账维持未动：admin HTTP 无认证（`cmd/server/main.go:350-358`）、publish 不 ValidateTopic（`client.go:1067-1074`，字面 `a.*` 可命中通配订阅者）、admin Survey 超时不钳制（`api_handler.go:177`）**

### 测试与 CI

**E7. CI 的 redis:7 默认库数下，redisbroker 集成测试很可能整套静默跳过**
- 位置：`pkg/redisbroker/cluster_command_bus_test.go:25`（`clusterCommandBusTestDB = 16`）vs `.github/workflows/ci.yml:18-21`（`redis:7` 无自定义 args）
- 问题：Redis 默认 `databases 16`（合法 0–15），DB 16 SELECT 失败 → 探针 Ping 失败 → 全套 `t.Skipf`。497a420 修本地互清库的同时，可能把 CI 上 redisbroker 的执行架空（runtime 的 DB 14/15、e2e 的 DB 13 不受影响，会掩盖问题）。本机因 Redis 非默认配置而真实跑过。
- 修复：改用空闲 DB 12，或 CI 加 `--databases 32`，或改 key 前缀隔离；CI 增设"Redis 测试非零执行"断言。
- 置信度：high（静态推断，建议以 CI 日志 skip 行核实）

**E8. 两个正确性要害修复零测试：aaf1743（enqueueBytes 编码一致性）与 cca9308（异步分片投递）**
- 位置：`errMarshalerChanged` 全仓测试零触达；`broker_memory.go` 的四项设计承诺（同频道 offset 有序、关停丢弃但 history 保留、shard 满背压、Transient 与 Publish 同频道定序）无一有测试钉住
- 问题：后续重构可无声破坏；`hub_test.go` 混合编码测试未断言"每编码只 marshal 一次"（字节相等是弱代理）、splice 的转义/非法 UTF-8 边界无测试。
- 修复：补并发契约测试（广播中切换 marshaler；发布 N 条断言到达顺序）+ countingMarshaler。

### 配置与部署

**E9. 示例与 README 三处"照抄即败"**
- `configs/cluster-example.yaml:11-19`：缺 `transport.grpc.addr` 与 `stream_approximate: true`，两处都会被 Validate 拒绝；
- `README.md:215`：指导配置已删除的 `server.presence.cluster_emit: true`，写了即启动失败；
- `server.grpc_admin.addr` 空值逃过 Validate（`config/config.go:429` vs `pkg/transport/grpc/server.go:34-37`），错误延迟到装配末期，与两处文档"必填"声明矛盾。
- 修复：补示例字段、删 README 尾句、Validate 加非空检查；CI 加示例 YAML 冒烟。

### 架构与性能

**E10. 架构债 9 项 OPEN 且无 backlog 载体**：God 包（Node 1556 行 + 36 方法 Runtime 胖缝 + aliases_local 294 行约 165 符号）、isPeerClosedError 编译期耦合双传输、syncClusterSessionState 三套 ctx、`Client=Session` 三胞胎、sim/admin 反向依赖、双重别名层、NewNode 冗余构造、跨包 helper 复制（`isWildcard` 4 份、`pingClusterRefreshInterval` 2 份等）。`docs/review/backlog.md` 未收录任何一条——修复批次持续绕开架构项与缺跟踪直接相关。建议补录并按既定 PR 序列推进。
**E11. 发布热路径两处大冗余**：redisbroker 每条发布 4 次串行 RTT（`redis.go:350-433`，其中 first_retained 的 XINFO+SET 2 次纯 bookkeeping 可并入 Lua 脚本）；memory broker `interested()` 仅为 bool 却做全量 cstrie Lookup（`broker_memory.go:248-255`，三处调用 + hub 再 Lookup 一次）。建议 `MatchExists` 短路 + 脚本合并返回。

---

## 4. 新发现 — Minor（按维度）

- **架构**：pkg/transport 硬编码 `*runtime.Node` 且 import internal/*（名实不符，建议窄接口或迁 internal/transport）；presence/gap 侧两份手写 fan-out 不会随 hub 广播优化同步受益（`node.go:1385-1442,1477+`）；三处过时注释（"root files keep compiling" 等）；config→proxy 传递依赖进会话核心。
- **并发**：WS Close 在 WriteTimeout=0 下可无限期阻塞（`transport.go:65-71`，先锁后关，建议先 conn.Close 打断写）；ping deadline 装填窗口可产生假 3511（`heartbeat.go:102-109`，建议先 arm 再 Send）；Fence 回滚窗口双 writerLoop 交错（`session.go:402-415`）；`SetOccupancyHandler` 裸写与 `SetGapHandler` 加锁不对称（`redis.go:494-507`）。
- **错误处理**：通用回退信封 `Message: err.Error()`（`client.go:225`）+ survey 顶层错误拼接 err.Error()（`:1562/:1573`）+ PROXY_ERROR 路径（`:1018/:1106`）可把 `HTTPStatusError` 内嵌的后端响应体带给客户端（与安全维度合并）；WS 解码错软失败（BAD_REQUEST 信封）而 gRPC 解码错经 Recv 拆流，两传输语义不一致；broker 无 `Ready()` 时启动错误无人消费（`node.go:211-219`）；saga 10 个 rollback 全部 `_ =` 静默（`node.go:504-566`）；3506-3509 四个断开码定义未用。
- **协议**：`MarshalerForALPN` 死代码 + 子协议映射三份（`streamframe.go:39`）；`heartbeatReadTimeout` WS/QUIC 逐字重复；WS 与 QUIC 编码协商偏好相反（裸 `messageloop`+`+proto` 同供时 WS 选 JSON、QUIC 选 proto）且文档未记载；splice 与 structpb 路径对同一消息可产出语义等价但字节不同的 JSON（HTML 转义差异），逐字节比对的客户端可见。
- **测试**：`survey_test.go` 仍剩 7 处裸 `Sleep(200ms)`（同文件已有 Eventually 助手）；sdks/go 与 shared 子模块 CI 不跑 `-race`；全仓仍约 47 处 time.Sleep；`t.Parallel()` 仅 16 处且 redisbroker 为零（一旦有人加 Parallel 将互清 DB）。
- **安全**：`Connect.session_id` 无字符集/长度校验（E5 前置）；`hmac_key_file` 无权限检查；无节点级总连接数/新建速率限制（匿名可堆积资源）；Redis PSUBSCRIBE glob 元字符（`[ ? \`）可致欠匹配丢消息（`interest.go:73`）；4MiB 上限不可配置；零配置默认"匿名全开"仅是文档脚注。
- **配置**：停机预算挤压（DrainAll 与 cluster.Shutdown 共享同一 10s ctx，`node.go:224-246`）；runtime.go 防泄漏注释与 lynx 实际行为不符（setup 失败 OnStop 不执行）；developer 文档包路径系统性漂移（`pkg/websocket`/`pkg/grpcstream`/`pkg/quicstream` 均不存在）；02-configuration.md 引用已删除的 config-node1/2.yaml；两层停机超时文档各说一半。
- **性能**：BroadcastPublication 固定 ~7 次堆分配，单订阅者频道净回归；`hub.index`/`chShard` 每次分配 FNV 对象（redisbroker 已有零分配实现未复用）；degradedMu 全局锁在未降级通道上也付出；writerLoop 逐帧写未用已有 `WriteMany` 批量接口。
- **规范**：BroadcastPublication 涨至约 194 行（f374126+e471dee 内联）且恰好没补 doc comment；5bccde7 新写的 Hub 注释含"16384 subscription shards"事实错误（subShards 实为 64，16384 是 node 层 saga 锁）；doc comment 缺口仍约 205 处；shared/v2 genproto 别名分裂（sharedv2 35 处 vs sharedpb 16 处）；无 `.golangci.yml`，import 三组规则无 CI 闸门；AGENTS.md 示例违反自身规则并引用不存在符号；中文注释约 30 处；`proxyproxy` 怪别名。

---

## 5. 旧发现逐条复核（按维度）

### 维度 1：架构与模块边界（1 FIXED / 9 OPEN）

| 旧发现 | 判定 | 现状 |
|---|---|---|
| isPeerClosedError 嗅探双传输 | OPEN | `session.go:14-18,624-633` |
| syncClusterSessionState 三套 ctx | OPEN | `node.go:375/452/545-547` |
| runtime God 包 + aliases_local | OPEN | node.go 1556 行；aliases_local 294 行/约 165 符号 |
| Runtime 36 方法胖缝 | OPEN | `session/runtime.go:96-150` |
| NewNode 冗余构造 broker/presence | OPEN | `node.go:87,137` + `main.go:69-76` |
| "until D15" 过时注释 | **FIXED** | 497a420 已更新 |
| Client=Session 命名三胞胎 | OPEN | `session.go:146` 等 |
| sim 反向依赖 + Sim* 生产导出 | OPEN | `cluster/sim/world.go:10` |
| admin import pkg/transport/grpc | OPEN | `admin_server.go:14` |
| 双重别名层 | OPEN | aliases_local vs session/runtime.go:29-86 |

### 维度 2：并发与正确性（1 FIXED / 1 PARTIAL / 5 OPEN）

| 旧发现 | 判定 | 现状 |
|---|---|---|
| C3 WS Close 并发读 | **FIXED** | `transport.go:58-76`（残留见 Minor：WriteTimeout=0 下 Close 可阻塞） |
| C4 广播同步等待/Background ctx | PARTIAL | 见 §1；B+D 未实施 |
| Subscribe 等 Redis ack 5s 阻塞读循环 | OPEN | `pubsub.go:92-99` 一字未动 |
| 本地 takeover 不等旧读循环 | OPEN | `client.go:431-507` |
| Close 快照后误发 leave 竞态 | OPEN | `session.go:450-453,474-479` |
| 无锁读 c.session/c.user | OPEN | `client.go` 7 处散点 |
| notFull 命名反语义 | OPEN | `session.go:198` |

### 维度 3：代码规范（1 FIXED / 2 PARTIAL / 4 OPEN）

| 旧发现 | 判定 | 现状 |
|---|---|---|
| import 三组分组 | **FIXED** | 621ecf9 全仓修净、后续未破坏；但无 CI 闸门 |
| 导出符号 doc comment | PARTIAL | 原点位已补；全仓仍约 205 处缺口，BroadcastPublication 恰好漏掉 |
| 超长函数 | OPEN（恶化） | BroadcastPublication 120→194 行 |
| 死代码 pubToProto 等 | PARTIAL | newHub 注释已修；死代码段与 3 处同类错名仍在 |
| 日志风格混用 | OPEN | kv 162 处 vs 位置参数 21 处 |
| genproto 别名不统一 | OPEN | 收敛至 shared/v2 二选一未拍板 |
| cstrie panic | OPEN | `cstrie.go:237/317/396` |

### 维度 4：错误处理与断开语义（2 FIXED / 2 PARTIAL / 4 OPEN）

| 旧发现 | 判定 | 现状 |
|---|---|---|
| gRPC 非 Disconnect 错误拆流+泄漏 | **FIXED** | `grpc/handler.go:52-60` log+continue，三传输对齐 |
| 缺字段报 INTERNAL_ERROR | **FIXED** | RPC/publish 走 sendRequestError BAD_REQUEST（`client.go:938/1069`） |
| authorizer 二次吞错 | PARTIAL | 失败已 Warn（`node.go:151`），回退构造仍 `_ =`，nil→Decide panic 理论残留 |
| broker 启动错误异步上报失效 | PARTIAL | startErr+Ready() 就位（`node.go:190-219`）；无 Ready() 的 broker 错误仍无人消费，ctx.Done 时带错返回 nil |
| saga rollback 静默 | OPEN | 10 个 rollback 均 `_ =` 无日志 |
| 3506-3509 未用 | OPEN | 仅定义+别名再导出 |
| enqueue ctx.Done 语义含糊 | OPEN | `session.go:744-749`；广播路径因 ctx=Background 不可达，通用路径仍在 |
| err.Error() 进客户端 Error.Message | OPEN | 通用回退 `client.go:225` + survey `:1562/1573` + PROXY_ERROR `:1018/1106` |

### 维度 5：协议与序列化（4 FIXED / 2 OPEN）

| 旧发现 | 判定 | 现状 |
|---|---|---|
| protocol.md WS 默认编码写反 | **FIXED** | e221db2；e2e 测试钉住 |
| Inbound/Outbound 表缺行 | **FIXED** | presence_query/presence/presence_event 已补 |
| "3500-3512" 注释 | **FIXED** | 已改 3514 |
| disconnect.go 旧路径引用 | **FIXED** | 已指 internal/protocol |
| MarshalerForALPN 死代码+三份映射 | OPEN | 未动；注意简单复用会因返回 JSONMarshaler{} 引入新问题 |
| heartbeatReadTimeout 重复 | OPEN | 未动 |

另验证为正确：v2 字段冻结完好（仅新增 gap_notice=19 与两枚举值）；inbound 11 个动词 dispatch 全覆盖无错配；版本门 fail-closed 且先于认证；aaf1743 校验覆盖全部 enqueue 路径；f374126 缓存键正确。

### 维度 6：测试质量与覆盖（0 FIXED / 2 PARTIAL / 3 OPEN）

| 旧发现 | 判定 | 现状 |
|---|---|---|
| 测试层级错位 | PARTIAL | session 的 hub/session 层实有包内测试（775 行，原报告基线即有，当时描述不准）；occupancy/survey 仍零包内测试；client.go 路由仍只经 runtime 间接覆盖 |
| Redis 测试静默 skip | PARTIAL | DB 隔离与 flush 语义已安全（497a420）；skip 依旧且 DB 16 引入新风险（E7） |
| 裸 t.Fatal/t.Error | OPEN | 12 文件 337 处；sdks/go 无 testify 依赖 |
| Benchmark 缺口 | OPEN | 广播/传输/redisbroker 热点恰是空白区 |
| defaultJitter 永不执行 | OPEN | 未动 |

### 维度 7：安全（3 FIXED / 1 PARTIAL / 4 OPEN）

| 旧发现 | 判定 | 现状 |
|---|---|---|
| C1 proxy encoding/json 静默丢字段 | **FIXED** | 三路径 protojson+DiscardUnknown；唯一残留是 RPC 成功路径无 DiscardUnknown（更严，方向安全） |
| 空 user 接管会话 | **FIXED** | `client.go:416`；三入口全覆盖、无旁路；但相邻面成为新 Critical S1 |
| proxy 响应体无上限 | **FIXED** | 4MiB LimitReader + gRPC MaxCallRecvMsgSize；上限不可配置（Minor） |
| admin HTTP 无认证/TLS/超时 | OPEN | 逐字未动 |
| admin Survey 超时不钳制 | OPEN | 未动 |
| QUIC 自签证书 24h | OPEN | 未动 |
| publish 不 ValidateTopic | OPEN | 未动，字面通配频道仍可命中通配订阅者 |
| 明文/加固杂项 | PARTIAL | 强制 token+常量时间比较+消息大小上限已就位；明文传输面维持 |

### 维度 8：配置与部署（4 FIXED / 2 PARTIAL / 1 OPEN）

| 旧发现 | 判定 | 现状 |
|---|---|---|
| 停机文档与代码漂移 | **FIXED** | 文档如实写"固定 30s 不可配置"；预算挤压另记 Minor |
| 集群示例缺 hmac_key | **FIXED** | deployment.md 与 configs 已带（但 configs 示例自身另有两处硬伤，E9） |
| stream_approximate 校验矛盾 | **FIXED** | 2007081 三态语义自洽 |
| cluster.backend 不校验 | **FIXED** | Normalize 闭集报错 |
| grpc_admin.addr 延迟暴露 | OPEN | 并入 E9 |
| 示例四子项 | PARTIAL | 双节点示例已重建；idle_timeout 注释与 require_auth 未展示仍在 |
| legacy_presence_channel | **FIXED** | 默认 false fail-closed，三处文档解释；唯 README 残留（E9） |

### 维度 9：性能热点（2 FIXED / 1 PARTIAL / 4 OPEN）

| 旧发现 | 判定 | 现状 |
|---|---|---|
| C2 广播 N 次序列化 | **FIXED** | per-encoding 一次 + enqueueBytes；小扇出净回归另记 Minor |
| JSON payload 双重转换 | PARTIAL | 机制就位但生产死分支（E1） |
| broker 发布路径全局写锁 | **FIXED** | RLock 快路径 + double-check 正确 |
| 一次 publish 两次 Lookup | OPEN | 且 interested() 仅为 bool 付全量代价（E11） |
| wcSubsMu 包住 matcher.Subscribe | OPEN | 未动 |
| cstrie COW 拷贝整层 | OPEN | 未动 |
| 广播 64 goroutine churn | OPEN | hub 内 fan-out 不变（cca9308 只挪了执行位置） |

---

## 6. 总结

- **修复链验收**：上一轮 4 个 Critical 修了 3 个、15 个 Important 修复了大半（65 项中 18 FIXED / 11 PARTIAL）；修复提交本身质量高（无越界 import、无协议破坏、带回归测试）。
- **本轮新增**：Critical 1 项（跨用户 resume 接管）、Important 约 11 项。最扎眼的是**三个性能/协议维度独立撞见的同一件事**：e471dee 的 JSON splice 因 marshaler 名称比较写错在生产永不生效、测试恰好测在死分支同侧（E1），连同它的防御回退隐患（E2）。
- **最优先三件事**：① 修 E1+E2（一行级条件改动 + 回退深拷贝，并把测试换成生产 marshaler）；② 决策 S1 的跨用户 resume 策略（默认拒绝或 resume token）；③ 修 E7 的 CI DB 编号并给 aaf1743/cca9308 补契约测试（E8）——否则 CI 的 Redis 覆盖与两个正确性修复都处于"看起来在、实际没验证"状态。
- **两条主线债务延续上一轮判断**：① 文档漂移只修了 deployment.md/protocol.md 两个面，developer 文档/README/AGENTS.md 仍系统性过期；② 平移式重构未完工（God 包、胖缝、双层别名），且架构债无 backlog 载体导致修复批次持续绕开——建议把 §5 维度 1 的 9 个 OPEN 补录 `docs/review/backlog.md` 后按既定 PR 序列推进。

---

## 附录：评审方法与分工

- 维度 1 架构、2 并发、3 规范、5 协议、6 测试、7 安全、8 配置、9 性能：8 个并行评审代理，各自通读上一轮报告对应章节 + `git show` 核对 16 个修复提交。
- 维度 4 错误处理：由主评审直接核对（gRPC handler 三传输读循环、client.go 错误路径、node.go 装配/回滚、disconnect.go 码表）。
- 基线命令：`go build ./...`、`go vet ./...`、`go test ./... -count=1`（结果见 §0）。

---

## 附：2026-08-21 修复批次（当日执行）

评审后同日落地了以下修复（详见工作区未提交改动）：

- **S1（Critical）**：跨用户 resume 拒绝——本地接管在 `existing.UserID() != "" && != authUser` 时以 `DisconnectInvalidToken` 断开（`client.go`）；跨节点路径在 CAS 之前校验 `lease.UserID`（`cluster_resume.go`，`ResumeRemoteSession` 增加 `authUser` 参数）。固化旧行为的测试已翻转，新增 `TestResumeRemoteSession_CrossUserDenied`。
- **E1/E2**：splice 条件改为 JSON wire 家族（`isJSONWireMarshaler`，生产 `ProtoJSONMarshaler` 生效）；防御回退改为 `ok=false` + 用真实 payload 重 marshal，不再别名池化 buffer。测试改用生产 marshaler 并新增 `countingMarshaler` 断言"每编码一次 MarshalAppend"、`TestSpliceRawJSONPayload_MissingPlaceholder`。
- **E3**：memory broker 的 offset 分配与分片入队在频道 history 锁下原子完成；`PublishTransient` 借同一把锁与 logged 发布保序；`Broker` 接口新增"跨频道并发、同频道按 offset 串行"的定序契约文档。新增 `TestMemoryBroker_ConcurrentPublishersDeliverInOffsetOrder`（8 并发 × 50 条严格递增）。
- **E4**：新增 `Session.contextSnapshot()`（锁下快照），替换 7 处 goroutine 生命周期内的裸 `ctx` 读。
- **E5**：HMAC 规范化升级为 v2 长度前缀编码（字段自定界，换行不可移位），`Channel` 与 `Metadata`（键排序）入签；新增换行碰撞回归测试 `TestCanonicalFormat_FieldBoundaryUnambiguous`。
- **E6**：admin Survey 超时与客户端路径同款钳制；客户端 publish 拒绝字面通配频道（BAD_REQUEST）；admin HTTP 支持可选 `server.http.auth_token`（Bearer + 常量时间比较），无 token 绑非 loopback 时启动告警。
- **E7**：redisbroker 测试库 DB 16 → 12（默认 16 库的 redis:7 上 DB 16 会让整套测试静默跳过）。
- **E9**：`configs/cluster-example.yaml` 补 `transport.grpc.addr` 与 `stream_approximate: true`；README 删除指向已移除 `cluster_emit` 的尾句；`server.grpc_admin.addr` 空值改为 Validate 直接拒绝。
- **C4-D**：三个传输的 `write_timeout: "0s"` 被 Validate 拒绝（消除无界写等待的唯一配置入口）。
- **Minor**：WS `Transport.Close` 不再抢 `writeMu`（WriteControl 文档化并发安全，WriteTimeout=0 下不再阻塞 Close）；ping deadline 改为先 arm 后发送（消除亚 RTT 应答的假 3511）；Hub "16384 subscription shards" 事实错误注释修正。
- 测试补充：`TestSession_EnqueueBytes_RejectsMarshalerMismatch`（aaf1743 契约）。

验证：`go build ./...`、`go vet` 全绿；`internal/session`、`internal/stream`、`pkg/transport/ws` 通过 `-race`；全量 `go test ./... -count=1` 结果见提交说明。

未处理（需要独立 PR 序列）：架构债（God 包/胖接口/双层别名，建议先补录 backlog）、C4-B（广播等待加预算）、文档批量翻新（developer 文档/AGENTS.md 路径）、sdks 测试规范、`sdks/go`/`shared` 子模块 `-race`。
