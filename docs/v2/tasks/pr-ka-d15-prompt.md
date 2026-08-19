# PR-KA-D15 第三方实现 Prompt(整段复制即可)

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop(Go 实时消息平台)的实现工程师,由主 agent 委派的子代理。项目根目录:

D:\Codes\qiulin\messageloop

当前分支应为 `v2`(D14 已合,tip `6e17bdb`)。工作区应为干净状态。

## 任务

独立实现 **PR-KA-D15**(KD-K26 收口:Node/Cluster 门面下沉 internal/runtime + 清除根 alias)。唯一规格书(必须先通读再动手):

`docs/v2/tasks/pr-ka-d15-runtime.md`

规格书 §1.2 已裁定 D14 留下的三道未决题;§3 含搬运集、常量拆分、消费方切换、测试迁留。动手前按 §3 核对现码;行号若漂移,以语义为准。

## 目标(一句话)

根包剩余编排(Node、Cluster 门面、recover、health、saga、Sim 钩子、session_runtime)→ `internal/runtime`;cmd/server、三 transports、admin、sim 改直引;删除 `aliases.go`/`marshaler.go`;根包只留无导出 `doc.go`。

## 硬约束

1. 只许改规格书 §2 路径。`shared/`、`sdks/`、`_examples/`、`config/`、`pkg/topics`、`pkg/redisbroker`、`proxy/`、`protocol/`、`internal/{protocol,channel,authz,cluster/hmac}` 零改动。`internal/occupancy`/`survey`/`stream` 仅允许 §2.3 授权新文件。
2. 整文件 `git mv`;迁入文件除 package/import、§3.3 包装改直引、可选本地 alias 外逐字节不变。常量值/JSON tag 不变。
3. **不建** `internal/rpc`;**不把** `recover.go` 并进 stream;**不把** survey 扇出搬进 `internal/survey`。
4. 锁序不变量保持;Sim 三条导出函数语义不变。
5. 新位置(internal/runtime)零根包引用。消费方生产+测试都不再 `import "github.com/messageloopio/messageloop"`(根已无符号)。
6. 根同包测试随迁 `package runtime`,避免再补跨包导出(D14 教训)。`error_codes_test.go` 按 §3.6 推荐留根改 `package messageloop_test`。
7. 测试串行,绝不并发两个根目录 `go test`;Redis 真实实例(127.0.0.1:6379,DB 14,容器 `messageloop-test-redis`)。已知 flake 同 D14 backlog #8。
8. 不做 git commit / tag / push;无格式 churn;`.go` 保持 CRLF。
9. `golangci-lint run ./...` 必须 0 issues。

建议落地顺序:mv + runtime 自洽编译 → 切消费方 → 删 aliases/marshaler/defaults → 写根 doc.go → §5。

## 验证

按规格书 §5 逐条执行并贴真实输出(含五条门禁)。

## 完成报告(作为你的最终回复全文返回)

- 改动文件列表(git mv / 删除 / 新增 / 消费方)
- §6 每条 过/失败 + 证据
- 包装函数直引对照
- 测试迁/留/删计数
- §5 命令真实输出
- 偏离

另外:把实现备注填入 `docs/v2/tasks/pr-ka-d15-runtime.md` §9。
````
