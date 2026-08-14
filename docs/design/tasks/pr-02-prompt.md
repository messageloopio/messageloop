# PR-02 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

本 PR **不依赖 PR-01**，可与 PR-01 并行（不同工作区/分支时互不改 proto）。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

## 任务

独立实现 **PR-02**。唯一规格书（必须先通读再改代码）：

`docs/design/tasks/pr-02-channel-policy.md`

背景设计（只读）：`docs/design/v1.0-platform-gaps.md` 缺口 7、KD-6、KD-8。规格书与设计冲突时 **以规格书为准**。

先读这些现码再动手：

- `config/config.go`（`Server`、`Validate`）
- `acl.go` 的 `matchChannelPattern`（复用，不要复制一份新 DSL）
- `client.go` `handlePublish`（约 1029 行起）
- `node.go` `NewNode`、`Publish` / `PublishTransient`
- `pkg/grpcstream/api_handler.go` 频道 `AddHistory` 分支
- `broker_memory.go` 的 `channelHistory` / `Publish`
- `pkg/redisbroker/redis.go` `Publish` 的 `XAdd` / `Expire`
- `publication.go`、`broker.go` 的 `Publication`
- `metrics.go` 的注册方式

## 目标

实现 `ChannelPolicyEngine`（glob first-match + overlay），并在 **发布路径** 兑现：

- `transient_only` / `history=false` → 客户端改走 `PublishTransient`（ack offset=0），`Node.Publish` 返回 `ErrHistoryDisabled`
- Admin `add_history=true` 且策略禁历史 → 不发布、计失败
- `history_size`：新频道首次 Publish 按该 cap 建 ring / Redis `XAdd.MaxLen`
- Redis `history_ttl` 可 per-pub 覆盖；memory 忽略并 Warn
- 已存在的 memory ring **不**因新的 HistorySize 重建

`recover` / `presence` / `survey` 只进策略结构体，本 PR **不要**改 Subscribe / Survey / Presence 行为。

## 硬约束

1. 只许改规格书 §2 列出的路径。禁止改 proto。禁止改 `handleSubscribe` / `handleConnect` / `handleSurvey`。禁止改 ACL 求值。
2. 不要加 `SkipHistory`。不要改 `Broker` 接口方法签名。
3. `TransientOnly==true` 时 `For()` 返回的 `History` 与 `Recover` 必须为 false。
4. 无 `server.channels` 时行为与现网一致（history/presence 开，survey 关）。
5. 策略匹配 first-match；ACL 仍 last-write-wins。文档必须写清二者相反。
6. 不要改 `NewNode` 的函数签名。
7. 不做 git commit / tag / push。
8. 新增测试必须对「旧代码会红」有意识：至少 `TestHandlePublish_PolicyForcesTransient` 和 `TestMemoryBroker_PerChannelHistorySize` 是新行为。

## 验证（你必须自己跑）

```bash
go test ./config/... . ./pkg/grpcstream/... ./pkg/redisbroker/...
go test -race .
go test ./...
```

对照规格书 §8 / §9 清单逐条自检。

## 完成报告（交回时必须包含）

- 改动文件列表
- `ChannelPolicyEngine.For` / overlay / first-match 的文件:行
- `handlePublish` 与 Admin 禁历史分支的文件:行
- memory ring 首次分配与「不重建已有 ring」的测试名
- §9 八条：每条 过/失败 + 证据
- `go test` 输出摘要
- 偏离规格的地方与理由

不要实现 PR-03 及之后的恢复/presence/Survey/心跳/按 user。
````
