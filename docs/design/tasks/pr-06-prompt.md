# PR-06 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

本 PR 依赖已合入的 PR-01（Admin proto 已有 `users` / `user_id` 字段，处理器还没读）。在当前 `main` 上开做。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

## 任务

独立实现 **PR-06**。唯一规格书（必须先通读再改代码）：

`docs/design/tasks/pr-06-admin-user.md`

背景设计（只读）：`docs/design/v1.0-platform-gaps.md` 缺口 3、KD-13。规格书与设计冲突时 **以规格书为准**。

动手前先读：

- `protocol/server/v1/api.proto` Destination / Disconnect / Subscribe / Unsubscribe（字段已在，不要改 proto）
- `pkg/grpcstream/api_handler.go` Publish（约 45 行：sessions+channels 都空视为失败——只填 users 会踩这个）
- `hub.go` `connShard.users`（约 150–180）
- `cluster.go` `SessionDirectory`
- `cluster_state.go` `noopSessionDirectory`、`PutSessionLease`
- `cluster_resume.go` CAS
- `pkg/redisbroker/cluster_directory.go` Put/CAS/Delete lease
- `cluster_commands.go` `PublishToSession` / `DisconnectSession` / `SubscribeSession`
- fake：`cluster_remote_test.go` `fakeSessionDirectory` 等，改接口后必须补方法

## 目标

Admin 按 user_id 展开为 session，再走现有 session API。单节点用 Hub；集群加 Redis 索引并在展开时校验 lease.UserID。

## 硬约束

1. 只许改规格书 §2 列出的路径。禁止改 proto、SDK、`client.go` 业务 handler。
2. 不新增 Admin RPC，不新增 ClusterCommand 类型。
3. 空 user_id / users 中的空串 → `InvalidArgument`，不扫描。
4. Publish **只填 destination.users** 必须成功投递，不能再当「无 destination」。
5. 展开必须校验 UserID（本地 Client 或 GetSessionLease）。索引不是权威。
6. 禁止索引 miss 时全集群 SCAN。
7. Put / CAS 成功 / Delete 都要维护 user 索引。空 UserID 不进索引。
8. 所有 SessionDirectory 实现（含测试 fake）补齐三方法。
9. 不要改 `NewNode` 签名。不要 git commit / push。
10. 现有按 session/channel 的 Admin 测试必须继续绿。

## 验证（你必须自己跑）

```bash
go test -count=1 . ./config/... ./pkg/grpcstream/... ./pkg/redisbroker/...
go test -race -count=1 .
```

对照规格书 §7 测试和 §9 八条逐条自检。

## 完成报告（交回时必须包含）

- 改动文件列表
- `SessionsByUser` / `expandUserSessions` / `syncUserIndex` / Publish users 分支（文件:行）
- §7 每个测试：过/失败/Skip
- §9 八条：过/失败 + 证据
- `go test` 摘要
- 偏离与理由

不要实现客户端 PublishToUser、Survey、SDK。
````
