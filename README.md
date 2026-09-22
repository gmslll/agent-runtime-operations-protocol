# Agent Runtime Operations Protocol

`agent-runtime-operations-protocol` 是供应商中立的开放 Agent 运行与运维互操作协议仓库，简称 AROP。

它解决的不是某一种 Agent 框架如何运行，而是不同企业平台中的 Agent 如何被标准化发布、注册、授权调用、可靠调度、流式通信、追踪、审计、升级和下线。

本项目是 Agent 运行与运维互操作层，不替代 MCP、A2A、ARD/AI Catalog、CloudEvents 或 OpenTelemetry。

## 设计目标

- 任意语言、框架和部署位置的 Agent 使用同一套接入契约。
- Web、飞书 Bot、后台服务和后续渠道调用同一个 AgentDefinition。
- 每次新调用必须先经过 Control Plane 授权并创建 Run。
- 获得短期 Dispatch Ticket 后，可信调用方可以直接请求 Agent，业务流量不必全部经过 Console。
- 支持 Direct HTTP、Control Plane Proxy 和内网 Worker Pull 三种交付方式。
- Protocol v1 原生支持结构化流式事件、断线恢复、幂等、租约、重试和全链路追踪。
- 注册发现借鉴 etcd/Nacos 的 Lease、Revision、Watch、CAS 和健康视图思想，但由金运自行实现，不依赖 etcd 或 Nacos。
- 协议仓不依赖 Console、飞书、cc-connect、Teable 或具体 Agent 框架。
- 任意第三方可以独立实现兼容 Control Plane 或 Runtime，不需要安装金运 Console。
- A2A 用于 Agent 间互操作，MCP 用于工具与资源接入，ARD/AI Catalog 用于跨域公开发现。
- 协议按 Core、Runtime Management、Delivery、Streaming 和 Governance 分层，接入方不必实现全部能力。

## 仓库交付物

计划交付：

```text
JSON Schema
OpenAPI
AsyncAPI
协议黄金样例
兼容性规则
Go / Python / TypeScript SDK
CLI 与脚手架
参考 Agent 与 Worker Adapter
轻量 Reference Control Plane
A2A / MCP / ARD 互操作 Adapter
提供方和消费方契约测试
```

## 文档导航

建议按以下顺序阅读：

1. [总体架构](docs/ARCHITECTURE.md)
2. [协议规范](docs/PROTOCOL_SPECIFICATION.md)
3. [注册与发现](docs/REGISTRY_AND_DISCOVERY.md)
4. [Run、事件与流式通信](docs/RUN_AND_STREAMING.md)
5. [安全与治理](docs/SECURITY_AND_GOVERNANCE.md)
6. [可靠性与运维](docs/RELIABILITY_AND_OPERATIONS.md)
7. [SDK 与开发者体验](docs/SDK_AND_DX.md)
8. [外部标准互操作](docs/INTEROPERABILITY.md)
9. [公共项目与采用策略](docs/PUBLIC_PROJECT_AND_ADOPTION.md)
10. [目录规划](docs/DIRECTORY_STRUCTURE.md)
11. [开发计划](docs/DEVELOPMENT_PLAN.md)
12. [架构决策与发布配置](docs/DECISIONS.md)

## 与 Console 的边界

本协议项目负责稳定、实现无关的契约、SDK、Conformance 和 Reference Implementation。

`kinglucky-agent-console` 负责：

- 身份、组织和权限。
- Agent Catalog 与发布审核。
- Run、Dispatcher、注册表和发现服务的实现。
- 用量、审计和渠道接入。
- Web 用户工作台和管理后台。

协议仓定义任意 Control Plane 与 Agent、Worker、可信调用方之间如何通信，但不实现 Console 的业务规则和数据库。金运 Console 是一个实现，不是协议正确性的唯一来源。

公共 Reference Control Plane 和服务端组件使用 Go 实现。仓库中的 Node.js 依赖只用于 JSON Schema、Fixture 和文档校验，不属于后端运行时；Python 用于 Agent Provider SDK 和参考 Agent。

## 当前状态

当前处于 Protocol v1 设计冻结和仓库基线阶段，已经开始第一批 Schema、Fixture 和 Go Reference Control Plane 基线实现。核心架构、wire 语义、Apache-2.0 许可证、GitHub/Go Module、SDK/CLI 首选命名和治理方式已经确认；项目域名、包注册表所有权和初始维护者名单仍需填写。在这些发布配置完成前不发布稳定软件包。

## 当前验证

```bash
npm ci
make validate
```

`make validate` 会校验全部 JSON Schema、有效和无效 Manifest Fixture、RFC 8785 Manifest Digest、Markdown 本地链接，并运行 Go 测试。

计算 Manifest Digest：

```bash
make manifest-digest FILE=examples/manifests/valid/minimal.yaml
```

当前 Schema `$id` 使用保留域名 `arop.invalid`，仅供预发布阶段稳定本地引用；项目域名确定后会在首个稳定公开版本前一次性迁移。

文档中的标记含义：

- **已确认**：本阶段可作为实现依据。
- **实现默认值**：可以通过兼容配置调整，调整时必须更新文档和契约测试。
- **发布配置待填写**：设计没有分歧，但必须填入真实账户、域名或维护者信息后才能公开发布。
