# PR-KA-D1 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

当前分支应为 `v2`（已含 A0–A4、B1–B4、C1–C6；C6 tip 为 `08e8a4c`，其后有 deflake 提交 `1cf7e51`）。

## 任务

独立实现 **PR-KA-D1**（转正收口：文档对齐 + 死代码清理）。唯一规格书（必须先通读再动手）：

`docs/v2/tasks/pr-ka-d1-graduation-docs.md`

背景（只读）：`docs/v2/kernel-architecture.md`、`docs/deployment.md:149-151`（HMAC 密钥的既有文档表述，保持口径一致）。

先读这些再动手：

- 规格书 §3 列出的每一处现状（行号已给，但先以实际文件为准核对）
- `config/config.go:44-85`（`ClusterConfig` HMAC 两键与 `ResolveHMACKey` 三个报错原文）
- `config/config.go:125-150`（`AuthorizerConfig` / `AuthorizerRule` 字段，README 示例要用）
- `session.go:190-260`（`sendQueue` 全貌：确认 `enqueue` 零调用、`notFull` 仍被 `tryEnqueue`/`dequeue`/`close` 使用）
- `configs/test.yaml`（新示例文件的风格基准）

## 目标（一句话）

公共文档与 v2 行为对齐（protocol.md / README / 配置文档 / 新增集群示例 / 靶心残留键形），并删除 `session.go` 的死方法 `sendQueue.enqueue`；零行为改动。

## 硬约束

1. 只许改规格书 §2 路径。
2. 除删 `session.go:208-231` 死方法外，不改任何 `.go` 文件；不改测试、proto、SDK。
3. 文档语义必须准确，照规格书 §3 给的字段语义与报错原文写，不得凭印象编配置字段或默认值。
4. `configs/cluster-example.yaml` 不得包含真实密钥，只放占位路径与注释。
5. `docs/v2/kernel-architecture.md` 只许动「三把时钟」表三个单元格 + Document History 追加一行；增量表与 README 索引由主 agent 负责。
6. `docs/design`、`docs/review`、`docs/archive`、`docs/v2/tasks/` 下既有规格保持原样。
7. 不做 git commit / tag / push。
8. 注意保持文件行尾与缩进风格，不产生格式 churn（终验会对照 `git diff --numstat` 与 `git diff --ignore-all-space --ignore-cr-at-eol --numstat`）。

## 验证

按规格书 §4 的命令逐条执行。`go build ./...` 与 `go test -count=1 .`（根包）必须绿；grep 门禁逐条贴真实输出。不需要跑全仓 `go test ./...`；若跑，必须串行。

对照规格书 §5 验收清单自检。

## 完成报告

- 改动文件列表（含新增）
- §5 每条 过/失败 + 证据
- 测试命令与结果（真实输出）
- 偏离（应无）

另外：实现完成后，把实现备注填入规格书 `docs/v2/tasks/pr-ka-d1-graduation-docs.md` §7。
````
