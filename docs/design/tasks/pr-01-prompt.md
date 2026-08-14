# PR-01 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要、不要改字段号。做完后把 agent 的完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

## 任务

独立实现 **PR-01**。唯一规格书（必须先通读再改代码）：

`docs/design/tasks/pr-01-protocol.md`

背景设计（只读、不要自行改设计）：`docs/design/v1.0-platform-gaps.md` 的「API / Interface Changes」与缺口 1–5 的协议提案。规格书与设计冲突时 **以规格书为准**。

## 目标（一句话）

把 v1.0 协议字段号一次性写进 proto 并 `buf generate`。只加字段和生成物，不改任何运行时行为。

## 硬约束

1. 字段号必须与规格书 §4 表完全一致。禁止改号、挪号、合并字段。
2. 只许改规格书 §2 列出的路径。禁止改 `client.go`、`hub.go`、`node.go`、`sdks/go/**`（除生成物）、`sdks/ts/src/client/**`。
3. 禁止新增 gRPC RPC，禁止 `DisconnectUser` / `SubscribeUser`。
4. `reserved 13`（Inbound）与 `reserved 16`（Outbound）必须出现在对应 message 上、oneof 之外。
5. 客户端 `SurveyResult` 定义在 `messageloop.client.v1`，禁止引用 `server.v1.SurveyResult`。
6. 复用已有 `Ping` / `Pong` 空 message，不要新建同名类型。
7. 不做 git commit / tag / push。
8. 改动最小化，不顺手重构。

## 生成

```bash
task generate-protocol
```

若环境没有 task：先读 `Taskfile.yml` 的 `generate-protocol`，用仓库根的 `buf generate`。生成物必须写入工作区（`shared/genproto/**`、`sdks/ts/src/proto/**`）。

## 验证（你必须自己跑）

```bash
go build ./...
go test ./...
cd sdks/go && go test ./...
```

TS 至少保证现有 `cd sdks/ts && npm test` 不因本 PR 红（不要去改 client 业务来“适应”新类型，除非是生成物 import 破裂这种纯类型问题）。

对照规格书 §7 十二条清单逐条自检。

## 完成报告（交回时必须包含）

- 改动文件列表
- generate 命令与是否成功
- §7 十二条：每条 过/失败 + 证据（文件:行 或 diff 摘要）
- `go test ./...` 结果
- 任何偏离规格的地方（应无）

不要实现恢复、presence、Survey 业务、心跳或按 user。那些是后续 PR。
````
