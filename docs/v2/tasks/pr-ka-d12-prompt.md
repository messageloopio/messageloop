# PR-KA-D12 第三方实现 Prompt(整段复制即可)

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop(Go 实时消息平台)的实现工程师,由主 agent 委派的子代理。项目根目录:

D:\Codes\qiulin\messageloop

当前分支应为 `v2`(D11 已合,tip `7dc4ee3`)。工作区应为干净状态。

## 任务

独立实现 **PR-KA-D12**(KD-K26 阶段二:authz/channel 下沉 + transport 改名 + admin 剥离)。唯一规格书(必须先通读再动手):

`docs/v2/tasks/pr-ka-d12-packages.md`

规格书 §3 已含全部现状核实(导出手术最小集、粘连点、import 更新点全集、测试面逐个判定),动手前按 §3 核对现码;行号若漂移,以语义定位为准。

## 目标(一句话)

`channel_policy.go`→`internal/channel`、`authorizer.go`→`internal/authz`(最小导出手术);`pkg/websocket`→`pkg/transport/ws`、`pkg/grpcstream`→`pkg/transport/grpc`、`pkg/quicstream`→`pkg/transport/quic`;grpcstream admin 面剥到 `internal/admin`(底座留 transport/grpc,导出 `PrepareServer`/`AdminAuthInterceptor`)。

## 硬约束

1. 只许改规格书 §2 路径。proto、`shared/`、`sdks/`、`_examples/`、`config/`(仅注释可动)、`pkg/topics`、`internal/cluster/`、`internal/{protocol,occupancy,stream}`、`pkg/redisbroker` 零改动。
2. 一律 `git mv`;除 §3.1/§3.3 授权导出手术、package/import 行、限定符改名外,搬动文件逐字节不变。常量/码表值不变。
3. 导出手术仅限规格列明项:`CompiledPolicySpec`(不透明,字段保持未导出)/`CompilePolicySpec`/`Overlay`/`DecideSubscribeSkipAllowLists`(入 `Authorizer` 接口)/`PrepareServer`/`AdminAuthInterceptor`。`Authorizer` 接口只加这一个方法。
4. 根 `aliases.go` 加 authz/channel 两组转发,根包与 cmd/server 的 authz/channel_policy 引用点零改动;新位置生产代码(internal/admin/authz/channel)直引 internal/*,不准引根 alias 的已下沉符号。
5. `pkg/transport/grpc` 内 `google.golang.org/grpc` 统一 alias 为 `googlegrpc`;旧 transport 路径全灭。
6. 测试串行,绝不并发两个根目录 `go test`;Redis 用真实实例(127.0.0.1:6379,DB 14)。
7. 不做 git commit / tag / push;不产生格式 churn;工作区 .go 文件 CRLF。
8. `golangci-lint run ./...` 必须保持 0 issues。

## 验证

按规格书 §5 逐条执行并贴真实输出(含三条门禁 grep)。

## 完成报告(作为你的最终回复全文返回,不要只写进文件)

- 改动文件列表(git mv 映射 / 新增 / 修改分组)
- §6 每条 过/失败 + 证据
- 导出手术逐符号清单(名字前→后)
- 测试搬运/拆分逐个判定表
- 测试命令与结果(真实输出)
- 偏离(应无)

另外:实现完成后,把实现备注填入规格书 `docs/v2/tasks/pr-ka-d12-packages.md` §9。

完成后把完成报告全文作为最终回复返回给主 agent。
````