# PR-KA-D7 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

当前分支应为 `v2`（已含 A0–C6、D1–D6；D6 tip 为 `e303c3d`）。

## 任务

独立实现 **PR-KA-D7**（错误码收口：一份 well-known 码表）。唯一规格书（必须先通读再动手）:

`docs/v2/tasks/pr-ka-d7-error-codes.md`

规格书 §3 已含全量现状（表外码 6 个的精确发射点行号、SDK 依赖面、测试断言影响面、既有表内码清单），动手前按 §3 核对现码；行号若漂移，以语义定位为准。

## 目标（一句话）

`ACL_DENIED`→`PERMISSION_DENIED`、`ACL_ERROR`→`PROXY_ERROR` 四处发射点换名；`INTERNAL_ERROR`/`SURVEY_FAILED`/`SURVEY_ANSWER_TOO_LARGE`/`DISCONNECT_ERROR` 入表；errors.proto 注释扩为 19 码分组定稿表并与 docs/protocol.md 逐字一致；新增码表守护测试。

## 硬约束

1. 只许改规格书 §2 路径。errors.proto **只加注释**：字段、编号、wire 零变化（§5 末条 grep 必须零输出）。
2. `node.go`/`pkg/grpcstream/api_handler.go` 的 `SURVEY_FAILED` 发射点不动（入表不换名）；`hub.go`/`session.go` 零改动；capability 门禁零改动。
3. 集群命令总线内部码（`ErrorCode` 字段）与 SDK 自产码一律不动。
4. `task generate-protocol`（buf 本机可用）；生成物 diff 仅限 errors.proto 注释带来的 churn，其余 `git checkout --` 还原。
5. 测试串行，绝不并发两个根目录 `go test`；Redis 用真实实例（127.0.0.1:6379，DB 14）。
6. 不做 git commit / tag / push；不产生格式 churn（终验对照 `git diff --numstat` 与 `--ignore-all-space --ignore-cr-at-eol`）。
7. 工作区 .go 文件约定 CRLF 行尾；用 sed/脚本批量改动时别把行尾打成 LF。

## 验证

按规格书 §5 逐条执行并贴真实输出。

## 完成报告（作为你的最终回复全文返回，不要只写进文件）

- 改动文件列表（新增/删除/修改分组）
- §6 每条 过/失败 + 证据
- 测试命令与结果（真实输出）
- 偏离（应无）

另外：实现完成后，把实现备注填入规格书 `docs/v2/tasks/pr-ka-d7-error-codes.md` §8。

完成后把完成报告全文作为最终回复返回给主 agent。
````
