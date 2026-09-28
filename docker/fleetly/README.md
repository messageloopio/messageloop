# MessageLoop × mlbridge —— fleetly 部署指南

本目录是 messageloop 栈的 **fleetly（受控子集平台）部署形态**：单 app 三服务
（redis + messageloop + mlbridge），域名经平台域名资源 API 声明、运行时配置经
平台 Config 资源提供、密钥与环境相关值经平台 env 注入。`docker/dokploy/` 的
现役 Dokploy 栈在割接验收前**并行保留**（回滚 = dokploy 栈未拆）。

- 受控子集校验（零告警）：`fleetly validate docker/fleetly/docker-compose.yml`
- 割接执行（前置检查 → 部署 → 数据面 → 验收 → 回滚）：[`cutover-runbook.md`](cutover-runbook.md)
- 真机段（DNS 切换、acme 签发、WebSocket/gRPC/API 域名连通、recover 探针）
  在本环境无 staging 凭据/访问权，均标注「待使用者执行窗口」，未虚构结果。

## 1. 部署形态总览

| 项 | 形态 |
|---|---|
| app 名 | `messageloop`（compose 顶层 `name`；应用随首次部署自动创建） |
| 服务 | `redis`（AOF 历史 + broker epoch）、`messageloop`（WS 9080 / 客户端 gRPC 9090 / Server API 9091 / health 8080 回环）、`mlbridge`（ProxyService gRPC 9070，栈内私有） |
| 网络 | 平台 per-app overlay；**服务别名 = compose 服务名**（`redis` / `messageloop` / `mlbridge` 按名互访，与 dokploy 栈内 DNS 语义一致） |
| 入口 | 三域名（见 §3）：WS `http` 9080、客户端 gRPC `h2c` 9090、Server API gRPC `h2c` 9091；TLS 在平台边缘终结（ACME HTTP-01） |
| 配置 | compose 字面量（非敏感基线）+ 平台 env（密钥/环境值，平台层覆盖 compose）+ Config 资源 `mlbridge.yaml`（config-file-only 的 proxy 接线） |
| 镜像 | `ghcr.io/messageloopio/messageloop:sha-b009c71`、`ghcr.io/messageloopio/mlbridge:sha-39d254d`（裁决见 §5） |
| 数据面 | `redis_data` 命名卷（割接回灌/清零选择见 runbook §3） |

与 dokploy 形态的三个**行为差异**（割接验收时如实核对）：

1. **无 80→443 重定向**（平台 v0.1 明确不做，OT-2 裁决）：客户端须直接使用
   `https://` / `wss://`；dokploy 的 Traefik 中间件行为在本形态不存在；
2. **无宿主回环端口**（9090/9091 不发布）：gRPC 域名直连是主通道（SDK
   `DialGRPC` TLS 已随 IMPL-T1-6 落地），SSH 隧道兜底退回 T3-1 opt-in；
3. **编排顺序**：`depends_on` 被受控子集拒绝，顺序归平台发布管线；三服务
   单次发布并行创建，redis 未就绪的秒级窗口内 messageloop/mlbridge 可能
   崩溃重启并自愈（诚实标注：启动顺序语义的量化基线归 T2-0④ spike）。

## 2. 改写清单（dokploy 现状 → fleetly 终态）

逐行核对 `docker/dokploy/docker-compose.yml` 与受控子集白名单
（`internal/compose/validate.go` + `testdata/whitelist.golden`）后的终态；
「承接面」= 该诉求在平台由哪个 API/资源/规则承载。

| # | dokploy 现状 | fleetly 终态 | 承接面 |
|---|---|---|---|
| 1 | 顶层 `name: messageloop` | 保留 | app 标识（命名公式 `fleetly-<team>-<prj>-messageloop-<service>`） |
| 2 | `x-app-env` / `x-bridge-env` YAML 锚点 | 内外联为各服务 `environment` | 插值禁用后锚点无共享需求；`x-*` 扩展键平台侧不可见（loader 移入 Extensions） |
| 3 | redis `image: redis:7-alpine` | 保留（tag 形态，见 §5） | 部署时 tag→digest 解析（DT-2） |
| 4 | redis `command: --appendonly yes` | 保留 | AOF 持久化 = streams 历史 + `ml2:broker:epoch` 载体 |
| 5 | redis `volumes: redis_data:/data` + 顶层 `volumes:` | 保留 | 平台卷注册表（`fleetly-<app>-redis_data-<appid8>`），`fleetly placement show` 可见 |
| 6 | redis `healthcheck` | 保留探针本体 | 平台健康门（interval/timeout/retries/start_period 走平台缺省 5s/3s/3/10s） |
| 7 | redis `restart: unless-stopped` | **删除** | 平台重启策略缺省 `any`/delay 5s（服务级 `deploy.restart_policy` 可表达） |
| 8 | messageloop `image: ${MESSAGELOOP_IMAGE:-…:latest}` | 字面 sha tag | 改镜像 = 改 compose 重新部署（平台 env 不参与 compose 插值） |
| 9 | messageloop `pull_policy: always` | **删除** | DT-2：部署时 tag→digest 钉定 + Redeploy 重解析 = `always` 等价物（§5） |
| 10 | messageloop `command: ["--config", "/etc/messageloop/mlbridge.yaml"]` | **改全命令形态** `["/usr/local/bin/messageloop", "--config", …]` | 平台 `command` = 覆盖镜像 ENTRYPOINT（镜像 CMD 才是 `--config …`）；只写参数会以 `--config` 为 entrypoint 启动失败（IMPL-T2-4 复核发现并修正） |
| 11 | messageloop `volumes: ./mlbridge.yaml:…:ro`（bind） | **删挂载，改 Config 资源** | 顶层 `configs: {mlbridge.yaml: external: true}` + 服务级 `{source, target}`（OT-3/IMPL-T1-4） |
| 12 | messageloop `networks: [default, dokploy-network]` | **全删** | 平台为 app 建专属 overlay；别名 = compose 服务名（external 网络在拒绝清单） |
| 13 | Traefik label ×13（三 router/service + 80 跳转） | **全部删除** | 域名资源 API：三条 `fleetly domains add`（§3）；80→443 不做 |
| 14 | messageloop `ports`（回环 9090/9091）+ QUIC/KCP 注释行 | **删除** | 宿主端口发布退回 T3-1（P2 opt-in）；gRPC 走域名 TLS |
| 15 | messageloop `environment` 全量 `${VAR}` 插值 | 字面量基线 + 密钥/环境值上平台 env | `fleetly env set`（平台层>compose 层，同键覆盖出 `W_ENV_PLATFORM_OVERRIDE` 警告） |
| 16 | messageloop `depends_on: redis healthy` | **删除** | 发布管线管顺序（校验拒绝 `depends_on`，落点见 §1 差异 3） |
| 17 | mlbridge `image: ${MLBRIDGE_IMAGE:-…:latest}` | 字面 sha tag | 同 #8 |
| 18 | mlbridge `pull_policy: always` | **删除** | 同 #9 |
| 19 | mlbridge `environment` 全量 `${VAR}` 插值 | 字面量基线 + 密钥上平台 env | 同 #15 |
| 20 | mlbridge `networks: [default, dokploy-network]` | **全删** | 同 #12（mlbridge→torchwood 暂走公网，T2 后切项目内网） |
| 21 | mlbridge `depends_on: redis/messageloop healthy` | **删除** | 同 #16；mlbridge 探针深度（TCP 存活）见 §6 |
| 22 | 顶层 `networks: dokploy-network: external: true` | **删除** | external 网络在拒绝清单（平台建网） |
| 23 | 顶层 `volumes: redis_data:` | 保留 | 同 #5 |
| 24 | `env.dokploy` 插值守卫/变量模板 | 平台 env 清单（§4）+ 本 README | 插值在平台禁用；必填经平台 env 注入（fail-closed 在应用侧） |
| 25 | SSH 隧道运维（README §4.4/§7） | 域名直连 + 平台 CLI（§7） | Server API 域名 `h2c` 直连（`authorization: Bearer`），metrics 走平台观测面 |

环境变量逐键映射（dokploy `env.dokploy` → fleetly 落点）：

| dokploy 变量 | fleetly 落点 | 说明 |
|---|---|---|
| `MESSAGELOOP_SERVER_API_AUTH_TOKENS` | **平台 env（必填）** | `openssl rand -hex 32`；缺失 messageloop fail-closed 拒绝启动 |
| `MESSAGELOOP_WS_DOMAIN` / `_GRPC_DOMAIN` / `_API_DOMAIN` | 域名资源（§3） | 域名不再经 label 插值 |
| `MESSAGELOOP_IMAGE` / `MLBRIDGE_IMAGE` | compose 字面（§5） | 升级 = 改 compose 提交 + 部署 |
| `MESSAGELOOP_GRPC_PORT` / `_API_GRPC_PORT` | **删除** | 无宿主端口 |
| `MESSAGELOOP_SERVER_HTTP_AUTH_TOKEN` | 平台 env（可选） | 仅当 8080 改非回环发布时必填 |
| `MESSAGELOOP_BROKER_REDIS_PASSWORD` / `_DB` | 平台 env（可选） | 栈内 redis 无密码、db 0（缺省一致，可按需覆盖） |
| `MESSAGELOOP_BROKER_REDIS_STREAM_MAX_LENGTH` | compose 字面 `"10000"` | 可按需改平台 env 覆盖 |
| `MESSAGELOOP_TRANSPORT_WEBSOCKET_ALLOW_ALL_ORIGINS` / `_ALLOWED_ORIGINS` | compose 字面 / 平台 env | 缺省放开；收紧需同时把 ALLOW_ALL 覆盖为 false |
| `MLBRIDGE_TORCHWOOD_BASE_URL` | 平台 env（**必填，覆盖 compose 占位**） | T1 临时公网网关；T2 后改项目内网别名 `http://torchwood-server:9080` |
| `MLBRIDGE_TORCHWOOD_PROJECTS` | **平台 env（必填）** | JSON 数组；缺失 mlbridge fail-closed 拒绝启动 |
| `MLBRIDGE_MESSAGELOOP_API_TOKEN` | **平台 env（必填）** | 与 `MESSAGELOOP_SERVER_API_AUTH_TOKENS` 同值（dokploy 靠 `${}` 复用，fleetly 靠两次 `env set`） |
| `MLBRIDGE_REVALIDATE_INTERVAL` / `_METERING_*` / `_WRITE_THROUGH_*` / `MLBRIDGE_REDIS_DB` | compose 字面或平台 env | 与 dokploy 同默认值；写穿四键在 compose 显式标注 |

## 3. 域名声明（三条命令）

平台不再有 Traefik label；域名是 state 资源，经 CLI/API 写入并经入口收敛即时生效
（OT-2；同一 host 全局独占）。三个域名的 `service` 都是 compose 服务名 `messageloop`，
端口/协议逐条不同：

```bash
# 变量（按环境替换）：FLEETLY_ADDR/FLEETLY_TOKEN/FLEETLY_PROJECT 见 cutover-runbook.md §0
fleetly domains add --service messageloop --port 9080 --protocol http  messageloop "<ws-host>"    # WebSocket：wss://<ws-host>/ws
fleetly domains add --service messageloop --port 9090 --protocol h2c   messageloop "<grpc-host>"  # 客户端 gRPC：TLS 终结 → h2c 9090
fleetly domains add --service messageloop --port 9091 --protocol h2c   messageloop "<api-host>"   # Server API gRPC：TLS 终结 → h2c 9091
fleetly domains list   messageloop
fleetly domains verify messageloop   # 平台侧解析 + 80/443 探测 + 证书材料（真机窗口执行）
```

- `--protocol http` 是纯 HTTP 后端（WebSocket upgrade 由边缘原生透传，路径 `/ws`）；
  `h2c` 表示 TLS 终结后以明文 HTTP/2 回源（gRPC 要求端到端 HTTP/2）；
- `--cert-mode http01`（缺省）：证书经 ACME HTTP-01 签发，**签发前提 = 域名解析
  已指向 fleetly 边缘**（DNS 切换与证书签发同在割接窗口内完成）；
- 上限不变（≤5/服务、≤10/app）；host 冲突 409 点名。

## 4. 平台 env 与 Config 清单

```bash
# —— 必填平台 env（密钥面；值不进对话/日志/仓库）——
fleetly env set messageloop MESSAGELOOP_SERVER_API_AUTH_TOKENS "$(openssl rand -hex 32)"
fleetly env set messageloop MLBRIDGE_MESSAGELOOP_API_TOKEN  "<同上值>"
fleetly env set messageloop MLBRIDGE_TORCHWOOD_BASE_URL     "https://<torchwood-公网网关>"
fleetly env set messageloop MLBRIDGE_TORCHWOOD_PROJECTS     '[{"project_id":"<proj>","api_key":"<sk-...>"}]'
fleetly env list messageloop

# —— Config 资源（值明文可回读；内容变更 = 新对象 + 引用服务滚动）——
fleetly configs set messageloop mlbridge.yaml --from-file docker/fleetly/mlbridge.yaml
fleetly configs ls messageloop
```

注意：

- **平台 env 是 app 级**（注入到每个服务；MESSAGELOOP_* 与 MLBRIDGE_* 互不
  识别的前缀在对方容器内天然惰性），同键覆盖 compose 层并在部署输出
  `W_ENV_PLATFORM_OVERRIDE` 警告（可见可查）；
- **首次部署的鸡与蛋**：应用随首次部署创建，而 env/Config 写入需要应用已存在
  → 首部署预期失败（缺失必填 env 时 messageloop/mlbridge fail-closed），失败后
  按本清单写入，再部署即成功（runbook §2 有逐步命令与预期输出）；
- `mlbridge.yaml` 的 config-file-only 语义不变（`proxy` 块 + `require_auth`）；
  与 `docker/dokploy/mlbridge.yaml` 的同步义务见该文件头注。

## 5. 镜像引用形态裁决（tag vs digest）

**裁决：compose 钉 sha tag（`ghcr.io/messageloopio/{messageloop,mlbridge}:sha-<short>`），
不用 `latest`、不用裸 digest。** 依据与操作口径：

- **平台已把「可变引用」收敛为部署期钉定**（DT-2）：部署时经 registry API 把
  tag 解析为 digest、以 digest 进 revision spec 与漂移对账；**Redeploy 重解析
  = `pull_policy: always` 的干净等价物**——因此 compose 里写 tag 不丢
  reproducibility（revision 记录的是 digest）；
- **不用 `latest`**：Redeploy 会静默换版（与 dokploy 现役 `always` 相同的
  风险面），部署形态应显式表达「部署哪个版本」；
- **不用 digest 字面**：`sha-<short>` tag 已一一对应 commit（不可变约定），
  可读可追溯；digest 字面会让「升级 = 查 digest」把日常 patch 升级复杂化，
  而平台记录 digest 已满足审计；
- 割接锚点（2026-09-28 经 GHCR manifest 查证）：
  `messageloop:sha-b009c71` → `sha256:19ba81f02f131799dbb47ca8d1ee03421f17a75881ef36e8731151db0ddcc8cc`（main 含 IMPL-T1-6）；
  `mlbridge:sha-39d254d` → `sha256:153cf41c2293efda83ca5ff203cee369d79c5e79ab481a81477a2fbc41993170`（main tip，`latest` 亦指向同一 digest）；
- 硬冻结形态（需要时逐行替换为 digest 引用，免部署期解析、airgap 快路径）：
  `image: ghcr.io/messageloopio/messageloop@sha256:19ba81f02f131799dbb47ca8d1ee03421f17a75881ef36e8731151db0ddcc8cc`
- 升级/回滚：改 compose 的 tag → `fleetly deploy`；回滚 = 改回旧 tag 重部署，
  或 `fleetly rollback`（revision 级，见 runbook §8）；
- 两镜像**成对升级**（桥与内核共享 proxy 协议契约，dokploy README §8 同款纪律）。

## 6. 验收探针与诚实标注

验收探针的逐步命令与断言在 [`cutover-runbook.md`](cutover-runbook.md) §6：
`fleetly domains verify`（解析/80/443/证书 SAN）→ WS 域名往返（SDK `wss://`）
→ 客户端 gRPC 域名往返（SDK `DialGRPC` + `WithTLS()`）→ Server API 域名可达
→ DT-8 数据面探针（epoch 相等 + 历史在场 + 客户端携偏移重连被信任）。

诚实标注（不隐藏的平台缺口）：

- **mlbridge 健康门 = TCP 存活**（本目录新增的 `nc -z` 探针）：镜像无 HTTP
  健康端口，gRPC health 服务（`grpc.health.v1`）在容器内无探针客户端；配置
  类故障（如 Torchwood 地址不可达）不阻断部署健康门，靠 runbook §6 端到端探针暴露；
- **无 80→443 重定向 / 无宿主管端口**：见 §1 行为差异；
- **编排顺序自愈窗口**：同 §1 差异 3，量化归 T2-0④；
- **mlbridge→torchwood 暂走公网**：`MLBRIDGE_TORCHWOOD_BASE_URL` 是 T1 的
  临时形态；torchwood 落 fleetly 项目网（T2）后单 env 改动切内网别名。

## 7. 日常运维（fleetly 形态）

| 操作 | 做法 |
|---|---|
| 升级 | 改 `docker-compose.yml` 镜像 tag → `fleetly deploy docker/fleetly/docker-compose.yml` |
| 回滚 | `fleetly rollback messageloop`（revision 级）或改回旧 tag 重部署；dokploy 栈未拆 = 终极回滚 |
| 看状态 | `fleetly apps get messageloop` / `fleetly deployments list messageloop` / `fleetly placement show messageloop`（卷与节点） |
| 看日志 | `fleetly logs history messageloop --limit 100`（服务归因；metrics 走平台观测面，8080 不发布） |
| 备份 | redis 数据卷 = 全部状态：`redis-cli BGREWRITEAOF` 后快照 `fleetly-<app>-redis_data-<appid8>` 卷（或 `BGSAVE` 取 RDB；注意 RDB-only 在 AOF 形态下不可直接启动，见 runbook 附录） |
| 配置轮换 | `fleetly configs set …` 后对服务的下次部署生效（内容变 = 滚动） |
| 密钥轮换 | `fleetly env set …` 后 Redeploy（pending → 生效） |

## 8. 文件清单

| 文件 | 用途 |
|---|---|
| `docker-compose.yml` | fleetly 受控子集编排（redis + messageloop + mlbridge） |
| `mlbridge.yaml` | Config 资源上传源（messageloop 的 proxy 接线；config-file-only 键） |
| `cutover-runbook.md` | 割接 runbook（前置检查/部署/DT-8 数据面/验收/回滚；真机段标注待窗口） |
| `sdks/go/example/recoverprobe/` | 割接 recover 探针（runbook §3.1.0/§6.2 调用，本地三态实证随 §4 记录） |
| `README.md` | 本指南（改写清单、域名/env/Config、镜像裁决） |
| 仓库根 `Dockerfile`、`configs/docker.yaml` | 镜像与内置默认配置（未变） |
| `docker/dokploy/` | 现役 Dokploy 形态（割接验收前并行保留） |
