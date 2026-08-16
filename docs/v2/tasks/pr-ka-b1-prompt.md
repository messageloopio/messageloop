# PR-KA-B1 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

当前分支应为 `v2`（已含 A0–A4）。

## 任务

独立实现 **PR-KA-B1**。唯一规格书（必须先通读再改代码）：

`docs/v2/tasks/pr-ka-b1-session.md`

背景（只读）：`docs/v2/kernel-architecture.md` 的 Session Plane、KD-K2、KD-K3、KD-K5。规格书与设计冲突时 **以规格书为准**。

先读这些现码再动手：

- `hub.go` `ReplaceSession`（扫 64 subShard + 重建通配 matcher）
- `client.go` `handleConnect` 本机 resume（`closeQuiet` + `ReplaceSession` + 失败回滚）
- `client.go` `close` / `closeQuiet` / `write`
- `cluster_resume.go` `evictSessionForTakeover`
- `pkg/grpcstream/transport.go` `sendCh`（深 64）
- `docs/v2/kernel-architecture.md` 状态机表与写队列数字

## 目标（一句话）

Session 指针在本机接管时保持稳定；Attachment 可撕可贴；三条关闭收成 Close/Fence/Detach；写队列 Control 优先。

## 硬约束

1. 只许改规格书 §2 路径。
2. 必须删除 `ReplaceSession`。本机 resume 禁止扫 subShard、禁止为换指针重建 matcher。
3. 先查 `maxConnsPerUser` 再 Detach。失败时旧 Session 仍 Attached。
4. 被抢只准 `Fence`（不 Leave、不 Unbind）。真走才 `Close`。
5. `NewClient` 之后状态必须是 `Authenticating`，不得再靠零值。
6. 写失败 `io.EOF` 必须是 3000，不得 3512。gRPC 不得保留深 64 的第二队列。
7. 不做 git commit / tag / push。不要做 Occupancy LiveBus、流式恢复、HMAC、internal/*、不要切 clientv2。
8. 测试禁止用固定长 Sleep 代替同步点。

## 验证

```bash
go test ./...
go test -race . ./pkg/websocket ./pkg/grpcstream ./pkg/quicstream
```

对照规格书 §9 测试表与 §10 清单自检。

## 完成报告

- 改动文件列表
- §10 每条 过/失败 + 证据
- 测试命令与结果
- 偏离（应无）
````
