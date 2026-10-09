# ADR-0007：双平面共用契约下沉单点；handler 层保留

- 状态：已接受（2026-10-04，落地于 commit `2ab08d9`）
- 关联：D8（批 2 落地）、[CONTEXT.md §三](../../CONTEXT.md)

## 语境

同一个契约在 client 平面（internal/session/client.go）与 Server API 平面（internal/serverapi/api_handler.go）各自持份，会静默漂移。落地前实测：survey 超时钳制两份逐字复制（注释自认 "exactly like the client survey path"）、presence 快照截断与 publish 的 add_history/policy 回退同病。同期也收到过「重组 8 个 Server API handler 为薄转发层」的建议。

## 决策

1. 共用契约下沉单点，按形态选归宿：
   - 纯函数 → 叶子包：`survey.ClampTimeout`（client 与 Server API 两路共用，免扩 38 方法的 Runtime 缝）；
   - 需 Node 状态 → Node 方法：`PresenceSnapshotLimit`（policy 覆盖 ‖ 上限的解析单源）、`PublishForAPI` + `channel.ErrAddHistoryDenied`（addHistory 且 policy 禁 history → **零发布** + sentinel，消费方 `errors.Is`）。
2. **不重组 handler 层**：探查证实 8 个 Server API handler 无一纯 pass-through（各带 scope 语义与聚合逻辑），该层通过 deletion test。
3. **有意保留的差异**不合并：client 面 handlePublish 的 forceTransient 透明转换（ack offset 0 + 指标）与 Server API 面的 fail-closed（ErrAddHistoryDenied → failed++）是**两个契约**，注释双向指认。

## 后果

- 新增「两平面都要」的行为时：先找单点归宿（叶子函数 > Node 方法），别复制第二份。
- 快照字段映射留在各自平面（目标 proto 类型不同：clientpb vs serverv2，强行共享即错位）；失败计数与日志归因留 handler（聚合语义）。
