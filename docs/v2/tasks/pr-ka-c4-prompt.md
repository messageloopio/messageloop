# PR-KA-C4 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

当前分支应为 `v2`（已含 A0–A4、B1–B4、C1–C3；C3 tip 为 `d7b01d8`）。

## 任务

独立实现 **PR-KA-C4**。唯一规格书（必须先通读再改代码）：

`docs/v2/tasks/pr-ka-c4-dense-seq.md`

背景（只读）：`docs/v2/kernel-architecture.md` StreamLog / Gap 合同、KD-K12、KD-K14、Q8。规格书与设计冲突时 **以规格书为准**。

先读这些现码再动手：

- `pkg/redisbroker/redis.go` `Publish`（XADD → offset → 二次序列化 → PUBLISH）、`updateFirstRetained`
- `pkg/redisbroker/message.go` `redisMessage`（stream `data` 与实时载荷共用的 JSON 信封）
- `pkg/redisbroker/history.go` `getHistory` / `parseStreamOffset` / `streamStartID`（offset = `ts<<20|seq`，**不要动编码**）
- `pkg/redisbroker/pubsub.go` `deliverOnce` / `catchUpMissed` / `checkCatchUpGap`（`lastOffsets` 簿记）
- `broker.go` `HistoryGapReason` / `HistoryPage` / `Publication`（A2 合同）
- `recover.go` `gapReasonV2` 与 gap 指标
- `protocol/shared/v2/types.proto` `GapReason` 枚举（加值后 `task generate-protocol`）

## 目标（一句话）

每条 history stream 条目带每频道稠密 seq（Lua 内 INCR+XADD 原子发号），History 页内与重连 catch-up 据此做真中洞检测；offset 编码与 Publish 成功合同不变，memory broker 不动。

## 硬约束

1. 只许改规格书 §2 路径。
2. **禁止** Go 侧先 INCR 再 XADD（崩溃留假洞）；发号必须在 Lua 脚本内与 XADD 原子完成。
3. **不要**改 offset 编码、`parseStreamOffset`/`streamStartID`、`first_retained` 机制、Publish 成功合同（PUBLISH 失败不 XDel）。
4. **不要**动 memory broker 行为、`client.go`/`hub.go`/`session.go`、集群命令总线（C3）、C2 epoch、C1 sim、SDK 业务代码。
5. proto 只加 `GAP_REASON_MIDDLE = 5`；`shared/genproto` 与 `sdks/ts/src/proto` 只允许 buf 再生产物 diff。
6. catch-up 中洞检测只到计数 + Warn，**不**新增 client-facing 信封。
7. 缺 `s` 字段的 legacy 条目断开证据链，不得诬报 Middle。
8. 不做 git commit / tag / push。
9. 测试禁止用固定长 Sleep。不要同时跑两个根目录 `go test ./...`。

## 验证

```bash
go test -count=1 ./pkg/redisbroker
go test -count=1 -run "TestSim_|TestClusterCommandBus" .
go test ./...
go test -race . ./pkg/redisbroker
Set-Location sdks/go; go test
```

对照规格书 §7 测试表与 §8 清单自检。

## 完成报告

- 改动文件列表
- §8 每条 过/失败 + 证据
- 测试命令与结果
- 偏离（应无）
````
