# 开发指南

本文面向希望在 MessageLoop 仓库中进行开发的工程师，涵盖环境准备、仓库结构、构建与测试、静态检查、Protobuf 生成流程、代码约定、测试约定、本地运行方式、TypeScript SDK 开发以及发布流程。所有命令均以仓库根目录为工作目录（另有说明的除外），并在 Windows（PowerShell）与 Unix 兼容。

## 环境要求

- **Go 1.26.5**：根模块的 `go.mod` 声明 `go 1.26.5`。`shared/` 与 `sdks/go/` 两个子模块声明 `go 1.25.5`。CI 通过 `actions/setup-go@v5` 的 `go-version-file: go.mod` 解析根模块声明的版本，建议本地安装同版本或更新的工具链。
- **task**：任务自动化使用 [Task](https://taskfile.dev)。安装方式：`go install github.com/go-task/task/v3/cmd/task@latest`。
- **buf 与 protoc-gen-go**：运行 `task init` 安装 `google.golang.org/protobuf/cmd/protoc-gen-go@latest` 与 `github.com/bufbuild/buf/cmd/buf@v1.65.0`（版本由 Taskfile.yml 固定，并与 `.github/workflows/ci.yml` 中钉的版本保持一致）。仅在修改协议文件、需要重新生成代码时使用。
- **Node.js >= 18**：TypeScript SDK（`sdks/ts/package.json` 的 `engines.node`）开发需要；测试使用 Jest。
- **Redis**：可选。运行 Redis broker 相关集成测试时需要本地 Redis（默认地址 `127.0.0.1:6379`），详见[构建与测试](#构建与测试)一节；内存 broker 模式不需要。
- **golangci-lint**：`task lint` 依赖 golangci-lint 可执行文件，需自行安装。

## 仓库布局

仓库由三个独立的 Go 模块构成，生成代码与共享类型集中在 `shared/` 子模块中：

| 条目 | 职责 |
| --- | --- |
| `node.go`、`client.go`、`hub.go`、`broker.go`、`broker_memory.go`、`transport.go`、`presence.go`、`survey.go`、`acl.go`、`metrics.go`、`heartbeat.go`、`health.go`、`disconnect.go`、`marshaler.go`、`defaults.go` 等根级 `*.go` | 核心服务端类型：`Node` 协调器、`Client` 会话、`Hub` 连接注册表、`Broker`/`Transport`/`Presence`/`Survey`/`ACL` 接口与实现 |
| `cluster.go`、`cluster_commands.go`、`cluster_state.go`、`cluster_resume.go`、`cluster_projection_repair.go` | 集群控制面（节点发现、命令总线、投影修复），Redis 支撑 |
| `subscription_saga.go` | 订阅状态机（订阅/退订的可靠交付） |
| `cmd/server/` | 服务端入口 `main.go` 与 gRPC 启动预检 `runtime.go`，基于 `lynx` 框架 |
| `config/` | 配置结构体定义与校验（`config.go`、`config_test.go`） |
| `protocol/` | Protobuf 源文件（单一 buf module），下分 `shared/v2/`、`client/v2/`、`server/v2/`、`proxy/v2/`（v1 已于 D6 删尽） |
| `shared/` | 独立 Go 模块 `github.com/messageloopio/messageloop/shared`；`genproto/` 为生成代码，`marshaler.go` 为 JSON/Protobuf 序列化器 |
| `pkg/websocket/` | WebSocket 传输实现（含集成测试） |
| `pkg/grpcstream/` | gRPC 流式传输（客户端流 `client_server.go`、管理 API `admin_server.go`、公共准备逻辑 `server.go`） |
| `pkg/quicstream/` | 可选 QUIC 客户端传输（一条双向流 + 长度前缀帧，TLS 1.3 / ALPN 协商编码） |
| `pkg/topics/` | 主题匹配器：`cstrie`（默认并发实现）、`trie`、`naive`、`inverted_bitmap` 等 |
| `pkg/redisbroker/` | Redis broker 实现：Streams 历史、Pub/Sub 实时分发、Redis 支撑的 presence 与集群命令总线 |
| `proxy/` | RPC 代理后端集成（HTTP/gRPC 后端、路由、超时） |
| `sdks/go/` | 独立 Go 模块 `github.com/messageloopio/messageloop/sdks/go`：客户端 SDK（`client.go`、`websocket.go`、`grpc.go`、`quic.go`、`proxy.go`、`mux.go`、`options.go`）与 `example/` 示例 |
| `sdks/ts/` | TypeScript SDK（`@messageloop/sdk`），含 `src/`、`test/` 与生成的 `src/proto/` |
| `docs/` | 文档：根目录为英文版协议与部署文档，`developer/` 为本开发者文档套件 |
| `Taskfile.yml`、`buf.yaml`、`buf.gen.yaml`、`buf.lock` | 任务定义与 buf 配置 |
| `config-example.yaml` | 完整配置参考示例 |
| `config.yaml`、`config-node1.yaml`、`config-node2.yaml`、`configs/test.yaml` | 本地运行配置（见[本地运行开发服务器](#本地运行开发服务器)） |

模块边界（`go.mod` 与 `replace` 指令）：

- **根模块** `github.com/messageloopio/messageloop`，`go 1.26.5`，通过 `replace github.com/messageloopio/messageloop/shared => ./shared` 在本地解析 shared 模块。
- **shared 模块** `github.com/messageloopio/messageloop/shared`，`go 1.25.5`，仅依赖 `grpc` 与 `protobuf`。
- **sdks/go 模块** `github.com/messageloopio/messageloop/sdks/go`，`go 1.25.5`，通过 `replace github.com/messageloopio/messageloop/shared => ./../../shared` 引用 shared 模块。

因此根目录执行 `go build ./...` 只会覆盖根模块；shared 与 sdks/go 需要在各自目录内单独构建与测试。根模块的 `require` 中同时保留了 shared 的远程版本（`v0.1.0`），本地开发依赖 `replace` 覆盖。

## 构建与测试

```bash
# 构建根模块（含 cmd/、pkg/、proxy/、config/）
go build ./...

# 运行根模块全部测试
go test ./...

# 带竞态检测运行全部测试（等价于 task test）
go test -race ./...

# 按包运行测试
go test ./pkg/topics/...

# 运行单个测试
go test -v ./pkg/topics/... -run TestCSTrieMatcher
```

`task test` 定义为 `go test -race ./...`，是 CI 之外最接近的门禁。shared 与 sdks/go 是独立模块，需在其目录内执行 `go test ./...`（shared 模块当前无测试；sdks/go 含 `client_test.go`、`message_test.go`、`proxy_test.go` 等）。

### 需要 Redis 的集成测试

以下测试在启动时通过 Redis `Ping` 探测可用性，失败则调用 `t.Skipf` 自动跳过，无需 build tag 或环境变量开关：

- 根目录 `cluster_redis_integration_test.go`：多节点集群的会话管理、查询与在线状态聚合，使用 **DB 15**，测试前后执行 `FlushDB`。
- `pkg/redisbroker/cluster_command_bus_test.go`（命令总线）、`pkg/redisbroker/publish_transient_test.go`（瞬时发布不进历史）、`pkg/redisbroker/history_test.go`：使用 **DB 14**。

连接参数通过环境变量配置：

- `MESSAGELOOP_TEST_REDIS_ADDR`：Redis 地址，默认 `127.0.0.1:6379`。
- `MESSAGELOOP_TEST_REDIS_PASSWORD`：密码，未设置时回退到 `REDIS_PASSWORD`。

示例（PowerShell）：

```powershell
$env:MESSAGELOOP_TEST_REDIS_ADDR = "127.0.0.1:6379"
go test -race -v ./pkg/redisbroker/...
```

没有 Redis 时这些测试直接跳过，其余测试不受影响。此外，`pkg/websocket/integration_test.go`、`pkg/grpcstream/integration_test.go` 与 `pkg/grpcstream/port_integration_test.go` 属于端到端用例：它们在测试内直接构造 `messageloop.NewNode(...)` 与进程内组件（不依赖外部运行中的服务器），随 `go test ./...` 一起执行。

## 静态检查

```bash
task vet   # go vet ./...
task lint  # golangci-lint run
```

CI（`.github/workflows/ci.yml`）在 push/PR 到 `main` 与 `v2` 时运行三个 job：

- `build-and-test`：挂 `redis:7` service（`127.0.0.1:6379`，无密码，供 Redis 集成测试真实运行），随后 `go build ./...`、`go vet ./...`、以钉版 buf（`v1.65.0`）执行 `buf generate` 并用 `git diff --exit-code` 校验生成物为最新、`go test -race -coverprofile=coverage.out -covermode=atomic ./...`，再依次跑子模块 `shared`/`sdks/go` 的 `go test ./...` 与 `_examples/chatroom` 的 `go build ./...`；PR 场景上传覆盖率产物。
- `ts-sdk`：`actions/setup-node@v4`（Node 24.11.1）后在 `sdks/ts` 执行 `npm ci`、`npm run build`、`npx jest`。
- `lint`：`golangci/golangci-lint-action@v6`（version 固定为 `v2.12.2`）。

CI 会以钉版 buf 执行 `buf generate` 并校验零 diff；协议代码变更必须本地用同一版本（`task init` 安装）重新生成后随提交进入仓库。

## Protobuf 工作流

协议源文件位于 `protocol/`，该目录是 buf v2 配置（`buf.yaml`）的模块根：

```yaml
version: v2
deps:
  - buf.build/googleapis/googleapis
  - buf.build/bufbuild/protovalidate
modules:
  - path: protocol
lint:
  use: [STANDARD]
breaking:
  use: [FILE]
```

依赖版本由 `buf.lock` 固定。`buf.gen.yaml` 定义了生成输出的映射：

| 插件（远程） | 输出目录 | 选项 |
| --- | --- | --- |
| `buf.build/protocolbuffers/go:v1.36.10` | `shared/genproto` | `paths=source_relative` |
| `buf.build/grpc/go:v1.6.0` | `shared/genproto` | `paths=source_relative` |
| `buf.build/grpc-ecosystem/gateway:v2.27.4` | `shared/genproto` | `paths=source_relative` |
| `buf.build/grpc-ecosystem/openapiv2:v2.27.3` | `shared/genproto` | 生成 `.swagger.json` |
| `buf.build/bufbuild/es:v2.10.0` | `sdks/ts/src/proto` | `target=ts`、`import_extension=none`、`include_imports=true` |

执行 `task generate-protocol`（即 `buf generate`）后：

- **Go 代码**生成在 `shared/genproto/<pkg>/<version>/`（`.pb.go`、`*_grpc.pb.go`、`.swagger.json`），如 `client/v2`、`proxy/v2`、`server/v2`、`shared/v2`。Go 侧导入路径为 `github.com/messageloopio/messageloop/shared/genproto/<pkg>/<version>`，包别名见[代码风格与约定](#代码风格与约定)。
- **TypeScript 代码**生成在 `sdks/ts/src/proto/<pkg>/<version>/`，使用 bufbuild/es 远程插件（仓库未安装 `@bufbuild/protoc-gen-es`）。

### 新增协议消息或字段的流程

1. 编辑 `protocol/<domain>/<version>/xxx.proto`，保持字段编号向后兼容（`buf breaking` 按 `FILE` 级别检查）。
2. 运行 `task generate-protocol`。
3. 检查 `shared/genproto` 下 Git diff，确认 Go 生成代码与 Swagger 文件符合预期。
4. 检查 `sdks/ts/src/proto` 下 TS 生成代码 diff；若 SDK 需暴露新类型，更新 `sdks/ts/src/` 中的手写封装并补充测试。
5. `buf build` 本地通过 lint 校验（`buf lint`、`buf breaking --against .git` 可按需执行）。

## 代码风格与约定

规范全文见 `AGENTS.md`，要点如下：

### 导入分组

导入按三组排列，组间空行分隔：标准库、第三方依赖、本地（本项目）导入。Protobuf 包统一使用短别名，与各 proto 文件 `go_package` 选项的包短名一致：

- `clientpb`：`github.com/messageloopio/messageloop/shared/genproto/client/v2`
- `serverv2`：`github.com/messageloopio/messageloop/shared/genproto/server/v2`
- `sharedv2`：`github.com/messageloopio/messageloop/shared/genproto/shared/v2`
- `proxypb`：`github.com/messageloopio/messageloop/shared/genproto/proxy/v2`

### 命名

- 导出的类型/函数/常量：PascalCase；未导出标识符：camelCase。
- 接口用简洁的能力名（`Broker`、`Transport`、`Marshaler`、`Matcher`）。
- 常量用 `const` 块组织，枚举值用 iota。
- 所有导出的符号必须有文档注释；接收者方法注释以类型名开头（如 `// Send writes a message to the client`）。

### 错误处理

- 主动断连使用类型化 `Disconnect` 错误（`disconnect.go`），带编号代码：`3000` 正常关闭，`3500–3509` 各类终端错误（`DisconnectBadRequest`、`DisconnectStale` 等），`3511` 空闲超时，`3512` 慢消费者，`3513` 内部错误（connect 路径失败时强制断连）。
- 用 `fmt.Errorf("context: %w", err)` 包装错误保留链，用 `errors.As` / `errors.Is` 判断类型或哨兵值。
- 返回前在适当级别记录日志，不要无理由吞掉错误。

### 其他

- 选项模式（option pattern）用于可选参数，如 `PublishOption`、`WithClientInfo`。
- 结构体按接收者类型分组组织方法，保持函数聚焦（约 100 行以内）。
- Hub 使用 64 分片、订阅锁使用 16384 分片以降低锁竞争。

## 测试约定

- 单元测试与被测源码同目录、同包，命名 `*_test.go`，函数为 `TestXxx(t *testing.T)` 或 `BenchmarkXxx(b *testing.B)`。
- 断言使用 `testify/assert`（轻量断言）与 `testify/require`（失败即中止，集成测试中常用）。
- 偏好表驱动测试（table-driven tests）。
- 集成测试位置：
  - `pkg/websocket/integration_test.go`：WebSocket 连接与发布。
  - `pkg/grpcstream/integration_test.go`、`pkg/grpcstream/port_integration_test.go`：gRPC 流端到端。
  - 根目录 `cluster_redis_integration_test.go`：需要 Redis 的多节点集群行为。
- 默认以 `task test`（`go test -race ./...`）作为完整门禁，与 CI 一致。

## 本地运行开发服务器

```bash
go run ./cmd/server --config ./config.yaml
```

命令行参数（`cmd/server/main.go` 中通过 pflag 定义）：

- `--config <path>`：配置文件路径，默认 `./config.yaml`。
- `--log-level <level>`：日志级别，默认 `info`。

默认监听端口（可通过配置覆盖，详见[《配置参考》](02-configuration.md)）：

| 监听器 | 配置键 | 默认值 |
| --- | --- | --- |
| WebSocket 客户端流量 | `transport.websocket.addr` | 无默认（必填） |
| 客户端 gRPC 流 | `transport.grpc.addr` | 无默认（必填） |
| 客户端 QUIC | `transport.quic.addr` | 空（不启动） |
| gRPC 管理 API | `server.grpc_admin.addr` | 无默认（必填，启动预检阶段无条件预绑定） |
| HTTP 健康检查与指标 | `server.http.addr` | `127.0.0.1:8080` |

仓库内的配置示例用途：

- `config.yaml`：默认开发配置；broker 类型为 `redis`，连接 `127.0.0.1:6379`（密码 `123456`，DB 10），并注册一个指向 `127.0.0.1:8090` 的示例代理。
- `config-node1.yaml` / `config-node2.yaml`：双节点集群演示，端口分别使用 `18/19/29` 前缀（如 WebSocket `:19080` / `:29080`），两个节点共享同一个 Redis 实例，用于本地验证集群功能。
- `configs/test.yaml`：端到端测试配置，明确 `grpc_admin` 为 `127.0.0.1:9091`，broker 为 Redis。
- `config-example.yaml`：完整字段参考，所有配置项的权威示例。

## TypeScript SDK 开发

SDK 位于 `sdks/ts/`，包名 `@messageloop/sdk`，要求 Node.js >= 18，运行依赖 `@bufbuild/protobuf`（`^2.0.0`），`@grpc/grpc-js` 为 peer 依赖：

```bash
npm install        # 安装依赖
npm run build      # 依次构建 ESM、CJS 与类型声明（dist/esm、dist/cjs、dist/types）
npm test           # Jest 测试（ts-jest，测试位于 test/）
npm run lint       # ESLint 检查 src/
```

构建与测试细节：

- `tsconfig.json`：ES2020 目标、`strict` 开启，输出 `dist/esm`；`tsconfig.node.json` 负责 CJS 输出。
- 测试由 `jest.config.js` 配置，preset 为 `ts-jest`，环境 `node`，roots 为 `test/`。
- `src/proto/` 下为 buf 生成的代码，不要手工编辑（见[Protobuf 工作流](#protobuf-工作流)）；手写封装位于 `src/client/`、`src/message/`、`src/transport/`。
- 详细用法见[《TypeScript SDK 指南》](08-sdk-ts.md)。

## 发布流程

版本变量定义在 `Taskfile.yml` 顶部：`Version: v0.2.0` 与 `Comment: "release v0.2.0"`。发布前更新这两个变量。

- `task release-all`：依次为三个模块打 annotated tag 并推送：`v0.2.0`（根）、`shared/v0.2.0`（shared 模块）、`sdks/go/v0.2.0`（Go SDK）。标签格式为 `git tag -a <tag> -m <comment>` 后 `git push origin <tag>`，三组标签共享同一版本号，用目录前缀区分模块。
- `task release-tag`：单独打一个标签并推送（`task release-tag Version=v0.2.0` 形式覆盖变量）。
- `task release-sdk-ts`：在 `sdks/ts/` 下执行 `npm exec rimraf -- dist` 清理、`npm run build` 构建、`npm publish --access public --registry https://registry.npmjs.org/` 发布。注意 npm 包版本（`package.json` 的 `version`，当前 `1.1.0`）独立于 Go 侧标签，需要单独递增。
- `task upgrade-lynx`：批量升级 `lynx` 框架及 contrib 依赖（`go get -u github.com/lynx-go/x` 等）后 `go mod tidy`。

CI 在 push/PR 合并前自动执行构建、vet、带竞态与覆盖率的测试以及 golangci-lint，发布动作均为手动触发。

## 交叉链接

- 部署：[《部署指南》](../deployment.md)
- 协议定义：[《客户端协议参考》](../protocol.md)
- 架构：[《架构指南》](01-architecture.md)
- 配置：[《配置参考》](02-configuration.md)
- 管理 API：[《管理 API 参考》](03-admin-api.md)
- 集群：[《分布式集群指南》](04-cluster.md)
- 可观测性：[《可观测性指南》](05-observability.md)
- Go SDK：[《Go SDK 指南》](07-sdk-go.md)
- TypeScript SDK：[《TypeScript SDK 指南》](08-sdk-ts.md)
- 文档总览：[《开发者文档》](README.md)
