# PR-05 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

本 PR 依赖已合入的 PR-01（Outbound Ping / Inbound Pong 字段）。在当前 `main` 上开做。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

## 任务

独立实现 **PR-05**。唯一规格书（必须先通读再改代码）：

`docs/design/tasks/pr-05-heartbeat.md`

背景设计（只读）：`docs/design/v1.0-platform-gaps.md` 缺口 4、KD-14。规格书与设计冲突时 **以规格书为准**。

动手前先读：

- `heartbeat.go` 现有单 idle ticker
- `node.go` 解析 `Heartbeat.IdleTimeout`（约 89–102）与 `GetHeartbeatIdleTimeout`
- `client.go` `HandleMessage` 刷新 `lastActivity`、`handlePing` 节流 refresh、`handleMessage` default（未知 envelope = BadRequest）
- `cluster_state.go` `defaultClusterSessionLeaseTTL` 及 `PutSessionLease` 调用点
- `cluster_resume.go` 约 70–72
- `pkg/websocket/handler.go` 70–77 读超时
- `config/config.go` `Heartbeat` / `Validate`
- `node_test.go` `TestNewNode_HeartbeatDefaultIdleTimeout`

## 目标

服务端可按 `ping_interval` 发 Outbound Ping；未在 `ping_timeout` 内收到任何入站则 3511（不等 idle）。`handlePong` 续 presence/lease。默认 idle=300s、不主动 ping。

## 硬约束

1. 只许改规格书 §2 列出的路径。禁止改 proto、SDK、`handlePublish` / `handleSurvey` / presence writer。
2. **不要改默认 idle=300s。不要默认打开 ping_interval。**
3. 策略 B：`pingDeadline` 到期即断开，不等下一个 tick / idle。
4. 任意入站（不只是 Pong）必须 Stop `pingDeadline`。
5. `handlePong` 与 `handlePing` 共用节流 refresh helper。
6. `idle=0 && ping=0` 时 WS 读超时不得变成 10s 地板；lease 保持 600s。
7. 所有 lease **写入**用 `n.sessionLeaseTTL()`，不要继续写死 600s。
8. 非 0 的 idle/ping/timeout 必须 ≥1s（Validate）。
9. 不要改 `NewNode` 签名。不要 git commit / push。
10. 现有 `TestNewNode_HeartbeatDefaultIdleTimeout`、`TestClient_HandlePing_ThrottlesClusterRefresh` 必须留下并绿。

## 验证（你必须自己跑）

```bash
go test -count=1 . ./config/... ./pkg/websocket/...
go test -race -count=1 .
```

对照规格书 §9 测试和 §11 八条逐条自检。

## 完成报告（交回时必须包含）

- 改动文件列表
- `heartbeatLoop` / `handlePong` / `sessionLeaseTTL` / WS 读超时（文件:行）
- §9 每个测试：过/失败
- §11 八条：过/失败 + 证据
- `go test` 摘要
- 偏离与理由

不要实现 SDK OnPing、Survey、按 user，不要把默认 idle 改短。
````
