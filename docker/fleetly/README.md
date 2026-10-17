# MessageLoop × mlbridge —— fleetly 部署指南（新平台形态，F1.15 dogfooding）

| 状态 | 日期 | 关联 |
|---|---|---|
| 现行（staging 双节点现役） | 2026-10-02 | fleetly ADR-0033/0034（compose intake/服务名别名）、ADR-0013（跨 Project 网络 peer）；实录见 fleetly 仓 docs/runbooks/staging-fleetly.md |

单 app 三服务（redis + messageloop + mlbridge），无部署期作业（数据面是
栈内 Redis AOF，无迁移）。本文件取代归档仓 v0.1 形态 README（`fleetly
env set`/`configs set`/`domains add` 等旧面均不存在）。

## 1. 形态总览

| 项 | 形态 |
|---|---|
| app 名 | `messageloop`（项目 = `messageloop`；跨 Project 互访 torchwood 走 F1.8 peer） |
| 服务 | `redis`（AOF 卷 = streams 历史 + broker epoch）、`messageloop`（WS 9080 / 客户端 gRPC 9090 / Server API 9091 / admin 8080 token 面，registry 目录 API 所在）、`mlbridge`（ProxyService gRPC 9070，栈内私有，经 registry 自注册） |
| 网络 | 项目网 `default` + mlbridge 跨挂 torchwood 项目网（`project:<TW_PROJECT_ID>/default`，peer declare/approve 后生效——compose 占位部署时渲染） |
| 密钥 | Project Secret 文件注入 + sh -c 包装（`ml-api-tokens`=`MESSAGELOOP_SERVER_API_AUTH_TOKENS` 与 `MLBRIDGE_MESSAGELOOP_API_TOKEN` 同值；`ml-http-token`=`MESSAGELOOP_SERVER_HTTP_AUTH_TOKEN` 与 `MLBRIDGE_REGISTRY_TOKEN` 同值，admin 面鉴权 + 注册面同一张票；`ml-tw-projects` = torchwood 项目 JSON） |
| 配置 | `mlbridge.yaml`（proxy 接线，config-file-only 键）经 **Secret 文件**投递：`--config /run/secrets/mlbridge-yaml`。fleetly 的 Config 挂载面（config_refs）运行期材料化未落地（挂账），Secret 文件面是等价投递通道（值非敏感，interim 形态） |
| 入口 | Route 三条：`ml.dev.fleetly.run`（9080 http，WS upgrade 透传 `/ws`）+ `ml-grpc.dev.fleetly.run`（9090 h2c）+ `ml-api.dev.fleetly.run`（9091 h2c）；TLS 平台边缘终结（LE staging CA） |

## 2. 从零部署（命令序）

```sh
P=$(fleetly projects create messaging --json | jq -r .project.id)
fleetly networks create --project $P default
fleetly volumes create --project $P redis_data

TOK="$(openssl rand -hex 32)"
fleetly secrets put --project $P --value "$TOK" ml-api-tokens
HTTPTOK="$(openssl rand -hex 32)"
fleetly secrets put --project $P --value "$HTTPTOK" ml-http-token
# torchwood 项目接线（api_key 来自 torchwood Console 的 scoped key）
fleetly secrets put --project $P --value '[{"project_id":"<tw-proj>","api_key":"<sk-...>"}]' ml-tw-projects
fleetly secrets put --project $P --value "$(cat docker/fleetly/mlbridge.yaml)" mlbridge-yaml

A=$(fleetly apps create --project $P messageloop --json | jq -r .app.id)

# 跨 Project 挂靠：torchwood 项目 default 网（declare 由挂靠方发起、接收方批准）
TW_NET=$(fleetly networks create --project <TW_PROJECT_ID> default --json | jq -r .network.id)   # torchwood 侧已建则跳过
fleetly networks declare --network $TW_NET --project $P
# torchwood 侧：fleetly networks approve <PEER_ID>
# 渲染 compose 占位后部署（__TW_PROJECT_ID__ → torchwood 项目 ID）
sed "s/__TW_PROJECT_ID__/$TW_PROJECT_ID/" docker/fleetly/docker-compose.yml > /tmp/ml-compose.yaml
fleetly deploy --app $A --compose-file /tmp/ml-compose.yaml --wait

fleetly routes create --project $P --host ml.dev.fleetly.run --app $A --process messageloop --port 9080 --protocol http
fleetly routes create --project $P --host ml-grpc.dev.fleetly.run --app $A --process messageloop --port 9090 --protocol h2c
fleetly routes create --project $P --host ml-api.dev.fleetly.run --app $A --process messageloop --port 9091 --protocol h2c
```

## 3. 与 dokploy 现役栈的差异

1. 无 80→443 重定向；无宿主回环端口（9090/9091 不发布——gRPC 走域名
   h2c 直连，SDK `DialGRPC` TLS）；
2. depends_on 显式拒：redis 未就绪的秒级窗口内 messageloop 崩溃重启自愈；
3. 密钥是文件（显式 secrets 列表）不是 app 级全服务 env；
4. mlbridge.yaml 从 bind mount / Config 资源改为 Secret 文件（§1 interim）。

## 4. 镜像纪律

钉 sha tag：`ghcr.io/messageloopio/{messageloop,mlbridge}:sha-<short>`；
升级 = 改 tag 重部署；两镜像**成对升级**（桥与内核共享 proxy 协议契约）。

## 5. 文件清单

| 文件 | 用途 |
|---|---|
| `docker-compose.yml` | 三服务受控子集（含跨 Project 网络占位） |
| `mlbridge.yaml` | Secret 投递源（proxy/registry 接线 config-file-only 键；与 docker/dokploy/ 同步义务至 dokploy 退役为止） |
| `cutover-runbook.md` | v0.1 割接 runbook（历史参考；实录以 fleetly 仓 runbook 为准） |

## 6. registry 服务注册发现（messageloop ≥ v1.4.0-rc.2 / mlbridge ≥ registry 支持版）

proxy 从静态 `endpoint: mlbridge:9070` 切到 `service: mlbridge`：mlbridge
启动时向 admin 面 `/registry/v1` 目录 API 自注册（TTL 30s、心跳 10s、
SIGTERM 排水相先注销），messageloop 经 Rendezvous hash 按实例动态解析。
本文件三处改动即全部接线：

1. `mlbridge.yaml`（messageloop 侧）：`registry.enabled: true` + proxy 条目
   `endpoint` → `service: mlbridge`（config-file-only 键，Config 重传滚动生效）；
2. compose：`MESSAGELOOP_SERVER_HTTP_ADDR` 0.0.0.0:8080 +
   `MESSAGELOOP_SERVER_HTTP_AUTH_TOKEN`（G5 fail-closed；admin 面不经
   平台 Route 对外，仅栈内 mlbridge 访问），healthcheck 带 Bearer 头；
3. compose：mlbridge 侧 `MLBRIDGE_REGISTRY_*` 四键（address 显式给 compose
   服务名 `mlbridge:9070`——默认 hostname 是容器 ID，跨容器不可解析）。

**在役栈升级序**（现役 R1 → registry 形态；成对升级纪律不变）：

```sh
HTTPTOK="$(openssl rand -hex 32)"
fleetly secrets put --project $P --value "$HTTPTOK" ml-http-token
fleetly secrets put --project $P --value "$(cat docker/fleetly/mlbridge.yaml)" mlbridge-yaml
# 两镜像升到 registry 支持版（messageloop ≥ rc.2 / mlbridge ≥ registry 支持版）改 sha tag 后：
fleetly deploy --app $A --compose-file /tmp/ml-compose.yaml --wait
# 验证：GET http://<messageloop>:8080/registry/v1/services/mlbridge（带 Bearer）应列出 mlbridge 实例
```

升级窗口语义：新 messageloop 起来前 mlbridge 注册会 fail-fast 重启（编排层
退避自愈）；老 messageloop 无 `/registry/v1` 会 404——先升 messageloop、
后升 mlbridge，或一次性 `--wait` 全栈滚动。
