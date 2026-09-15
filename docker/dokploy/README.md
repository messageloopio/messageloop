# MessageLoop × Dokploy 部署指南

本目录提供 Dokploy（自托管 PaaS，Traefik + Docker Compose）一键部署所需的全部文件：
单 Compose 栈内含 **Redis（broker 持久化）+ messageloop + mlbridge**，域名路由以
Traefik label 直接声明在 compose 内（不走「Domains」UI）。

**mlbridge 集成模型**：messageloop 以 `require_auth: true` 部署，连接认证、按租户
频道策略、凭证撤销传播、用量计量与 send/history 写穿全部委托给栈内的 mlbridge
（ProxyService gRPC，`docker/dokploy/mlbridge.yaml` 挂载接线）；mlbridge 只与
Torchwood 的公开 Server API 通信——部署时需提供 Torchwood 网关地址与项目 API key
（`MLBRIDGE_TORCHWOOD_*` 环境变量）。客户端凭证即 Torchwood access token，
会话命名空间 = Torchwood project id。资源供给（集合/索引）用 mlbridge 仓库的
`runbooks/` 一键应用。

与典型 Web 应用不同的三点，先说清楚：

1. **端口形态**：MessageLoop 暴露四个 TCP 监听（WebSocket 9080 / 客户端 gRPC 9090 /
   admin gRPC 9091 / health+metrics 8080），可选 QUIC/KCP（UDP）。Traefik 只路由
   HTTP——WebSocket 与 gRPC 各配一条域名（TLS 终结），UDP 不走 Traefik；
2. **没有迁移作业**：MessageLoop 无数据库 schema，Redis 键在首节点启动时自举，
   不需要 one-shot 作业链；
3. **镜像只拉不编**：compose 刻意不设 `build:` 段——Dokploy 的 compose
   部署恒带 `--build`，服务一旦有 build 段就会在部署机上源码构建并冒用镜像名
   （GHCR 拉取路径被遮蔽，小内存 VPS 还可能 OOM）。镜像来自 GHCR
   （`ghcr.io/messageloopio/messageloop` 与 `ghcr.io/messageloopio/mlbridge`，
   GitHub Actions docker-publish workflow 随 main/v* 自动发布）。

## 0. 前置条件

- 一台装好 Docker 的服务器 + Dokploy（≥ v0.10）；
- 本仓库可被 Dokploy 访问（GitHub/GitLab/直接 Git URL；私有仓库需配置凭证）；
- 两个指向服务器的域名（如 `ws.example.com` 与 `grpc.example.com`），分别承载
  WebSocket 与客户端 gRPC；admin 面不占域名（仅隧道/栈内访问）。

## 1. 创建 Compose 服务

1. Dokploy 控制台：**Projects → Create Project → Create Service → Docker Compose**；
2. 来源选你的 Git 仓库与分支；
3. **Compose Path** 填：`./docker/dokploy/docker-compose.yml`；
4. 先别急着 Deploy——到 **Environment** 页签粘贴 **`env.dokploy`** 的内容，
   填齐「必填」段五项（其余保持注释即取默认），再回来点 **Deploy**。
   `env.dokploy` 是本部署全部环境变量的收口清单（compose 里每个
   `${VAR}` 引用都能在其中找到）；本地可用
   `docker compose --env-file env.dokploy -f docker-compose.yml config`
   校验插值链——不填必填项会得到对应中文报错，渲染通过即插值完整。

首次部署从 GHCR 拉取镜像（两镜像合计约百 MB 量级，取决于带宽）。

## 2. 环境变量（Environment 页签）

> 逐项清单以 **`env.dokploy`** 为准（下表是说明，不是全集）。必填五项：
> `MESSAGELOOP_SERVER_GRPC_ADMIN_AUTH_TOKEN`、`MESSAGELOOP_WS_DOMAIN`、
> `MESSAGELOOP_GRPC_DOMAIN`、`MLBRIDGE_TORCHWOOD_BASE_URL`、
> `MLBRIDGE_TORCHWOOD_PROJECTS`。

| 变量 | 必填 | 说明 |
|------|------|------|
| `MESSAGELOOP_SERVER_GRPC_ADMIN_AUTH_TOKEN` | ✅ | admin gRPC 的 Bearer token，`openssl rand -hex 32`；未设置拒绝启动（compose 内 `:?` 强制） |
| `MESSAGELOOP_WS_DOMAIN` | ✅ | WebSocket 域名，如 `ws.example.com`（Traefik label 路由，见 §3） |
| `MESSAGELOOP_GRPC_DOMAIN` | ✅ | 客户端 gRPC 域名，如 `grpc.example.com`（TLS 终结 → h2c） |
| `MLBRIDGE_TORCHWOOD_BASE_URL` | ✅ | Torchwood 网关地址。同机 Dokploy 部署走内网直连 `http://torchwood-server:9080`（Torchwood 的 server 容器名；mlbridge 已挂 dokploy-network，别名里没有 `torchwood`）；跨机/独立部署用公网 `https://tw.example.com` |
| `MLBRIDGE_TORCHWOOD_PROJECTS` | ✅ | JSON 数组 `[{"project_id":"myproj","api_key":"sk-..."}]`；key 需含 `users.read`、`databases.read/write`、`runbooks.read/write`、`analytics.write`（资源供给用 mlbridge 仓库 `runbooks/`） |
| `MLBRIDGE_IMAGE` | | mlbridge 镜像引用，默认 `ghcr.io/messageloopio/mlbridge:latest`；建议钉版本 tag |
| `MLBRIDGE_REVALIDATE_INTERVAL` | | 凭证复验环间隔，默认 `5m`（撤销上界 = 复验间隔 + TW 校验缓存 30s） |
| `MLBRIDGE_WRITE_THROUGH_CHANNELS` | | 写穿频道 JSON 数组，默认 `["**"]`（全频道写穿；瞬态频道不会被 send 路径触及） |
| `MLBRIDGE_METERING_ENABLED` / `MLBRIDGE_METERING_FLUSH_INTERVAL` | | 用量计量开关与批量间隔，默认开 / `15s` |
| `MESSAGELOOP_BROKER_REDIS_PASSWORD` | | 栈内 redis 未设密码（仅 `default` 内网可达）；接外部 redis 时改 `MESSAGELOOP_BROKER_REDIS_ADDR` 并配套密码/DB |
| `MESSAGELOOP_BROKER_REDIS_STREAM_MAX_LENGTH` | | 每频道 streams 裁剪上限，默认 `10000` |
| `MESSAGELOOP_TRANSPORT_WEBSOCKET_ALLOW_ALL_ORIGINS` | | 默认 `true`（token 鉴权下 Origin 不是授权边界） |
| `MESSAGELOOP_TRANSPORT_WEBSOCKET_ALLOWED_ORIGINS` | | 收紧来源：逗号分隔列表，**且必须同时设** `…ALLOW_ALL_ORIGINS=false`（allow_all 优先级更高） |
| `MESSAGELOOP_SERVER_HTTP_AUTH_TOKEN` | | /health、/metrics 的 token，默认空（8080 不发布不路由）；若自行发布 8080 必须设置，并同步改 healthcheck |
| `MESSAGELOOP_GRPC_PORT` / `MESSAGELOOP_ADMIN_GRPC_PORT` | | 宿主回环端口，默认 `9090` / `9091`；冲突时改 |
| `MESSAGELOOP_IMAGE` | | 镜像引用，默认 `ghcr.io/messageloopio/messageloop:latest`；建议钉版本 tag |
| `MESSAGELOOP_TRANSPORT_QUIC_ADDR` / `…KCP_ADDR` | | 启用 UDP 传输（默认空=关闭）；还需放开 ports 的 udp 行并配置 TLS（§4.3） |

## 3. 域名绑定（compose Traefik label，不用 Domains UI）

两条 Traefik router/service 已直接以 label 声明在 compose 的 `messageloop` 服务上，
改域名/换环境只动 Environment 变量：

| 变量 | 承载 | 入口 |
|------|------|------|
| `MESSAGELOOP_WS_DOMAIN` | WebSocket（9080；Traefik 原生透传 upgrade） | `wss://<域名>/ws`；80 自动 301 → https |
| `MESSAGELOOP_GRPC_DOMAIN` | 客户端 gRPC（9090；Traefik 终结 TLS 后以 **h2c** 转发——gRPC 要求端到端 HTTP/2） | `<域名>:443` + TLS（见 §4.2） |

HTTPS 同用 Dokploy 内置的 `letsencrypt` 证书解析器。

> ⚠️ **Dokploy「Domains」UI 里不要保留本服务的域名条目**（包括历史添加的）：
> UI 生成的 router 与 label 声明的 router 规则相同，并存时 Traefik 二选一不可控。
> UI 只留空即可；DNS 记录照常指向服务器，与本配置无关。
>
> 插值落空（规则渲染成空 Host）时先检查变量拼写。

## 4. 客户端接入

### 4.1 WebSocket（推荐入口）

```
wss://<WS域名>/ws
```

Go SDK：`sdk.Dial("wss://ws.example.com/ws")`；TypeScript SDK 同理。
路径由 `transport.websocket.path`（默认 `/ws`）决定，可用
`MESSAGELOOP_TRANSPORT_WEBSOCKET_PATH` 改。

### 4.2 gRPC

- **域名 TLS 通道**：`grpc.<域名>:443`，面向自带 TLS 凭据的标准 gRPC 客户端
  （Traefik 终结 TLS，后端 h2c）。
- **仓库 Go SDK 注意**：`sdk.DialGRPC` 当前为明文实现（`insecure` 硬编码），
  走不了 TLS 域名。SDK 用户的接入路径是 **SSH 隧道 + 回环端口**：

  ```bash
  ssh -L 9090:127.0.0.1:9090 <服务器>     # 宿主 9090 已只绑回环
  ```

  ```go
  client, err := sdk.DialGRPC("127.0.0.1:9090")   // 隧道内明文
  ```

  多数客户端场景用 WebSocket 即可（两者承载同一套信封协议）；SDK 的 TLS
  支持跟进后，域名通道即插即用。

### 4.3 QUIC / KCP（UDP，可选）

Traefik 不路由 UDP。启用：Environment 设
`MESSAGELOOP_TRANSPORT_QUIC_ADDR=:4433`（或 KCP 对应项）与 TLS 证书路径
（`MESSAGELOOP_TRANSPORT_QUIC_TLS_CERT_FILE` / `…_KEY_FILE`，自签开发可用
`MESSAGELOOP_TRANSPORT_QUIC_INSECURE=true`），放开 ports 中对应 udp 行再
Redeploy。客户端直连 `宿主IP:端口`（`sdk.DialQUIC` / `sdk.DialKCP`，
KCP 的 FEC 参数需与服务器一致）。

### 4.4 admin gRPC（服务器侧）

仅两条路：栈内网络（`messageloop:9091`，供同栈后端服务调用）或 SSH 隧道
（`ssh -L 9091:127.0.0.1:9091`）。调用必须携带
`authorization: Bearer <MESSAGELOOP_SERVER_GRPC_ADMIN_AUTH_TOKEN>` metadata。

## 5. 验证

```bash
# 容器内健康（broker=redis ready）
docker exec <messageloop容器> wget -q -O - http://127.0.0.1:8080/health
# Redis 自举键（broker epoch 已创建）
docker exec <redis容器> redis-cli --scan --pattern 'ml2:*'
# 浏览器/客户端连通
#   wss://<WS域名>/ws 发起 WebSocket 握手
curl -I http://<WS域名>/            # 301 → https
```

## 6. 栈内行为说明

- **Redis AOF**：compose 已带 `--appendonly yes`。streams 历史（客户端 recover
  依赖）与 `ml2:broker:epoch` 都在 Redis：无持久化重启 = 历史清零；epoch 重建后
  旧客户端携带的偏移不被信任（表现为恢复失败重连，而非错消息）。
- **清空 Redis 的正确姿势**：若手工 flush 了 streams，必须同时删除
  `ml2:broker:epoch`，否则携带旧偏移的客户端会静默跳过消息。
- **命名空间**：所有频道都在 `MESSAGELOOP_SERVER_NAMESPACE` 之下；接入鉴权代理
  （`proxy:` 段，按连接返回 `UserInfo.namespace`）属进阶场景——proxy 后端是
  文件专用配置，需自行挂载 YAML（见下）。
- **文件专用配置**：authorizer 规则与 proxy 后端不进环境变量。需要时在宿主放
  一份 YAML（以 `configs/docker.yaml` 为基线增改），compose 里给 `messageloop`
  加 `volumes: ["./config.yaml:/etc/messageloop/your.yaml:ro"]` 并
  `command: ["--config", "/etc/messageloop/your.yaml"]`。
- **memory broker 替代**：单节点无历史持久化诉求时，可删掉 redis 服务并把
  `MESSAGELOOP_BROKER_TYPE` 改 `memory`（删除 `depends_on`）——重启丢历史，
  且以后不能平滑升级为多节点。

## 7. 日常运维

| 操作 | 做法 |
|------|------|
| 升级 | Dokploy **Redeploy**（重新构建/拉取镜像 → 滚动替换；SIGTERM 触发 30s 优雅排水，连接端收到 `ForceNoReconnect` 后重连即恢复会话） |
| 回滚 | `MESSAGELOOP_IMAGE` 钉到旧 tag → Redeploy（无 schema 迁移，无回滚禁忌） |
| 备份 | Redis 数据卷（`redis_data`）即全部状态：定期 `redis-cli BGSAVE` + 备份 `dump.rdb`，或直接快照卷 |
| 多节点 | 同一 Redis 下扩第二节点：共享 `MESSAGELOOP_BROKER_REDIS_*`，设 `MESSAGELOOP_CLUSTER_ENABLED=true`、每节点唯一 `MESSAGELOOP_CLUSTER_NODE_ID`、共享 ≥32 字节 `MESSAGELOOP_CLUSTER_HMAC_KEY`；两节点须都能被客户端寻址（Dokploy 单 Compose 栈只出一个实例，跨栈部署时两栈指同一外部 Redis） |
| 看指标 | 未发布 8080：`ssh -L 8080:127.0.0.1:8080` 后抓 `127.0.0.1:8080/metrics`；Prometheus 常驻抓取建议在栈内跑 exporter 侧抓或发布 8080 并配 token |

## 8. 构建与镜像

- **默认（GHCR 预构建）**：GitHub Actions `docker-publish` workflow 随 main
  push 发布 `ghcr.io/messageloopio/messageloop:latest`（+ `main`/sha 标签）、
  随 `v*` tag 发布 semver 标签；mlbridge 镜像同理。Redeploy 只拉不编；回滚 =
  Environment 把 `MESSAGELOOP_IMAGE` / `MLBRIDGE_IMAGE` 钉到旧 tag。
  ⚠ 两个镜像建议成对升级（桥与内核共享 proxy 协议契约，见 mlbridge
  docs/integration.md §3）。
- **为什么不设 `build:` 段**：Dokploy 对 Compose 部署恒带
  `up -d --build`，服务带 build 段就会每次部署在服务器上 `go build` 并把产物
  打成 GHCR 的镜像名（拉取路径被遮蔽；⚠ ≤1GB 内存 VPS 上 OOM 表现为部署
  cancelled）。要本地出镜像，在开发机 `docker build .` 后推送自己的引用，
  再用 `MESSAGELOOP_IMAGE` / `MLBRIDGE_IMAGE` 指向它。

## 9. 文件清单

| 文件 | 用途 |
|------|------|
| `docker-compose.yml` | 全栈编排（redis + messageloop + mlbridge）+ Traefik 域名路由（相对路径均相对本目录） |
| `mlbridge.yaml` | messageloop 挂载的桥接线配置（proxy 块 + require_auth，config-file-only 键） |
| `env.dokploy` | Dokploy Environment 变量模板（必填占位 + 可选默认；本地 `--env-file` 校验源） |
| `README.md` | 本指南 |
| 仓库根 `Dockerfile` | 镜像构建（内置容器默认配置 `configs/docker.yaml`） |
| `cmd/server/envconfig.go` | `MESSAGELOOP_*` 环境变量覆盖的完整键表 |
