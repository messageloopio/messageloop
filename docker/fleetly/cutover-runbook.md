# MessageLoop 栈割接 runbook（dokploy → fleetly）

| 状态 | 日期 | 关联 |
|---|---|---|
| **待使用者执行窗口**（本环境无 staging 凭据/访问权，真机步骤未执行、未虚构结果；本地等价实证见附录 A/B） | 2026-09-28 | [README](README.md)（部署形态/改写清单/镜像裁决）；[Dokploy 形态基准](../dokploy/README.md)（§10 fleetly 章） |

范围：把现役 Dokploy 栈（redis + messageloop + mlbridge）割接到
`docker/fleetly/` 的 fleetly 部署形态。**dokploy 栈并行保留至验收**，
回滚 = 旧栈未拆（§8）。以下命令中 `<...>` 为占位符，按实际环境替换；
凭据只经环境变量传递，不进对话/日志/仓库。

前置阅读：[`README.md`](README.md) §2（改写清单）、§3（域名）、§4（env/Config）、
§5（镜像引用裁决）。

## 0. 前置检查（窗口前完成）

```bash
# 0.1 控制面凭据与项目上下文（真机窗口由使用者提供）
export FLEETLY_ADDR="<fleetlyd gRPC 地址>"          # 等价 --addr
export FLEETLY_TOKEN="<API token>"                  # 等价 --token；或 fleetly auth login
export FLEETLY_PROJECT="<team>/<prj>"               # 项目上下文（裸名或 team/prj）
# 控制面 TLS 时：export FLEETLY_TLS=true（或 --tls / --tls-insecure）

fleetly apps list                                  # 控制面可达；空列表合法
fleetly validate docker/fleetly/docker-compose.yml  # 文本形态应与本次仓库版本一致
# 预期：messageloop: valid (spec_hash …, 3 services, 1 volumes)，零告警

# 0.2 旧栈盘点（在 Dokploy 服务器上；容器名以 docker ps 实际为准）
OLD_REDIS="<dokploy redis 容器名>"
docker exec "$OLD_REDIS" redis-cli GET ml2:broker:epoch    # 记录 OLD_EPOCH（§3 还要用）
docker exec "$OLD_REDIS" redis-cli DBSIZE
docker exec "$OLD_REDIS" redis-cli --scan --pattern 'ml2:stream:*' | head -n 5

# 0.3 DNS：三个生产域名 TTL 预先调低（建议 ≤300s，切割前 ≥1 个 TTL 提前做）；
#     记录当前 A/AAAA（回滚回切要用）。
dig +short "<ws-host>"
dig +short "<grpc-host>"
dig +short "<api-host>"
```

占位符基线（本仓默认值，按环境替换）：`<ws-host>` / `<grpc-host>` / `<api-host>`
= 现役 Dokploy 三域名；`<namespace>` = Torchwood 项目 id（会话命名空间，
客户端频道必须带 `ns:` 前缀）。

## 1. 作业顺序总览

| 步 | 章节 | 内容 | 真机窗口 |
|---|---|---|---|
| B | §2 | 首部署（预期失败：应用创建 + 全栈 scale=0 保留现场） | 是 |
| C | §2 | 平台 env 写入 + Config 上传 | 是 |
| D | §3 | DT-8 数据面（旧栈冻结 → 回灌二选一） | 是 |
| E | §4 | 正式部署（新栈起，redis 载入种子数据） | 是 |
| F | §5 | 域名声明 + DNS 切换 + 证书收敛 | 是 |
| G | §6 | 验收探针（数据面/域名/客户端 recover） | 是 |
| H | §7 | 割接记录（二选一必填） | 是 |
| I | §8 | 回滚预案（仅故障时执行） | 按需 |

## 2. 部署（两阶段 bootstrap；首次失败是预期行为）

**为什么两阶段**：应用随首次部署自动创建，而 `env set` / `configs set` /
`domains add` 都要求应用已存在；同时 messageloop（缺
`MESSAGELOOP_SERVER_API_AUTH_TOKENS`）与 mlbridge（缺
`MLBRIDGE_TORCHWOOD_PROJECTS`）都 fail-closed 拒绝启动——首部署必然失败。
失败后平台把全栈置 **scale=0 保留现场**（`substrate_halted`），这正是 §3
回灌的操作窗口。

```bash
# B. 首部署（预期失败）
fleetly deploy docker/fleetly/docker-compose.yml
# 预期：约 5 分钟（看门狗预算）后 deployment … failed  error=E_HEALTH_TIMEOUT；
#       messageloop/mlbridge 崩溃重启属预期（缺必填 env）。检查方式：
fleetly deployments list messageloop
fleetly logs history messageloop --service messageloop --limit 50   # 可看到 auth_tokens 缺失的拒绝启动原因
# 若不想等满看门狗：观察到失败原因后提前收口（未切流可取消；首发取消同样 scale=0）：
# fleetly deployments cancel <deployment-id>
fleetly apps get messageloop        # app 已存在（derived_state=down）

# C. 平台 env（密钥面；此处 ML_API_TOKEN 与 MESSAGELOOP token 必须一致）
ML_API_TOKEN=$(openssl rand -hex 32)
fleetly env set messageloop MESSAGELOOP_SERVER_API_AUTH_TOKENS "$ML_API_TOKEN"
fleetly env set messageloop MLBRIDGE_MESSAGELOOP_API_TOKEN "$ML_API_TOKEN"
fleetly env set messageloop MLBRIDGE_TORCHWOOD_BASE_URL "https://<torchwood 公网网关>"
fleetly env set messageloop MLBRIDGE_TORCHWOOD_PROJECTS '[{"project_id":"<proj>","api_key":"<sk-...>"}]'
fleetly env list messageloop        # 四项在场；status=pending（随下次部署生效）

# Config 资源（内容源 = 仓库内 docker/fleetly/mlbridge.yaml）
fleetly configs set messageloop mlbridge.yaml --from-file docker/fleetly/mlbridge.yaml
fleetly configs ls messageloop
```

> `MLBRIDGE_TORCHWOOD_BASE_URL` 是 T1 临时公网形态；T2（torchwood 落 fleetly
> 项目网）后改 `http://torchwood-server:9080`，单 env 改动、重新部署生效。

## 3. DT-8 数据面（二选一，不许默认静默）

先确认新栈处于 scale=0 窗口（§2 首部署失败后）：

```bash
fleetly apps get messageloop
docker ps --format '{{.Names}}' | grep 'fleetly-.*-messageloop' || true   # 预期无输出（全栈 0 副本）
```

### 3.1 回灌（推荐）：冻结旧栈 → 整目录复制 → 灌入新卷

```bash
# 3.1.0 割接前基线与客户端游标（在旧栈、停止前执行）
docker exec "$OLD_REDIS" redis-cli BGREWRITEAOF
# 重复 INFO 直到 aof_rewrite_in_progress:0 且 aof_last_bgrewrite_status:ok
docker exec "$OLD_REDIS" redis-cli INFO persistence | grep -E 'aof_rewrite_in_progress|aof_last_bgrewrite_status'
OLD_EPOCH=$(docker exec "$OLD_REDIS" redis-cli GET ml2:broker:epoch)
echo "$OLD_EPOCH"
OLD_XLEN=$(docker exec "$OLD_REDIS" redis-cli XLEN 'ml2:stream:<namespace>:<sample-channel>')
echo "$OLD_XLEN"
# 注意：<sample-channel> 选一个真实业务频道（避开 cutover.probe——探针自身会往它写记录消息）。

# 客户端游标记录（在旧栈上；record 阶段回显 ML_PROBE_OFFSET）
cd sdks/go
ML_PROBE_URL="wss://<ws-host>/ws" \
ML_PROBE_TOKEN="$TW_ACCESS_TOKEN" \
ML_PROBE_CHANNEL="<namespace>:cutover.probe" \
go run ./example/recoverprobe record
cd ../..

# 3.1.1 冻结旧栈写入（redis 保持运行；messageloop 会 SIGTERM 优雅排水）
docker stop "<dokploy mlbridge 容器>" "<dokploy messageloop 容器>"
# 3.1.2 停止写入后再压缩一次 AOF，然后复制（复制期间不再有写入）
docker exec "$OLD_REDIS" redis-cli BGREWRITEAOF
# 重复 INFO 直到 aof_rewrite_in_progress:0 且 aof_last_bgrewrite_status:ok
docker exec "$OLD_REDIS" redis-cli INFO persistence | grep -E 'aof_rewrite_in_progress|aof_last_bgrewrite_status'
rm -rf ./redis-data-bak
docker cp "$OLD_REDIS":/data ./redis-data-bak
ls -la ./redis-data-bak ./redis-data-bak/appendonlydir

# 3.1.3 定位新栈 redis 卷并确认无任务占用
fleetly placement show messageloop          # volumes 段列出卷名
NEW_VOLUME=$(docker volume ls --format '{{.Name}}' | grep '^fleetly-messageloop-redis_data-')
echo "$NEW_VOLUME"
test -n "$NEW_VOLUME"
docker ps -a --filter volume="$NEW_VOLUME" --format '{{.Names}} {{.Status}}'
# 预期无输出；如有容器占用 → 停止推进，先查部署/缩放状态。

# 3.1.4 灌入新卷（清掉首跑留下的空 AOF 后整目录复制；属主 = 镜像内 redis 用户）
docker run --rm \
  -v "$NEW_VOLUME":/data \
  -v "$PWD/redis-data-bak":/src:ro \
  redis:7-alpine \
  sh -c 'rm -rf /data/appendonlydir /data/dump.rdb && cp -a /src/. /data/ && chown -R redis:redis /data && ls -la /data /data/appendonlydir'
```

> **为什么整目录复制、而不是只放 `dump.rdb`**：Redis 7 在
> `--appendonly yes` 下若发现 `appendonlydir`（哪怕为空）就只认 AOF，
> **不会加载 `dump.rdb`**——只灌 RDB 会静默得到空库（实证见附录 A）。
> AOF 目录（含 `appendonly.aof.manifest` 与 base/incr 文件）才是数据载体；
> `dump.rdb` 只是伴随物，一并复制但非必需。

### 3.2 明示清零（备选）：接受历史与 epoch 重建

清零路径 **跳过 §3.1 全部步骤**（旧栈一路在线到 §5 DNS 切换，新 redis 首跑即
产生**新 epoch**）：旧客户端携带的偏移不再对应旧历史（recover 表现为「从新
历史开头恢复」= 空历史，而非错消息）。**选择清零必须在 §7 割接记录写明**——
§6.1 的 epoch 比对会把它判成「未回灌」，不写明即为事故。

## 4. 正式部署

```bash
fleetly deploy docker/fleetly/docker-compose.yml
# 预期：deployment … succeeded；redis 任务自 scale=0 起回，载入 §3.1 种子数据。
fleetly apps get messageloop               # derived_state=running
fleetly logs history messageloop --service mlbridge --limit 50   # 可选：确认无启动期报错
```

## 5. 域名声明与 DNS 切换

```bash
# 5.1 域名资源（三域名单服务三端口三协议；可提前到 §2 之后执行，幂等重跑用 set/list 核对）
fleetly domains add --service messageloop --port 9080 --protocol http messageloop "<ws-host>"
fleetly domains add --service messageloop --port 9090 --protocol h2c  messageloop "<grpc-host>"
fleetly domains add --service messageloop --port 9091 --protocol h2c  messageloop "<api-host>"
fleetly domains list messageloop

# 5.2 DNS 切换（DNS 服务商控制台；把三域名指向 fleetly 边缘地址）
dig +short "<ws-host>"

# 5.3 触发入口收敛 + 证书签发（ACME HTTP-01 要求解析已指向 fleetly 边缘）
fleetly domains set messageloop "<ws-host>"   # 空 flag = 保持现值，仅触发收敛
fleetly domains verify messageloop            # 期望：解析、:80、:443、证书 SAN/有效期均正常
```

> 已知行为差异：平台 **不做 80→443 重定向**（客户端直接使用 `https://` /
> `wss://`）；证书签发依赖 DNS 已切至 fleetly 边缘，切换前 443 可能无证书
> （路由与后端仍可用 `--resolve` 预探）。

## 6. 验收探针（全部通过才算割接完成）

### 6.1 数据面（决定性；DT-8）

```bash
NEW_REDIS=$(docker ps --format '{{.Names}}' | grep 'fleetly-.*-messageloop-redis' | head -n 1)
echo "$NEW_REDIS"
# ① epoch 键在场且值与割接前相等 —— 回灌成功的决定性断言（清零则必然不等）
docker exec "$NEW_REDIS" redis-cli GET ml2:broker:epoch        # == §3 记录的 OLD_EPOCH
# ② 样本频道历史在场（业务频道，见 §3.1.0 注：割接窗口无新消息时应与 OLD_XLEN 相等；
#    有正常流量则为 ≥ OLD_XLEN——下降到 0/明显小于 OLD_XLEN 即数据未回灌）
docker exec "$NEW_REDIS" redis-cli XLEN 'ml2:stream:<namespace>:<sample-channel>'
# ③ AOF 健康
docker exec "$NEW_REDIS" redis-cli INFO persistence | grep -E 'aof_enabled|aof_last_bgrewrite_status'
```

### 6.2 客户端 recover 探针（携偏移重连被信任 + live 往返）

```bash
cd sdks/go
ML_PROBE_URL="wss://<ws-host>/ws" \
ML_PROBE_TOKEN="$TW_ACCESS_TOKEN" \
ML_PROBE_CHANNEL="<namespace>:cutover.probe" \
ML_PROBE_EPOCH="$OLD_EPOCH" \
ML_PROBE_OFFSET="<§3.1.0 record 阶段的 ML_PROBE_OFFSET>" \
go run ./example/recoverprobe verify
cd ../..
# 期望 exit 0：cursor honored（settle 窗口内无 ≤ 游标的重复回放）+ marker 实时送达。
```

> 探针能力边界（如实）：服务端对**新会话**的 recover 只把 cursor.offset 当
> 续读下界，不校验客户端携带的 epoch，所以本探针证明「偏移续读点被接受、
> 无重复回放、新边缘端到端可达」；**「数据是否真的回灌」由 6.1① 的 epoch
> 比对判定**（空实例与全量回灌实例对客户端不可区分——附录 B 实证）。

### 6.3 域名与传输连通（WebSocket / 客户端 gRPC / Server API）

```bash
fleetly domains verify messageloop
# ① WebSocket：wss://<ws-host>/ws 握手成功（浏览器/前端或 SDK 最小连接）
curl -sS -o /dev/null -w '%{http_code}\n' --resolve "<ws-host>:443:<fleetly 边缘 IP>" "https://<ws-host>/ws"
# ② 客户端 gRPC：TLS 域名 + h2c 后端（ALPN=h2）
echo | openssl s_client -connect "<fleetly 边缘 IP>:443" -servername "<grpc-host>" -alpn h2 2>&1 | grep -E 'ALPN|Verify return code'
# ③ Server API gRPC：同 h2 检查（域名 <api-host>），业务侧以
#    authorization: Bearer <MESSAGELOOP_SERVER_API_AUTH_TOKENS> 调用
# ④ gRPC 功能往返（SDK，TLS）：Go SDK DialGRPC("<grpc-host>:443", sdk.WithTLS())
#    + 一次发布/订阅往返（与 6.2 同一信封协议；前端/客户端团队可按需补做）
```

## 7. 割接记录（必填；二选一 + 证据）

| 项 | 值 |
|---|---|
| 割接窗口 | `<起止时间>` |
| 数据面选择 | ☐ 回灌（§3.1） ☐ 明示清零（§3.2） |
| OLD_EPOCH / NEW_EPOCH | `<...>` / `<...>`（相等 = 回灌成功） |
| 样本频道 XLEN（前/后） | `<...>` / `<...>` |
| 客户端探针结果 | `<record offset / verify exit code>` |
| 域名与证书 | `<domains verify 摘要>` |
| 镜像引用（实际部署） | `messageloop:sha-b009c71` / `mlbridge:sha-39d254d`（或实际替换值） |
| 回滚点 | dokploy 栈未拆；DNS 回切值 `<...>` |

## 8. 回滚（dokploy 栈未拆）

```bash
# 触发条件：§6 任一探针失败，或切换后业务验收不通过。
# 8.1 启动旧栈（旧 redis 一直未拆、数据未被修改：§3 只读复制）
docker start "<dokploy mlbridge 容器>" "<dokploy messageloop 容器>"
# 8.2 DNS 回切到 dokploy 边缘（§0.3 记录的 A/AAAA）
# 8.3 新栈处置（可选，保留现场以便复盘）：停止在途部署或保持原样即可；
#     新栈域名资源保留（再次割接可复用，domains set 触发收敛）。
fleetly deployments list messageloop
fleetly rollback messageloop        # 新栈已起过版本时的 revision 级回滚
```

## 9. 待使用者执行窗口项（本环境未执行，如实标注）

- 真机凭据/地址（`FLEETLY_ADDR`/`FLEETLY_TOKEN`/`FLEETLY_PROJECT`、Torchwood 网关与项目 key、`TW_ACCESS_TOKEN`）；
- §2 首部署失败判定与 `fleetly env/configs/domains` 实写；
- §3 冻结、`BGREWRITEAOF`、复制与灌卷（附录 A/B 为本地等价实证，非 staging 结果）；
- §5 DNS 切换与 ACME HTTP-01 签发（T1-1 ⑤ 真机复验一并在此窗口核对）；
- §6.3 的 gRPC `h2c` 与 API 域名连通（含 SDK `DialGRPC` TLS 实网往返）；
- 附录 A 的 RDB-only 反例与 B 的重置对照可在 staging 窗口一并复跑。

---

## 附录 A：RDB-only 灌卷不生效（本地实证）

Redis 7 + `--appendonly yes` 下，仅把旧实例 `dump.rdb` 放进新卷会**静默得到空库**：

```bash
# 源：写入键值并 BGSAVE（产生 dump.rdb）；目标：只复制 dump.rdb，启动同参数 redis
docker volume create ml-dt8-src
docker volume create ml-dt8-dst
docker run -d --name ml-dt8-a -v ml-dt8-src:/data redis:7-alpine redis-server --appendonly yes
docker exec ml-dt8-a redis-cli SET ml2:broker:epoch probe-epoch-123
docker exec ml-dt8-a redis-cli BGSAVE
docker run --rm -v ml-dt8-src:/src:ro -v ml-dt8-dst:/dst redis:7-alpine sh -c 'cp -a /src/dump.rdb /dst/ && chown -R redis:redis /dst'
docker run -d --name ml-dt8-b -v ml-dt8-dst:/data redis:7-alpine redis-server --appendonly yes
sleep 3
docker exec ml-dt8-b redis-cli GET ml2:broker:epoch    # 实测：空（未加载 dump.rdb；新建了空 AOF）
# 对照：整目录（appendonlydir + dump.rdb，含 manifest）复制后启动 → epoch/流数据在场（实测 PASS）
```

结论：灌卷必须复制 **`appendonlydir` 整个目录**（`/data` 整目录复制即可），
只放 RDB 不行——§3.1.4 的 `rm -rf appendonlydir && cp -a /src/.` 即据此。

## 附录 B：客户端探针能力边界（本地实证）

- 同数据、新进程（模拟新边缘）：`verify` exit 0，epoch 前后相等，样本 XLEN
  从 1 → 2（record + verify marker）——「携偏移被接受 + live 往返」成立。
- **重置对照**（空 redis、旧 epoch/offset）：`verify` 也 exit 0（新实例无旧
  历史可回放），但 `GET ml2:broker:epoch` 与旧值**不等**、样本流不存在——
  判死由 6.1① 承担，而非客户端探针。故：**epoch 比对必须跑**。
- 本地复跑方式：`redis:7-alpine`（`--appendonly yes`）+ `go run ./cmd/server`
  指向该 redis（`require_auth: false`、`namespace: default`），按 §3.1.0 /
  §6.2 的 `recoverprobe record|verify` 流程执行；频道用 `default:cutover.probe`。
