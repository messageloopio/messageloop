# PR-KA-D9 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师，由主 agent 委派的子代理。项目根目录：

D:\Codes\qiulin\messageloop

当前分支应为 `v2`（已含 A0–C6、D1–D8）。

## 任务

独立实现 **PR-KA-D9**（双进程黑盒 e2e：真服务器 × 真 SDK）。唯一规格书（必须先通读再动手）:

`docs/v2/tasks/pr-ka-d9-e2e.md`

规格书 §3 已含全部现状（cmd/server 入口与配置键形、SDK API 位置、admin 合同、CI 接线事实、双平台注意事项），动手前按 §3 核对现码。

## 目标（一句话）

在 `sdks/go` 模块新增黑盒冒烟 e2e：测试 `go build` 出真实 `cmd/server` 子进程并用真实 Go SDK 过 socket 跑通 WS 全流程、历史回放、gRPC 传输、admin gRPC 与（有 Redis 时的）Redis 变体。

## 硬约束

1. 只许改规格书 §2 路径。服务端/根包源码、SDK 生产代码、ci.yml、proto/生成物一律不碰。
2. 冒烟若炸出真实 bug，**停下来在完成报告里报告**，不许顺手修服务端。
3. 禁止固定长 Sleep；子进程 `t.Cleanup` 必杀；端口抢占式分配、零硬编码。
4. 测试串行，绝不并发两个根目录 `go test`；Redis 用真实实例（127.0.0.1:6379）。
5. 不做 git commit / tag / push；不产生格式 churn（终验对照 `git diff --numstat` 与 `--ignore-all-space --ignore-cr-at-eol`)。
6. 工作区 .go 文件约定 CRLF 行尾，新测试文件同样用 CRLF。
7. Windows 本地 + Linux CI 双平台都要能跑（二进制 `.exe`、路径、`cmd.Dir`)。

## 验证

按规格书 §5 逐条执行并贴真实输出（含 Redis 变体 PASS 非 SKIP 的证据、3 连跑）。

## 完成报告（作为你的最终回复全文返回，不要只写进文件）

- 改动文件列表（新增/删除/修改分组）
- §6 每条 过/失败 + 证据
- 测试命令与结果（真实输出）
- 偏离（应无）

另外：实现完成后，把实现备注填入规格书 `docs/v2/tasks/pr-ka-d9-e2e.md` §8。

完成后把完成报告全文作为最终回复返回给主 agent。
````
