# PR-10 实现规格：文档对齐 + 集群 e2e

| 字段 | 值 |
| --- | --- |
| 标题 | `docs: v1.0 protocol, architecture, admin, cluster, and observability` |
| 状态 | **Accepted**（2026-08-15 主 agent 终验通过） |
| 依赖 | **PR-01–PR-09 已合**。本 PR 只对齐文档，并补齐设计要求的集群 e2e |
| 设计来源 | [v1.0-platform-gaps.md](../v1.0-platform-gaps.md) PR Plan PR-10；缺口验收总表「文档与实现对齐」 |
| 验收人 | 主 agent |

## 1. 目标

v1.0 文档不再描述已废弃的行为。集群 e2e 覆盖设计点名的四条路径（已有测试能证明的条目写进完成备注即可，不要为了凑数再写一份同样的）。

1. 文档与当前实现一致：一等 Presence、客户端 Survey、`CanSurvey`、频道策略、按 user Admin、服务端 ping、SDK 能力。
2. 禁止再出现「Subscribe 写了 recover 但代码不读」「Presence 只靠 `__presence` 伴生频道」「Survey 只是 echo」这类过时句子。
3. 集群 Redis 集成测（无 Redis 则 Skip）覆盖：按 user 断开、通配 presence 跨节点、Subscribe recover、客户端异步 Survey。

本 PR **不**实现：新协议字段、改 SDK API、改默认配置、修已知 flake `TestClusterRedis_SurveyAggregatesAcrossNodes`（除非一行超时就能稳住）、新 example。

## 2. 允许改动的文件

- `README.md`（仓库根）
- `docs/protocol.md`（只改仍过时的句子；协议字段已在 PR-01/07 对齐过）
- `docs/developer/01-architecture.md`
- `docs/developer/02-configuration.md`（只改残留过时句）
- `docs/developer/03-admin-api.md` / `04-cluster.md`（只改残留过时句）
- `docs/developer/05-observability.md`（补 v1.0 指标）
- `docs/developer/README.md`（目录描述若落后）
- `config-example.yaml`（注释若仍写旧行为）
- `cluster_redis_integration_test.go` 和/或新 `cluster_v1_e2e_test.go`（同包 `messageloop`）
- 必要时 `presence_test.go` / `survey_test.go` **只加**缺失的集群/集成用例，不要改既有断言
- `docs/design/tasks/pr-10-docs.md`（完成备注）

禁止：改 proto、`shared/`、`sdks/**` 业务、`client.go` / `node.go` / `acl.go` 行为、默认 idle/ping、git 写操作。

## 3. 现状（动手前再读，按源码改文档）

**已经对齐、不要重写**：`02-configuration.md` 的策略/ACL/`allow_survey`/`cluster_emit`；`03-admin-api.md` 的 `users`/`user_id`；`04-cluster.md` 的 user 索引与 `cluster_emit`；`07-sdk-go.md` / `08-sdk-ts.md`；`protocol.md` 的 Client Survey / Ping 双向。

**仍过时（必须改）**：

| 位置 | 过时说法 | 改为 |
| --- | --- | --- |
| 根 `README.md` Presence And History | join/leave 发到 `/<channel>/__presence` 伴生频道 | 默认一等 `presence_event`；`legacy_presence_channel` 才写伴生；跨节点要 `server.presence.cluster_emit` |
| `01-architecture.md` §3.5 | join/leave 经 `ch + "/__presence"` + `PublishPresenceJoin` JSON | `emitPresence` → 本节点 `deliverPresenceEvent` 或（`cluster_emit`）`PublishTransient` + `ml.type=presence`；broadcast **改写**为一等信封，不当 `publication`；通配订阅者收精确频道事件；ephemeral / 通配不进 store |
| `01-architecture.md` 图 (e) | 伴生频道扇出 | 同上 |
| `01-architecture.md` §3.6 / 图 (d) | 只写 Admin `Node.Survey` | 补客户端 `handleSurvey`：同步校验 + worker、不 Wait、默认关、`CanSurvey` deny、`count_only` 预检；Admin **不加**订阅者上限 |
| `01-architecture.md` §3.7 | ACL 用 `path.Match`；只有 Subscribe/Publish | 分段 glob `matchChannelPattern`；补 `AllowSurvey` / `CanSurvey` 默认 deny |
| `05-observability.md` §3 | 缺 v1.0 指标 | 补全（见 §4） |
| `docs/developer/README.md` | 08-sdk-ts 描述未提 Survey/Presence | 一句补上 |

读源码再写，不要臆造行号：`node.go` `emitPresence`、`hub.go` broadcast presence 分支、`acl.go` `CanSurvey`、`metrics.go`、`client.go` `handleSurvey`。

## 4. 可观测性必须补的指标

`metrics.go` 已有、`05-observability.md` 未列的，全部补一行（类型/标签/含义，风格与现表一致）：

- `messageloop_channel_policy_transient_forced_total`
- `messageloop_recovery_total{status}`
- `messageloop_recovery_publications`（histogram）
- `messageloop_recovery_truncated_total`
- `messageloop_heartbeat_idle_disconnects_total`
- `messageloop_admin_user_fanout{op}`
- `messageloop_survey_client_total{result}`
- `messageloop_presence_publish_failures_total` / `messageloop_presence_failures_total`（若文档完全没写）

标签名以源码 `prometheus.CounterOpts` / `HistogramOpts` 为准。

## 5. 集群 e2e

全部 `requireClusterRedis`（或现有 helper）——无 Redis **Skip**，不要 Fail。超时用 `Eventually`，独立测试，**禁止**合成一个 30s 大杂烩。

| 场景 | 已有则可引用 | 没有则新增 |
| --- | --- | --- |
| 按 user 跨节点断开 | `TestAdmin_DisconnectUsersAcrossNodes` | — |
| 通配 × presence 跨节点 | 本机 `TestPresence_WildcardSubscriberReceivesExactJoin` **不够**（单节点）。`TestPresence_ClusterEmitRedisExactlyOne` 是精确频道。**必须新增**：nodeA 订 `im.**`，nodeB 订/加入 `im.room.1`，`cluster_emit=true`，A 收到 `PresenceEvent{channel=im.room.1}`，且 **零** `publication` |
| Subscribe recover | 单测很多。**新增一条 Redis broker**：发布 ≥2 条 → 新连接 `Subscribe{recover=true, offset=第一条, epoch}` → `SubscribeAck.publications` 含后续消息，`recovered=true` |
| 客户端异步 Survey | `TestClusterRedis_SurveyAggregatesAcrossNodes` 是 **Admin** `Node.Survey`，不能顶客户端路径。**必须新增**：两节点各一订阅者，策略+ACL 打开，A `HandleMessage(SurveyRequest)`，B 按 outbound `request_id` `SurveyReply`，A 异步收到 `SurveyResult`（含 B）。禁止 inbound echo loopback |

新增测试命名建议：

- `TestPresence_ClusterEmitWildcardAcrossNodes`
- `TestSubscribe_RecoverRedisHistory`
- `TestClientSurvey_AggregatesAcrossRedisNodes`

复用 `requireClusterRedis`、`newClusterRedisTestNode`、`integrationCapturingTransport`、`respondToSurvey`（已按 outbound request_id Reply）。

不要改 `TestClusterRedis_SurveyAggregatesAcrossNodes` 的超时策略去「修 flake」，除非完成备注里写明只加长 wait。

## 6. 必须存在的验证

文档：用搜索自检，下列字符串不得再作为**现行行为**出现（历史设计文档 `docs/design/**`、`docs/review/**`、已 Accepted 规格的「现状」节除外）：

- 「join/leave 发到 `__presence`」且未同时写明「仅 `legacy_presence_channel`」
- 「`handleSurvey` 回显 / echo payload」
- 「ACL 使用 `path.Match`」
- 「`channels.policies[].survey` 本版本尚未读取」

测试：

```bash
go test -count=1 -timeout 180s -run "TestAdmin_DisconnectUsersAcrossNodes|TestPresence_ClusterEmit|TestSubscribe_RecoverRedisHistory|TestClientSurvey_AggregatesAcrossRedisNodes" .
cd sdks/ts && npm test
cd sdks/go && go test -count=1 .
```

无 Redis 时集群测 Skip 算过。根包全量 `go test -count=1 .` 应仍绿（允许已知 Survey Admin 集成 flake 单独注明）。

## 7. 验收清单

1. README / 架构 Presence 不再把伴生频道写成默认路径。
2. 架构 Survey 写清客户端异步 + Admin 无订阅者上限。
3. 架构 ACL 写 `matchChannelPattern` + `CanSurvey` 默认 deny。
4. `05-observability.md` 列出 §4 全部指标。
5. 通配 presence 跨节点 e2e 存在且零 publication。
6. Redis Subscribe recover e2e 存在。
7. 客户端 Survey 跨节点 e2e 存在（不是 Admin echo）。
8. 无 proto / SDK / 服务端行为变更；`go test` 摘要见完成报告。

## 8. 完成报告

- 文件列表
- 每处过时句子：旧 → 新（文件:节）
- §5 四条：已有测试名 / 新增测试名 / Skip
- §7 八条 + 证据
- `go test` / `npm test` 摘要
- 偏离与理由

## 9. 实现备注（落地后填写）

**状态：已实现（2026-08-15）**。改动文件：

- `README.md`（Presence And History）
- `docs/developer/01-architecture.md`（§3.5 / §3.6 / §3.7 + 图 (d)(e)）
- `docs/developer/02-configuration.md`（`channel_pattern` 一行残留过时句）
- `docs/developer/05-observability.md`（§3 新增 §3.5 指标表；§3 引言标签说明；§5 断连码指标说明；§6.1 心跳告警行）
- `docs/developer/README.md`（08-sdk-ts 描述一行）
- `cluster_redis_integration_test.go`（`newClusterRedisTestNode` 拆出可配置变体 `newClusterRedisTestNodeWithConfig`，原有 4 处调用签名不变）
- `cluster_v1_e2e_test.go`（新增，同包 `messageloop_test`）
- `docs/design/tasks/pr-10-docs.md`（本节备注）

**§5 四条**：

| 场景 | 测试 | 状态 |
| --- | --- | --- |
| 按 user 跨节点断开 | `TestAdmin_DisconnectUsersAcrossNodes` | 已有（未动） |
| 通配 × presence 跨节点（cluster_emit=true，零 publication） | `TestPresence_ClusterEmitWildcardAcrossNodes` | 新增 |
| Subscribe recover（Redis broker） | `TestSubscribe_RecoverRedisHistory` | 新增 |
| 客户端异步 Survey 跨节点（outbound request_id 应答，非 echo） | `TestClientSurvey_AggregatesAcrossRedisNodes` | 新增 |

全部 `requireClusterRedis`，无 Redis 时 Skip。未改 `TestClusterRedis_SurveyAggregatesAcrossNodes`。

**验证**（本机 Redis 可用，全部真跑未 Skip）：

- `go test -count=1 -timeout 180s -run "TestAdmin_DisconnectUsersAcrossNodes|TestPresence_ClusterEmit|TestSubscribe_RecoverRedisHistory|TestClientSurvey_AggregatesAcrossRedisNodes" .` → ok
- `go test -count=1 .` → ok（56.4s，含已知 Admin Survey 集成测试，本次全过）
- `cd sdks/go && go test -count=1 ./...` → ok
- `cd sdks/ts && npm test` → 5 suites / 77 tests passed

**客户端 Survey e2e 的时序事实**（与 Admin 集成测一致）：`Node.Survey` 先跑完本节点 `localSurvey`（超时窗口内），再发 `ClusterCommandSurvey` 广播，因此对端订阅者的 outbound `SurveyRequest` 只在本节点 local survey 完成后才到——发起方必须先答自己的请求，再等对端请求并应答；两端 localSurvey 各自用自己生成的 request_id（不同），应答路由到各自节点。结果 user_id 元数据只给结果构建节点本地已知的会话。

**偏离与理由**：`newClusterRedisTestNode` 硬编码 `RequireAuth: true`，客户端 Survey e2e 需要开策略+ACL，故拆出 `newClusterRedisTestNodeWithConfig`（原签名与行为不变）。无其他偏离。
