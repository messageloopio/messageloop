# PR-03 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

本 PR 依赖已合入的 PR-01（proto）和 PR-02（`ChannelPolicyEngine`）。在当前 `main` 上开做。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

## 任务

独立实现 **PR-03**。唯一规格书（必须先通读再改代码）：

`docs/design/tasks/pr-03-recover.md`

背景设计（只读）：`docs/design/v1.0-platform-gaps.md` 缺口 1、KD-2、KD-9。规格书与设计冲突时 **以规格书为准**。

动手前先读：

- `client.go` `handleConnect` 恢复块（约 656–805）和 `handleSubscribe`（约 1145–1218）
- `channel_policy.go` 的 `For()`（`transient_only` 已强制 Recover=false）
- `cluster_state.go` `ChannelOffsets` 注释
- `hub.go` `publicationID` / `isWildcard`
- `defaults.go` `MaxRecoveredPublications`
- `client_test.go` `TestNode_Connect_RecoveryCap`

## 目标

Connect 与 Subscribe 共用 `Node.recoverSubscription`。History 成败/截断/跳过必须出现在 `RecoverResult` 里。订阅成功不依赖恢复成功。

## 硬约束

1. 只许改规格书 §2 列出的路径。禁止改 proto。禁止改 `handlePublish` / `handleSurvey` / `handlePing`。禁止改 SDK 业务代码。
2. **禁止**填充 `Connected.presence` / `SubscribeAck.presence`（那是 PR-04）。
3. resume 且快照没有 `ChannelOffsets[ch]` → `RecoverSkipped`，即使客户端 `recover=true, offset=0`。禁止倒全部历史。
4. 非 resume 且 `recover=true, offset=0` → 从头拉（KD-2，仅此路径）。
5. History 失败不得 `continue` 吞掉；不得回滚已成功的订阅。
6. 恢复消息 ID 必须用 `publicationID(channel, offset)`。
7. 通配频道、`ChannelPolicy` 的 `!Recover` / `!History` / `TransientOnly` → Skip，客户端要了 recover 则 `RECOVER_SKIPPED`。
8. 一次 Connect 或一次 Subscribe 共用 `MaxRecoveredPublications`（1000）配额。
9. 不要改 `NewNode` 签名。不要做 git commit / push。
10. 改动最小化。现有 `TestNode_Connect_RecoveryCap` 必须留下并补 truncated / recover_results 断言。

## 验证（你必须自己跑）

```bash
go test -count=1 .
go test -race -count=1 .
```

对照规格书 §9 十个测试和 §11 十条清单逐条自检。

## 完成报告（交回时必须包含）

- 改动文件列表
- helper 与 handleConnect / handleSubscribe 调用点（文件:行）
- resume union 集合怎么构造的
- §9 每个测试：过/失败
- §11 十条：过/失败 + 证据
- `go test` 摘要
- 偏离规格的地方与理由

不要实现 Presence 信封、Survey、心跳、按 user、SDK。
````
