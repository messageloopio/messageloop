# 开发者文档

本目录是 MessageLoop 的开发者文档套件，覆盖服务端架构、配置、协议、管理接口、集群、可观测性、开发流程与官方 SDK。全部内容以简体中文撰写，文档中的类型名、函数名、指标名、配置字段与行为均以仓库源码为准。

与英文文档的分工：协议与部署的既有文档位于 `docs/` 根目录（[`protocol.md`](../protocol.md)《客户端协议参考》、[`deployment.md`](../deployment.md)《部署指南》），本套件通过交叉链接引用它们，不重复内容。

## 阅读路径

按角色选择入口：

| 角色 | 推荐阅读顺序 |
| --- | --- |
| 新加入的开发者 | [开发指南](06-development.md) → [架构指南](01-architecture.md) → [配置参考](02-configuration.md) |
| 理解系统设计 | [架构指南](01-architecture.md) → [分布式集群指南](04-cluster.md) → [客户端协议参考](../protocol.md) |
| 看 v1.0 做什么 | [产品 ROADMAP](../../ROADMAP.md) → [v1.0 功能缺口设计](../design/v1.0-platform-gaps.md) |
| 部署与运维 | [配置参考](02-configuration.md) → [部署指南](../deployment.md) → [可观测性指南](05-observability.md) → [分布式集群指南](04-cluster.md) |
| 服务端集成（管理操作） | [管理 API 参考](03-admin-api.md) + [客户端协议参考](../protocol.md) |
| SDK 用户 | [Go SDK 指南](07-sdk-go.md) 或 [TypeScript SDK 指南](08-sdk-ts.md) → [客户端协议参考](../protocol.md) |

## 文档目录

| 文档 | 内容 |
| --- | --- |
| [架构指南](01-architecture.md) | 总体设计：Node/Hub/Client/Broker/Presence/Survey/ACL/Proxy 核心组件、传输层、主题匹配、消息流走查、断连模型、并发模型、模块布局 |
| [配置参考](02-configuration.md) | 全部配置项逐字段参考：默认值、校验规则、内置 ACL 语义、代理路由与钩子、三层超时、完整示例走查 |
| [管理 API 参考](03-admin-api.md) | 服务端 gRPC 管理接口（`messageloop.server.v1.APIService`）：鉴权、8 个 RPC 的字段与语义、错误模型、grpcurl 示例、集群感知行为 |
| [分布式集群指南](04-cluster.md) | 多节点部署：Redis broker 与控制面的区别、节点租约、命令总线与去重、会话归属与远端接管、集群级 Survey、投影修复、Presence 聚合、故障与恢复 |
| [可观测性指南](05-observability.md) | 健康检查、Prometheus 指标全集、日志、断连码参考、监控告警建议、故障排查 |
| [开发指南](06-development.md) | 环境要求、仓库布局、构建/测试/lint、Protobuf 工作流、代码风格、发布流程 |
| [Go SDK 指南](07-sdk-go.md) | Go 客户端 SDK：安装、快速开始、客户端选项、消息 API、传输、重连与会话恢复、RPC 与代理后端、RPCMux |
| [TypeScript SDK 指南](08-sdk-ts.md) | TS/JS 客户端 SDK（`@messageloop/sdk`）：安装、快速开始（Node/浏览器）、客户端与选项、消息 API、编解码、重连与恢复、Survey 应答/发起与 Presence 事件 |

## 相关文档（docs/ 根目录）

- [产品 ROADMAP](../../ROADMAP.md)：v0.2 → v1.0 缺口与排期
- [v1.0 功能缺口设计](../design/v1.0-platform-gaps.md)：协议、服务端、SDK、集群与验收
- [客户端协议参考](../protocol.md)：传输协商、消息信封（`InboundMessage` / `OutboundMessage`）、连接生命周期、错误码与断连码、频道命名
- [部署指南](../deployment.md)：单二进制部署、监听器模型、TLS、管理 API 鉴权、Docker、多节点部署

## 维护约定

- 文中事实（默认值、常量、行为）应与源码保持一致；修改代码后如影响本套件所述内容，请同步更新对应文档。
- 新增章节时在 [文档目录](#文档目录) 与各文档的"配套文档"列表中登记，保持交叉链接闭合。
- 协议层细节（消息格式、断连码）只写在 `docs/protocol.md`，其他文档一律交叉引用，避免多处维护。
