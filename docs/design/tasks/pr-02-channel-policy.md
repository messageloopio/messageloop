# PR-02 实现规格：频道前缀策略引擎

| 字段 | 值 |
| --- | --- |
| 标题 | `server: add ChannelPolicyEngine and per-prefix history/presence/recover/survey switches` |
| 状态 | 待实现 |
| 依赖 | **无**。可与 PR-01 并行。不要等 proto。 |
| 设计来源 | [v1.0-platform-gaps.md](../v1.0-platform-gaps.md) 缺口 7、KD-6/KD-8 |
| 验收人 | 主 agent |

## 1. 目标

让一台集群能按频道前缀区分行为：IM 要历史，游戏 tick 强制瞬时，IoT 可关 presence。

本 PR 交付：

1. YAML `server.channels` + `Validate()`
2. 根包 `ChannelPolicyEngine`（first-match glob，复用 `matchChannelPattern`）
3. `Node` 装配；`Node.ChannelPolicy(ch)` 供后续 PR 读取
4. **发布路径兑现** `transient_only` / `history=false` / `history_size` / Redis `history_ttl`
5. 单测 + 配置文档

本 PR **不**实现：Subscribe 恢复、presence 信封、客户端 Survey、服务端 ping、按 user、ACL `allow_survey`（那是 PR-07）。`recover` / `presence` / `survey` 开关只进策略对象，本 PR 不在 subscribe/survey 路径读取它们。

## 2. 允许改动的文件

- `config/config.go`、`config/config_test.go`
- `channel_policy.go`、`channel_policy_test.go`（新，根包 `messageloop`）
- `node.go`（`NewNode` 建引擎；`Publish` 注入 HistorySize/TTL 并在策略禁历史时拒绝）
- `client.go` **仅** `handlePublish`：策略要求瞬时则走 `PublishTransient`
- `pkg/grpcstream/api_handler.go` **仅** Admin 频道发布：`add_history=true` 但策略禁历史 → 计失败，不调用 `Publish`
- `publication.go` / `broker.go`：`Publication` 增加 `HistorySize int`、`HistoryTTL time.Duration`（零值 = 用 broker 全局）
- `broker_memory.go`、`broker_memory_test.go`：每频道 ring 在**首次 Publish** 按 `pub.HistorySize` 分配
- `pkg/redisbroker/redis.go`、必要时 `history_test.go` / `publish_transient_test.go`：`XAdd.MaxLen` 与 `Expire` 可用 per-pub 覆盖
- `metrics.go` / `metrics_test.go`：`channel_policy_transient_forced_total`（客户端因策略被改成 transient 时 +1）
- `config-example.yaml`、`docs/developer/02-configuration.md`
- 测试里所有实现了 `Broker` 的 fake：给新字段零值即可，**不要**为假对象加业务

禁止：改 proto、改 `handleSubscribe`/`handleConnect`/`handleSurvey`、改 ACL 求值、改 presence 伴生频道、git 写操作。

## 3. 配置契约

### 3.1 YAML

```yaml
server:
  channels:
    default:
      history: true
      history_size: 0          # 0 = broker 全局（memory 256 / redis stream_max_length）
      history_ttl: ""          # 空 = broker 全局；memory 忽略并 Warn
      presence: true
      recover: true
      survey: false            # KD-6：客户端 survey 默认关
      recover_limit: 0         # 0 = MaxRecoveredPublications
      max_survey_subscribers: 256
      max_survey_timeout: 5s
      legacy_presence_channel: false
      presence_snapshot_limit: 256
    policies:
      - pattern: "game.tick.**"   # 更具体的必须写在前面
        history: false
        presence: false
        recover: false
        survey: false
        transient_only: true
      - pattern: "im.**"
        history: true
        history_size: 5000
        presence: true
        recover: true
        survey: false
```

未写 `server.channels` 时，引擎仍存在，解析结果 = 上表 `default`（兼容现网：history 开、presence 开、survey 关）。

**不要**在本 PR 增加 `server.presence.cluster_emit`（留给 PR-04）。

### 3.2 Go 配置结构（`config` 包）

指针字段用于「策略 overlay」：nil = 不覆盖 default。

```go
type Server struct {
    // 现有字段...
    Channels ChannelConfig `yaml:"channels" json:"channels" mapstructure:"channels"`
}

type ChannelConfig struct {
    Default  ChannelPolicySpec   `yaml:"default" json:"default" mapstructure:"default"`
    Policies []ChannelPolicyRule `yaml:"policies" json:"policies" mapstructure:"policies"`
}

type ChannelPolicyRule struct {
    Pattern string `yaml:"pattern" json:"pattern" mapstructure:"pattern"`
    ChannelPolicySpec `yaml:",inline" mapstructure:",squash"`
}

type ChannelPolicySpec struct {
    History                *bool  `yaml:"history" json:"history" mapstructure:"history"`
    HistorySize            *int   `yaml:"history_size" json:"history_size" mapstructure:"history_size"`
    HistoryTTL             string `yaml:"history_ttl" json:"history_ttl" mapstructure:"history_ttl"`
    Presence               *bool  `yaml:"presence" json:"presence" mapstructure:"presence"`
    Recover                *bool  `yaml:"recover" json:"recover" mapstructure:"recover"`
    Survey                 *bool  `yaml:"survey" json:"survey" mapstructure:"survey"`
    TransientOnly          *bool  `yaml:"transient_only" json:"transient_only" mapstructure:"transient_only"`
    RecoverLimit           *int   `yaml:"recover_limit" json:"recover_limit" mapstructure:"recover_limit"`
    MaxSurveySubscribers   *int   `yaml:"max_survey_subscribers" json:"max_survey_subscribers" mapstructure:"max_survey_subscribers"`
    MaxSurveyTimeout       string `yaml:"max_survey_timeout" json:"max_survey_timeout" mapstructure:"max_survey_timeout"`
    LegacyPresenceChannel  *bool  `yaml:"legacy_presence_channel" json:"legacy_presence_channel" mapstructure:"legacy_presence_channel"`
    PresenceSnapshotLimit  *int   `yaml:"presence_snapshot_limit" json:"presence_snapshot_limit" mapstructure:"presence_snapshot_limit"`
}
```

`HistoryTTL` / `MaxSurveyTimeout` 用 string 以区分「未设置」与 `"0s"`。空字符串 = 不覆盖。

### 3.3 `Validate()` 增补

对 `default` 与每条 policy：

- `policies[i].pattern` 非空
- `history_size` 若设置则 `>= 0`
- `history_ttl` / `max_survey_timeout` 若非空则 `time.ParseDuration` 成功
- pattern 建议调用 `topics.ValidateTopic`；非法 pattern 启动失败（`a.`、中间 `**` 与 matcher 一致）。ACL 仍允许中间 `**`；**策略 pattern 与 matcher 对齐：只允许末尾 `**`**。若你选择策略也允许中间 `**`（与 ACL 同一 `matchChannelPattern`），必须在报告里写明，并补测试 `a.**.b`。

推荐：**策略 pattern 用 `topics.ValidateTopic`**（末尾 `**` 合法，`a.**.b` 非法）。匹配实现仍用根包 `matchChannelPattern`（它能匹配合法的末尾 `**`）。

## 4. ChannelPolicyEngine

新文件 `channel_policy.go`，包 `messageloop`。

```go
type ChannelPolicy struct {
    History                 bool
    HistorySize             int           // 0 = broker 全局
    HistoryTTL              time.Duration // 0 = broker 全局
    Presence                bool
    Recover                 bool
    Survey                  bool
    TransientOnly           bool
    RecoverLimit            int
    MaxSurveySubscribers    int
    MaxSurveyTimeout        time.Duration
    LegacyPresenceChannel   bool
    PresenceSnapshotLimit   int
}

func DefaultChannelPolicy() ChannelPolicy {
    return ChannelPolicy{
        History: true, Presence: true, Recover: true, Survey: false,
        MaxSurveySubscribers: 256, MaxSurveyTimeout: 5 * time.Second,
        PresenceSnapshotLimit: 256,
    }
}

// NewChannelPolicyEngine compiles cfg. Server.Channels may be zero.
func NewChannelPolicyEngine(cfg config.ChannelConfig) (*ChannelPolicyEngine, error)

// For returns the first matching policy overlaid on default.
// No match → compiled default only.
func (e *ChannelPolicyEngine) For(channel string) ChannelPolicy
```

**匹配**：按 `policies` 数组顺序，第一条 `matchChannelPattern(pattern, channel)` 命中即停（与 proxy Router 相同，**与 ACL last-write-wins 相反**）。文档必须写清。

**Overlay**：`base = compile(DefaultChannelPolicy(), cfg.Default)`，再 `overlay(base, firstMatch)`。nil 指针不改 base。

**隐含**：`TransientOnly==true` 时，`For` 的返回值必须 `History=false` 且 `Recover=false`（即使 YAML 漏写），避免后续 PR 把 tick 频道当成可恢复。

`Node`：

```go
func (n *Node) ChannelPolicy(ch string) ChannelPolicy
```

`NewNode`：`cfg==nil` 或 Channels 全零 → `NewChannelPolicyEngine(config.ChannelConfig{})`。解析失败使 `NewNode` 怎样处理？**不要改 `NewNode` 签名**（现为 `NewNode(cfg *config.Server) *Node`）。非法 duration 在 `Validate()` 已拦；引擎里 duration 解析失败回退 0 并打 Warn。单测覆盖。

## 5. 发布路径（本 PR 必须兑现）

### 5.1 `Publication` 新字段

```go
type Publication struct {
    // 现有字段...
    HistorySize int           // 0 = broker 默认 cap
    HistoryTTL  time.Duration // 0 = broker 默认 TTL；memory 忽略
}
```

不要加 `SkipHistory`。不要改 `Broker` 接口方法集。

### 5.2 `Node.Publish`

在调 `n.broker.Publish` 之前：

1. `pol := n.ChannelPolicy(ch)`
2. 若 `pol.TransientOnly || !pol.History` → 返回 `(0, ErrHistoryDisabled)`（新 sentinel，`errors.New` 即可，根包导出）
3. 若 `pub.HistorySize == 0` 且 `pol.HistorySize > 0` → `pub.HistorySize = pol.HistorySize`
4. 若 `pub.HistoryTTL == 0` 且 `pol.HistoryTTL > 0` → `pub.HistoryTTL = pol.HistoryTTL`

`Node.PublishTransient` 不看 history 策略（瞬时本来就不写历史）。

### 5.3 `handlePublish`（`client.go`）

在 ACL 通过、组好 `pub` 之后：

```
pol := c.node.ChannelPolicy(channel)
forceTransient := publish.Transient || pol.TransientOnly || !pol.History
if forceTransient {
    if !publish.Transient && c.node.metrics != nil {
        c.node.metrics.ChannelPolicyTransientForced.Inc()
    }
    PublishTransient ... offset 0 ack
    return
}
Node.Publish ...
```

客户端 **不要** 因策略返回错误；tick 频道无 `transient` 标志也必须成功，ack offset=0。

### 5.4 Admin `api_handler.go` 频道分支

现逻辑：`AddHistory` → `Publish`，否则 `PublishTransient`。

改为：

- `pol.TransientOnly || !pol.History`：
  - 若 `AddHistory==true` → Warn「admin add_history denied by channel policy」、`failed++`、**不发布**
  - 否则 `PublishTransient`
- 否则保持：`AddHistory` → `Publish`，否则 transient

不要把 `ErrHistoryDisabled` 变成整个 RPC 失败（现有部分成功语义：`api_handler.go:28-30`）。

### 5.5 内存 broker

`channelHistory` 增加自己的 `size int`。`Publish` 首次创建：

```
cap := b.historySize
if pub.HistorySize > 0 {
    cap = pub.HistorySize
}
h = &channelHistory{entries: make([]*Publication, cap), size: cap}
```

之后该频道的 modulo / 满员覆盖一律用 **`h.size`**，禁止再读 `b.historySize`。

- **已存在的 ring 不因策略改大/改小而重建**（设计原文）。测试：先用默认 256 发一条，再带 `HistorySize=5` 发，ring 仍是 256。
- 频道被 Unsubscribe 回收且 ring 空后，下次 Publish 用新 size。
- `pub.HistoryTTL != 0`：`log.Warn` 一次即可（可 per-channel sync.Once），不实现 TTL。

现有 `TestMemoryBroker_*` 必须继续绿。新增：`HistorySize=3` 发 5 条，`History` 只剩最后 3。

### 5.6 Redis broker

`Publish` 的 `XAdd`：

```
maxLen := b.opts.StreamMaxLength
if pub.HistorySize > 0 {
    maxLen = int64(pub.HistorySize)
}
ttl := b.opts.HistoryTTL
if pub.HistoryTTL > 0 {
    ttl = pub.HistoryTTL
}
```

`PublishTransient` 不变。无 Redis 时现有测试 skip，保持。

## 6. 指标

`metrics.go` 增加：

```
messageloop_channel_policy_transient_forced_total
```

Counter，无 label。仅 `handlePublish` 在「客户端未声明 transient、但策略迫使 transient」时 Inc。Admin 路径不加。

`NewMetrics` 必须注册。`metrics_test.go` 若枚举指标数量，更新计数。

## 7. 文档

`docs/developer/02-configuration.md` 增加 `server.channels` 表：

- first-match vs ACL last-write-wins，附设计里的 `game.tick.**` / `game.**` 例子
- `transient_only` 对客户端（改瞬时）与 Admin `add_history`（拒绝）的差异
- memory 无 TTL；改小 `history_size` 对已有频道不立即生效
- IM 大 `history_size` 应使用 Redis

`config-example.yaml` 加一段注释掉或生效的 `server.channels` 示例（至少 `game.tick.**` + `im.**`）。

## 8. 必须存在的测试

写在 `channel_policy_test.go` 与现有包测试中。

| 测试 | 断言 |
| --- | --- |
| `TestChannelPolicy_DefaultWhenEmpty` | 无 YAML → History/Presence/Recover true，Survey false |
| `TestChannelPolicy_FirstMatchWins` | `game.tick.fps` 命中 `game.tick.**` 不是后面的 `game.**` |
| `TestChannelPolicy_OverlayNilKeepsDefault` | policy 只设 `history_size: 5` → History 仍为 default true |
| `TestChannelPolicy_TransientOnlyImpliesNoHistory` | `transient_only: true` → History=false Recover=false |
| `TestChannelPolicy_ValidateEmptyPattern` | `Validate()` 报错 |
| `TestHandlePublish_PolicyForcesTransient` | tick 频道不带 transient，ack offset=0，History 为空，订阅者仍收到（可用 memory node） |
| `TestNodePublish_HistoryDisabled` | 直接 `Node.Publish` 返回 `ErrHistoryDisabled` |
| `TestMemoryBroker_PerChannelHistorySize` | `HistorySize=3` 发 5 条只留 3 |
| `TestMemoryBroker_ExistingRingKeepsCap` | 先默认发 1 条，再 HistorySize=2 发多条，cap 仍是默认 |
| `TestValidate_ChannelHistoryTTL` | 非法 duration 失败 |

Admin：`TestAPIServiceHandler_AddHistoryDeniedByPolicy`（可放 `pkg/grpcstream/api_handler_test.go`）：策略禁历史 + `add_history` → 不调用能写历史的 broker（用可探测的 fake 或查 History 为空）。

## 9. 验收清单

1. 无 `server.channels` 时发布/订阅与改前一致（survey 本就不能真发起，默认 false 无观察差异）。
2. `game.tick.**` + 客户端普通 Publish → offset 0、无历史、实时可达。
3. `im.**` `history_size=5` + 发 10 条 → History 长度 5（新频道）。
4. `Node.Publish` 在禁历史频道返回 `ErrHistoryDisabled`。
5. Admin `add_history` 在禁历史频道不写入。
6. 策略 first-match 单测绿。
7. `go test ./config/... . ./pkg/grpcstream/... ./pkg/redisbroker/...` 与 `go test -race .` 绿。
8. `git diff` 无 proto 变更（除非你误改了——不允许）。

## 10. 完成报告

- 文件列表
- Overlay / first-match 的实现位置
- `TransientOnly` 是否强制 History/Recover=false
- 上表 8 条自检
- 偏离与理由
