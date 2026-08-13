# 任务书 03：Go SDK 协议对齐（订阅 token / PublishAck / disconnect_code）

## 角色

你是 MessageLoop（项目根 `D:/Codes/qiulin/messageloop`）的实现工程师，负责 Go SDK（`sdks/go/`）的协议能力补齐。本任务实现 backlog 的 B3-Go + A2（决策已定，见 `docs/review/backlog.md`）。

## 背景与现状

协议（`protocol/client/v1/service.proto`，生成代码已在 SDK 内）早已支持，但 Go SDK 未暴露：

- `Subscription.token`（service.proto:75）——订阅级鉴权 token；
- `Publish.token`（service.proto:103）——发布级 token；
- `PublishAck`（service.proto:107-110）——服务端对发布的确认（id + offset），Go SDK 目前在 `sdks/go/client.go:395-397` 收到后直接忽略；
- 服务端 gRPC 传输在断开前会带内发送 `DISCONNECT_ERROR` 错误信封，数值断连码编码在信封 `metadata.disconnect_code`（服务端实现：`pkg/grpcstream/transport.go:145-168`）；WS 路径的断连码走 close frame，Go SDK WS 侧已有对应处理，gRPC 侧目前不解析该 metadata（`sdks/go/grpc.go:104-114` Recv 直通）。

Go SDK 已有：`SubRefresh`、`OnSurvey`/`SendSurveyReply`、`SubscribeWith`+`WithEphemeral`（client.go:604-636）——新能力须与这些既有风格一致。

## 文件归属（只许改这些）

- `sdks/go/**`（含 README、MIGRATION_GUIDE.md、测试、示例）

服务端与协议文件一律不动；若发现协议/服务端缺口，只在报告中列出。

## 任务 1：订阅级 token 透传

1. 新增 `WithToken(token string) SubscribeOption`，镜像 `WithEphemeral`（client.go:607-614）的写法，设置 `clientpb.Subscription.Token`。
2. 重连自动重订阅时 token 必须随订阅状态一起保存与恢复——参照 `isEphemeral`（client.go:696-702）的存储模式，`subscriptions map[string]bool` 需要扩展为能存 token 的结构（注意 `Unsubscribe` 构造回读 ephemeral 的既有用法，client.go:663-684，保持对称）。
3. `Publish` 的发布级 token：提供可选方式设置 `clientpb.Publish.Token`（如 `PublishWith` 选项模式或新选项参数，与 SDK 现有风格一致）；不改 `Publish` 现有签名行为。

## 任务 2：PublishAck 消费

1. 新增等待确认的发布方法（建议 `PublishWithAck(ctx context.Context, channel string, msg *Message, opts...) (offset uint64, err error)`）：以消息 `id` 为键挂 pending map，收到 `PublishAck`（client.go:395-397 的 case）后 resolve；ctx 超时/断连时 reject 并清理 pending。
2. 保持 `Publish`（fire-and-forget）行为不变。
3. pending map 的清理必须与断连/关闭路径对称（参照 SDK 内 pendingRPC 的既有清理模式）。

## 任务 3（A2）：gRPC 侧消费 `disconnect_code`

1. 解析错误信封 `metadata.disconnect_code`（structpb Number 值），映射为与 WS 路径一致的 typed disconnect（先看 Go SDK WS 侧如何暴露断连码，保持同一类型/接口）。
2. 解析失败/缺失 metadata 时保持现有行为（不因新代码引入新 panic 或新错误路径）。

## 测试要求

- 订阅 token：构造断言出站 `Subscribe` 消息含 token 的测试；重连重订阅 token 保持的测试。
- PublishAck：fake transport 下 ack 到达 resolve、超时 reject、断连清理三类测试。
- disconnect_code：构造带 metadata 的错误信封，断言得到的 disconnect 数值码与 WS 路径同型同值。
- 通过：`cd sdks/go && go build ./... && go test -race ./... && go vet ./...`。
- 文档：`sdks/go/README.md` 与 `MIGRATION_GUIDE.md` 补新 API 说明（新 API 非 breaking，归入新增能力一节）。

## 纪律

- 不做任何 git 写操作；改动最小化；与既有 SDK 风格（选项模式、注释密度）一致。
- 报告格式：完成项清单（file:line 证据）、行为变更显著标注、测试验证方式与结果、遗留问题。
