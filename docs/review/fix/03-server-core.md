# 修复任务 03：服务端核心（根包）

## 背景

你要修复的项目是 **MessageLoop**（工作目录：`D:/Codes/qiulin/messageloop`），Go 实时消息平台服务端。先读根目录 `AGENTS.md` 了解代码规范与测试命令。

刚完成一轮全项目代码评审，评审发现已经过主 agent 逐条核实。完整方案见 `docs/review/fix-plan.md`（可读）。**先读相关代码再动手。**

## 文件归属（严格，多 agent 并行修复）

- 你拥有：根目录的 `client.go`、`hub.go`、`node.go`、`survey.go`、`acl.go`、`heartbeat.go`、`presence.go`、`presence_event.go`、`disconnect.go`、`subscription_saga.go` 及对应 `_test.go`。
- 禁止修改：`cluster*.go`、`broker*.go`、`metrics.go`（其他 agent 负责）；`pkg/`、`proxy/`、`config/`、`cmd/`、`sdks/`、`docs/`。
- 例外：任务 1（ephemeral presence）涉及 `cluster_resume.go:143` 的一处 presence 恢复逻辑——**该文件由 broker/cluster 修复 agent 拥有，你只改 `client.go` 内的部分，并在报告中明确交接这一条**。

## 任务清单

### P0（必修）

1. **ephemeral 订阅仍登记 presence**（`client.go:626-633,1050-1056`）：文档与协议承诺 ephemeral 订阅"不登记在线状态"（`docs/protocol.md:166`），但 connect 携带订阅与 handleSubscribe 两处仅判 `!alreadySubscribed` 即 `presence.Add` + 发布 join/leave 事件；`Subscriber.Ephemeral` 字段被存储（`hub.go:210-217`）却从不参与 presence 决策。修复：跳过 ephemeral 订阅的 presence.Add 与 join/leave 发布（ unsubscribe/close 路径的 leave 也要对称处理）。补测试：ephemeral 订阅不产生 presence 记录与事件。**交接**：`cluster_resume.go:143` 恢复订阅的 presence 逻辑由 broker/cluster agent 修复，你在报告中注明。

### P1（必修）

2. **connect 失败半打开连接**（`client.go:526-559` 配合 `305-321`）：`AddClient`/`syncClusterSessionState`/`resumeRemoteSession` 返回普通 `fmt.Errorf` 时，`HandleMessage` 只发 INTERNAL_ERROR 帧不断连，连接处于半注册态且无法再 Connect（再发 Connect 会被判 BadRequest）。修复：这些失败统一包装为 `Disconnect`（如 `DisconnectInternal`）或在失败分支显式 `c.close(...)`，保证 connect 失败必断连。注意 resume 失败场景保持 `DisconnectStale` 语义。
3. **resume 失败僵尸会话**（`client.go:517-522`；`hub.go:501-519,673-698`）：本地 resume 先 `oldSession.closeQuiet()` 再 `ReplaceSession`，后者失败（如连接数超限）直接 return——旧会话 transport 已关但永久残留在 `sessions` map + connShards + subShards，订阅照收广播、投递报错累加 `DeliveryFailures`。修复：失败分支回滚——`RemoveSessionIfMatches(sessionID, oldSession)` 清理 hub 残留及其订阅，并清理 cluster 状态。补回归测试：构造超限使 ReplaceSession 失败，断言旧会话从各处清除。
4. **`close()` 与并发 `Subscribe` 的订阅泄漏窗口**（`client.go:147-169`）：`close()` 在锁内拷贝 `subscribedChannels` 后即释放 `c.mu`，16-worker 随后才移除；此窗口内 `handleSubscribe` 新增的订阅不会被清理，残留在 hub subShards。修复：`handleSubscribe` 加 status 检查（closing/closed 拒绝），或 close 清理期间阻塞新增订阅。
5. **`ClientInfo()` 无锁读取**（`client.go:754-763`）：`c.client/c.session/c.user` 在 `handleConnect`/`cluster_resume` 锁内写入，此处无锁读，`-race` 可复现。修复：加 `c.mu.RLock()` 快照，或复用 `ClientID()/SessionID()/UserID()` 三个已有加锁 getter（`connectedAt` 不可变，无需保护）。
6. **无界 presence goroutine**（`client.go:183-189,1117-1120`）：close/handleUnsubscribe 按频道各起一个 `go PublishPresenceLeave(...)`，单连接大量订阅断开时瞬间产生等量 goroutine + Redis 往返。修复：复用 close 中已有的 16-worker 模式或信号量限流。
7. **broker 启动失败 goroutine 内 panic**（`node.go:124-130`）：`Node.Run` 中 `go func(){ ... panic(err) }()`——Redis 启动失败时进程在 `Run` 返回后崩溃，lynx 无法感知启动失败。修复：错误经 ready/error 通道并入 `Run` 的返回路径，goroutine 内只记日志。

### P2（顺手修）

8. `statusConnected` 死常量（`client.go:128-134`）：生产代码只写 `statusClosed`，三态机实际只有两态——在 connect 成功后置 `statusConnected` 并让 status 真正参与状态判断（配合任务 4），或删除死常量。二选一，保持文档一致。
9. `subShard.broadcastPublication` 仅测试调用的 ~80 行重复实现（`hub.go:291-375` vs `396-489`），且无 exact+wildcard 按 session 去重：删除 subShard 版本，`hub_test.go:425/449` 改走 Hub 层。
10. ACL `path.Match` 语义（`acl.go:84,111`）：`chat.**` 无特殊语义、`*` 跨点匹配与 CSTrie 单段通配不一致、管不到 `*/__presence` 频道。改为与 matcher 一致的分段通配实现（或最小改动：修正注释文档+补 `**`/`/` 频道测试锁定现状语义，报告中说明选择）。
11. `GetActiveChannels` 把通配模式当频道列出且 exact+wildcard 重复计数（`hub.go:624-657`）：通配订阅不计入（或投影到实际匹配频道），按 session 去重。
12. `ReplaceSession` 连接数检查 TOCTOU（`hub.go:673-698`）：connShard 写入移入 `h.mu` 临界区，或 per-shard 原子 check-and-add。
13. `Survey.Close` 单条 drain + "channel full" 假警报（`survey.go:174-181,107-108`）：改循环 drain 或删除；修正告警文案（响应已先写 map）。
14. 订阅上限检查虚增计数（`client.go:591-598,1012-1019`）：重复频道与 ACL 拒绝频道计入上限——按去重新增频道计数，先 ACL 过滤再计数。
15. 未认证 connect 把客户端伪造的 session ID 传给认证代理（`client.go:387-391,431`）：认证请求使用原始 session 或标记未验证。
16. `handleSurveyReply` 的 `lastSurveyRequestID` 单槽回退串路由（`client.go:1274-1279`）：移除回退，强制要求 `RequestId`。
17. `hub.broadcastPublication` 串行分支缺 panic recover（`hub.go:447-458`），与并行分支（468-474）不一致：补 recover 转错误日志。

## 测试要求

- 修复前跑 `go build ./... && go test .` 确认基线全绿。
- 每条 P0/P1 配回归测试（方案 `docs/review/fix-plan.md` 的测试清单第 5、10 条及模块 01 报告的建议测试可参考）：重点为任务 1（ephemeral presence）、2（connect 失败必断连）、3（ReplaceSession 失败无僵尸）、4（并发 Subscribe 不泄漏）、5（`-race`）。
- 完成后 `go test -race .` 全绿。

## 纪律

- 不做 git commit/push。最小改动；`client.go` 是大文件，改动保持局部。
- 对外协议行为变更（如 ephemeral presence）在报告中显著标注。
- 完成后返回报告：每条任务处置、改动文件清单、测试结果、与其他 agent 的交接项（任务 1 的 cluster_resume.go 部分）、遗留问题。
