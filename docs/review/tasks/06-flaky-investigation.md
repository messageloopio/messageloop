# 任务书 06：既有时序 flaky 测试排查（只读调查）

## 角色

你是 MessageLoop（项目根 `D:/Codes/qiulin/messageloop`）的测试稳定性调查员。本任务是 backlog 的 A5：**纯调查，不修改任何文件**。

## 背景

评审修复周期中记录到两处既有时序相关的偶发失败（flaky），两轮全量测试（含 `go test -race -count=1 .`）均未复现，判定与已完成的修复无关。其中之一出现在根包 client 相关测试（`client_fix_test.go` 一带，-race 全量跑约 32s 的过程中偶发一次）。

## 任务

1. **复现尝试**：对可疑范围做压力重复跑：
   - `go test -race -count=30 .`（根包，重点 client/hub/presence 相关测试）
   - `go test -race -count=20 ./pkg/topics/... ./proxy/... ./pkg/grpcstream/... ./pkg/websocket/...`
   - 单测试粒度：对根包中涉及时间、goroutine、channel 超时的测试（grep `time.After|time.Sleep|context.WithTimeout|eventually|assert.Eventually` 找候选），逐个 `-run` 高 count 复跑。
2. **候选分析**：无论是否复现，都对根包测试做一次时序脆弱性走查——依赖真实 wall-clock 睡眠、无同步点的 goroutine 交错断言、固定超时阈值过小的测试，列出候选清单（file:line + 脆弱原因）。
3. **若复现**：记录失败输出原文、复现频率、最小复现命令；用 `git stash`-free 方式（只读，不改动工作区）分析根因，给出修复建议但不实施。
4. **若未复现**：给出结论——每处的复跑轮次、覆盖率推断、建议处置（接受现状 / 加日志待再发 / 建议改造为同步点式测试）。

## 约束

- 只读 + 运行测试：不修改任何文件、不做任何 git 写操作。
- 测试时间预算可控：单条命令超 5 分钟的降 count 或拆包跑。
- 报告格式：复现结果（命令、频率、失败输出）、时序脆弱候选清单（file:line + 原因）、逐项处置建议。
