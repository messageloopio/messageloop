# 评审任务 03：Proxy 与传输层

## 背景

你要评审的项目是 **MessageLoop**（工作目录：`D:/Codes/qiulin/messageloop`），一个用 Go 编写的实时消息平台服务端，通过 WebSocket 和 gRPC 双向流提供 pub/sub 消息能力。先读根目录 `AGENTS.md` 了解构建命令与代码规范。

你是独立评审 agent，没有任何先前上下文。**只做只读评审，不修改任何代码。**

## 评审范围（Proxy 与传输层）

- `proxy/`：后端集成层，把认证、ACL、RPC、生命周期事件通过 HTTP 或 gRPC 转发到业务后端，按 channel/method glob 路由。文件：`proxy.go`、`http.go`、`grpc.go`、`router.go` 及全部测试。
- `pkg/websocket/`：WebSocket 传输适配（gorilla/websocket），把连接包装成 `messageloop.Transport`。全部文件及测试。
- `pkg/grpcstream/`：gRPC 传输与 admin API。客户端双向流端口与 admin unary 端口分离；`server.go`（共享准备与拦截器）、`client_server.go`、`admin_server.go`、`handler.go`、`api_handler.go`、`transport.go`、`codec.go` 及全部测试。
- 参考文档：`docs/developer/01-architecture.md`、`docs/developer/03-admin-api.md`、`docs/protocol.md`。

## 模块职责与关键契约（供定位，需你自行通读验证）

- `proxy.Proxy` 接口（`proxy/proxy.go`）：`RPC/Authenticate/SubscribeAcl/PublishAcl/OnConnected/OnSubscribed/OnUnsubscribed/OnDisconnected/Name/Close`；默认 RPC 超时 30s，无重试。
- `Transport` 接口（根包 `transport.go`）：`Write/WriteMany/Close(Disconnect)/RemoteAddr`。
- WebSocket：读超时默认 60s（或 2×心跳 idle timeout），写超时默认 10s；close 发送 close frame 后 5s drain。
- gRPC stream：每个 Transport 一个 send worker goroutine，`sendCh` 容量 64 串行化写；`Close` 用 closeOnce 保证一次性，先 enqueue 断连帧再关闭。
- admin API（`api_handler.go`）：`serverpb` unary 服务，Publish 采用部分成功语义；bearer token 拦截器鉴权。

## 评审维度

1. **连接生命周期正确性**：读写超时、关闭握手、closeOnce 语义、慢消费与背压（`sendCh` 满时的行为）、 goroutine 泄漏。
2. **proxy 边界**：超时传播、后端错误到客户端错误信封的映射、HTTP 非 200 与网络错误路径、gRPC 错误码映射。
3. **路由正确性**：`Router` 的 glob 匹配顺序、并发读写、AddFromConfig 解析。
4. **安全**：TLS 配置校验、admin token 拦截器覆盖、Origin 检查。
5. **与核心的契约一致性**：`Transport.Write/Close` 的语义是否满足核心层 `Client` 的假设（如 Close 幂等、Write 错误即慢消费）。
6. **测试缺口**：TLS、畸形帧、MaxMessageSize、压缩等路径。

## 已知线索（需你独立核实真伪，确认或推翻，并给出证据）

1. `pkg/grpcstream/transport.go` 的 `sendWithTimeout` 用同一个 timer 先后 select enqueue 和 errCh；若 timer 在两次 select 之间触发，第二次会立即超时，疑误判发送失败。
2. `Transport.Close`（grpcstream）先置 `closed=true` 再 `writeError`（走 `sendWithTimeout`）；若 `sendCh` 已满，断连帧本身可能超时——评估该路径的实际行为。
3. `proxy/http.go` 的 `doRequest` 疑仅接受 200，非 200 把原始 body 文本当错误返回，后端无法用结构化 `sharedpb.Error` 表达 HTTP 级错误。
4. `proxy/grpc.go` 安全模式使用 `&tls.Config{}`，疑未校验 ServerName。
5. `proxy/router.go` 的 `Router.Close` 疑只保留最后一个 error。
6. `pkg/websocket/handler.go` 用 `strings.Contains` 匹配子协议名选择 marshaler，依赖命名隔离而非精确映射。
7. `api_handler.go` 的 Publish 约 90 行且 payload 转换逻辑与根包 `client.go:handlePublish` 重复。
8. 测试缺口：proxy 的 gRPC 通知方法、HTTP 非 200/网络错误、TLS 路径；WebSocket 的压缩/子协议异常/畸形二进制帧；grpcstream 的 streaming 端口认证、Survey admin 路径。

## 工作流程

1. 先跑 `go build ./...` 和 `go test ./proxy/... ./pkg/websocket/... ./pkg/grpcstream/...` 确认基线。
2. 通读范围内代码，逐维度评审。
3. 逐条核实"已知线索"：确认（给出决定性证据）或推翻。
4. 补充你自己发现的新问题。

## 输出格式

用中文输出。先给基线测试结果与总体评价（3-5 句），然后逐条 findings：

```
[级别] Critical / Important / Minor
[位置] path:line
[问题] ...
[证据] 关键代码摘录或推理
[修复建议] ...
[置信度] high / medium / low
```

最后单独一节列出"建议补充的测试"。不要贴大段代码，每条 finding 引用不超过 10 行。
