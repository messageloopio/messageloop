# PR-KA-D2 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

当前分支应为 `v2`（已含 A0–C6 与 D1；D1 tip 为 `11bfa04`）。

## 任务

独立实现 **PR-KA-D2**（握手版本门）。唯一规格书（必须先通读再动手）：

`docs/v2/tasks/pr-ka-d2-version-gate.md`

背景（只读）：`docs/v2/kernel-architecture.md` KD-K31；`docs/v2/tasks/pr-ka-a0-protocol.md` §4.7（Connect 字段表）。规格书与设计冲突时**以规格书为准**。

先读这些现码再动手：

- `client.go:242-300`（`handleConnect`：插入点、AUTH_REQUIRED「先 Error 再断开」范式）
- `disconnect.go`（内置码 3500-3513 的写法）
- `protocol/shared/v2/errors.proto`（well-known 码注释清单）
- `testhelpers_test.go`（测试 helper；根包约 96 处 `clientpb.Connect{` 字面量要补 version，建议在此定义统一常量）
- `sdks/go/options.go:97`、`sdks/ts/src/client/options.ts:53`、`sdks/ts/src/client/client.ts:1470`、`sdks/ts/test/client.test.ts:162`

## 目标（一句话）

服务端在 `handleConnect` staging 之前校验 `Connect.version` 主世代（只认 2），不合格先回 `VERSION_UNSUPPORTED` Error 再以 3514 断开；双 SDK 默认 Version 升 `"2.0.0"`；文档同步。

## 硬约束

1. 只许改规格书 §2 路径。
2. proto 只允许 `errors.proto` 注释补码；改后跑 `task generate-protocol`（buf 1.65.0 本机可用），只保留 `types.pb.go` 注释级变化，swagger json 纯排序 churn 一律 `git checkout --` 还原。若 buf 远程插件不可达，还原 errors.proto 并在完成报告偏离节说明。
3. 版本门 fail-closed：空串/非数字/非 2 世代一律拒；拒绝发生在 session ID staging 之前。
4. 约 109 处既有测试 `Connect{}` 字面量显式补合法 v2 版本；不得删既有断言蒙混。
5. 不做 WS 子协议改名、不动 caps 语义、不动 `hub.go`/`session.go`/broker/cluster/sim/HMAC/SDK 消费路径/`_examples/`。
6. 不做 git commit / tag / push。
7. 测试串行执行，绝不并发两个根目录 `go test`；Redis 集成测试用真实 Redis（127.0.0.1:6379，DB 14，沿用 `requireCommandBusRedis` 机制）。禁止固定长 Sleep 等异步。
8. 不产生格式 churn（终验对照 `git diff --numstat` 与 `--ignore-all-space --ignore-cr-at-eol`）。

## 验证

按规格书 §5 逐条执行并贴真实输出（含 `go test -count=1 ./...` 全量、Go SDK、TS jest、各 grep 门禁）。

## 完成报告

- 改动文件列表（按生产/测试/SDK/文档分组）
- §6 每条 过/失败 + 证据
- 测试命令与结果（真实输出）
- 偏离（应无）

另外：实现完成后，把实现备注填入规格书 `docs/v2/tasks/pr-ka-d2-version-gate.md` §8。
````
