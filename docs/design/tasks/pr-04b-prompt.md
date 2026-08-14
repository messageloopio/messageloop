# PR-04b 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

本 PR 依赖已合入的 PR-04a。在当前 `main` 上开做。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

## 任务

独立实现 **PR-04b**。唯一规格书（必须先通读再改代码）：

`docs/design/tasks/pr-04b-presence-emit.md`

背景设计（只读）：`docs/design/v1.0-platform-gaps.md` 缺口 2 集群节、KD-3、KD-16。规格书与设计冲突时 **以规格书为准**。

动手前先读：

- `node.go` `emitPresence`（约 1018–1024）与 `Run` 里注册的 `broadcastPublication`
- `presence_event.go` `presencePublication` / `parsePresencePublication`
- `hub.go` `broadcastPublication` 开头的 `ml.type=presence` 分支（现在 `excludeSession` 是 `""`）
- `broker_memory.go` `PublishTransient`（同步调 handler）
- `pkg/redisbroker/redis.go` `PublishTransient` + `pubsub.go` `deliverOnce`（offset 0 无去重）
- `config/config.go` `Server`
- `presence_test.go` `TestPresence_JoinEventAndSnapshot` / `TestPresence_NoCompanionByDefault`
- `cluster_redis_integration_test.go` 的 Redis skip 模式

## 目标

`server.presence.cluster_emit`（默认 false）。`true` 时 `emitPresence` **只** `PublishTransient(exact, presencePublication(evt))`。`false` 时保持 04a 只本地 deliver。两条路径禁止叠用。

## 硬约束

1. 只许改规格书 §2 列出的路径。禁止改 proto、SDK、`client.go` 生产代码、`channel_policy.go`。
2. **`cluster_emit=true` 时 emitPresence 不得调用 deliverPresenceEvent。`false` 时不得 PublishTransient 精确 first-class 帧。**
3. 默认必须是 false。不要改 `NewNode` 签名。
4. 发布频道是精确业务频道，不是 `ch/__presence`。用已有 `presencePublication`。
5. `broadcastPublication` 改写时必须 `exclude=evt.Info.SessionId`，否则 Phase 2 会 self-join。
6. 通配 pattern 不得 `PublishTransient`。通配订阅者靠 rewrite + matcher 收精确频道事件。
7. `legacy_presence_channel` 路径不要动。
8. Redis 双节点测试无 Redis 时 Skip，不要为此失败。
9. 不要做 git commit / push。改动最小化。
10. 现有 `TestPresence_JoinEventAndSnapshot`、`TestPresence_BroadcastPresenceNotPublication`、`TestPresence_NoCompanionByDefault` 必须继续绿。

## 验证（你必须自己跑）

```bash
go test -count=1 . ./config/...
go test -race -count=1 .
```

对照规格书 §7 测试和 §9 八条逐条自检。

## 完成报告（交回时必须包含）

- 改动文件列表
- `emitPresence` / `presenceClusterEmit` / rewrite exclude（文件:行）
- §7 每个测试：过/失败/Skip
- §9 八条：过/失败 + 证据
- `go test` 摘要
- 偏离与理由

不要实现 SDK、Survey、心跳、按 user，不要把 cluster_emit 默认打开。
````
