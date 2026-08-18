# PR-KA-D8 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师，由主 agent 委派的子代理。项目根目录：

D:\Codes\qiulin\messageloop

当前分支应为 `v2`（已含 A0–C6、D1–D7；D7 tip 为 `2635cf1`）。

## 任务

独立实现 **PR-KA-D8**(CI 修复：v2 分支触发 + Redis service + 子模块/TS 覆盖 + buf 工具链固定）。唯一规格书（必须先通读再动手）:

`docs/v2/tasks/pr-ka-d8-ci.md`

规格书 §3 已含全部现状（ci.yml 结构、模块与 Go 版本、本机工具链、Redis 测试 helper 形态）,§1.5 有 buf 版本决策规则，动手前按 §3 核对现码。

## 目标（一句话）

让 CI 在 v2 分支上跑真实验证面：挂 Redis 让集成测试不再空心 skip,覆盖 shared/sdks-go/chatroom/TS 四个根测试够不到的模块，并把 buf 版本在 CI 与 Taskfile 钉死（顺带刷新 swagger 旧码表残留）。

## 硬约束

1. 只许改规格书 §2 路径。不改任何 Go 源码、proto、测试、SDK 手写代码。
2. buf 版本决策严格按 §1.5：先试 v1.63.0 零 diff 则钉它；否则用 v1.65.0 全量重生成并把刷新产物纳入 PR。两个版本试出来的 diff 证据都要贴进完成报告。
3. 测试串行，绝不并发两个根目录 `go test`;Redis 用真实实例（127.0.0.1:6379)。
4. 不做 git commit / tag / push；不产生格式 churn。
5. 集成测试「真跑非 SKIP」要有 `-v` 输出证据。
6. 本 PR 验收含一段 push 后的 `gh run watch`（主 agent 执行）——你要做的是让本地复跑与 workflow 文本经得起审查。

## 验证

按规格书 §5 逐条执行并贴真实输出。

## 完成报告（作为你的最终回复全文返回，不要只写进文件）

- 改动文件列表（新增/删除/修改分组）
- §6 每条 过/失败 + 证据（§1.5 走了哪个分支、为什么）
- 测试命令与结果（真实输出）
- 偏离（应无）

另外：实现完成后，把实现备注填入规格书 `docs/v2/tasks/pr-ka-d8-ci.md` §8。

完成后把完成报告全文作为最终回复返回给主 agent。
````
