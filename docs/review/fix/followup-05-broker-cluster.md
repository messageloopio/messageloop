# 继续修复：工作流 05 Broker 与集群（第 2 轮）

背景：你此前完成了 MessageLoop（`D:/Codes/qiulin/messageloop`）Broker/集群层的修复。深度验收结论：全部条目正确落地，但有 1 项必修（验收新发现）。范围与上次相同（`pkg/redisbroker/`、`broker*.go`、`cluster*.go`），禁止 git 写操作。

## 必修

1. **驱逐回滚丢失 ephemeral 标志**（`cluster_resume.go:277` 附近）：`evictSessionForTakeover` 的回滚路径调用 `restoreLocalSubscription(rollbackCtx, ch, NewSubscriber(client, false))`，把原本 ephemeral 的订阅恢复为 `Ephemeral=false`。后果：回滚后该订阅在 hub 中变为永久订阅，后续 `handleUnsubscribe`/`close` 会按非 ephemeral 路径发布 presence leave 事件，与本轮全链路"ephemeral 不产 join/leave"语义矛盾（你在 `cluster_resume.go:136-156` 已正确实现恢复路径的 ephemeral 跳过）。
   修复：回滚时保留原订阅的 ephemeral 标志——从回滚上下文可获取的原始订阅信息中读取（如驱逐前 LookupSubscriber 保存的 `stored.Ephemeral`），传入 `NewSubscriber`。
   回归测试：构造 ephemeral 订阅的驱逐 + `broker.Unsubscribe` 失败触发回滚，断言 hub 中恢复的订阅 `Ephemeral==true` 且不发布 leave 事件。

## 验收标准

- `go test -race -count=1 . ./pkg/redisbroker/...` 全绿（本机无 Redis 时集成测试跳过即可，注明）。
- 返回报告：处置、改动文件、测试结果。
