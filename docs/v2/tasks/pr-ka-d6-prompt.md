# PR-KA-D6 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

当前分支应为 `v2`（已含 A0–C6、D1–D5；D5 tip 为 `847c32a`）。

## 任务

独立实现 **PR-KA-D6**(admin 切 server/v2 + 清除 shared/v1，移除 v1 收尾）。唯一规格书（必须先通读再动手）:

`docs/v2/tasks/pr-ka-d6-admin-v2.md`

规格书 §3 已含完整现状（v1↔v2 proto 差异全量、handler 行号、shared/v1 残留清单、Epoch 访问器范式、两条 backlog 细节），动手前按 §3 核对现码。

## 目标（一句话）

admin gRPC 面切到已生成的 server/v2(PresenceInfo/GetHistory/HistoryPublication 三处形状重做，其余机械）,删光 server/v1 与 shared/v1 及其生成物，修复 chatroom e2e 编译错与一个 Redis 测试 flake；此后全仓零 v1 proto。

## 硬约束

1. 只许改规格书 §2 路径。**不改任何 proto 文件**(server/v2 形状已冻结；发现表达不了所需语义就停下来报告，不要自作主张）。
2. capability 门禁（`requireAdminCaps`/`AdminDecide`）与 `Broker.History` 签名零改动；`client.go`/`node.go`/`hub.go`/`session.go` 零改动。
3. flake 修复必须有根因；禁止固定长 Sleep；若根因指向 `pubsub.go` 生产代码，先报告主 agent 再动。
4. `task generate-protocol`(buf 本机可用）；无关生成物 churn 照 D5 先例 `git checkout --` 还原。
5. 测试串行，绝不并发两个根目录 `go test`;Redis 用真实实例（127.0.0.1:6379,DB 14)。
6. 不做 git commit / tag / push；不产生格式 churn（终验对照 `git diff --numstat` 与 `--ignore-all-space --ignore-cr-at-eol`)。

## 验证

按规格书 §5 逐条执行并贴真实输出（全量 `go test ./...`、Go SDK、TS jest、chatroom build、flake 5 连跑、grep 门禁、v1 目录 ls)。

## 完成报告

- 改动文件列表（新增/删除/修改分组）
- §6 每条 过/失败 + 证据
- 测试命令与结果（真实输出）
- 偏离（应无）

另外：实现完成后，把实现备注填入规格书 `docs/v2/tasks/pr-ka-d6-admin-v2.md` §8。
````
