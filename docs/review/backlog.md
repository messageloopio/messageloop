# 评审遗留项 Backlog

> 来源：2026-08 全项目评审（`docs/review/summary.md`）→ 修复（`docs/review/fix-plan.md` + `docs/review/fix/`）→ 两轮 followup 验收。
> 本文档记录评审周期结束后用户拍板的遗留项决策，作为下一轮实现的输入。

## 一、已拍板的新工作（4 项）

### B1. 精确频道全入口拒绝空分段（原 C1）

**决策**：与通配订阅统一——精确频道（如 `"a."`、`"..b"`）也一律走 `validTopic` 校验并拒绝。

- 现状：`validTopic` 未导出（`pkg/topics/matcher.go:74-83`），通配订阅入口已校验，精确订阅/发布入口未覆盖。
- 实现要点：
  - 需要全入口校验，需决定导出 `validTopic` 或在 hub/broker 层复用语义（倾向导出 `ValidateTopic`，供 hub 订阅入口、publish 入口、redis broker 共用）。
  - 错误处理链路要完备：畸形输入必须返回显式错误，不得静默失败。

### B2. Matcher 支持后缀式 `**` 多段通配（原 C2）

**决策**：采用"设计最优"方案——matcher 支持 MQTT 风格后缀 `**`（仅允许位于模式末尾），ACL 侧不动（已支持 `**`）。

- 各实现分工：
  - CSTrie：Lookup 每层多检查一次 `**` 分支即可，无需回溯。
  - naive / trie：简单遍历匹配。
  - inverted_bitmap：按前缀处理（`**` 之前的字面前缀做索引，命中后再按段校验）。
- `validTopic`/`ValidateTopic` 需同步允许末尾 `**`（与 B1 共用校验函数，必须同一工作流实施）。
- 需要确认 ACL `*`（单段）/ `**`（多段）语义与 matcher 全对齐，补 acl_test 锁定。

### B3. 双 SDK 协议对齐补齐（原 C3）

**决策**：全部补齐。

- TS SDK：补 survey 应答（respond）、SubRefresh 支持。
- Go SDK + TS SDK：补订阅级 token 透传（Subscribe 携带 token）、PublishAck 消费（发布方等待 ack）。

### B4. 跨节点精确续读：填充 `ChannelOffsets`（原 C4）

**决策**：排期实现。

- 现状：`cluster_state.go:67-73` 已预留 `ChannelOffsets map[string]uint64` 字段并注明 future work——hub 广播路径不记录每频道最后投递 offset，跨节点 resume 只能回退到客户端自报 offset。`BrokerEpoch`（`cluster_state.go:74-77`）已填充。
- 实现要点：
  - hub 广播路径增加每会话每频道投递簿记（数据源待确认：`client.go` 恢复逻辑用 `sub.Offset+1`，但 hub/subscriber 是否持续记录最后投递 offset 未确认，实现前先查）。
  - 快照填充 + resume 侧消费（`cluster_resume.go`）+ 回滚/清理对称。
  - 与 B1 同触 hub 订阅路径，需合并工作流或严格串行。

## 二、A 类：无争议小改动（5 项）

### A1. payload→Publication 重复转换抽取

- 位置：`pkg/grpcstream/api_handler.go:35-59,283-297` 与 `client.go`（约 1070-1090、1370、1420 三处 oneof 分支）。
- 方案：根包抽共享函数（如 `PublicationFromPayload`），两处调用方收敛。

### A2. Go SDK 消费 `disconnect_code`

- 服务端已在错误信封 metadata 中编码数值断连码（`pkg/grpcstream/transport.go:145-168`，注释说明 gRPC 流无 close frame，故走带内信封）。
- Go SDK 侧目前 Recv 直通不解析（`sdks/go/grpc.go:104-114`）：需解析错误信封 `metadata.disconnect_code`，暴露为带数值码的 typed Disconnect，与 WS 路径对齐。

### A3. metrics 增加 `transport` label

- 现状：`metrics.go` 全部指标无 label（无 `node_id`，亦无 ws/grpc 区分）。
- 方案：`connections_total`（必要时 `messages_delivered_total`）加 `transport`（ws/grpc）label。

### A4. HTTP proxy 非 200 错误体改用 protojson

- 位置：`proxy/http.go:413-420`，目前用 encoding/json 解析 `sharedpb.Error`，改用 protojson 与 proto 契约对齐。

### A5. 排查两处既有时序 flaky 测试

- 两轮全量测试（含 `-race -count=1`）未复现；03 工作流曾记录 1 次既有时序 flake，判定与修复无关。
- 处置：单独排查，不阻塞其他项。

## 三、B 类：已定保持现状（不再讨论，设计即如此）

1. Go SDK `Build*` 构造器不支持 ephemeral presence（SDK 层简化，服务端协议已支持）。
2. TS SDK 双 handler API 并存（`add` 系列为推荐，`onXxx` 为别名，已在 README/类型注释明确）。
3. Go `OnMessage`/`ReceivedMessage` 与 TS 语义差异（各 SDK 按本语言惯例设计，无协议层歧义）。

## 四、实施约束（给下一轮工作流切分用）

- **B1 + B2 必须同一工作流**：同改 `pkg/topics/matcher.go` 校验函数与各 matcher 实现。
- **B4 与 B1/B2 有 hub 文件交叉**（hub 订阅入口）：合并进同一工作流或严格串行，不得并行。
- **B3 拆两个工作流**：TS SDK 一个、Go SDK 一个（PublishAck/token 透传两侧协议对称，需对齐语义）。
- **A1-A4 按文件归属分发**：A1/A3/A4 属根包+proxy，A2 属 Go SDK（可与 B3-Go 合并）。
- 纪律：测试先行（回归测试须对旧代码会红）；改动最小化；文档同步（protocol.md / AGENTS.md / docs/developer/）。

## 五、实施结果与验收结论（2026-08，已放行）

B1-B4、A1-A5 全部由任务书（`docs/review/tasks/`）分派实施完成并通过严格验收（代码级逐条核实 + 旧代码红验证 + 整仓终验全绿）。裁定记录：

- 可接受偏差：B4 簿记为"每 publication 一次 subShard 写锁"（非零锁）；`sub.Offset==0` 时不消费服务端快照 offset（旧语义遗留，`client.go:727` 门槛）；A1 实际收敛 5 个调用点。
- 命名偏离：Go SDK 订阅 token 选项定名 `WithSubscriptionToken`（`WithToken` 已被 Dial 鉴权选项占用，`sdks/go/options.go:129`）。
- 任务书前提修正：Go SDK WS 侧原本没有断连码处理，实施时已把 WS close frame 与 gRPC 信封统一接线为 `*DisconnectError`。

## 六、下一轮候选（2026-08-19 对照现码复核）

一、二节的 B1–B4 / A1–A5 已在 §五落地，现码仍在（`ValidateTopic`、matcher `**`、SDK token/PublishAck/Survey/SubRefresh、`ChannelOffsets`、`PublicationFromPayloadV2`、gRPC `disconnect_code`、`connections_total{transport}`、非 200 protojson）。本节只跟踪验收当时新增的 10 条。

### 已落地（不再排期）

1. **presence × `**`**：`shouldTrackPresence` 跳过通配；`TestPresence_LegacyCompanionExactOnly` 锁死 `im.**` 不写伴生频道。
4a. **`SubscriptionSpec`/`ChannelOrSpec` 重复定义**：`converters.ts` 已从 `client/types.ts` re-export。

### 本轮修复（2026-08-19）

2. **时序脆弱测试**：`client_fix_test` 的 50ms presence 睡眠、`survey_test` 三处 500ms、`pkg/transport/grpc/transport_test.go` 的 SlowEnqueue 450ms 改为 `Eventually`/`Never`。并发 WriteMany+Close 不等 `sentMessages`（测试载荷不是合法 protobuf，SendMsg 不会入账），改为等所有 writer 进入 `WriteMany` 再 Close。
3. **TS `setAutoSubscribe` 携带 token**：`autoSubscribe` 改为 `ChannelOrSpec[]`，首次 Connect 可带每频道 token。
4b. **`createSubRefreshMessage` 死参数 `token`**：签名改为 `string[]`，只组 `channels`。
7. **HTTP proxy 200 路径 protojson**：Authenticate/ACL/通知请求体改 `protojson`（`UseProtoNames` 保持 snake_case 兼容）；通知 200 的 `error` 成员用 protojson 解析（与非 200 一致）。
8. **Redis 集成测试 DB 隔离**：command bus 测改 DB 16，不再与 runtime 原子写测（DB 14）互 `FlushDB`。多进程仍可能撞固定 DB，未做随机 DB。

### 仍挂着（未排期）

5. **`Node.Publish` 兜底 `ValidateTopic`**：校验仍在 hub + memory/redis broker；`Node.Publish` 本身不调，自定义 Broker 无防护。
6. **cluster 测试关闭顺序**：`Shutdown` 后异步 presence leave 打 `redis: client is closed`（噪音，非功能缺陷）。
9. **Go SDK `handleSubscribeAck` 锁范围**：仍每频道 `Lock/Unlock`，单 goroutine 无实际竞态。
10. **B4 热路径压测观察**：不是缺陷。

## 七、2026-08-21 评审遗留补录（v2 合并 main 后录入）

> 来源：`docs/review/2026-08-21-comprehensive-review.md` E10（架构债 9 项，原报告 36 OPEN/11 PARTIAL 中的结构性条目）与规范轴遗留。C4-B（广播 `frame.done` 等待预算）已随 v2 合并当批修复（`internal/session/hub.go` `broadcastWriteBudget` + `TestHub_BroadcastPublication_SlowWriterHitsBudget`），不再列入。

### E10 架构债（按既定 PR 序列推进，动包结构前先补 `recordDeliveredOffsets` 乱序/单调性测试）

1. **God 包**：`internal/runtime/node.go` 约 1556 行，装配/发布/survey/代理/生命周期混杂。
2. **Runtime 胖缝**：internal/session 侧 Runtime 接口 36 方法，测试还需 `fake_runtime_test.go` + `var newHub` 双壳。
3. **过渡别名层**：`internal/runtime/aliases_local.go` 294 行约 165 符号（git-mv 过渡产物，消费者已可直接引子包）。
4. **`isPeerClosedError` 编译期耦合双传输**：session 包同时感知 WS/gRPC 错误形态。
5. **`syncClusterSessionState` 三套 ctx**：调用链 ctx 语义不统一。
6. **`Client=Session` 三胞胎**：root/session/runtime 三处类型名并存的历史层。
7. **sim/admin 反向依赖**：仿真与管理面引用生产包内部符号。
8. **双重别名层**：runtime 别名 + genproto `sharedv2`/`sharedpb` 双名（35 处 vs 16 处）。
9. **`NewNode` 冘余构造**：多入口重复装配默认件。
10. **跨包 helper 复制**：`isWildcard` 4 份、`pingClusterRefreshInterval` 2 份、`positionFrom`（session/runtime 各一份，import-cycle 所迫，可下沉 `internal/protocol`）。

### C4 广播等待：剩余中期/长期项（B+D 已落地）

- **中期**：小扇出（≤8）串行分支可无条件走并行发送（`broadcastParallelLimit` 已有界），消除扇出延迟累加。
- **长期**：若 profile 显示广播等待仍是投递延迟主因，再评估方案 C（fire-and-forget + 异步簿记），前置条件为先补 `recordDeliveredOffsets` 的乱序/单调性测试。

### 规范轴遗留（2026-08-21 报告规范节 + 本轮 Standards 轴复核）

- **超长函数**（AGENTS.md "under ~100 lines"）：`handleConnect` 约 262 行、`finishConnect` 约 222 行、`BroadcastPublication` 约 204 行、`recoverSubscription` 约 141 行、`handlePublish` 约 127 行、`cluster_command_bus.handleMessage` 约 126 行、`handleRPC` 约 116 行（均 `internal/session` / `internal/runtime` / `pkg/redisbroker`）。
- **`OutboundMessage_Error` 信封构造**在 client.go 约 25 处重复，helper 未统一。
- **`finishConnect` 8 参数**，后半 5 个总是结伴，可收拢为 struct。
- **结构化日志风格混用**：位置式 err 与键值式 `"error", err` 并存（node.go 多处）。
- **doc comment 缺口约 205 处**、中文注释约 30 处（authorizer.go Principal 一处已翻英文）、无 `.golangci.yml`（import 三组规则无 CI 闸门）。
- **E11 发布热路径冗余**：redisbroker 每条发布 4 次串行 RTT（first_retained bookkeeping 可并入 Lua）；memory broker `interested()` 仅为 bool 做全量 cstrie Lookup（建议 `MatchExists` 短路）。

### 其他未排期（原修复批次"未处理"清单）

- developer 文档批量翻新（`pkg/websocket`/`pkg/grpcstream`/`pkg/quicstream` 等路径已不存在；02-configuration.md 引用已删除的 config-node1/2.yaml）。
- AGENTS.md 示例违反自身规则并引用不存在符号。
- sdks 测试规范；`sdks/go`、`shared` 子模块 `-race` 覆盖。
- 安全面候选：`Connect.session_id` 字符集/长度校验、`hmac_key_file` 权限检查、节点级总连接数/新建速率限制、Redis PSUBSCRIBE glob 元字符转义、4MiB 上限可配置化。
