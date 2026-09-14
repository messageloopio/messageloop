# MessageLoop 机制缺口排查（2026-09-14，基于同日架构评审）

对评审发现做机制层归因：每个问题类还原逃逸路径（每层防线为何失守）、判定个案/一类、给出指认层的机制补齐方案。验收标准统一为一句话："**下一个同类问题会在哪一层被拦住？**"

现有防线盘点（回溯基准）：CI = build + `go vet` + 生成代码一致性 + `go test -race`（主模块/shared/Go SDK）+ TS 构建；无 golangci-lint。仓库已有三个"一致性测试钉语义"的机制先例可沿用：`error_codes_test.go`（码表普查）、`pkg/topics/consistency_test.go`（五匹配器语义一致）、`cmd/server/config_consistency_test.go`（仓库配置全量 Validate + 预绑定）。

---

## G1 入口守卫遗漏类（SubRefresh 越权，评审 #4）

**逃逸路径**：namespace 检查无结构约束，靠每个 handler 自觉调用 `checkNamespace`。subscribe/unsubscribe/survey/presence 都记得了，SubRefresh 漏了——途经：类型层（无约束，channel 字段不强制过守卫）、代码评审（每个 PR 局部看"这个 handler 有没有检查"凭记忆，新 handler 没有对照清单）、测试（未覆盖跨 ns SubRefresh）、运行时（越权尝试无指标）。每层都靠"人记得"。

**类别判定**：一类。判据：SubRefresh 本身就是"同成因换地方再发生"的实证——每新增一个携带频道的消息类型，风险重演一次。

**机制补齐**（最早拦截层 = 结构层，沿用 `handleMessage` 已有的中央检查惯例）：
1. **结构层**：在 `handleMessage` 的 switch 之前（`client.go:246`）增加中央 namespace 预检——对所有携带 channel/频道列表的 envelope 统一 `checkNamespace`，与 242 行的认证门并列。handler 内的检查降级为纵深防御。新 handler 不再承担记忆义务。
2. **测试层**：表驱动回归——枚举 `InboundMessage` 所有携带频道的 oneof 变体，逐一断言跨 ns 请求在入口被拒；新变体加入时该表必须登记（漏登记即测试失败，同 `error_codes_test.go` 的普查惯例）。

**验收**：下一个携带频道的新消息类型，即使 handler 一行 namespace 代码都不写，也在 `handleMessage` 入口被拒；或编译期因未登记进频道载体枚举而失败。

## G2 无界状态增长类（lastApplied 等 6 处，评审 #7）

**逃逸路径**：每处 map 的写入都局部正确，缺的是删除路径。途经：代码评审（单 PR 视角看不出全局无删除）、测试（无"churn 后回到基线"断言）、运行时（条目数无 Gauge，增长不可见）、vet/race（均不检测逻辑泄漏）。六个实例横跨三个模块，逐个手工修是补丁姿势。

**类别判定**：一类，且已在评审中被 6 处兑现。

**机制补齐**：
1. **运行时暴露（立即）**：每处无界结构加条目数 Gauge（复用既有 `SetMetrics` 接线）；测试加"会话/频道 churn 后条目归零"断言。
2. **结构层（收敛清理义务）**：定义会话销毁时的统一清理点（如 `StateJanitor` 接口，Node 在 Close 路径统一调用）；新增 per-session/per-channel 状态时实现该接口即自动被清理——清理成为类型义务而非记忆义务。六处既有实例的修复全部挂到该机制上。

**验收**：下一个新增的 per-session 内存结构若 Close 后未清理，会在 churn 归零回归测试失败或条目 Gauge 告警中被立刻发现——而不是数周后 OOM。

## G3 锁纪律类（MarkMetricsCharged 死锁，评审 #1）

**逃逸路径**：RWMutex 不可重入是运行时行为；`go vet` 的 copylocks 不查重入；race detector 只报数据竞争不报死锁；测试只覆盖活会话路径，AddClient×Close 竞态窗口无压力用例；评审看不出来（`TransportLabel()` 是合法公开方法，调用点"看起来"正确）。团队其实已有锁纪律惯例（`*Locked` 后缀方法族），但惯例靠自觉。

**类别判定**：一类（持锁段调用带锁辅助方法是可复发的模式，非一次手滑）。

**机制补齐**：
1. **结构层（消灭诱因）**：遵循既有 `*Locked` 惯例——持锁路径一律调用无锁版本（本例 `metricsTransportLabelLocked(protocol)`）；grep 审计同模式调用点。
2. **测试层（拦截复发）**：AddClient × Close 双 goroutine 交错压力回归，跑在 CI 已有的 `-race` 下；死锁以测试超时形式暴露。

**验收**：下一个在持锁段调用带锁方法的改动，在竞态回归测试（-race + 超时）中暴露，而非生产连接僵死。

## G4 复制式一致性漂移类（四传输 + 两 SDK，评审 #10/#34/#5/#6）

**逃逸路径**：复制副本时注释写"rules match"代替共享；改动一个副本不会令任何测试失败（其他副本没有一致性断言）；CI 按模块独立测试，跨实现语义无夹具。已实际漂移三处（WriteTimeout=0 语义、超长帧反馈、close-code 0）+ TS/Go SDK 断连码语义分叉。

**类别判定**：一类，漂移已兑现四次。

**机制补齐**：
1. **结构层（最早拦截）**：`heartbeatReadTimeout`、断连信封构造、自签证书下沉到 `pkg/transport/internal` 共享包，副本归一为一份，传输差异显式为参数——漂移在结构上不可能。
2. **测试层（钉行为 + 钉 SDK）**：
   - 跨传输行为一致性表驱动测试：同一场景（超长帧、0 写超时、code-0 关闭、读超时日志级别）四传输断言同一结果——直接沿用 `pkg/topics/consistency_test.go` idiom。
   - SDK：一份共享码表夹具（JSON：code → 期望行为：重连/停止/降级），Go 与 TS 测试消费同一份——两侧漂移即 CI 失败。

**验收**：下一个传输层行为改动只改一处，漏改在跨传输一致性测试失败；SDK 码表/重连策略漂移在共享夹具测试失败。

## G5 配置解析分散类（14 处 ParseDuration + proxy 段 Validate 盲区，评审 #12）

**逃逸路径**：字符串时长字段被 config / cmd/server / NewNode / redisbroker options 各自解析，失败策略三种并存（Validate 拒绝 / 静默回退 / 静默默认）；proxy 段整体不在 Validate 覆盖内，`TestRepositoryConfigsValidateAndPrebind` 只钉仓库样例配置（负 timeout 不在其中）；新增配置项无任何检查强制"必须进 Validate"。

**类别判定**：一类（每次新增配置项都重演）。

**机制补齐**：
1. **类型层（最早拦截）**：config 包归一化产出 `time.Duration` 字段，Validate 完成全部解析、失败即拒绝；下游删除自己的 ParseDuration——非法值在启动前被拦，"静默回退"在结构上消失。
2. **门禁层**：Validate 补 proxy 段用例；字段普查测试——枚举 config 的字符串时长字段表，每个字段喂非法值断言 Validate 必须报错（新增字段漏登记即测试失败，`error_codes_test.go` 惯例）。

**验收**：下一个新增的时长配置项，非法值在 Validate（启动前）被拒；未进 Validate 的字段被普查测试点名。

## G6 依赖库安全语义误判类（deflate 炸弹；QUIC MaxIncomingUniStreams=-1 为同族第二例）

**逃逸路径**：库语义与直觉相反（gorilla 的 SetReadLimit 作用于压缩后字节；quic-go 把负值流上限归一为 0），文档埋在源码/issue；评审时"有 readLimit"表面成立；无发送高压缩比帧的安全测试；无连接内存上限告警。

**类别判定**：个案倾向，但"库默认值 ≠ 语义"已两次兑现，按轻量机制处理（不配重型监控）。

**机制补齐**：**测试层**——传输层安全夹具：高压缩比帧（解压后 ≫ maxMessageSize）必须被拒且服务端内存增幅有界；QUIC 流数行为断言。挂在 CI 已有 race test 即可。修复本身（解压路径强制上限）作为纵深防御保留。

**验收**：下一个"库默认值语义误判"类缺陷在安全夹具中暴露。

## G7 无超时 Background ctx 类（Close presence 清理、nextOccupancyGen、onGap）

**逃逸路径**："后台清理用 Background"看似合理，缺超时不报错只变慢；无 Redis 调用耗时指标，慢路径不可见；无静态检查能发现"存储调用无 deadline"。

**类别判定**：一类（3+ 处同形），但影响是延迟放大而非数据错误——力度取轻。

**机制补齐**：约定 + 轻量门禁——提供 `storeCtx(parent)`（默认短超时）作为存储层调用约定；CI 加一条模式化 grep 检查（`context.Background()` 直接传入存储调用即失败，白名单豁免）；可选：Redis 调用耗时直方图。**不**为此上重型监控（机制过敏警戒）。

**验收**：下一处对 Redis 的无 deadline 调用在 CI grep 门禁被点名。

---

## 优先级与补丁-机制配对

| 机制 | 拦截层 | 价值 | 成本 | 配对的当期补丁（评审编号） |
|------|--------|------|------|---------------------------|
| G1 中央 namespace 预检 | 结构（dispatch 入口） | 高（多租户隔离） | 低 | #4 SubRefresh |
| G2 清理钩子 + 条目 Gauge | 结构 + 运行时 | 高（OOM 级） | 中 | #7 六处无界状态 |
| G4 传输内核 + 共享夹具 | 结构 + 测试 | 高（已漂移 4 次） | 中 | #10/#34/#5/#6 |
| G5 配置归一化 + 字段普查 | 类型 + 门禁 | 中 | 中 | #12 proxy Validate |
| G3 `*Locked` 惯例 + 竞态回归 | 结构 + 测试 | 中 | 低 | #1 死锁 |
| G6 安全夹具 | 测试 | 中 | 低 | #2 deflate |
| G7 storeCtx 约定 + grep 门禁 | 约定 + CI | 低 | 低 | #20/#29 |

行动建议：G1/G3/G6 的机制与 P0 补丁同一条 PR 落地（成本低到不值得拆）；G2/G4/G5 作为独立机制任务与对应补丁并行立项——按 skill 纪律，**补丁注释里标注机制任务号，禁止"补丁打完问题关闭"**。
