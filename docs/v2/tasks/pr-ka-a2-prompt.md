# PR-KA-A2 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

当前分支应为 `v2`（已含 A0 proto、A1 fencing）。

## 任务

独立实现 **PR-KA-A2**。唯一规格书（必须先通读再改代码）：

`docs/v2/tasks/pr-ka-a2-history.md`

背景（只读）：`docs/v2/kernel-architecture.md` 的 StreamLog / Gap 合同、KD-K12、KD-K14、KD-K31。规格书与设计冲突时 **以规格书为准**。

先读这些现码再动手：

- `broker.go` `Broker` 接口与错误合同注释
- `broker_memory.go` `Publish` / `PublishTransient` / `History` / `Subscribe`
- `pkg/redisbroker/redis.go` `Publish`（XDel 分支）、`Subscribe` / `interested`
- `pkg/redisbroker/history.go` `getHistory`
- `recover.go` History 调用与 RecoverOK / Truncated
- `pkg/grpcstream/api_handler.go` `GetHistory`
- 各测试里的 `Broker` fake（改签名即可）

## 目标（一句话）

History 返回带可检测 gap 的页；XADD 成功后 PUBLISH 失败不得 XDel 且 Publish 对调用方成功；memory 按 Interest（精确+通配）调 handler，handler 失败不否定 Publish。

## 硬约束

1. 只许改规格书 §2 列出的路径，外加「因 History 签名而编译失败的 fake」。
2. 不要改 `PSubscribe`、不要切 `clientv2`、不要做流式 RecoverComplete、不要做稠密 seq。
3. `sinceOffset>0` 且空批不得 `HistoryGapNone`。
4. memory 只 `Subscribe("im.**")` 时 `Publish("im.room.1")` **必须** 调 handler。
5. Redis `Publish` 在 PUBLISH 失败时 **零** `XDel`，返回 `(offset, nil)`。
6. 不做 git commit / tag / push。不顺手重构 Hub / 集群 / proto。
7. 测试禁止用固定长 Sleep 代替同步点。

## 验证（你必须自己跑）

```bash
go test ./...
go test -race . ./pkg/redisbroker
```

对照规格书 §10 测试表与 §11 清单逐条自检。

## 完成报告（交回时必须包含）

- 改动文件列表
- §11 每条 过/失败 + 证据（文件:行）
- 测试命令与结果
- 任何偏离（应无）

不要实现 A3 LiveBus 编译、A4 Authorizer、B1 Session 拆分。
````
