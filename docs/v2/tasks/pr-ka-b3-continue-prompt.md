# PR-KA-B3 续做 Prompt（崩了之后开新 session 用）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。这是续做，不是从零实现。做完把完成报告交回主 agent 终验。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

当前分支：`v2`。上一个实现 session 在 PR-KA-B3 做到一半崩了。工作区里已有大量未 commit 的服务端改动，**不要丢掉、不要 revert、不要 git commit / tag / push**。

## 规格（唯一合同）

先通读：`docs/v2/tasks/pr-ka-b3-recover.md`

背景只读：`docs/v2/kernel-architecture.md` Protocol 恢复节、KD-K11、KD-K16、KD-K22。冲突以规格书为准。

## 已完成（不要重做，不要回退）

服务端恢复流已经落地，主 agent 抽查过这些测试为绿：

- `recover.go`：`client.v2`；`ChannelRecovery` 不再堆 `[]Publication`；按频道 `Session.Send` replay，每频道一条 `RecoverComplete`
- `client.go`：`finishConnect` / `handleSubscribe` 先发裸 `Connected` / `SubscribeAck`（无 publications / recover_results），再 `streamRecoveries`
- `session.go`：已删 `Connected` 的 `MaxMessageSize` 豁免；`RecoverComplete` 标 Control
- 根包及大部分 `pkg/*` 客户端信封已改 `client/v2`
- 已有测试（保持绿）：
  - `TestSubscribe_AckThenReplayThenCompleteOrder`
  - `TestSubscribe_FreshReplaysFromStart`
  - `TestSubscribe_OffsetZeroFreshFalseIsNotFromStart`
  - `TestSubscribe_RecoverHistoryError`
  - `TestSubscribe_RecoverEmptyBatchGapIsTruncated`
  - `TestSession_OutboundFrameHonorsMaxMessageSize`
  - `TestConnect_RecoverTruncated`

内部 History 仍是 A2 的 `uint64` + `HistoryPage`。从头只有 `fresh=true` 或 epoch 重置。

## 你要补完的缺口（按这个顺序）

### 1. 先让全仓编译/测试绿（服务端）

工作区可能还有漏网的 v1 测试。至少这些文件仍 import `client/v1`，要改成 v2 并改断言（Connected 没有 `GetPublications` / `GetPresence` / `GetRecoverResults`；Presence 是独立信封；PublishAck 是 `position` 不是 `GetOffset`；Subscription 用 `cursor`+`fresh` 不是 `Offset`/`Epoch`）：

- `pkg/websocket/handler_test.go`
- `pkg/websocket/integration_test.go`
- `pkg/quicstream/e2e_test.go`
- `shared/marshaler_test.go`（若测的是客户端信封）

删掉仓库根的 `err.txt`（旧编译日志，不是交付物）。

跑通：

```bash
go test ./...
```

### 2. Go SDK 一条消费路径（规格 §5 / §7.5，必做）

`sdks/go` 是独立 module：`cd sdks/go`。

今日仍是 `client/v1`，仍有：

- `client.go`：`connected.GetPublications()` / `applyRecoverResults` / `ack.GetPublications()`
- `WithRecover(offset uint64, epoch string)` 且文档还把 offset 0 当从头

改成：

- import `client/v2` + `shared/v2`
- `Publication` 无论 `replay` 都进 **同一个** 用户回调 / `handlePublication`
- **删除** `applyRecoverResults`；游标只在 `RecoverComplete.position` 以及 live `Message.position` 更新
- `WithRecover`：`recover=true` + 可选 `*Position` cursor；另加 `WithFresh()`（或等价）设 `fresh=true`
- 禁止 API/注释再说「offset 0 = 从头」
- 单测：replay 与 live 打到同一回调；`RecoverComplete` 更新游标；不再读 `recoverResults`
- 改写 `pr08_test.go` 等所有 v1 恢复测试

```bash
cd sdks/go && go test ./...
```

`shared` 的 replace 路径按该 module 现有 `go.mod` 走，不要另发版本。

### 3. TS SDK 恢复路径（规格要求有恢复路径必须改）

`sdks/ts` 仍用 `src/proto/client/v1`，`src/client/client.ts` 仍 `applyRecoverResults`。

A0 已生成 v2 TS（看 `sdks/ts/src/proto/client/v2/`）。切 codec / client 到 v2：

- 删 `applyRecoverResults` 与 Connected/SubscribeAck 批次投递
- 消费 `publication`（replay 与 live 同一路径）+ `recover_complete` 写游标
- `subscribe({ recover, cursor, fresh })`，不要 offset 0 = 从头
- 改 `test/pr09.test.ts` 等

```bash
cd sdks/ts && npm test
```

### 4. 文档与规格闭环

- `docs/developer/01-architecture.md`、`docs/protocol.md`：删「Ack 内嵌最多 1000 条 / RecoverResult」写法，改成流式恢复
- `docs/v2/tasks/pr-ka-b3-recover.md` §9 填写实现备注（文件列表 + 偏离）

## 硬约束

1. 只改规格书 §2 允许的路径（SDK、漏网测试、文档、recover/client 已改过的文件如需修编译/测试可以继续改）。
2. 不要 revert 服务端流式恢复。不要把 pubs 塞回 Connected/SubscribeAck。
3. 不要改 `protocol/**/v1/**`。不要改 A2 History 算法、A3 CompileInterest、A4 Decide、B2 Occupancy 总线。
4. 不要做 HMAC / internal/* / 不要切 admin 到 server.v2。
5. 不做 git commit / tag / push。
6. 禁止用固定长 Sleep 代替 Send done / Eventually。
7. 发现与规格冲突时以规格书为准，不要发明第二套恢复合同。

## 验证（交报告前必须全绿）

```bash
go test ./...
go test -race .
cd sdks/go && go test ./...
cd sdks/go && go test -race ./...
cd sdks/ts && npm test
```

（Windows 上逐条跑；`sdks/go` 在该目录执行。）

对照规格 §6 测试表与 §7 清单自检。

## 完成报告（交回主 agent）

- 改动文件列表（写明哪些是续做新增）
- §7 每条 过/失败 + 证据
- 上面验证命令与结果
- 偏离（应无）
````
