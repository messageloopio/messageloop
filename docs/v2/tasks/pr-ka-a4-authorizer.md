# PR-KA-A4 实现规格：Authorizer 一张表、语言包含、Capability 闭集

| 字段 | 值 |
| --- | --- |
| 标题 | `auth: one Authorizer table, language inclusion, closed capabilities` |
| 状态 | **Ready**（尚未实现） |
| 依赖 | A3 已合（`CompileInterest` / `ErrPatternNotRoutable`）。在 `v2` 分支上做 |
| 设计来源 | [kernel-architecture.md](../kernel-architecture.md) Authorizer、KD-K10、KD-K15、KD-K17、KD-K21、KD-K31 |
| 验收人 | 主 agent |

## 1. 目标

把「十个授权谓词 + ACL 与 channel policy 两张表 + 代理短路」收成 **一个 Decide**。

1. 根包新增 `Authorizer`：`Decide(principal, action, channel) → Decision`。
2. `SubscribePattern` 用 **语言包含**（`L(p) ⊆ AllowLang`），不是「此刻已存在的精确频道」。
3. 通配语言与 `pkg/topics` 同一套：`*` 单段、`**` 仅末段。取消 ACL 中段 `**`。
4. 配置只读 `server.authorizer` 一张表（pattern → allow/deny + Effects）。**拒绝**再读 `server.acl` / `server.channels`（KD-K31，无旧 YAML 兼容期）。
5. Admin Capability **闭集**；`GetHistory` / `GetPresence` / `GetChannels` / 代订 必须持位，不得旁路。
6. Proxy 的允许/拒绝流回 Decide，**不得**命中代理就跳过内置规则。
7. RPC 无匹配代理 → 软失败 `NO_PROXY`，不再 echo 请求体。

**不做：** Session/Attachment 拆分（B1）；Occupancy 换 LiveBus（B2）；流式恢复（B3）；HMAC 命令总线（B4）；切运行时到 `clientv2` proto；改 `CompileInterest` / Redis live 订阅；改 A1 fencing、A2 gap。

## 2. 允许改动的文件

- `authorizer.go`（新）+ `authorizer_test.go`（新）：`Authorizer`、`Decide`、语言求交、Capability
- `acl.go` / `acl_test.go`：删除 `ACLEngine` 与中段 `**` 匹配；`matchChannelPattern` 若仍被 Effects 使用，改为 `topics.Match`（不得再接受 `a.**.b`）
- `channel_policy.go` / `channel_policy_test.go`：`ChannelPolicy` 类型与 `DefaultChannelPolicy` 可留；引擎改为从 Authorizer 规则取 Effects（或删引擎、由 `Authorizer.Effects` 替代）
- `config/config.go`、`config/config_test.go`：`server.authorizer`；`server.acl` / `server.channels` 出现则 `Validate` 失败；Capability 名校验
- `config-example.yaml`、`config.yaml`（若仓库里有）：迁到新表
- `node.go`：装配 `Authorizer`；`ChannelPolicy(ch)` 改为 `authorizer.Effects(ch)`；删 `node.acl`
- `client.go`：订阅/发布/Survey 走 `Decide`；Proxy 不再短路；RPC `NO_PROXY`
- `cluster_commands.go`：`AdminCanSubscribe` / `AdminCanPublish` 走 Decide + Capability
- `pkg/grpcstream/api_handler.go`：`GetHistory` / `GetPresence` / `GetChannels` / 代订 / 按 user 扇出 持位检查
- `recover.go`：仅当 Recover 判定改读 `Decide(ActionRecover)` / `Effects.Recover` 时才动（语义与今日 `ChannelPolicy.Recover` 对齐即可）
- **所有**构造 `config.ACLConfig` / `config.ChannelConfig` / `NewACLEngine` / `NewChannelPolicyEngine` 的测试与示例配置（根包、`pkg/grpcstream`、`pkg/websocket`、`survey_test.go`、`cluster_*_test.go` 等）
- `docs/developer/01-architecture.md` §3.7 ACL、§3.8 代理优先级
- `docs/developer/02-configuration.md` `server.acl` / `server.channels` 段
- `docs/v2/tasks/pr-ka-a4-authorizer.md`（完成备注）

禁止：改 proto、SDK 业务逻辑、A1 fencing、A2 History/gap、A3 `pubsub.go` live 编译、`hub.go` 扇出算法、git commit/push。

## 3. 现状（动手前再读）

- `acl.go`：`CanSubscribe`/`CanPublish` 默认允许、`CanSurvey` 默认拒绝；`DenyAll` 短路；其余 last-write-wins；`**` 允许出现在中间（`a.**.b`）。
- `channel_policy.go`：另一张表，**first-match** overlay，与 ACL 顺序相反。
- `client.go` `checkSubscribeACL` / `handlePublish`：`FindProxy` 命中则 **只信代理、跳过内置 ACL**。
- `client.go` `handleRPC`：`proxy.ErrNoProxyFound` 时把请求 payload **echo** 成 `RpcReply`（`client_test.go` `TestClientSession_HandleMessage_RpcRequest_NoProxy`）。
- `node.go`：`cfg.ACL.Rules` 非空才建 `ACLEngine`；`ChannelPolicyEngine` 总是建。
- Admin：`adminPrincipal = "admin"` 走同一套 `CanSubscribe`；`GetHistory`/`GetPresence` **无** Capability 位。
- 订阅不可路由已由 A3 在 broker + client 软失败；本 PR 的 Decide 对 `*.room` 也必须先于 ACL 给出 `not_routable`（双保险，信封仍是 `PATTERN_NOT_ROUTABLE`）。

## 4. 类型

放在根包 `authorizer.go`：

```go
type Action int

const (
    ActionSubscribePattern Action = iota
    ActionPublish
    ActionRecover
    ActionPresence
    ActionSurvey
)

type PrincipalKind int

const (
    PrincipalUser PrincipalKind = iota
    PrincipalAdmin
)

type Principal struct {
    Kind   PrincipalKind
    UserID string      // User 的用户 ID；Admin 规则匹配仍可用 "admin"
    Caps   Capability  // 仅 Admin 有意义；User 为 0
}

type Capability uint32

const (
    CapPresenceLargeSnapshot Capability = 1 << iota // presence.large_snapshot
    CapSurveyBypassGate                             // survey.bypass_gate
    CapHistoryRead                                  // history.read
    CapPresenceRead                                 // presence.read
    CapChannelsList                                 // channels.list
    CapSessionAct                                   // session.act
    CapUserFanout                                   // user.fanout
    CapSubscribeAny                                 // subscribe.any
    CapPatternGlobal                                // pattern.global
)

// ClosedCapabilityNames is the closed set. Unknown YAML names are a Validate error.
var ClosedCapabilityNames = map[string]Capability{
    "presence.large_snapshot": CapPresenceLargeSnapshot,
    "survey.bypass_gate":      CapSurveyBypassGate,
    "history.read":            CapHistoryRead,
    "presence.read":           CapPresenceRead,
    "channels.list":           CapChannelsList,
    "session.act":             CapSessionAct,
    "user.fanout":             CapUserFanout,
    "subscribe.any":           CapSubscribeAny,
    "pattern.global":          CapPatternGlobal,
}

// DefaultAdminCapabilities is used when server.grpc_admin.capabilities is omitted:
// every closed bit except CapPatternGlobal (holding ** Interest must be explicit).
// Implement as the OR of all Cap* constants except CapPatternGlobal.

type Decision struct {
    Allow   bool
    Reason  string // "default" | "deny_all" | "allow_list" | "effects" | "not_routable" | "missing_capability" | "language"
    Effects ChannelPolicy
}

func NewAuthorizer(cfg config.AuthorizerConfig) (*Authorizer, error)
func (a *Authorizer) Decide(p Principal, action Action, channel string) Decision
func (a *Authorizer) Effects(channel string) ChannelPolicy
func (a *Authorizer) ReplaceRules(cfg config.AuthorizerConfig) error
func (a *Authorizer) PatternsToRevoke(p Principal, subscribed []string) []string
```

`ChannelPolicy` 保持今日字段（`History`/`Presence`/`Recover`/`Survey`/`TransientOnly`/…）。`DefaultChannelPolicy()` 语义不变。

## 5. 规则语言与求交

### 5.1 合法 pattern

规则 `pattern` 与订阅 key 同一语言：

1. `topics.ValidateTopic` 失败 → 配置非法（`NewAuthorizer` / `Validate` 报错）。
2. 含 `*` 时：最后一段必须是 `*` 或 `**`，前面每段都是字面。否则非法（含 `*.room`、`im.*.tick`、`a.**.b`）。
3. 字面前缀为空（`*` / `**`）→ 非法（与 KD-K13 / A3 一致；`pattern.global` 是 Admin 位，不是 YAML 里写一条 `**` 规则）。

编译形（仅实现内部，测试可通过 `Decide` 观察）：

| 写法 | kind | prefix 段 |
| --- | --- | --- |
| `chat.room.1` | exact | `[chat, room, 1]` |
| `im.room.*` | star | `[im, room]`（再恰好一段） |
| `im.**` | dstar | `[im]`（零段或多段） |

### 5.2 `L(a) ∩ L(b)` 非空（必须按此实现，禁止枚举频道）

对称。记 `lenP` 为 prefix 段数。

| A \ B | exact | star | dstar |
| --- | --- | --- | --- |
| **exact** | 段序列相等 | `len(exact)==lenP(star)+1` 且 star 的 prefix 是 exact 的前缀 | exact 的段序列以 dstar prefix 开头（含相等：零段） |
| **star** | （对称） | 两个 star prefix 相等 | 见下 |
| **dstar** | （对称） | 见下 | 一个 prefix 是另一个的前缀（含相等） |

**star ∩ dstar**（S = star prefix，D = dstar prefix）：

- S 以 D 开头（含相等）→ 非空（`S.X` 落在 `D.**`）。
- D 以 S 开头且 `len(D)==len(S)+1` → 非空（交在 `D` 这个精确名）。
- D 以 S 开头且 `len(D)>len(S)+1` → 空。
- 否则空。

表驱动必须锁住（实现写成测试，频道列是订阅 key `p`，deny 列是一条 `deny_all` 规则）：

| p | deny | SubscribePattern | 说明 |
| --- | --- | --- | --- |
| `im.**` | `secret.**` | Allow | 不相交 |
| `**` | `secret.**` | `not_routable` | 先于 ACL；客户端不得订 `**` |
| `im.*` | `im.secret` | Deny | `im.secret ∈ L(im.*)` |
| `im.room.*` | `im.**` | Deny | `L(im.room.*) ⊂ L(im.**)` |
| `chat.**` | （无） | Allow | 投递类默认允许 |
| `*.room` | — | `not_routable` | A3 / KD-K13 |
| `im.**` | `im.secret` | Deny | `im.**` 盖住精确名 |
| `im.room.a.**` | `im.*` | Allow | `im.*` 只有两段，不相交 |
| `im.room.**` | `im.*` | Deny | 交在 `im.room` |
| `a.b.c` | `a.*` | Allow | `a.*` 只有两段 |
| `a.b` | `a.*` | Deny | |

`Publish` / `Recover` / `Presence` / `Survey` 的主语是**精确频道**（不是 pattern）。`Decide(..., ActionPublish, "im.room.*")` 仍按精确名处理（不会去订 Redis glob）；客户端发布通配本来就会在别处失败，本 PR 不必新开发布通配路径。

### 5.3 一条规则何时构成 deny / allow

对给定 `(principal, action)`：

- `deny_all: true` → 对该规则 `L(pattern)` 上的 **Subscribe / Publish / Survey / Recover / Presence** 都是 deny。
- `allow_<action>` **省略**（YAML 未写，Go `nil`）→ 该规则**不约束**该 action（可以只带 Effects）。
- `allow_<action>: []`（空列表）→ 对该 action 是 deny（「空 allow 列表」）。
- `allow_<action>` 含 `"*"` 或含 `principal.UserID` → 对该 action 是 **allow**。
- 否则（非空名单且不含该 principal）→ 对该 action 是 **deny**。

Admin 的 `UserID` 按 `"admin"` 匹配名单（与今日 `adminPrincipal` 一致），**另外**还受 §7 Capability 约束。

### 5.4 默认叙事与求值顺序

| Action | 默认 | Allow 条件 |
| --- | --- | --- |
| `SubscribePattern` | 允许 | `CompileInterest` 成功 **且** 不存在对该 principal 的 subscribe-deny 规则 d 使 `L(p)∩L(d)` 非空 |
| `Publish` | 允许 | 精确频道不被任何 publish-deny 命中（`topics.Match(d.pattern, ch)`） |
| `Survey` | 拒绝 | Effects.Survey==true **且** 存在 survey-allow 命中该精确频道 **且** 无 survey-deny 命中 |
| `Recover` | Effects.Recover | 精确频道；`Effects(ch).Recover==true` 且无 deny_all 命中。通配 channel → Deny（skip） |
| `Presence` | Effects.Presence | 精确频道；`Effects(ch).Presence==true` 且无 deny_all 命中。通配 → Deny |

**deny 不可被更具体的 allow 打洞。** 旧 ACL 的 last-write-wins **废除**。要开洞就缩小 deny 的 pattern。

求值注释（与设计文一致，实现按上表即可）：

`DenyAll / 显式 deny →（订阅当时）Proxy 输入 → 显式 allow → 默认`。

Proxy **不进入** AllowLang（TOCTOU）：静态 `Decide` 不算代理；订阅/发布当时再问一次代理，代理拒绝只否决这一次请求，代理允许也 **不能** 跳过静态 deny。

### 5.5 Effects

`Effects(ch)` = `DefaultChannelPolicy()` overlay `authorizer.default`，再按表顺序 overlay **每一条** `topics.Match(rule.pattern, ch)` 的规则 Effects（后写覆盖先写）。`TransientOnly` 仍强制 `History=false` 且 `Recover=false`。

这与旧 policy 的 first-match **不同**：通用规则在前、特殊规则在后即可。`channel_policy_test.go` 按此改，不要再断言 first-match。

通配订阅不把 Effects 拆到精确频道；Recover 对通配 skip（与今日 `recover.go` / A2 一致）。

## 6. 配置

```yaml
server:
  authorizer:
    default:
      history: true
      presence: true
      recover: true
      survey: false
      max_survey_subscribers: 256
      max_survey_timeout: "5s"
      presence_snapshot_limit: 256
    rules:
      - pattern: "secret.**"
        deny_all: true
      - pattern: "chat.public.*"
        allow_subscribe: ["*"]
        allow_publish: ["alice", "bob"]
      - pattern: "csurvey.**"
        allow_survey: ["*"]
        survey: true
      - pattern: "game.tick.**"
        transient_only: true
        presence: false
  grpc_admin:
    capabilities:              # 省略 = DefaultAdminCapabilities
      - history.read
      - presence.read
      - channels.list
      - session.act
      - subscribe.any
```

`config.Server`：

- 新增 `Authorizer AuthorizerConfig`。
- **删除** `ACL` / `Channels` 字段（或留下但 `Validate` 用 `mapstructure` 未识别之外的显式检查：YAML 仍写 `acl:` / `channels:` 则失败）。独立版本：测试与 example 全部改新键。
- `GRPCAdmin.Capabilities []string`：未知名 → Validate 错误；字段省略（nil）→ 运行时用 `DefaultAdminCapabilities`；显式 `[]` → 零位（锁死 Admin 数据面）。

`AuthorizerConfig` 空（零值）必须能 `NewAuthorizer`：无规则、默认 Effects = `DefaultChannelPolicy()`。未配置即「订阅/发布全开，Survey 关」。

## 7. Capability 与 Admin

| 位 | 行为 |
| --- | --- |
| `history.read` | `GetHistory` 前置。无位 → 软失败，**不**读 Stream |
| `presence.read` | `GetPresence` 前置 |
| `channels.list` | `GetChannels` 前置 |
| `session.act` | 按 session 投递 / 断开 / 订阅（现有 Admin RPCs） |
| `user.fanout` | 按 user 展开后再走 `session.act`（现有 fanout） |
| `subscribe.any` | Admin 代订：跳过「admin 用户 ID 必须出现在 allow_subscribe」；**仍**走 `CompileInterest`；**仍不得**把 ephemeral 写成 Occupancy（沿用 `shouldTrackPresence`） |
| `pattern.global` | 预留：将来节点内部 / Admin 持有裸 `**` Interest。本 PR **不**放行 `broker.Subscribe("**")`（A3 / KD-K13 硬约束仍在）。无此位时 `Decide(SubscribePattern, "**")` 仍 `not_routable`；有此位时 Admin 代订 `**` 也仍 `not_routable`，并在 Decision.Reason 里写 `not_routable`（位先入闭集，订 `**` 留给后续节点内部 Interest） |
| `survey.bypass_gate` | Admin `Node.Survey` 跳过人数门 / CanSurvey / 客户端 in-flight（今日 Admin Survey 已跳过；无此位则 Admin Survey 走与客户端相同的门） |
| `presence.large_snapshot` | 快照超过 `PresenceSnapshotLimit` 时仍返回全量；无此位则截断到上限（客户端路径保持今日截断） |

`GetHistory` 成功还要求 `Decide(admin, ActionRecover, ch).Allow`（Effects.Recover；transient 频道拒绝）。

`GetPresence` 成功还要求 `Decide(admin, ActionPresence, ch).Allow`。

Admin **无 Session Coverage**（KD 已裁定）。客户端 Presence / Survey 仍要 `sessionCoversChannel`（本 PR 不拆 Coverage）。

软失败码：缺位或 Decide 拒绝 → 顶层 / gRPC `PERMISSION_DENIED`（Admin 用现有 gRPC 错误路径即可，不要新造平行码表）。客户端信封：`ACL_DENIED` + `type=acl_error` 可继续用于静态拒绝；`PATTERN_NOT_ROUTABLE` 留给不可路由。

## 8. 接入点

### 8.1 客户端

`checkSubscribeACL`：

1. `CompileInterest(ch)` 失败 → `PATTERN_NOT_ROUTABLE` / `BAD_REQUEST`（与 A3 同一对码），`continue`，不断连。
2. `Decide(user, SubscribePattern, ch)` 拒绝 → `ACL_DENIED`。
3. 若 `FindProxy(ch, "subscribe")` 命中：问代理；代理错 → `ACL_ERROR`；代理拒 → 透传代理 Error。**代理允许不得跳过第 2 步**（第 2 步已在代理之前）。
4. 无代理：到此通过。

`handlePublish` 同样：先 `Decide(user, Publish, ch)`，再问 `PublishAcl`；代理不得放行静态 deny。

`handleSurvey`：用 `Decide(user, Survey, ch)` 替换 `CanSurvey` + `ChannelPolicy.Survey` 的组合（Decide 已含 Effects.Survey）。Coverage / 人数门 / in-flight 保留。

`handleConnect` 初始订阅走同一 `checkSubscribeACL`（已有循环）；不可路由 / ACL 都是单频道软失败。

### 8.2 Proxy

`Router` 仍可用 gobwas glob **选后端**（选谁服务）。**允许/拒绝**不得再「命中即整层短路」。

### 8.3 RPC

`handleRPC`：`errors.Is(err, proxy.ErrNoProxyFound)` → 顶层 Error `code=NO_PROXY` `type=request_error`，**不要** `RpcReply` echo。改 `TestClientSession_HandleMessage_RpcRequest_NoProxy`。

### 8.4 Node

- 删 `node.acl`。`node.authorizer` 永非 nil。
- `ChannelPolicy(ch)` → `authorizer.Effects(ch)`。
- `AdminCanSubscribe`：`CompileInterest` 过；有 `subscribe.any` 则过静态 allow 名单；无则 `Decide(admin, SubscribePattern, ch)`。`**` / `*` 一律失败（A3），即使持有 `pattern.global`。
- `AdminCanPublish`：`Decide(admin, Publish, ch)`。
- `ReplaceRules` 之后对每个本地 client 调 `PatternsToRevoke`，失败的 pattern **整条** `RemoveSubscription`（不按精确频道拆）。单测覆盖即可，不必做文件热加载。

### 8.5 `PatternsToRevoke`

对 `subscribed` 里每一项再跑 `Decide(p, SubscribePattern, key)`；`Allow==false` 的放进返回切片。用于规则热更新。

## 9. 必须存在的测试

1. **语言包含表**：§5.2 全行（含 `im.room.a.**` vs `im.*` Allow、`im.room.**` vs `im.*` Deny）。
2. **deny 不打洞**：`secret.**` DenyAll + `secret.lobby` allow `alice` → `alice` 订 `secret.lobby` 仍 Deny；订 `im.**` 仍 Allow。
3. **省略 vs 空名单**：规则只有 Effects（省略 `allow_subscribe`）不拒绝订阅；`allow_subscribe: []` 拒绝订阅。
4. **Survey 默认拒绝**：无 `allow_survey` → Deny；`allow_survey: ["*"]` 且 Effects.survey=true → Allow；`deny_all` 压过 allow。
5. **Publish 不要求 Coverage**：未订阅也可 `Decide(Publish)` Allow（KD-K21）；与今日未订阅可发一致。
6. **不可路由**：`Decide(SubscribePattern, "*.room")` Reason=`not_routable`；客户端仍不断连。
7. **Proxy 不短路**：内置 `secret.**` DenyAll + 对 `secret.1` 返回允许的假代理 → 客户端仍 `ACL_DENIED`。
8. **NO_PROXY**：无代理 RPC → Error 信封，不是 echo `RpcReply`。
9. **Capability**：无 `history.read` 时 `GetHistory` 失败且 broker History **零调用**（spy broker）；省略 capabilities 时 `GetHistory` 仍可用（默认位含 `history.read`）；显式 `[]` 时不可用。
10. **Effects overlay**：`game.**` history=true 在前，`game.tick.**` transient_only 在后 → `game.tick.1` 为 transient 且 Recover=false。
11. **ReplaceRules**：先订 `chat.**`，换成 `chat.**` DenyAll，`PatternsToRevoke` 含 `chat.**`；调用后 hub 无该订阅。
12. **配置**：YAML 仍写 `server.acl` 或 `server.channels` → `config.Validate` 错误；规则 `a.**.b` → 错误；未知 capability 名 → 错误。
13. `go test ./...`；`go test -race . ./config ./pkg/grpcstream`。

禁止固定长 Sleep 代替同步点。

旧测试契约必须改写，不要用兼容层让 last-write-wins / 中段 `**` / first-match policy / RPC echo 继续绿。

## 10. 验收清单

1. 仓库无 `NewACLEngine` / `ACLEngine.CanSubscribe` 热路径；客户端订阅/发布/Survey 只问 `Authorizer.Decide`。
2. §5.2 表 + deny 不打洞 全绿。
3. `server.acl` / `server.channels` 不再被读取；Validate 拒绝旧键。
4. Proxy 允许不能越过静态 deny。
5. 无代理 RPC = `NO_PROXY`，不 echo。
6. Admin 数据面缺位软失败，默认位保持现有集成测试能读 History/Presence。
7. 未改 A1/A2/A3 热路径。
8. 测试命令绿。

## 11. 完成报告

- 文件列表
- §10 逐条证据
- 测试命令与结果
- 偏离（应无）

## 12. 实现备注（完成后填写）

（实现者填写）
