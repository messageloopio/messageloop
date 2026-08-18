# PR-KA-D10 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师，由主 agent 委派的子代理。项目根目录：

D:\Codes\qiulin\messageloop

当前分支应为 `v2`（已含 A0–C6、D1–D9；D9 tip 为 `8eab8a8`）。工作区应为干净状态。

## 任务

独立实现 **PR-KA-D10**（Hydrate 去 saga + 集群写路径原子性收口）。唯一规格书（必须先通读再动手）:

`docs/v2/tasks/pr-ka-d10-hydrate.md`

规格书 §3 已含全部现状（resume 调用链、saga 三写面、snapshot 写路径、Directory 实现清单、epoch 现状、测试缺口），动手前按 §3 核对现码；行号若漂移，以语义定位为准。

## 目标（一句话）

恢复段从「逐频道 saga + 失败删库 3502」改为「逐频道软失败 + 部分订阅存活 + RECOVER_FAILED 信封告知」;lease CAS 与 snapshot 写合成一次原子操作（Redis Lua，新可选接口 type-assert 接线）;同节点旧世代 resume 跳过注定失败的 takeover RPC。

## 硬约束

1. 只许改规格书 §2 路径。proto/生成物、`hub.go`/`session.go`、SDK 手写代码零改动。
2. **C1 sim 六场景语义与 CAS 四字段谓词形状是门禁**，原样保持。
3. §3.4 列出的每个 Directory 实现/fake 必须逐个点名核对（实现新接口或显式 fallback)，完成报告里给核对表。
4. hydrate 的 ACL 语义按 §1.1：不求值，作为显式决策写进 docs/developer/04-cluster.md。
5. 失败信封必须是 D7 码表内码（RECOVER_FAILED / recover_error),metadata 带 channel；不许造新码、不许动 proto。
6. 测试串行，绝不并发两个根目录 `go test`;Redis 用真实实例（127.0.0.1:6379,DB 14）。
7. 不做 git commit / tag / push；不产生格式 churn（终验对照 `git diff --numstat` 与 `--ignore-all-space --ignore-cr-at-eol`)；工作区 .go 文件 CRLF。
8. `golangci-lint run ./...` 必须保持 0 issues(D8 已清零，别引入新发现）。

## 验证

按规格书 §5 逐条执行并贴真实输出。

## 完成报告（作为你的最终回复全文返回，不要只写进文件）

- 改动文件列表（新增/删除/修改分组）
- §6 每条 过/失败 + 证据
- §3.4 Directory 实现逐点核对表
- 测试命令与结果（真实输出）
- 偏离（应无）

另外：实现完成后，把实现备注填入规格书 `docs/v2/tasks/pr-ka-d10-hydrate.md` §8。

完成后把完成报告全文作为最终回复返回给主 agent。
````
