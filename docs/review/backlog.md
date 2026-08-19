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

## 六、下一轮候选（验收新增发现 + 已接受遗留，未排期）

按建议优先级排序：

1. **presence × `**` 冲突修复**（行为副作用）：通配订阅 `a.**` 的 presence join/leave 事件被 `ValidateTopic` 拒绝（`a.**/__presence` 末段含 `**` 但不等于 `**`），仅 warning + `PresencePublishFailures` 指标兜底，无测试覆盖。方向：`node.go` 对通配订阅跳过 presence 发布，或定义通配 presence 的正式语义。
2. **时序脆弱测试改造**（8 处，A5 调查报告）：如 `client_fix_test.go` 的 50ms 固定睡眠等待异步 presence、`survey_test.go:240` 的 500ms 睡眠、`pkg/grpcstream/transport_test.go:265` 与 1s deadline 耦合的 450ms 睡眠——改为同步点式或 `assert.Eventually`。
3. **TS `setAutoSubscribe` 携带 token**（`sdks/ts/src/client/options.ts`）：首次连接自动订阅目前只接受 `string[]`，无法带每频道 token（重连/手动订阅已支持）。需扩展 options 类型。
4. **`SubscriptionSpec`/`ChannelOrSpec` 重复定义收敛**（`sdks/ts/src/client/types.ts` 与 `message/converters.ts` 各一份）；`createSubRefreshMessage` 的死参数 token 标注或移除。
5. **node.go 发布兜底校验**：`ValidateTopic` 目前由 hub/broker 各自调用，第三方自定义 Broker 不受保护；Node 层可加防御性校验。
6. **cluster 测试关闭顺序**：`node.Shutdown()` 关闭 Redis client 后仍有异步 goroutine 发 presence leave（`redis: client is closed` 警告噪音）。
7. **HTTP proxy 200 路径 protojson 化**（`proxy/http.go` 的 Authenticate/SubscribeAcl/OnConnected 等仍用 encoding/json，与非 200 路径不一致）。
8. **Redis 集成测试进程隔离**：`TestClusterRedis_*` 固定 DB 15 + 固定 node ID，多进程并发跑测试会互相 `FlushDB`；建议按进程随机 DB 或 key 前缀。补充（2026-08-19 D13 终验实测）：DB 14 同样中招——根包 `TestClusterRedis_CompareAndSwapSessionState_Atomic`（`clusterAtomicWriteTestDB=14`）与 `pkg/redisbroker/cluster_command_bus_test.go`（`clusterCommandBusTestDB=14`，:233/:235 `FlushDB`）在 `go test ./...` 包间并发下互清，曾致 nil lease panic（单测与全量复跑均过，确认 flake 而非回归）。
9. **Go SDK `handleSubscribeAck` 锁范围统一**（`sdks/go/client.go:570-586`，当前单 goroutine 无实际竞态，样式隐患）。
10. **B4 热路径压测观察**：broadcast 每 publication 新增一次分片写锁，性能敏感场景留意。
