# 修复任务 08：文档批次与仓库卫生

## 背景

你要修复的项目是 **MessageLoop**（工作目录：`D:/Codes/qiulin/messageloop`），Go 实时消息平台服务端。

刚完成一轮全项目代码评审，发现大量"实现演进后文档未跟上"的漂移（全部经主 agent 对照源码核实，**代码为准、改文档**，除特别注明）。完整方案见 `docs/review/fix-plan.md` P3 节。

## 文件归属（严格，多 agent 并行修复）

- 你拥有：`docs/` 全部、`README.md`、`AGENTS.md`、`RPC_TIMEOUT.md`、`CLAUDE.md`、`.gitattributes`（新建）、根目录 `server.exe`/`nul`（删除）。
- 禁止修改：任何 `.go`/`.ts`/`.proto`/`.yaml`/`.yml` 代码与配置文件；`sdks/`（归其他 agent）。
- 注意：其他 agent 正在并行改代码，你引用的行号可能有小幅漂移——以语义定位为准，引用行号不必逐一对新。

## 任务清单（全部为文档修正，代码事实已核实）

1. **心跳**：`docs/developer/02-configuration.md:44,103,357` 与 `defaults.go` 表述（defaults.go 是代码文件，你只改 docs）——"`idle_timeout` 为空 = 完全禁用心跳"改为"为空或解析失败均回退 300s 默认；`"0s"` 时 HeartbeatManager 不启动（`heartbeat.go:27-29`）"。
2. **History 语义**：`docs/developer/04-cluster.md:295,356` 与 `docs/developer/03-admin-api.md:311-317,554`——删除"Redis broker 的 since_offset 为 exclusive"表述，统一为 inclusive（`offset >= sinceOffset`，两实现一致，`broker.go:105-108` 契约为准）。
3. **epoch 语义**：`docs/developer/04-cluster.md:236-238`（§4.4 重写：Redis epoch 存于 `ml:broker:epoch`，SETNX 集群共享、跨重启持久，跨节点恢复的 epoch 校验可通过）与 `docs/developer/01-architecture.md:177`（区分内存 broker 随机 UUID 与 Redis 共享 epoch 两种实现）。
4. **TTL**：`docs/developer/04-cluster.md:201,308` 会话租约 90s→600s（`cluster_state.go:20`）；`:281` presence 索引 TTL `PresenceTTL*2`=120s→60s（`presence_redis.go:50`）。
5. **admin 鉴权**：`README.md:44-61` Quick Start 配置补 `auth_token`（或 `allow_insecure: true` 并注明仅限开发）；`docs/developer/02-configuration.md` 补 `allow_insecure` 字段行、"auth_token 空 = 不鉴权"改为"addr 非空时必须 auth_token 或 allow_insecure（config.go:184-189）"、Validate 规则清单补第 6 条；`docs/developer/03-admin-api.md:35` "为空则不启用鉴权"同步修正。
6. **deployment.md**：`:172` "DisconnectStale"→"DisconnectIdleTimeout（3511）"（`heartbeat.go:54`）；`:18-23` 监听器表的 "Default" 列改为"无默认/必填"（仅 `server.http.addr` 有 `127.0.0.1:8080` 回退）；`docs/developer/06-development.md:208` 同款表同改。另在 Multi-Node Cluster 章节（`:95-147`）补一段："集群命令总线无签名/认证，Redis 网络隔离是集群安全前提"（`pkg/redisbroker/cluster_command_bus.go:1-11`）。
7. **断连码表**：`docs/protocol.md:341-355`——3502 语义改为"集群会话恢复失败"（`cluster_resume.go:77`），3506-3509 标注"保留定义，当前无触发点"；`docs/developer/05-observability.md:154` 的 3502 行"是否触发"改"是（集群恢复失败）"。
8. **protocol.md**：`:62-76` OutboundMessage 信封表补 `sub_refresh_ack` 行（`service.proto:46`，`client.go:1190-1194`）。
9. **gRPC 断连码**：`docs/developer/01-architecture.md:397` 与 `docs/developer/05-observability.md:145`——现状是数值码在 gRPC 路径丢失（`pkg/grpcstream/transport.go:106-121` 只传固定串+reason）。**传输 agent 正在并行修复此项（把数值码编入错误信封）**：你在文档中按"数值码随错误信封传递"的目标语义写，并在句末标注实现版本；若发现代码未改，在报告中交接而非改代码。
10. **read_timeout**：`docs/developer/02-configuration.md:150`——"未配置且心跳禁用时默认 60s"分支不可达（心跳永开），改为"未配置时取 2×idle_timeout（默认 600s）"。
11. **TS SDK 版本号**：`docs/developer/08-sdk-ts.md:5`、`docs/developer/06-development.md:242` 的 1.0.5→1.1.0。
12. **03-admin-api.md:565**："Go 与 TypeScript SDK 的后端集成均通过本管理 API"→"Go SDK 的后端集成……"（TS 无管理 API 客户端）。
13. **归档**：`docs/fix-plan.md`、`RPC_TIMEOUT.md`、`docs/superpowers/plans/` 移入 `docs/archive/`（用 `git mv` 或文件移动；`README.md` 与 `CLAUDE.md` 中对 `RPC_TIMEOUT.md`/`fix-plan.md` 的引用同步更新为归档路径或删除）。`docs/review/` 保留原位。
14. **删除工作区残留**：`server.exe`（32MB）与 `nul`（误重定向产物）——均未 git 追踪，直接 `rm`。
15. **新建 `.gitattributes`**：仓库内 `.go` 文件全部以 CRLF 存储，为防止跨平台贡献者引入行尾混乱，添加归一化规则（如 `*.go text eol=crlf` 或统一转 LF 的 `* text=auto`——**建议保守选前者**，与现状一致，避免全仓行尾翻动；报告中说明选择）。

16. **启动命令修正**（配置 agent 交接项）：`AGENTS.md` 与 `README.md`（如含）的单文件命令 `go run cmd/server/main.go --config ./config.yaml` 编译失败（main.go 依赖 runtime.go 的 `prepareGRPCServers`）。改为 `go run ./cmd/server --config ./config.yaml`；`go build -o messageloop cmd/server/main.go` 同理改为 `go build ./cmd/server`（`docs/deployment.md:10` 同款命令一并修正）。

## 纪律

- 不做 git commit/push。只改文档与删除指定文件，逐条核对改后表述与代码事实一致。
- 完成后返回报告：每条任务处置、改动文件清单、与其他 agent 的交接项（任务 9 及你发现的新漂移）、遗留问题。
