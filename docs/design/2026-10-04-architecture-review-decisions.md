# messageloop 架构评审与深化（deepening）裁决

| 字段 | 值 |
| --- | --- |
| 文档标题 | messageloop 架构评审与深化裁决（module / seam / depth 视角） |
| 作者 | qiulin + agent 会话评审（三个并行探查代理收敛） |
| 日期 | 2026-10-04 |
| 状态 | 已裁决，分批实施中 |
| 仓库路径 | `docs/design/2026-10-04-architecture-review-decisions.md` |
| 评审方法 | 按 git 热点圈定范围（namespace P1、Server API 鉴权链、session/recover 修复）；`/codebase-design` 词汇（module、interface、depth、seam、adapter、leverage、locality）+ deletion test；产出 HTML 报告（临时件 `/tmp/architecture-review-20261004-220057.html`，结论以本文为准） |
| 上位裁决 | `docs/v2/kernel-architecture.md`（KD-\*）与 [2026-09-16 admin-api-key-authz](2026-09-16-admin-api-key-authz.md)（D\*/G\*）视为既定，本文不重新裁决；凡涉及其中的硬规则均在对应决策处标注 |

---

## 一、候选与总裁决

六个 deepening 候选（编号沿用评审报告）：

| # | 候选 | 一句话 | 裁决 | 批次 |
| --- | --- | --- | --- | --- |
| C1 | Takeover 模块 | resume handoff 的 attachment 身份仲裁从 Session 各 close 路径收拢为一个 deep module | 采纳 | 第 4 批（最重） |
| C2 | Transport seam 加深 | framing kit + 错误分类 + ReadDeadline，消 ~250 行 adapter 重复 | 采纳（两个子项不做，见 D13/D17） | 第 3 批 |
| C3 | Recover 契约归一 | entry 分类入 recover 模块；epoch 能力上 `stream.Broker` 接口；`positionFrom` 收敛单份 | 采纳 | 第 1 正餐批 |
| C4 | Node 按调用方切片 | ServerAPIRuntime 接口 + 删零调用导出；拆包不做 | 部分采纳 | 第 5 批 + 热身批 |
| C5 | Server API 钳制下沉 | survey 钳制 / presence 快照截断 / publish history 回退三契约下沉 Node，两平面共用 | 采纳 | 第 2 批 |
| C6 | 浅 seam 三修 | Router.ByName；capability 单源 + census；matcher 四族退役 | 采纳（matcher 有前置，见 D20） | 热身批（matcher 可另插） |

**执行顺序**：热身批（D1-D4）→ C3 → C5 → C2 → C1 → C4 剩余。每批以 `go build ./... && go test ./...` 全绿为门槛（Redis 集成项本地有 Redis 则必跑，见 §六）。

**首选依据**：C3 有最新活体事故（TS SDK "fresh implies recover"，commit 7ce8784 归因即契约散落）、Strong 候选中爆炸半径最小、并消除一个静默失效模式（broker 缺 epoch 时校验静默放行）。C1 价值最高但动 single-activation 硬不变量，压在 recover 契约稳定之后。

---

## 二、关键决策及理由

### 热身批（浅 seam 修复，micro-PR）

**D1 · `Router.ByName(name)` 替代 api_auth 的 glob 重探。** 现状 `cmd/server/runtime.go` `apiAuthFindProxy` 把显式指派的 entry 自带 routes 再喂回 `FindProxy` glob 匹配（依赖"glob 总匹配自身模式文本"的技巧），无 routes 时静默得 nil。G3 裁定 api_auth 指派是"显式、唯一、可点名"的——解析路径应同样直接。加 `Router.ByName` 后按名取用，静默 nil 失效模式消失。deletion test：删探查函数后复杂度不迁移反而缩小。

**D2 · capability 闭集单源化 + census 兜底。** `internal/authz.ClosedCapabilityNames` 与 `config.CapabilityNames` 双份手工同步（两处注释互认同步义务），无结构性钉住。authz 本已 import config，故以 config 为单源、authz 引用之；另加一条 census 断言两表键集相等，把约定升格为红线（对齐 G1/G6 census 哲学在此处的缺位）。

**D3 · UserInfo 镜像 census。** `proxy.UserInfo` ↔ `proxypb`（root module 内反射钉字段完备性）；`sdks/go` 的服务端 SDK 镜像在 SDK 模块内自钉（跨 module 无法反射）。新增字段漏改任何一处镜像即测试红。

**D4 · 删 Node 零调用导出。** `APIPrincipal`、`AddProxy`、`GetHeartbeatIdleTimeout`、`PublishPresenceJoin/Leave` 共 5 个导出无生产调用方（S4 principal 参数化与 presence 链路改造的遗留）。实施时逐个复核（含 sdks/_examples），测试引用同步清理。

### 第 1 批 · C3 Recover 契约归一

**D5 · epoch 能力上 `stream.Broker` 接口本体。** 现为匿名 `interface{ Epoch() string }` 在 `recover.go`、`cluster_state.go`、`serverapi/api_handler.go` 三处断言——broker 缺该能力时 epoch 失配门**静默放行**。不引入独立 Epocher 接口：三个 producer（memory / redis / 测试替身）皆可实现，匿名可选接口这个后门整个删除。对 KD-K14（memory ≡ Redis）是加固而非冲突：能力显式化后等价性断言才有单一落点。

**D6 · `positionFrom` 收敛单份。** `internal/runtime/recover.go:74` 与 `internal/session/runtime.go:355` 字节级重复。单份落 internal/runtime（recover 模块），session 侧经既有 `session.Runtime` seam 取用（internal/runtime 已 import internal/session，方向不反转）。

**D7 · entry 分类移入 recover 模块。** resume 判定（nil snapshot 约定）、fresh 语义（fresh 仅在 Recover=true 时有意义）、ack-before-stream 顺序、snapshot-only 频道的 Recover:true 合成，从 `internal/session/client.go` 移入 recover 模块；serverapi GetHistory 的 epoch 校验改为委托。**语义零变化，只集中**——KD-K11/KD-K22（Position 为游标权威、"从头"仅经显式 fresh 或 StreamEpoch 重置）原文不动。TS SDK 事故的 bug 类由此获得单一归属地。

### 第 2 批 · C5 钳制下沉

**D8 · 三契约各下沉为一个 Node 方法。** survey 超时钳制（`api_handler.go` ↔ `client.go` 逐字复制，注释自认"exactly like the client survey path"）、presence 快照截断 + firstNonEmpty 映射、publish 的 AddHistory/策略回退——client 平面与 Server API 平面各自持份、静默漂移。下沉后两平面共一个实现；matrix 集成测试照旧从 Server API 面进入。**明确不做 handler 层重组**：探查证实 8 个 handler 无一是纯 pass-through（各带 scope 语义与聚合逻辑），该层通过 deletion test，保留。

### 第 3 批 · C2 Transport seam 加深

**D9 · framing kit 沉入 pkg/transport 共享。** read-loop 骨架（unmarshal → BAD_REQUEST 信封 → HandleMessage → 软失败 continue）、DISCONNECT_ERROR 信封、长度前缀帧——ws/quic/kcp/grpc 四 adapter 各持私份，quic/kcp 的 transport 层近乎 148 行克隆。收编后 adapter 只剩真正的传输差异。四个 adapter 满足同一 `session.Transport` seam，seam 本就真实——这是把实现侧的重复沉为一份，不新增 seam。

**D10 · `HeartbeatManager.ReadDeadline()` 导出。** `heartbeatReadTimeout` 三份手抄（ws/quic/kcp handler），其不变量（探测窗口不可被截断）本是 heartbeat 的属性。公式单源，改策略 B 时三处不再静默失效。

**D11 · adapter 以类型化 sentinel 错误包装对端关闭。** `session.go` 现反向 import gorilla/websocket 与 grpc status/codes，靠 `isPeerClosedError` 嗅探各 adapter 错误形状——seam 缺错误分类能力，Session 替 adapter 收拾。改为 kit 内定义 sentinel（`errors.Is` 判定），session 删两个反向 import。`WriteMany()` 零参探活一并在错误分类落地后重估（见 D13）。

**D12 · `Marshaler.IsJSONWire()` rider。** hub 现嗅探 `Name()` 字符串判 JSON 线格式（splice 有效性）。小拓宽，随手带上。

**D13 · 不做：显式 Ping 能力。** 为 `session.go:293` 的零参 `WriteMany` 探活加接口方法需 4 个 adapter 同改；D11 落地后该需求可能自消，届时重评。

### 第 4 批 · C1 Takeover 模块

**D14 · attachment 交换与 close 仲裁收拢为 Takeover 模块**（internal/session 新文件）。`canonical` / `delegate` / `loopAtt` / `closeFromAttachment` / `closeFromLoop` / `closeIfServingHandoff`（第三重身份比较器，按 Transport 身份而非指针）构成无 seam 的私有协议，散布于每条 close 路径；近期全部 P0（锁重入死锁、stale-shell delegate 误杀）落点在此。收拢后各 close 路径与 resume entry 调用之；`cluster_sim.go`（仅为测试再导出 fencing 入口、"生产代码不得调用"）随之消失。deletion test：身份规则今天就在每条 close 路径重现，收拢是真集中。**前置**：C3 先行（resume entry 与 handoff 交织，契约先稳）。护航：`client_fix_test.go` 44 例已从 Session 公开面驱动，新增 Takeover 接口表驱动用例。

**D15 · 创建 `GLOSSARY.md`。** 落 Session / Attachment / delegate（shell）/ Takeover / Position 五术语（评审探查确认的 load-bearing 域词）。

### 第 5 批 · C4 Node 切片（剩余）

**D16 · ServerAPIRuntime 接口。** internal/runtime 内为 serverapi 命名 ~20 方法接口（其真实调用切片）；KD-K26 不违反——接口留在 internal/runtime，facade home 不动。session.Runtime（37 方法）保持现状，不趁机重构。

**D17 · 不做：transport 3 方法切片命名。** transport 对 Node 的全部依赖是 `runtime.NewClient` + `MaxMessageSize` + `GetHeartbeatConfig`，构造函数已是那个 seam，命名 3 方法接口是仪式。

**D18 · 不做：Node 拆包。** KD-K26 已裁 internal/runtime 为 facade home；文件级已分 7 文件，包级拆分只损 locality。

**D19 · 测试 reach-in 机会主义处理。** 40+ 白盒触点（`node.surveys`、`SetNamespaceForTest` 等）不专项重构；仅在阻塞 C1/C3 实施时顺手命名为观察查询。

### 杂项

**D20 · matcher 四族退役（有前置）。** 先把 duplicate-subscription 语义统一进 `Matcher` 接口契约（现状接口注释自认各实现语义分歧、`Subscription.ID` 仅 bitmap 系有意义），再删 naive / trie / inverted_bitmap / optimized_inverted_bitmap（~530 行，仅测试与基准引用）。一个 adapter = 假想 seam。对比基准数据留 git 历史；实施时核对 ROADMAP 与 `pkg/topics` 基准引用。

**D21 · 每批全绿门槛。** `go build ./...` + `go test ./...`；本地起 Redis（`docker run --rm -p 6379:6379 redis:7-alpine`）则 Redis 集成项必跑（否则明示跳过，不得假绿——对齐 MESSAGELOOP_TEST_REDIS_REQUIRED 机制建立的纪律）。

---

## 三、风险与对策

| 风险 | 对策 |
| --- | --- |
| C1 动 single-activation 硬不变量 | 压在第 4 批；client_fix_test 全量护航 + Takeover 接口表驱动用例；一次只动一个 close 路径 |
| C7 语义移动引入行为漂移（D5-D7 名义"零变化"） | recover_test / cluster_resume_test / cluster_redis_integration 全量对照；Redis 在场必跑（D21） |
| C2 四 adapter 同改漏改一个 | framing kit 落地后逐 adapter 迁移、逐个跑 transport 测试与 sdks e2e，不一次切换四个 |
| matcher 删除丢失基准对比（D20） | git 历史留档；删前快照基准数字入本文件附注 |
| 下位实现偏离本裁决 | 每批落地即在本文追加"实现还原点"行（对齐 admin-api-key-authz 文档惯例） |

---

## 四、测试策略（按批门槛）

- **热身批**：router/proxy/authz/config 既有单测 + 新增 census 两条；`go test ./proxy/... ./internal/authz/... ./config/... ./internal/runtime/...`。
- **C3**：`internal/runtime` recover/cluster_resume 全量 + `internal/session` 相关 + Redis 集成（在场必跑）。
- **C5**：`internal/serverapi` matrix（api_key_scope_test 走真实 Server API 面）+ `internal/session` survey/presence 用例。
- **C2**：`pkg/transport/...` 四族 + `internal/session` 心跳/关闭路径 + `sdks/go` e2e（Redis 在场时）。
- **C1**：`internal/session` client_fix 全量 + 新增 Takeover 表驱动 + `internal/runtime` 集群 resume。
- **C4**：`internal/serverapi` 全量（接口引入不应有行为差）。

---

## 五、明确不做的事（汇总）

1. handler 层重组（D8 附注：8 个 handler 零 pass-through，层通过 deletion test）。
2. 显式 Ping 接口能力（D13，条件重估）。
3. transport 3 方法切片命名（D17）。
4. Node 包级拆分（D18，KD-K26 已裁）。
5. 测试面专项重构（D19，机会主义）。
6. 本轮不引入任何新 seam：所有深化都在既有 seam（session.Transport、session.Runtime、stream.Broker、scope 层）上加深度，不开新口。

---

## 附：改动面清单（按批）

| 批 | 文件 |
| --- | --- |
| 热身 | `proxy/router.go`、`cmd/server/runtime.go`、`internal/authz/authorizer.go`、`config/config.go`、`internal/serverapi/census_test.go`（或新文件）、`proxy/proxy_test.go`（UserInfo 镜像）、`sdks/go`（自钉）、`internal/runtime/node.go`（删导出） |
| C3 | `internal/stream/broker.go`、`pkg/redisbroker/redis.go`、`internal/stream/broker_memory.go`、`internal/runtime/recover.go`、`internal/runtime/cluster_state.go`、`internal/session/client.go`、`internal/session/runtime.go`、`internal/serverapi/api_handler.go` |
| C5 | `internal/runtime/node.go`、`internal/serverapi/api_handler.go`、`internal/session/client.go` |
| C2 | `pkg/transport/`（ws/quic/kcp/grpc + 新 kit）、`internal/session/heartbeat.go`、`internal/session/session.go`、`internal/session/hub.go`（IsJSONWire）、`shared/marshaler.go` |
| C1 | `internal/session/`（新 takeover 文件 + session.go/client.go 改造）、`internal/runtime/cluster_sim.go`（删）、`GLOSSARY.md`（新） |
| C4 | `internal/runtime/`（ServerAPIRuntime）、`internal/serverapi/` |
| 杂 | `pkg/topics/`（删四族 + 接口契约统一） |

---

## 附二：实现还原点（逐批追加）

### 热身批（2026-10-04 落地）

- **D1 已落地**：`Router.ByName`（名字索引首注优先，空名忽略，`Close` 随路由一并清空）；`Node.FindProxyByName`；`apiAuthFindProxy` 改按名解析。顺带消掉两个既有隐患——① glob 重探走 first-match，更早注册的宽模式可截走显式指派的 proxy（`TestRouter_ByName_NotShadowedByEarlierBroadPattern` 钉住）；② 无 routes 的指派 entry 静默 nil → 现按名可达（`TestRouter_ByName_RoutelessEntryStaysReachable` 钉住）。
- **D2 落地形态偏离裁决原文，记录如下**：单源化被依赖环卡住——authz 已 import config（`config.AuthorizerConfig`），config 无法反向 import 拿到 `Capability` 位，故 name→bit 映射无法并入 config。落地为：① `TestCapabilityNameCensus`（authz 包内可同时见两表）钉 `ClosedCapabilityNames` ↔ `config.CapabilityNames` 键集相等；② 新增 `authz.ParseCapabilityNames` 收敛 serverapi 与 node 两处同构的 ceiling 解析循环（`clampIdentity` 的 WARN 语义不同，保留原样）；③ 两侧"手工同步"注释改为指向 census。结构性根治（把 `AuthorizerConfig` 移入 authz 反转依赖环、名集归一）记为 C4 批的候选随行项。
- **D3 已落地**：root 侧 `proxy/userinfo_census_test.go`（镜像键集相等 + `FromProtoAuthenticateResponse` 全字段携带 + 往返）；SDK 侧 `sdks/go/userinfo_census_test.go` 同型。SDK 测试纯 stdlib——该模块保持零 testify 依赖纪律，`go.mod`/`go.sum` 零改动。
- **D4 落地形态偏离裁决原文，记录如下**：复核后发现"5 个零生产调用"需修正——`PublishPresenceJoin/Leave` 在 node.go 有内部调用点（legacy companion 路径），`AddProxy`/`APIPrincipal` 被 `cluster_redis_integration_test.go`（**外部**测试包 `runtime_test`）用作注册/静态超管身份 seam。故落地为：`publishPresenceJoin/Leave`、`heartbeatIdleTimeout` 三个**转未导出**（同包测试随改）；`AddProxy`、`APIPrincipal` **保留导出**并改注释如实记载其测试 seam 角色。`runtime` 包别名表随删无人使用的 `ClosedCapabilityNames` 再导出。
- **D21 执行注记**：本环境无 Redis 亦无容器运行时，Redis 集成项本轮全部跳过（显式记录，不假绿）；非 Redis 全量 `go build ./... && go test ./...` 绿。

### 批 1 · C3 Recover 契约归一（2026-10-04 落地）

- **D5 已落地**：`Epoch() string` 上 `stream.Broker` 接口本体（含 KD-K11/KD-K22 契约注释：epoch 为 "" 的 position 按未校验处理而非视为匹配）。生产侧三处匿名断言全删——recover.go `streamEpoch()` 改直调（保 nil 守卫）、cluster_state.go 快照直写 `BrokerEpoch`、serverapi GetHistory 改经新导出的 `Node.StreamEpoch()` 委托；测试侧四处断言同步改直调，14 个缺 `Epoch()` 的 Broker 测试替身补桩。
- **D6 已落地**：单份归宿为 **`shared.PositionFrom`**（shared 模块新文件 position.go，比裁决原文预估的 recover 模块内收敛更进一步——session 与 runtime 两侧均已 import shared，协议级函数落 shared 使两侧零新增依赖方向）。runtime/session 两份局部 `positionFrom` 删除，7 处调用点迁移。
- **D7 已落地**：① entry 分类新成员 `Node.SnapshotRecoverySubs(snapshot, requestChannels, failedChannels)`——快照独有频道的合成 `Recover:true`、hydrate 失败排除、请求频道去重、**快照内部自去重**（原 session 内联循环经 seenRecovery 隐式达成，迁移时显式化保真）——与 `recoveryCursor`/`recoverySkip` 同文件同模块；session 经 `session.Runtime` seam 新增一方法调用（37→38），`fakeRuntime` 补桩。② `Node.StreamEpoch()` 导出，serverapi 的 epoch 失配校验由本地重实现改为委托。③ ack-before-stream 顺序保留在 session（协议帧序，非 recover 语义，移动即错位）；nil-snapshot 约定在 `SnapshotRecoverySubs`/`recoverSubscription` 注释单一落点。
- **验证**：`go build ./...` + `go vet ./...` + 全量 `go test ./...` 绿（Redis 集成项缺环境跳过，同 D21 注记）；shared 模块独立 `go test ./...` 绿。

### 批 2 · C5 钳制下沉（2026-10-04 落地）

- **survey 钳制 → `survey.ClampTimeout`**（落在 internal/survey 叶子包而非 Node 方法——两侧已 import 该叶子，纯函数免扩 session.Runtime seam；命名常量 DefaultSurveyTimeout/MaxSurveyTimeoutCeiling/MinSurveyTimeout 随之入 defaults.go）：client.go 与 api_handler.go 两份逐字复制删除，新增 8 例表驱动单测钉契约。
- **presence 截断上限 → `Node.PresenceSnapshotLimit(ch)`**：policy 覆盖 || MaxPresenceSnapshotClients 的解析单源；`presenceSnapshot`（client 面）与 GetPresence 截断（Server API 面）共用。快照字段映射（firstNonEmpty 等）留在各自平面——目标 proto 类型不同（clientpb vs serverv2），强行共享即错位。
- **publish 回退 → `Node.PublishForAPI(ch, pub, addHistory)` + `channel.ErrAddHistoryDenied`**（sentinel 邻接 ErrHistoryDisabled，runtime 别名表转发）：Server API channel 循环的 add_history/policy 路由收敛为一个 Node 方法;失败计数与日志归因留 handler（聚合语义）。**有意保留的差异**：client 面 handlePublish 的透明转换（forceTransient → ack offset 0 + 指标）与 Server API 面的 fail-closed（ErrAddHistoryDenied → failed++）是两个不同契约，不合并——注释双向指认。
- **验证**：build/vet/受影响包（serverapi、session、runtime、survey、channel）全绿。

### 批 3 · C2 Transport seam 加深（2026-10-04 落地，D11 除外）

- **D9 已落地**：新包 `pkg/transport/framing`——`framing.Writer`（串行帧 + 每写时限 + closed 旗标 + 断连帧写，QUIC/KCP 收编其 ~148 行克隆，各 adapter 只剩 conn 类型与关闭语义差异）+ `framing.DisconnectMessage`（DISCONNECT_ERROR 信封唯一形状，gRPC 的 writeError 同步复用，信封三份归一）。每 adapter 的 `ErrTransportClosed` 措辞经 closedErr 参数保留（测试与日志归因不变）。
- **D10 已落地**：`session.HeartbeatConfig.ReadDeadline(configured)`——读时限公式单源（原三份手抄，耦合仅靠注释"rules match the WebSocket handler"）；ws/quic/kcp read loop 改经 `GetHeartbeatConfig().ReadDeadline(...)`；三份 per-transport 公式测试删除，收敛为 session 包一份表驱动（`TestHeartbeatConfigReadDeadline`，用例为三处旧测并集）。
- **D12 已落地**：`Marshaler.IsJSONWire() bool` 上接口（protojson 线格式能力，杜绝 Name() 字符串分支）；hub 的 `isJSONWireMarshaler` 嗅探删除，splice 分支改能力调用。
- **D11 顺延至批 4（有意重排，记录如下）**：错误分类（session 反向 import gorilla/websocket + grpc status 的 `isPeerClosedError`）与 C1 动同一片 close/error 路径（handleWriteError → Fence/Close 仲裁正是 Takeover 的地盘），合并到批 4 一次翻动 session.go，避免同文件两轮抖动。批 3 结束时该债务原样在册。
- **验证**：root `go build`/`go vet`/全量 `go test` 绿；shared 模块绿；sdks/go 模块 build + `go test ./...` 绿（4.3s，含 transport 拨号回归）。

### 批 4 · C1 Takeover 模块 + D11 错误分类 + GLOSSARY（2026-10-04 落地）

- **D14 已落地**：`internal/session/takeover.go`——canonical 路由、`handoffAttachment`（新对象包交接 transport，指针身份与 transport 身份分离的根由）、`takeoverBy`（client.go 的 takeover 执行块收拢：Detach→Attach 新对象→壳置 delegate）、`closeFromAttachment`/`closeFromLoop`/`closeIfServingHandoff` 全部迁入，文件头注记三重身份比较器的适用次序。session.go 只留 §7 错误映射（handleWriteError）。
- **D11 已落地**：`session.ErrPeerGone` sentinel 落 seam 文件（transport.go）；ws adapter 包装 normal/going-away close 形状、grpc adapter 包装 Canceled/Unavailable（errors.Join 保 errors.Is 双链）；quic/kcp 不包装（现状本就不判 peer-gone，保持零行为变化）；session 的 `isPeerClosedError` 缩为 io.EOF/net.ErrClosed/ErrPeerGone 三判，**gorilla/websocket 与 grpc status/codes 的反向 import 删除**。
- **D15 已落地**：`GLOSSARY.md` 创建——Session / Attachment / delegate（shell）/ Takeover / Position 五词条 + 既有高频词登记。
- **D14 偏差记录**：裁决原文预期"`cluster_sim.go`（仅测试再导出）随之消失"**不成立**——该文件再导出的是 Node 侧 resume/fencing 内部入口（`SimSyncClusterSessionState`/`SimResumeRemoteSession`/`SimMembershipOnce`），供外部 sim 测试包驱动确定性 fencing 场景，与 session 身份仲裁无关，保留原样。
- **事故记录（透明起见）**：批内一次脚本失误将 session.go 截断为空；因本会话此前对 session.go 零改动，`git checkout --` 从 HEAD 无损恢复后以 Edit 工具重做，无净损失。
- **验证**：root build/vet/全量测试绿；session/runtime/transport 相关包全绿；sdks/go、shared 模块绿。

### 批 5 · C4 ServerAPIRuntime 接口（2026-10-04 落地）

- **D16 已落地**：`internal/runtime/serverapi_runtime.go`——serverapi handler 的真实调用切片（18 方法：授权三元组、user fan-out、session 命令四件、survey/presence/channels、Broker+StreamEpoch、PublishForAPI）命名为 `ServerAPIRuntime` 接口，`*Node` 为唯一生产 adapter（编译期断言）；`api_handler.go` 的字段与构造参数从 `*runtime.Node` 改为该接口。PrepareServer 装配侧继续持具体 `*Node`（KD-K26：接口留在 internal/runtime，包零迁移）。session.Runtime 37→38 方法保持现状未动（D16 原文如此）。
- **验证**：serverapi 全量（含 matrix 1280 行集成）绿。

### 杂项 · D20 matcher 四族退役（2026-10-04 落地）

- **已落地**：naive / trie / inverted_bitmap / optimized_inverted_bitmap 四族及其测试、跨实现 consistency_test、五族对比 throughput_test 删除（共 ~1300 行含测试）；`Matcher` 接口注释的"duplicate-subscription 语义各实现不同"分歧段改写为单一契约（幂等 per (topic, Subscriber)）；`Subscription.ID` 字段删除（仅 bitmap 系有意义，外部零引用）。生产三处（hub、memory broker、redisbroker）本就只用 CSTrie；namespace_test 的跨实现表收缩为 cstrie 单实现。
- **基准快照**（删前留档，`go test -bench BenchmarkPopulate -benchtime=200x`，20 核）：Naive 159µs/103KB/2750allocs · Trie 227µs/151KB/3760 · **CSTrie 235µs/153KB/3784** · OptBitmap 457µs/306KB/3764 · InvertedBitmap 78ms/85MB/176万——CSTrie 与 Trie 同档，退役四族无性能回退风险。
- **验证**：topics/session/stream/redisbroker 包绿；全模块最终全绿（root + sdks/go + shared）。

### 补验 · Redis 集成项（2026-10-04，容器启动后）

- **D21 注记更新**：Redis 容器就绪后以 `MESSAGELOOP_TEST_REDIS_REQUIRED=1` 严格模式补跑此前显式跳过的全部集成项——`pkg/redisbroker` 60.8s、`internal/runtime`（含 cluster_redis_integration / cluster_v1_e2e）73.5s、`internal/session`、`internal/stream`、`sdks/go` 全量 4.3s（`TestE2EProcess/RedisBroker` 真实执行非跳过，0.16s）——**全部通过**。至此本裁决全部改动在 Redis 在场条件下验证完毕，热身批 D21 的"缺环境跳过"注记就此关闭。
