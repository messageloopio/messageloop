# PR-KA-D5 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

当前分支应为 `v2`（已含 A0–C6、D1–D4；D4 tip 为 `f021ebe`）。

## 任务

独立实现 **PR-KA-D5**(proxy 协议升 v2 + 拆 v1 桥 + 删死 v1 proto)。唯一规格书（必须先通读再动手）:

`docs/v2/tasks/pr-ka-d5-proxy-v2.md`

规格书 §3 已含完整现状探查（桥调用点、proxy 合约、死 proto 确认、内部模型、buf 机制），动手前按 §3 行号再核对一遍现码。

## 目标（一句话）

proxy 合约复刻为 `protocol/proxy/v2`(shared/v2 类型、wire 兼容）,`proxy/` 包与 Go SDK 切换，`client.go` 删三个 v1↔v2 桥函数，删除全死的 `client/v1`、`event/v1` proto 及全部 v1 生成物；shared/v1 收缩到 admin 白名单（D6 再清）。

## 硬约束

1. 只许改规格书 §2 路径；proto 字段号/消息形状零改动（proxy/v2 是逐字段复刻）。
2. **不做** admin 切 server/v2、不删 `protocol/server/v1` / `shared/v1` / admin 白名单文件（broker.go/publication.go 的 v1 twin、pkg/grpcstream admin 及其测试）——全部是 D6。
3. shared/v1→shared/v2 的切换是纯机械替换（Payload/Metadata/Error 两版字段号逐位相同），不得夹带任何逻辑改动。
4. `task generate-protocol`(buf 1.65.0 本机可用）；纯排序 churn 的无关 swagger/pb 文件 `git checkout --` 还原（参照 git log D2 提交 `83c7faa` 的先例）。
5. 测试串行，绝不并发两个根目录 `go test`;Redis 测试用真实 Redis(127.0.0.1:6379,DB 14)。禁止固定长 Sleep。
6. 不做 git commit / tag / push；不产生格式 churn（终验对照 `git diff --numstat` 与 `--ignore-all-space --ignore-cr-at-eol`)。
7. TS SDK 只动生成物目录（buf 输出），非生成物源码零改动。

## 验证

按规格书 §5 逐条执行并贴真实输出（全量 `go test ./...`、Go SDK、TS jest、三组 grep 门禁）。

## 完成报告

- 改动文件列表（新增/删除/修改分组）
- §6 每条 过/失败 + 证据
- 测试命令与结果（真实输出）
- 偏离（应无）

另外：实现完成后，把实现备注填入规格书 `docs/v2/tasks/pr-ka-d5-proxy-v2.md` §8。
````
