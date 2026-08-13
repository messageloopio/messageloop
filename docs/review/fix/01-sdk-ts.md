# 修复任务 01：TypeScript SDK

## 背景

你要修复的项目是 **MessageLoop**（工作目录：`D:/Codes/qiulin/messageloop`），Go 实时消息平台，你的范围是其 TypeScript SDK（`sdks/ts/`，WebSocket 客户端，浏览器+Node 双环境）。

刚完成一轮全项目代码评审，评审发现已经过主 agent 逐条核实（无一条被推翻）。完整方案见 `docs/review/fix-plan.md`（可读），你的任务范围如下。**先读 `sdks/ts/src/` 下相关代码再动手。**

## 文件归属（严格）

- 你拥有：`sdks/ts/` 全部（含 `src/`、`test/`、`package.json`）。
- 禁止修改：仓库其他任何目录（服务端 Go 代码、Go SDK、docs/ 由其他 agent 并行处理）。
- 例外：`docs/review/fix-plan.md` 只读。

## 任务清单

### P0（必修）

1. **pong 超时永久杀死客户端**（`src/client/client.ts:468-471`）：`sendPing` 的 pong 超时回调先 `handleError()`（已调度重连）再 `close()`，`close()` 清掉重连定时器并置 `isClosedFlag=true`，一次网络抖动即永久断开。改为走 `handleDisconnect()` 路径（不置 closed flag），保活重连。
2. **`recv()` 无法传播 close/error**（`src/transport/websocket.ts:172-214`）：① onclose 只置标志，从不 resolve/reject 挂起的 Promise resolver → `recv()` 的 `yield await new Promise(...)` 永久挂起，监听器泄漏；② errorHandler 把 resolver 解析为 `{done:true, value:null}`，但生成器忽略 done、把 null 当消息 yield 出去，靠 `parseOutboundMessage(null)` 崩溃偶然传播错误；③ 生成器不在挂起点时 resolver 为 null，错误被完全吞掉。重构：close 时以 throw 结束迭代器（抛"连接已关闭"错误）；errorHandler 抛真实错误；所有挂起 resolver 在 close/error 时都必须 settle。
3. **error 信封处理错误**（`src/client/client.ts:276-303`）：① 服务端 RPC 失败以 `OutboundMessage_Error` 返回且 `Id`=请求 Id，但 TS 只处理 `rpcReply.error`，pending RPC 干等 30s rpcTimeout；② connected 状态下**任何** error 信封（ACL 拒绝、RPC 超时、限流）都走 `handleError → handleDisconnect → 整连接重连`。改为：先按 id 匹配 pendingRPC 并 reject；无匹配且已连接时仅调 errorHandler，不触发重连。对照 Go SDK `sdks/go/client.go:253-260`（仅 `!connected` 时才当连接错误）。
4. **`Connected.publications` 被丢弃**（`src/client/client.ts:189-214`）：服务端在恢复会话时把离线消息放进 `Connected.Publications`，TS "connected" 分支从不访问该字段，也不更新 `channelOffsets`；下次重连以旧 offset+1 恢复 → **永久跳过消息**。对照 Go SDK `sdks/go/client.go:319-333`：遍历 publications 投递给消息 handler 并更新各频道 offset。同分支还要消费 `parsed.data.subscriptions` 同步服务端权威订阅列表（见任务 9）。
5. **浏览器 protobuf 不可用**（`src/transport/websocket.ts:34-43`）：构造器未设置 `socket.binaryType = "arraybuffer"`，浏览器二进制帧默认是 Blob，`codec.decode` 只接受 `Uint8Array|string` 直接抛 TypeError。Node 侧 `ws` 默认 nodebuffer 恰好可用所以没暴露。修复：dial 成功后按环境设置 binaryType（浏览器置 arraybuffer）；decode 增加 Blob 分支（`await blob.arrayBuffer()`）。

### P1（必修）

6. **`reconnect()` 不等待 Connected**（`client.ts:379-415`）：发送 Connect 后即返回，服务端不回复 Connected 时永久卡 "connecting"。复用 `waitForConnection` 的等待+超时逻辑，超时后走下一轮重试。
7. **`close()` 与 sendQueue**（`src/transport/websocket.ts:216-225` 与 `client.ts:399-407`）：① `close()` 固定 `setTimeout(resolve, 100)` 不等真实 close 事件，改为 close 事件驱动 + 超时兜底；② close 时 reject/清空 sendQueue 中挂起的 send Promise（否则 `await publish()` 永久挂起）；③ `reconnect()` 在 `await dial(...)` 之后不回查 `isClosedFlag`，会复活已关闭的客户端——dial 完成后检查，已关闭则立即关新 transport 并 return。
8. **认证失败 UX**（`client.ts:160-180`）：connecting 状态收到 error 信封（如 token 非法）既不 reject `waitForConnection` 也不 fail dial，只能等 30s 笼统 "Connection timeout"。connecting 阶段收到的 error 信封应 reject `waitForConnection` 并携带 code/type。
9. **会话恢复对齐 Go SDK**（`client.ts:188-214,500-503,572-580`）：① "connected" 分支消费服务端下发的 `subscriptions` 列表同步本地状态；② 退订时删除 `channelOffsets[ch]`（否则退订重订后重连按旧 offset 恢复，收到历史消息重复投递）；③ 重连的 `recover` 标志对齐 Go（`sdks/go/client.go:753` 恒定 `Recover: true`，epoch 为空时服务端自会兜底），去掉 `epoch !== ""` 条件。
10. **依赖与默认值**（`package.json:42-54`、`src/transport/websocket.ts:101-107`、`src/client/options.ts`）：① `ws` 从 devDependencies 移入 dependencies（或 peerDependencies + `peerDependenciesMeta.optional` + 缺失时报可读错误）——engines 声明 node>=18 但全局 WebSocket 21+ 才稳定；② 删除 `@grpc/grpc-js` peerDependency（无 gRPC 传输实现）；③ `headers` 选项是死代码（运行时给 socket 赋 `additionalHeaders` 无效），改为 ws 构造参数透传，浏览器不支持时文档注明；④ 与 Go SDK 对齐默认值：`autoReconnect` 与 `connectTimeout` 的现状差异需在代码注释或 README 中明确说明（不必强行改默认值，但差异必须是显式决定）。
11. **类型/API 一致性**：① `src/client/types.ts:26` `IClient.publish` 补 `transient?: boolean`，并让 `MessageLoopClient implements IClient`；② `options.ts:178-183` 新增 `setReconnectBackoff(initial, max, multiplier)`（对齐 Go `WithReconnectBackoff`）；③ `websocket.ts:144-162` `processSendQueue` 递归改 while 循环 + 队列上限（背压），监听器数组提供 remove API；④ `src/transport/codec/protobuf.ts` 去掉 `(x as any).fromBinary/toBinary` 类型绕过，用 schema 官方 API。
12. **仓库卫生**：删除 `src/proto/v1/service_pb.ts`（旧布局残留生成文件，全 src 无引用）；`package.json` 的 `lint` 脚本指向未安装的 eslint——补 eslint+配置或删除该脚本（选成本低的：删除脚本，并在 README 移除相关表述）。

## 测试要求

- 修复前先跑 `cd sdks/ts && npm test` 确认基线（2 套件 30 用例全绿）。
- 为以下修复补回归测试（jest，fake/mock WebSocket 即可，不要求真实 socket）：
  1. pong 超时不回复 → 走重连而非永久关闭（P0-1）；
  2. 服务端主动 close → `recv()` 迭代器抛错终止而非挂起（P0-2）；
  3. error 信封带请求 id → pending RPC 快速 reject 且连接不重连（P0-3）；
  4. `Connected.publications` 恢复消息被投递且 offset 更新（P0-4）；
  5. Blob 输入 decode 不抛错（P0-5，模拟浏览器）；
  6. 退订后 `channelOffsets` 清除（P1-9）。
- 完成后 `npm test` 全绿 + `npm run build` 通过。

## 纪律

- 不做 git commit/push。最小改动，不顺手重构无关代码。
- 协议行为对照来源：服务端 `client.go`（根目录）、Go SDK `sdks/go/client.go`——以这两者为语义基准。
- 完成后返回报告：每条任务的处置（已修/未修+原因）、改动文件清单、测试结果、留下的遗留问题。
