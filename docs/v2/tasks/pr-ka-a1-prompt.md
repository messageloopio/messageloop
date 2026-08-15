# PR-KA-A1 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

可与 PR-KA-A0 并行（A1 不改 proto）。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

## 任务

独立实现 **PR-KA-A1**。唯一规格书（必须先通读再改代码）：

`docs/v2/tasks/pr-ka-a1-fencing.md`

背景（只读）：`docs/v2/kernel-architecture.md` 的 Cluster / Bind 节、KD-K4、KD-K5、KD-K30、KD-K31。规格书与设计冲突时 **以规格书为准**。

先读这些现码再动手：

- `cluster_state.go`：`syncClusterSessionState`、`clusterSessionLease`、`deleteClusterSessionState`
- `cluster_resume.go`：`resumeRemoteSession`、`requestSessionTakeover`
- `pkg/redisbroker/cluster_directory.go`：`PutSessionLease`、`CompareAndSwapSessionLease`、`clusterSessionLeaseEqual`
- `client.go`：`throttledClusterRefresh`、`handlePing`、`handlePong`
- `node.go`：`AddClient` 里对 `syncClusterSessionState` 的调用
- `cluster.go`：`SessionDirectory` 接口

## 目标（一句话）

热路径禁止 `PutSessionLease`。续约用 same-fence CAS；首次创建用 CAS(nil)；Directory 已是别人则 `ErrSessionFenced` 并在 ping 路径断开；跨节点 resume 在活节点 takeover 失败时回滚 CAS。

## 硬约束

1. 只许改规格书 §2 列出的路径。
2. `syncClusterSessionState` 内禁止调用 `PutSessionLease`。
3. 刷新 **不得** `LeaseVersion++`。升版本只留在现有 `resumeRemoteSession` 抢权。
4. 死节点旁路必须保留（takeover 失败且旧 node lease 已无 → 不回滚、继续 resume）。
5. fenced 时禁止 `deleteClusterSessionState`（不要误删新 owner）。
6. 独立版本：不必兼容「仍在盲写 Put 的旧二进制」，也不要为它们留开关。
7. 不做 git commit / tag / push。不顺手重构 Hub / Broker / proto。
8. 测试禁止用固定长 Sleep 代替同步点。

## 验证（你必须自己跑）

```bash
go test ./...
go test -race .
```

对照规格书 §6 测试表与 §8 清单逐条自检。

## 完成报告（交回时必须包含）

- 改动文件列表
- §8 每条 过/失败 + 证据（文件:行）
- `go test ./...` 与 `go test -race .` 结果
- 任何偏离（应无）

不要实现 Session 拆分、v2 协议切换、HMAC 命令总线、LiveBus 或 Authorizer。
````
