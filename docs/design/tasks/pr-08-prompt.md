# PR-08 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

本 PR 依赖已合入的 PR-01–PR-07。在当前 `main` 上开做。只改 Go SDK。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

## 任务

独立实现 **PR-08**。唯一规格书（必须先通读再改代码）：

`docs/design/tasks/pr-08-sdk-go.md`

背景设计（只读）：`docs/design/v1.0-platform-gaps.md` 缺口 1/2/4/5 的 SDK 小节。规格书与设计冲突时 **以规格书为准**。

动手前先读：

- `sdks/go/client.go` `Client` 接口、`handleMessage`、`handleConnected`（publications）、`handleSubscribeAck`（现在丢掉 publications/presence）、`OnSurvey` / `handleSurveyRequest`、`pingLoop` / `handlePong`
- `sdks/go/client.go` `SubscribeOption` / `WithEphemeral`（照这个写 `WithRecover`）
- `sdks/go/client.go` `RPC` pending 模式（Survey / Presence 复用这个，不要阻塞 receive loop）
- `sdks/go/client_test.go` `fakeTransport`
- `sdks/go/fix_regression_test.go` survey echo / pong timeout
- `protocol/client/v1/service.proto`：`SubscribeAck.publications/presence`、Outbound `presence=14` / `presence_event=15` / `ping=17` / `survey_result=18`、Inbound `pong=14` / `presence_query=12`
- 服务端只读：`client.go` `handlePresenceQuery`（成功回 oneof presence，带入站 id）；`handleSurvey`（结果异步 SurveyResult；worker 错误可能不带 id）

## 目标

Go SDK：WithRecover、Presence 事件/快照/查询、Survey() 发起、Outbound Ping 回 Pong。旧 OnSurvey 签名不动。

## 硬约束

1. 只许改规格书 §2 列出的路径。禁止改 proto、服务端、`sdks/ts`、`proxy.go` 业务。
2. 不要改 `OnSurvey` 函数签名。加 `OnSurveyRequest` 带 channel。
3. 不要改默认 `PingInterval=30s` / `PingTimeout=10s`。
4. Survey / Presence 的 Wait 必须在调用方 goroutine，禁止在 `handleMessage` 里阻塞。
5. 单测只用 `fakeTransport`，禁止依赖 Redis 或已启动的 server。
6. 现有 SDK 测试必须继续绿（pong timeout、survey echo、resume Recover=true）。
7. 不要把 `clientpb.PresenceEvent` / `clientpb.SurveyResult` 直接做成公共 API；包一层。
8. `SurveyAnswer.UserID` 从 `metadata.entries["user_id"]` 读（proto 无该字段）。
9. 不要 git commit / push。
10. 不要在收包回调里要求用户同步调 Survey/Presence；文档写明。

## 验证（你必须自己跑）

```bash
cd sdks/go && go test -count=1 ./...
cd sdks/go && go test -race -count=1 ./...
```

根包回归（不应被本 PR 改到）：

```bash
go test -count=1 .
```

对照规格书 §8 测试和 §10 八条逐条自检。

## 完成报告（交回时必须包含）

- 改动文件列表
- WithRecover / handleSubscribeAck publications / OnPresence / Presence / Survey / server Ping（文件:行）
- §8 每个测试：过/失败
- §10 八条：过/失败 + 证据
- go test 摘要
- 偏离与理由

不要实现 TS SDK，不要改 proto，不要改服务端 Survey/Presence。
````
