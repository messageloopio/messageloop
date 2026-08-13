# 修复任务 04：Proxy 与传输层

## 背景

你要修复的项目是 **MessageLoop**（工作目录：`D:/Codes/qiulin/messageloop`），Go 实时消息平台服务端。先读根目录 `AGENTS.md`。

刚完成一轮全项目代码评审，评审发现已经过主 agent 逐条核实（本模块全部确认）。完整方案见 `docs/review/fix-plan.md`。**先读相关代码再动手。**

## 文件归属（严格，多 agent 并行修复）

- 你拥有：`proxy/`、`pkg/websocket/`、`pkg/grpcstream/` 全部。
- 禁止修改：根包 `client.go` 等、`pkg/redisbroker/`、`pkg/topics/`、`config/`、`cmd/`、`sdks/`、`shared/`、`protocol/`、`docs/`。
- 注意：任务 1/5 会发现 `client.go` 侧存在与你重复的 payload 转换代码——**不要抽公共函数到根包**（跨归属），在本模块内修复即可，重复抽取问题写进报告交接。

## 任务清单

### P1（必修）

1. **HTTP proxy payload 编解码损坏**（`proxy/http.go:87-92` 请求侧、`103-107` 及各 doRequest 解析回调）：`*sharedpb.Payload` 的 oneof 经 `encoding/json` 序列化/反序列化后 `Data` 永不还原——经 HTTP proxy 的 RPC 拿不到任何实际载荷。现有 `TestHTTPProxy_RPC` 只断言 `Payload` 非 nil 掩盖了问题。修复：payload 部分改用 `protojson.Marshal/Unmarshal`（与 gRPC 路径的 proto3 JSON 契约一致）。**先补会失败的回归测试**：`TestHTTPProxy_RPC_PayloadRoundTrip` 断言 `resp.Payload.GetData()` 非 nil。
2. **WS 子协议协商 marshaler/帧类型不一致**（`pkg/websocket/handler.go:46-48,111-119`）：marshaler 由客户端 offer 列表经 `strings.Contains` 决定，帧类型由协商后的 `conn.Subprotocol()` 决定——offer `["messageloop","messageloop+proto"]` 时协商结果是 `messageloop`（文本帧）但选中 ProtobufMarshaler；任意含 "proto" 子串的未知协议名也会误中。修复：用协商结果 `conn.Subprotocol()` 做精确 switch 映射，删除子串匹配。补协商矩阵测试：offer 顺序变化、未知子协议、无子协议 × 断言 marshaler 与帧类型一致。
3. **gRPC `sendWithTimeout` 共享 timer 假超时**（`pkg/grpcstream/transport.go:63-82`）：同一个 timer 先后用于 enqueue 和等待投递确认两个 select；enqueue 逼近 deadline 时第二个 select 立即命中 `timer.C` 返回假超时——核心层据此把健康连接判为慢消费者断开。修复：enqueue 成功后重置 timer（或第二阶段独立计时），保证两个阶段的预算语义清晰。
4. **gRPC `Close` 断连帧竞态**（`pkg/grpcstream/transport.go:84-104,139-148`）：① sendCh 满时 `writeError` 的 enqueue 阻塞满 writeTimeout（默认 10s）且断连帧未入队；② worker 的 select 在 sendCh/closeCh 同时就绪时可随机退出丢弃 DISCONNECT_ERROR。修复：worker 退出前先排空 sendCh（对遗留请求回填 errCh）；`writeError` 失败时降级为直接关闭（不阻塞 10s）。
5. **HTTP proxy 错误与 metadata**（`proxy/http.go:392-395,87-92`）：① 非 200 只把 body 文本拼进 error——先尝试解析 body 中的结构化 error（参考 notificationErrorResponse 模式），成功则作为 `sharedpb.Error` 语义返回，失败回退文本；② RPC 请求体丢弃了 `Metadata`，补透传。
6. **WS Transport `Close` 失败路径 fd 泄漏**（`pkg/websocket/transport.go:56-91`）：`WriteControl`/`SetReadDeadline` 失败即 return，跳过 `conn.Close()`——对端 RST 时底层 fd 泄漏。改 `defer t.conn.Close()` 或所有失败路径统一关闭。

### P2（顺手修）

7. `Router.Close` 只保留最后 error（`proxy/router.go:84-96`）：改 `errors.Join` 聚合。
8. `Router.AddFromConfig` 半初始化（`proxy/router.go:74-81`）：失败时回滚已添加路由，或先全部编译再一次性提交。
9. WS handler 升级失败/NewClient 失败后重复 `WriteHeader(500)`（`pkg/websocket/handler.go:39-55`）：升级失败直接 return（gorilla 已写握手错误）；NewClient 失败先关已升级连接再返回。
10. `GRPCProxyConfig` 缺 TLS 配置（`proxy/grpc.go:44-46`）：增加 `ServerName`/`InsecureSkipVerify` 字段并传入 `tls.Config`（注：现状不会跳过 ServerName 校验，这只是配置缺口补齐）。
11. **gRPC 断连数值码丢失**（`pkg/grpcstream/transport.go:106-121`）：DISCONNECT_ERROR 信封只携带固定字符串 code 与 reason 文本，数值断连码（3500-3512）被丢弃，gRPC 客户端无法区分断连原因。把数值码编码进错误信封（如 metadata 或结构化 message），保持 WS 路径语义对齐。

## 测试要求

- 修复前跑 `go test ./proxy/... ./pkg/websocket/... ./pkg/grpcstream/...` 确认基线。
- 回归测试：任务 1（payload 往返）、2（子协议矩阵）、3（慢队列下 enqueue 逼近 deadline 不误报，可注入可控时延 fake stream）、4（sendCh 满时 Close 行为）、6（对端 RST 后 Close 仍关闭 fd）。
- 完成后 `go test -race ./proxy/... ./pkg/websocket/... ./pkg/grpcstream/...` 全绿，`go vet` 干净。

## 纪律

- 不做 git commit/push。最小改动。
- 完成后返回报告：每条任务处置、改动文件清单、测试结果、交接项（payload 转换重复抽取问题）、遗留问题。
