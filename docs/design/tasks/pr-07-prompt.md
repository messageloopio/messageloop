# PR-07 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

本 PR 依赖已合入的 PR-01（Survey 字段）、PR-02（策略 `survey`）、PR-04a（`sessionCoversChannel`）。在当前 `main` 上开做。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

## 任务

独立实现 **PR-07**。唯一规格书（必须先通读再改代码）：

`docs/design/tasks/pr-07-survey.md`

背景设计（只读）：`docs/design/v1.0-platform-gaps.md` 缺口 5、KD-5、KD-6、KD-15。规格书与设计冲突时 **以规格书为准**。

动手前先读：

- `client.go` `handleSurvey`（约 1448，现在是 echo）
- `client.go` `sessionCoversChannel`、`handleSurveyReply`
- `node.go` `Survey` / `localSurvey` / `sendSurveyRequest`（不填 channel）
- `cluster_commands.go` `handleClusterSurveyCommand`
- `acl.go` `CanSubscribe`（照这个写 `CanSurvey`，但默认 deny）
- `channel_policy.go` `Survey` 默认 false
- `survey_test.go` 里把 inbound SurveyRequest 喂回 HandleMessage 的 echo 测
- `cluster_redis_integration_test.go` Survey 集成测

## 目标

客户端 SurveyRequest 走 Node.Survey，异步回 SurveyResult。废除 echo。读循环不得 Wait。默认关。集群先 count_only 预检。

## 硬约束

1. 只许改规格书 §2 列出的路径。禁止改 proto、SDK、Admin Survey 语义。
2. **禁止在 handleSurvey 里调用 Survey.Wait / 阻塞读循环。**
3. 无 channel / 通配 pattern → BAD_REQUEST。不要 echo。
4. 默认 `ChannelPolicy.Survey==false` → SURVEY_DISABLED。`CanSurvey` 默认拒绝。
5. 未 sessionCoversChannel → PERMISSION_DENIED。禁止只用 CanSubscribe。
6. 超 MaxSurveySubscribers：零条 outbound SurveyRequest。Admin 不加这道门。
7. count_only 不得调用 localSurvey。
8. sendSurveyRequest 必须填 channel。
9. 不要改 NewNode 签名。不要 git commit / push。
10. 改写 echo 测试：读 outbound SurveyRequest.request_id，再发 inbound SurveyReply。

## 验证（你必须自己跑）

```bash
go test -count=1 . ./config/...
go test -race -count=1 .
```

对照规格书 §8 测试和 §10 八条逐条自检。

## 完成报告（交回时必须包含）

- 改动文件列表
- handleSurvey / worker / CanSurvey / count_only（文件:行）
- §8 每个测试：过/失败
- §10 八条：过/失败 + 证据
- go test 摘要
- 偏离与理由

不要实现 SDK Survey()、不要改 proto、不要改 Admin Survey 上限。
````
