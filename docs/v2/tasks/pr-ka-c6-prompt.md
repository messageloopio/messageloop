# PR-KA-C6 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

当前分支应为 `v2`（已含 A0–A4、B1–B4、C1–C5；C5 tip 为 `1debf5a`）。

## 任务

独立实现 **PR-KA-C6**。唯一规格书（必须先通读再改代码）：

`docs/v2/tasks/pr-ka-c6-gap-notice.md`

背景（只读）：`docs/v2/kernel-architecture.md` Gap 合同 / LiveBus / KD-K12、KD-K14。规格书与设计冲突时 **以规格书为准**。

先读这些现码再动手：

- `pkg/redisbroker/pubsub.go` `catchUpMissed` / `checkCatchUpSeqGap`（C4 中洞检测）/ `checkCatchUpGap`（尾截）/ `deliverOnce` / `dispatch`
- Occupancy 先例：`broker.go` `SetOccupancyHandler`、`node.go` `onOccupancy`/`deliverPresenceEvent`、`hub.go` `presenceRecipients`
- `protocol/client/v2/service.proto` `OutboundMessage.oneof`（现有 field 3–18）；`protocol/shared/v2/types.proto` `GapReason`（现有 0–5）
- `session.go` `outboundFrameClass`（Control/Data 车道）
- SDK：`sdks/go/client.go` `handleMessage`；`sdks/ts/src/message/converters.ts:402-447` 与 `client.ts` 的 switch（**未知信封会触发 error 回调，必须处理**）
- 测试装配先例：`pubsub_test.go` `CatchUpMissed_MiddleGapCounted`（直接调 `catchUpMissed`，无 Sleep）

## 目标（一句话）

catch-up 检出洞（中洞/尾截）时给该频道本地订阅者发 `GapNotice` 信封（channel + gap_reason + 最后安全 position），检测规则不变，两 SDK 把它当一等公民。

## 硬约束

1. 只许改规格书 §2 路径。
2. 检测语义（C4 稠密 seq、尾截、legacy 断链不诬报）**不变**；每频道每次 catch-up 至多一条通知；nil handler 零行为变化；handler panic 不得炸 catch-up。
3. 扇出只在 node/hub 层做（broker 不知道订阅者），沿用 Occupancy 第二管道模式；不给 broker 加订阅者簿记。
4. proto 只加 `GapNotice`（oneof field 19）与 `GAP_REASON_REPLAY_TRUNCATED = 6`；`shared/genproto` 与 `sdks/ts/src/proto` 只允许 buf 再生产物 diff。
5. `GapNotice` 归 Control 车道；Replayer 恢复路径（`recover.go` / `RecoverComplete`）零改动；C1–C5 合同零改动。
6. TS SDK 必须消费 GapNotice（消掉「Unknown message type」路径）；Go SDK 必须正常消费不报错；两 SDK 的 cursor 语义不变。
7. 不做 git commit / tag / push。
8. 测试禁止用固定长 Sleep。多个 `go test` 命令串行执行，不要同时跑两个根目录 `go test ./...`。

## 验证

```bash
go test -count=1 ./pkg/redisbroker
go test -count=1 -run "TestSim_|TestClusterCommandBus" .
go test ./...
go test -race . ./pkg/redisbroker
cd sdks/go && go test ./...
```

TS SDK 若有测试/构建设施（`sdks/ts` 下 package.json 的 test/build 脚本）一并跑过。

Redis 集成测试需要真实 Redis（127.0.0.1:6379，测试用 DB 14；沿用仓库既有 `requireCommandBusRedis` 同款机制）。若环境无 Redis，标 skip 并在报告中写明。

对照规格书 §6 测试表与 §7 清单自检。

## 完成报告

- 改动文件列表
- §7 每条 过/失败 + 证据
- 测试命令与结果（真实输出）
- 偏离（应无）

另外：实现完成后，把实现备注填入规格书 `docs/v2/tasks/pr-ka-c6-gap-notice.md` §9（参照 C5 规格书 §8 的写法）。
````
