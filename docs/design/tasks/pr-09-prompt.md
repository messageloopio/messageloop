# PR-09 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

本 PR 依赖已合入的 PR-01–PR-08。在当前 `main` 上开做。只改 TypeScript SDK。语义对齐 Go SDK（PR-08）。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

## 任务

独立实现 **PR-09**。唯一规格书（必须先通读再改代码）：

`docs/design/tasks/pr-09-sdk-ts.md`

背景设计（只读）：`docs/design/v1.0-platform-gaps.md` 缺口 1/2/4/5 的 SDK 小节；对照已落地的 Go 实现 `sdks/go/client.go`（PR-08）。规格书与设计冲突时 **以规格书为准**。

动手前先读：

- `sdks/ts/src/client/client.ts` `handleMessage`、`subscribe`、`resubscribeAllChannels`、`onSurvey` / `handleSurveyRequest`、`sendPing`
- `sdks/ts/src/client/types.ts` `IClient`、`SubscriptionSpec`
- `sdks/ts/src/message/converters.ts` `parseOutboundMessage`、`createSubscribeMessage`（现在不设 recover）
- `sdks/ts/test/client.test.ts`、`protocol.test.ts`（mock transport 风格）
- `docs/developer/08-sdk-ts.md`（仍写「不支持 Survey」）
- Go 对照：`sdks/go/client.go` `WithRecover` / `Presence` / `Survey` / `handleServerPing`

## 目标

TS SDK：subscribe 带 recover、Presence 事件/快照/查询、survey() 发起、Outbound Ping 回 Pong。旧 onSurvey 签名不动。未 resumed 重连的 Subscribe 必须带 recover。

## 硬约束

1. 只许改规格书 §2 列出的路径。禁止改 proto 源、`sdks/ts/src/proto/**`、服务端、`sdks/go`。
2. 不要改 `onSurvey` 函数签名。加 `onSurveyRequest` 带 channel。
3. 不要改默认 `pingInterval=30000` / `pingTimeout=10000` / `autoReconnect=true`。
4. survey / presence 的 Wait 必须在调用方 Promise，禁止在 `handleMessage` 里阻塞。
5. 单测只用 jest + mock transport，禁止依赖 Redis 或已启动的 server。
6. 现有测试必须继续绿（token subscribe、publish transient、onSurvey echo）。
7. 不要把 proto PresenceEvent / SurveyResult 直接做成公共 API；包一层。
8. `SurveyAnswer.userId` 从 `metadata.entries["user_id"]` 读。
9. 不要 git commit / push。
10. `resubscribeAllChannels` 必须发 recover=true + 已存 offset/epoch。
11. 新测试文件顶部用注释贴规格 §3 的 Go/TS 对照表。

## 验证（你必须自己跑）

```bash
cd sdks/ts && npm test
```

若缺依赖：`cd sdks/ts && npm install && npm test`。

对照规格书 §8 测试和 §10 八条逐条自检。

## 完成报告（交回时必须包含）

- 改动文件列表
- SubscriptionSpec.recover / subscribeAck publications / onPresence / presence / survey / server Ping / resubscribeAllChannels（文件:行）
- §8 每个测试：过/失败
- §10 八条：过/失败 + 证据
- npm test 摘要
- 偏离与理由

不要实现 gRPC 传输，不要改 proto，不要改服务端或 Go SDK。
````
