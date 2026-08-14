# PR-10 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

本 PR 依赖已合入的 PR-01–PR-09。在当前 `main` 上开做。只改文档 + 补集群 e2e。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

## 任务

独立实现 **PR-10**。唯一规格书（必须先通读再改代码）：

`docs/design/tasks/pr-10-docs.md`

背景设计（只读）：`docs/design/v1.0-platform-gaps.md` PR Plan PR-10、缺口验收总表。规格书与设计冲突时 **以规格书为准**。

动手前先读（只读，核对过时句）：

- 根 `README.md`「Presence And History」
- `docs/developer/01-architecture.md` §3.5 Presence、§3.6 Survey、§3.7 ACL、图 (d)(e)
- `docs/developer/05-observability.md` §3 指标表
- `metrics.go`（文档漏列的 Counter/Histogram）
- `node.go` `emitPresence`、`hub.go` broadcast 的 presence 分支、`acl.go` `CanSurvey`、`client.go` `handleSurvey`
- `cluster_redis_integration_test.go`：`requireClusterRedis`、`TestAdmin_DisconnectUsersAcrossNodes`、`TestPresence_ClusterEmitRedisExactlyOne`、`respondToSurvey`
- `survey_test.go` 客户端 Survey 测（禁止再 inbound echo）

## 目标

文档与 v1.0 实现一致。补齐规格 §5 里还没有的集群 e2e。

## 硬约束

1. 只许改规格书 §2 列出的路径。禁止改 proto、SDK、服务端业务行为。
2. 不要把 `__presence` 伴生频道再写成默认路径。
3. 不要把客户端 Survey 写成 echo。
4. ACL 不要再写 `path.Match`。
5. 新 e2e 无 Redis 必须 Skip。禁止一个巨型串测。
6. 客户端 Survey e2e：读 outbound SurveyRequest.request_id 再 Reply。禁止 inbound SurveyRequest 喂回 HandleMessage。
7. 不要改 `TestClusterRedis_SurveyAggregatesAcrossNodes` 去「修 flake」。
8. 不要 git commit / push。
9. 不要改默认 idle=300s / ping_interval=0。

## 验证（你必须自己跑）

```bash
go test -count=1 -timeout 180s -run "TestAdmin_DisconnectUsersAcrossNodes|TestPresence_ClusterEmit|TestSubscribe_RecoverRedisHistory|TestClientSurvey_AggregatesAcrossRedisNodes" .
go test -count=1 .
cd sdks/go && go test -count=1 .
cd sdks/ts && npm test
```

对照规格书 §5 / §7 逐条自检。用搜索确认过时句子已改。

## 完成报告（交回时必须包含）

- 改动文件列表
- 每处过时句子：旧 → 新（文件:节）
- §5 四条：已有 / 新增 / Skip
- §7 八条：过/失败 + 证据
- go test / npm test 摘要
- 偏离与理由

不要改 proto、不要改 SDK、不要改服务端 Survey/Presence 行为。
````
