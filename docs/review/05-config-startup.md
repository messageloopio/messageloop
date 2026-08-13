# 评审任务 05：配置、启动与可观测性

## 背景

你要评审的项目是 **MessageLoop**（工作目录：`D:/Codes/qiulin/messageloop`），一个用 Go 编写的实时消息平台服务端，通过 WebSocket 和 gRPC 提供 pub/sub 消息能力。先读根目录 `AGENTS.md` 了解构建命令与代码规范。

你是独立评审 agent，没有任何先前上下文。**只做只读评审，不修改任何代码。**

## 评审范围（配置、启动与可观测性）

- `config/`：`config.go`（配置结构 + `Validate()`）、`config_test.go`
- `cmd/server/`：`main.go`（装配：Cluster/Broker/Proxy/WebSocket/Admin 服务器）、`runtime.go`（gRPC 预绑定与预飞行）、`runtime_test.go`
- 配置文件：`config-example.yaml`、`config.yaml`、`config-node1.yaml`、`config-node2.yaml`、`configs/test.yaml`
- 可观测性（根包）：`metrics.go`（Prometheus 指标）、`health.go`（健康检查端点）及 `health_test.go`
- `Taskfile.yml`、`.github/` CI 配置
- 参考文档：`docs/developer/02-configuration.md`、`docs/developer/05-observability.md`、`docs/deployment.md`

## 模块职责与关键契约（供定位，需你自行通读验证）

- 启动管线：配置 → `Validate()` → 构造 → 预绑定 gRPC 监听器 → `lynx` 框架生命周期启停。
- 四个监听面：WebSocket（默认 `:9080`）、客户端 gRPC（`:9090`）、admin gRPC（`:9091`）、admin HTTP `/health` `/metrics`（`:8080`）。
- `Node.Run/Shutdown` 由 lynx 调用；`DefaultShutdownTimeout=10s`，超时未排空记 Warn。
- `HealthHandler`：broker 未就绪 503；集群模式下 Redis ping 2s 超时失败 503。

## 评审维度

1. **配置校验完整性**：`Validate()` 是否覆盖所有非法组合；配置字段是否有声明但代码未读取的"死字段"。
2. **启动/关闭正确性**：资源释放顺序、预绑定端口泄漏、失败路径清理、shutdown 排空语义。
3. **装配代码质量**：构造函数副作用、依赖装配顺序、错误处理。
4. **可观测性充分性**：指标覆盖与 label 设计、健康检查语义、日志关键点。
5. **部署安全**：`allow_insecure` 逃生口、admin token、默认配置的安全性。
6. **CI/构建**：GitHub Actions 与 Taskfile 的覆盖面（测试、lint、生成代码校验）。
7. **测试缺口**：装配路径、TLS 加载、配置边界。

## 已知线索（需你独立核实真伪，确认或推翻，并给出证据）

1. `docs/developer/02-configuration.md` 称 `server.heartbeat.idle_timeout` 为空则"完全禁用心跳"，但 `node.go` 疑始终创建 `HeartbeatManager` 并回退到 300s 默认值——文档与代码必有一方错误。
2. `cmd/server/runtime.go` 的 `runNodeWithPreflight` 疑仅在自身测试中使用，`main.go` 直接 `app.OnStart(node.Run)`——死代码。
3. `setupCluster`（`cmd/server/main.go`）疑在构造时附带 `node.SetPresenceStore` 副作用，且两次调用 `messageloop.NewCluster` 重新装配依赖。
4. `config/config.go` 的 `ProxyConfig.ToProxyConfig` 疑丢失 `Timeout` 字段（超时在 `main.go` 中单独解析），逻辑分散。
5. `preparedGRPCServers.Close()` 疑在 `main.go` 未显式 defer，注册后启动前失败可能泄漏预绑定端口。
6. `metrics.go` 所有指标疑无 label，无法按频道/节点细分；registry 疑不含 Go runtime/process 指标。
7. `broker.redis.consumer_group` 疑声明但代码未读取；`stream_approximate: false` 疑被忽略（仅 true 时覆盖默认值）；WebSocket 空 path 疑会导致 `http.ServeMux` panic。
8. 测试缺口：`cmd/server/main.go` 的装配函数无直接测试；`metrics.go` 无独立测试；config 未覆盖 `proxy[].timeout`/`check_origin`/`allowed_origins`。

## 工作流程

1. 先跑 `go build ./...` 和 `go test ./config/... ./cmd/... .` 确认基线。
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
