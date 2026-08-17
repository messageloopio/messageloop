# PR-KA-C1 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

当前分支应为 `v2`（已含 A0–A4、B1–B4；B4 tip 为 `a0ea543`）。

## 任务

独立实现 **PR-KA-C1**。唯一规格书（必须先通读再改代码）：

`docs/v2/tasks/pr-ka-c1-sim.md`

背景（只读）：`docs/v2/kernel-architecture.md` Cluster / 状态机、KD-K3、KD-K4、KD-K5、KD-K20、KD-K30。规格书与设计冲突时 **以规格书为准**。

先读这些现码再动手：

- `cluster_state.go` `syncClusterSessionState` / `ErrSessionFenced` / `CompareAndSwapSessionLease` 相等字段
- `cluster_resume.go` `resumeRemoteSession` / takeover
- `session.go` `Fence` / `Detach` / `Attach` / `Close`
- `cluster_repair.go` `membershipOnce`（首拍只 prime）
- `cluster_state_test.go` `TestClusterSessionSync_FencedWhenAnotherOwnerWins`（A1 单 slot 版，C1 要两节点共享 Dir）
- `cluster_remote_test.go` `fakeSessionDirectory`（不要删）

## 目标（一句话）

进程内确定性夹具（共享内存 Directory + 可编排 Bus + 两 Node），锁住 Bind / Evict / Fence；不改生产 fencing 算法。

## 硬约束

1. 只许改规格书 §2 路径。
2. **不要**改 A1 CAS 谓词、B1 Fence/Detach 语义、B4 HMAC、命令总线传输。
3. **不要**把生产 `IncarnationID` 改成 Redis INCR（KD-K27 另刀）。模拟里写死 `inc-a` / `inc-b`。
4. **不要**上 Redis、不要 HMAC、不要 Stream、不要整仓 `internal/*` 搬家。
5. 不要写 FoundationDB 式全网络模拟，不要随机调度。
6. 宪法测试禁止 `time.Sleep`，禁止依赖 Redis。
7. 两个 Node 必须是真 `*messageloop.Node`，不要再写一套平行状态机。
8. 不做 git commit / tag / push。
9. **不要**在仓库根同时跑两个 `go test ./...`。

## 验证

```bash
go test ./internal/cluster/sim
go test -count=1 .
go test ./...
go test -race . ./internal/cluster/sim
```

对照规格书 §5 场景表、§7 测试表与 §8 清单自检。

## 完成报告

- 改动文件列表
- §8 每条 过/失败 + 证据
- 测试命令与结果
- 偏离（应无）
````
