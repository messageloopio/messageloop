# 修复任务 07：配置、启动与可观测性

## 背景

你要修复的项目是 **MessageLoop**（工作目录：`D:/Codes/qiulin/messageloop`），Go 实时消息平台服务端。先读根目录 `AGENTS.md`。

刚完成一轮全项目代码评审，评审发现已经过主 agent 逐条核实。完整方案见 `docs/review/fix-plan.md`。**先读相关代码再动手。**

## 文件归属（严格，多 agent 并行修复）

- 你拥有：`config/`、`cmd/server/`、`metrics.go`、`health.go`、根目录全部 `*.yaml`（`config.yaml`、`config-example.yaml`、`config-node1.yaml`、`config-node2.yaml`）、`configs/`、`.github/`、`Taskfile.yml`。
- 禁止修改：根包其他文件（`client.go`/`node.go`/`hub.go` 等）、`pkg/`、`proxy/`、`sdks/`、`shared/`、`protocol/`。
- 例外与交接：`docs/`、`README.md`、`AGENTS.md` 归文档 agent——你发现代码改动使文档失准时，只改代码并在报告中交接文档修正项。任务 1② 的启动命令修正（README/AGENTS.md）也交接给文档 agent，你在报告中给出正确命令写法。

## 任务清单

### P1（必修）

1. **默认配置无法启动 + Validate 脱节 + 启动命令错误**：
   - ① `config.yaml`、`config-node1.yaml`、`config-node2.yaml` 均缺 `server.grpc_admin` 段，而 `prepareGRPCServers` 无条件预绑定 admin 监听器（`pkg/grpcstream/server.go:34-37` 要求 addr 非空）→ 启动即报 `grpc-admin-server addr is required`。为三份配置补 `grpc_admin`（addr + auth_token 或 allow_insecure 注释引导；node1/2 用不同端口如 `127.0.0.1:19091`/`:29091`）。
   - ② （交接给文档 agent）`AGENTS.md` 与 `README.md` 的单文件命令 `go run cmd/server/main.go --config ./config.yaml` 编译失败（`undefined: prepareGRPCServers`，main.go 依赖 runtime.go）。正确写法：`go run ./cmd/server --config ./config.yaml`，`go build` 同理改 `./cmd/server`——你在报告中明确交接，不要自己改这两个文件。
   - ③ `Validate()`（`config/config.go:147-150`）允许"至少一个传输"，但实际启动无条件要求 gRPC client addr 且无条件构造 WS 服务器：仅配 gRPC 时 WS 空 addr 落 80 端口、空 path 直接 panic（`pkg/websocket/server.go:51`）。`Validate()` 增加：gRPC client addr 必填；WS addr 非空则 path 必填；WS addr 为空时拒绝（与启动行为对齐）或显式跳过 WS 构造（二选一，报告中说明）。
2. **`transport.websocket.read_timeout` 死字段**（`config/config.go:87,159`；`cmd/server/main.go:189-195`）：声明+校验+文档俱全但装配时从未赋值给 `websocket.Options.ReadTimeout`，显式配置完全无效。在 `newWebSocketServer` 中解析赋值（沿用 `WriteTimeout` 的解析模式），补生效性测试。
3. **默认 `config.yaml` 安全姿态**（`config.yaml:3,12,21`）：无鉴权 `/health`/`/metrics` 绑 `:8080` 全接口（示例文件是 `127.0.0.1`）、Redis 明文密码、已废弃的 `check_origin: true`。收敛：`127.0.0.1:8080`、密码占位符、改 `allowed_origins` 示例。
4. **CI 缺口**（`.github/workflows/ci.yml`）：① 无 proto 生成物一致性校验——加 `buf generate && git diff --exit-code` 步骤（或 task generate-protocol 等效）；② golangci-lint `version: latest` 改为固定版本；③（可选）coverage 加阈值或 codecov。

### P2（顺手修）

5. `cmd/server/runtime.go:17-39,83-90` 死代码：`runNodeWithPreflight`/`nodeRunner` 仅被自身测试引用、`preparedGRPCServers.Close()` 无调用点——删除（连同其测试），或把 Close 挂进 main 的 OnStop 保留防御语义。二选一。
6. `setupCluster`（`cmd/server/main.go:101-143`）：双重 `NewCluster`（空 deps 构造仅为 normalize/校验）+ `SetPresenceStore` 副作用埋藏。拆出 normalize 辅助函数一次构造；`SetPresenceStore` 移到 main 装配区显式调用。
7. `ToProxyConfig` 丢失 `Timeout`（`config/config.go:115-123`；`cmd/server/main.go:168-179`）：解析逻辑收进 `ToProxyConfig` 内部（返回 error 已有），删除 setupProxy 的重复解析。
8. 死配置字段（`config/config.go:143`；`pkg/redisbroker/options.go:111-113`——**options.go 归 broker agent 拥有，你只改 config 侧**）：`consumer_group` 无读取点——在 `Validate()` 中拒绝并提示"未实现"，或删除字段；`stream_approximate:false` 被静默忽略——改显式三态（`*bool`）或在 `Validate()` 拒绝 false 并提示。
9. 指标可观测性（`metrics.go:26-98`；`cmd/server/main.go:39`）：registry 注册 `collectors.NewGoCollector()` 与 `NewProcessCollector()`；为关键指标加 label（至少 `transport`（ws/grpc）维度，cluster 指标加 node_id——量力而行，保持改动小）。

## 测试要求

- 修复前跑 `go test ./config/... ./cmd/... .` 确认基线。
- 回归测试：① 配置-启动一致性——逐份解析仓库内全部 yaml，断言 `Validate()` 通过且 `prepareGRPCServers` 可预绑定（用临时端口改写），防"默认配置无法启动"复发；② `read_timeout` 生效性（断言 `websocket.Options.ReadTimeout` 被赋值）；③ `Validate()` 新规则的边界用例。
- 完成后 `go test ./config/... ./cmd/... .` 全绿；`go run ./cmd/server --config ./config.yaml` 实测能启动到监听阶段（本机有 Redis；没有则在报告中注明未实测）。

## 纪律

- 不做 git commit/push。最小改动。
- 完成后返回报告：每条任务处置、改动文件清单、测试结果、交接文档 agent 的文档修正项、遗留问题。
