# PR-KA-A0 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要、不要改字段号。做完后把完成报告交回主 agent 做严格验收。

可与 PR-KA-A1 并行（不同文件）。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

## 任务

独立实现 **PR-KA-A0**。唯一规格书（必须先通读再改代码）：

`docs/v2/tasks/pr-ka-a0-protocol.md`

背景（只读，不要自行改设计）：`docs/v2/kernel-architecture.md` 的 Protocol 节与 KD-K31。规格书与设计冲突时 **以规格书为准**。

## 目标（一句话）

新增 `messageloop.{shared,client,server}.v2` proto 并 `buf generate`。只加 v2 与生成物，不改运行时，不删不改 v1。

## 硬约束

1. 字段号必须与规格书 §4 表完全一致。禁止改号、挪号、合并字段。
2. 只许改规格书 §2 列出的路径。禁止改 `protocol/**/v1/**`、`client.go`、`hub.go`、`node.go`、`sdks/go/**`（除生成物）、`sdks/ts/src/client/**`。
3. 禁止把任何现有 Go/TS 运行时代码的 import 从 v1 改成 v2。
4. `Connected` 禁止带 publications / recover_results。禁止定义 `RecoverResult`。
5. `Subscription` 用 `Position cursor` + `fresh`，禁止 v1 那种标量 offset/epoch。
6. `PresenceInfo.client_id` 语义是设备/端，不是 session。
7. `Error` 注释禁止 `ACL_DENIED`。
8. 不做 git commit / tag / push。
9. 改动最小化，不顺手重构。

## 生成

```bash
task generate-protocol
```

若环境没有 task：先读 `Taskfile.yml` 的 `generate-protocol`，在仓库根执行 `buf generate`。生成物必须写入工作区。

## 验证（你必须自己跑）

```bash
go build ./...
go test ./...
```

对照规格书 §7 逐条自检。

## 完成报告（交回时必须包含）

- 改动文件列表
- generate 命令与是否成功
- §7：每条 过/失败 + 证据（文件:行 或 diff 摘要）
- `go test ./...` 结果
- 任何偏离规格的地方（应无）

不要实现恢复流、fencing、LiveBus、Authorizer 或切换运行时到 v2。那些是后续 PR。
````
