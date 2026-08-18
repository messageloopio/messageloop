# PR-KA-D11 第三方实现 Prompt(整段复制即可)

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop(Go 实时消息平台)的实现工程师,由主 agent 委派的子代理。项目根目录:

D:\Codes\qiulin\messageloop

当前分支应为 `v2`(已含 A0–C6、D1–D10;D10 tip 为 `6f77006`)。工作区应为干净状态。

## 任务

独立实现 **PR-KA-D11**(KD-K26 包重划阶段一:叶子契约下沉 internal/*)。唯一规格书(必须先通读再动手):

`docs/v2/tasks/pr-ka-d11-packages.md`

规格书 §3 已含全部现状核实(五组文件出边、DefaultHistoryLimit 使用点、留根耦合清单、测试搬运规则、error_codes_test 注意点),动手前按 §3 核对现码;行号若漂移,以语义定位为准。

## 目标(一句话)

把 `disconnect.go`/`version.go` → `internal/protocol`、`interest.go` → `internal/channel`、`presence.go`/`presence_event.go`/`occupancy.go` → `internal/occupancy`、`broker.go`/`publication.go`/`broker_memory.go`+`DefaultHistoryLimit` → `internal/stream`;根包新增 `aliases.go` 集中过渡转发;`pkg/redisbroker` 对这五组符号改引新路径。零行为变化、零接口形状变化。

## 硬约束

1. 只许改规格书 §2 路径。`cmd/server`、`pkg/websocket`、`pkg/quicstream`、`pkg/grpcstream`、`proxy`、`config`、`shared/`、`sdks/`、`_examples/`、`pkg/topics`、`internal/cluster/`(hmac+sim)零改动——它们经根 alias 继续编译。
2. 一律 `git mv`;搬动文件除 package 行/import 块/`GenerationOK` 导出名外逐字节不变。接口形状(Broker/PresenceStore/OccupancyGenSource/SyntheticLeaveReporter/CompiledInterest/Disconnect 码表值)逐字节等价。
3. alias 集中根 `aliases.go` 一个文件,文件头注明「PR-KA-D11 过渡转发,D13 清除;新代码不准引根 alias」。
4. redisbroker 对五组符号只准引新路径,不准经根 alias;cluster 契约继续引根包。
5. 测试搬运按 §3.3 规则逐个判定,报告给表;`error_codes_test.go` 普查路径适配后必须仍绿。
6. 测试串行,绝不并发两个根目录 `go test`;Redis 用真实实例(127.0.0.1:6379,DB 14)。
7. 不做 git commit / tag / push;不产生格式 churn(终验对照 `git diff --numstat` 与 `--ignore-all-space --ignore-cr-at-eol`);工作区 .go 文件 CRLF(搬动文件保持 CRLF)。
8. `golangci-lint run ./...` 必须保持 0 issues。

## 验证

按规格书 §5 逐条执行并贴真实输出(含两条门禁 grep)。

## 完成报告(作为你的最终回复全文返回,不要只写进文件)

- 改动文件列表(git mv 映射 / 新增 / 修改分组)
- §6 每条 过/失败 + 证据
- 测试搬运逐个判定表(搬/留 + 理由)
- 测试命令与结果(真实输出)
- 偏离(应无)

另外:实现完成后,把实现备注填入规格书 `docs/v2/tasks/pr-ka-d11-packages.md` §9。

完成后把完成报告全文作为最终回复返回给主 agent。
````