# ADR-0004：single-activation 仲裁归属 internal/session 的 Takeover 模块

- 状态：已接受（2026-10-04，落地于 commit `2ab08d9`；接线缺口 2026-10-05 闭合）
- 关联：D14、GLOSSARY「Session」「Attachment」「delegate / shell」「Takeover」、[CONTEXT.md §二](../../CONTEXT.md)

## 语境

本地 resume 的交接与各条 close 路径的身份仲裁，曾是一套无 seam 的私有协议（canonical 路由 / delegate / loopAtt / closeFromAttachment / closeFromLoop / closeIfServingHandoff），散布于每条 close 路径。近期全部 P0（锁重入死锁、stale-shell delegate 误杀）落点在此。single-activation（一个 Session 至多一个活 attachment 服务）是硬不变量。

## 决策

1. 身份仲裁收拢 `internal/session/takeover.go`，三重身份比较器按次序适用：
   - ① **delegate 路由**：壳（shell）不自行其是，读写与关闭请求经 delegate 指到被恢复的 Session；
   - ② **attachment 指针身份**：被替换的 attachment 属于已脱离的连接，其读循环死掉不得拆会话；
   - ③ **transport 身份**：被恢复会话的新 attachment 是包着交接 transport 的**新对象**，链式 resume 重绑后旧壳失语。
2. 交接执行块收拢为 `takeoverBy` + `handoffAttachment`（Detach → 新 Attachment 包交接 transport → Attach → 壳置 delegate/停心跳/停 pingDeadline）。
3. `cluster_sim.go` 保留：其再导出的是 Node 侧 resume/fencing 测试 seam，与 session 身份仲裁无关（批 4 偏差记录）。

## 后果与现状

- close 路径仲裁器已接线（client.go:71 / :230 / :167 等调用 canonical / closeFromLoop / closeFromAttachment）。
- 交接执行已收拢（曾「抽而未接」——2026-10-04 走查发现，2026-10-05 闭合）：`handleConnect` 改调 `takeoverBy(existing)`，Detach 按决策 2 的范围并入其中，内联副本删除；错误路径逐一等价（tempAtt nil 的错误文案、Attach 失败 `Close(DisconnectInternal)`）。验证：`MESSAGELOOP_TEST_REDIS_REQUIRED=1` 严格模式 root 全量（runtime Redis 集成真实执行）+ `internal/session` 强制重跑 + sdks/go / shared 模块，全绿。
- 护航：`client_fix_test.go` 44 例从 Session 公开面驱动 close/resume 路径，动此地盘前必跑。
