# PR-KA-B3 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

当前分支应为 `v2`（已含 A0–A4、B1–B2）。

## 任务

独立实现 **PR-KA-B3**。唯一规格书（必须先通读再改代码）：

`docs/v2/tasks/pr-ka-b3-recover.md`

背景（只读）：`docs/v2/kernel-architecture.md` Protocol 恢复节、KD-K11、KD-K16、KD-K22。规格书与设计冲突时 **以规格书为准**。

先读这些现码再动手：

- `recover.go` `recoverSubscription`（今日把 Publications 堆进 ChannelRecovery）
- `client.go` `finishConnect` / `handleSubscribe` 如何把 pubs 塞进 Connected/SubscribeAck
- `session.go` `outboundFrameClass` 与 `Connected` MaxMessageSize 豁免
- `protocol/client/v2/service.proto` `RecoverComplete` / `Subscription.cursor` / `fresh` / `Message.replay`
- `sdks/go/client.go` `handlePublications` / `applyRecoverResults`
- `sdks/ts/src/client/client.ts` 同样的批次路径

## 目标（一句话）

恢复改成流：Ack 不再内嵌批次；replay Publication + RecoverComplete；client v2 信封；SDK 一条消费路径。

## 硬约束

1. 只许改规格书 §2 路径。
2. 禁止再往 `Connected` / `SubscribeAck` 塞 publications / RecoverResult。
3. `offset==0` 不得表示从头；只有 `fresh=true` 或 epoch 重置。
4. 恢复失败不撤订阅。每个 recover 频道必须有 `RecoverComplete`。
5. 不要改 `protocol/**/v1/**`。不要改 A2 History 算法、A3 live 编译、B2 Occupancy 总线。
6. 不要做 HMAC / internal/* / 不要切 admin 到 server.v2。
7. 不做 git commit / tag / push。
8. 测试禁止用固定长 Sleep 代替 Send 完成 / Eventually。

## 验证

```bash
go test ./...
go test ./sdks/go
go test -race . ./sdks/go
```

对照规格书 §6 测试表与 §7 清单自检。

## 完成报告

- 改动文件列表
- §7 每条 过/失败 + 证据
- 测试命令与结果
- 偏离（应无）
````
