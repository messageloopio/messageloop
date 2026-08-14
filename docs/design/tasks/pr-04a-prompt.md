# PR-04a 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

本 PR 依赖已合入的 PR-01（proto）、PR-02（`ChannelPolicy`）、PR-03（recover，同改 `client.go`）。在当前 `main` 上开做。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

## 任务

独立实现 **PR-04a**。唯一规格书（必须先通读再改代码）：

`docs/design/tasks/pr-04a-presence.md`

背景设计（只读）：`docs/design/v1.0-platform-gaps.md` 缺口 2、缺口 6、KD-3、KD-16。规格书与设计冲突时 **以规格书为准**。

动手前先读：

- `node.go` `PublishPresenceJoin` / `PublishPresenceLeave` / `presenceChannel` / `SetPresenceForSession`
- `client.go` `handleConnect` / `handleSubscribe` 的 presence 块、`handleUnsubscribe`、`close`、`refreshPresence`、`handleMessage` 的 default 分支
- `hub.go` `broadcastPublication`（约 329–448）与 `isWildcard`
- `cluster_resume.go` `restoreSessionSubscriptions`
- `cluster_commands.go` `handleClusterSubscribe` / `handleClusterUnsubscribe`
- `channel_policy.go` `Presence` / `LegacyPresenceChannel` / `PresenceSnapshotLimit`
- `client_fix_test.go` `newPresenceEventObserver`（订的是伴生频道，本 PR 之后必须改）
- `pkg/topics/matcher.go` `matchCriteria` / `ValidateTopic`

## 目标

join/leave 成为本节点一等 `PresenceEvent`。Connect / SubscribeAck 带快照，客户端可 `PresenceQuery`。通配不进 store、不写伴生。`broadcastPublication` 能识别 `ml.type=presence` 并改写/丢弃。

## 硬约束

1. 只许改规格书 §2 列出的路径。禁止改 proto。禁止改 `handlePublish` / `handleSurvey` / `handlePing` 的非 presence 逻辑。禁止改 SDK 业务代码。
2. **`emitPresence` 只调 `deliverPresenceEvent`。禁止 `PublishTransient(exactCh, {ml.type=presence})`。不要实现 `cluster_emit`。**
3. 默认不写 `ch/__presence`。只有 `legacy_presence_channel=true` 且精确频道才走现有 `PublishPresenceJoin/Leave`。
4. 通配 / ephemeral / `ChannelPolicy.Presence==false`：不 SetPresence、不 join/leave、不快照。
5. 加入者快照含自己，**不**给加入者发 self-join。Leave 不给离开者自己。
6. `broadcastPublication` 遇到 `ml.type=presence` 不得产出 `publication`，不得 `MessagesDelivered++`。
7. PresenceQuery：未 `sessionCoversChannel` → `PERMISSION_DENIED`（即使 ACL `*`）；通配频道名 → `BAD_REQUEST`；`presence=false` → `POLICY_DENIED`。禁止只用 `CanSubscribe`。
8. `sessionCoversChannel` 的通配匹配必须用导出的 `topics.Match`（包装现有 `matchCriteria`），禁止新 DSL。
9. 不要改 `NewNode` 签名。不要做 git commit / push。
10. 改动最小化。现有 `TestClient_EphemeralSubscription_NoPresenceOrEvents`、`TestNode_PublishPresenceJoin_DistinctMessageIDs` 必须留下。`newPresenceEventObserver` 改为订精确频道、数 `presence_event`。

## 验证（你必须自己跑）

```bash
go test -count=1 . ./config/... ./pkg/topics/... ./pkg/grpcstream/...
go test -race -count=1 .
```

对照规格书 §7 测试和 §9 十条清单逐条自检。

## 完成报告（交回时必须包含）

- 改动文件列表
- `emitPresence` / `deliverPresenceEvent` / `presenceJoin` / `sessionCoversChannel` / Query 分支（文件:行）
- writer 门闩改了哪几处
- §7 每个测试：过/失败
- §9 十条：过/失败 + 证据
- `go test` 摘要
- 偏离规格的地方与理由

不要实现 cluster_emit、Survey、心跳、按 user、SDK OnPresence。
````
