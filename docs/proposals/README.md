# 提案

本目录存放**尚未拍板**的功能提案:问题、调研事实、方案骨架、备选取舍、开放问题。提案回答「要不要做、往哪个方向做」;被确认(Accepted)后,细化设计移入 [docs/design](../design/README.md) 并排期,本目录只留提案定稿与评审记录。

| 提案 | 状态 | 说明 |
| --- | --- | --- |
| [RPC 一致性 hash 路由与服务注册发现](2026-10-15-rpc-consistent-hash-routing.md) | Accepted(2026-10-15) | 游戏战斗服场景:按 key 一致性 hash 把 RPC 定向到后端服务实例;基于 lynx contrib/registry(consul/redis 后端);设计已转入 [docs/design](../design/2026-10-15-rpc-consistent-hash-routing.md),排期 v1.1 |
