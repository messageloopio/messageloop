# 任务书 04：TS SDK 协议对齐（订阅 token / PublishAck / survey / SubRefresh）

## 角色

你是 MessageLoop（项目根 `D:/Codes/qiulin/messageloop`）的实现工程师，负责 TypeScript SDK（`sdks/ts/`）的协议能力补齐。本任务实现 backlog 的 B3-TS（决策已定，见 `docs/review/backlog.md`）。

## 背景与现状

协议（`protocol/client/v1/service.proto`）与 SDK 内 converters 层已具备基础，但 client 公共 API 未暴露：

- 订阅级 token：`Subscription.token`（service.proto:75）存在，`createSubscribeMessage` 目前写死 `token: ""`（`sdks/ts/src/message/converters.ts:106` 附近；connect 自动订阅路径 converters.ts:67-80 已支持 token）；`client.subscribe(...channels)`（client.ts:661-669）只收频道名。
- PublishAck：converters 已解码 `publishAck` 信封（converters.ts:248-249），但 `client.publish`（client.ts:690-697）fire-and-forget 返回 `Promise<void>`，无人消费 ack。
- survey：converters 已解码 `surveyRequest`/`surveyReply`（converters.ts:258-261），client 没有 `onSurvey` 处理器也没有发送 `surveyReply` 的 API。
- SubRefresh：`createSubRefreshMessage` 已存在（converters.ts:189-201，支持每频道 token），client 没有 `subRefresh()` 公共方法。

参照系：Go SDK 已有对应能力（`sdks/go/client.go:860-930` 的 `OnSurvey`/`SendSurveyReply`/`SubRefresh`，默认行为是未设置 handler 时把请求 payload 原样回显）。TS 侧语义须与 Go 侧对齐，但 API 风格遵循 TS 惯例（options 对象、Promise）。

## 文件归属（只许改这些）

- `sdks/ts/**`（src、测试、README、类型注释）

服务端与协议文件一律不动；若发现协议/服务端缺口，只在报告中列出。

## 任务 1：订阅级 token

1. 扩展 `subscribe` 支持每频道 token——建议 options 形式（如 `subscribe(...channels: (string | { channel: string; token?: string })[])` 或新增 `subscribeWith(channel, { token })`，选择与 SDK 现有风格最贴合的一种；`unsubscribe` 与内部 `subscribedChannels` 簿记同步适配）。
2. 重连自动重订阅（connect 的 autoSubscribe 链路已支持 token）时 token 必须保持——核查 client.ts 重连重订阅使用的数据源，确保 token 不丢。

## 任务 2：PublishAck 消费

1. 新增 `publishWithAck(channel, msg, options?: { transient?: boolean; timeout?: number }): Promise<{ id: string; offset: bigint }>`：以消息 id 为键挂 pending，ack 到达 resolve，超时/断连 reject 并清理。
2. `publish` 现有签名与行为不变（fire-and-forget）。
3. pending 清理由断连路径统一兜底（参照 `pendingRPC` 在 client.ts:637-640 的清理模式）。

## 任务 3：survey 应答

1. `onSurvey(handler)`：注册 survey 请求处理器；收到 `surveyRequest` 信封时调用，handler 返回的 Message 作为 `surveyReply` 回发（带 `request_id`）；handler 抛错时回发带 error 的 reply。
2. 未注册 handler 时的默认行为与 Go 侧对齐：payload 原样回显。
3. `sendSurveyReply` 可不单独暴露（被 onSurvey 内部复用），若暴露须与 Go 语义一致。

## 任务 4：SubRefresh

1. 新增 `subRefresh(...channels: string[])`（或 options 形式带 token），复用 `createSubRefreshMessage`；`subRefreshAck` 到达无需特殊处理（与 Go 一致）。

## 测试要求

- 每项能力配 fake transport 层测试：出站消息字段断言（token）、ack resolve/超时 reject/断连清理、survey 默认回显与自定义 handler、subRefresh 消息构造。
- 通过：`cd sdks/ts && npm test && npm run build`。
- 文档：`sdks/ts/README.md` 补新 API；类型注释完整（TSDoc）。

## 纪律

- 不做任何 git 写操作；改动最小化；与既有代码风格一致。
- 报告格式：完成项清单（file:line 证据）、行为变更显著标注、测试验证方式与结果、遗留问题。
