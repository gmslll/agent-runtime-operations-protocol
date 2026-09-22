---
title: Protocol v1 架构决策与发布配置
status: active
updated: 2026-09-22
---

# 1. 已确认方向

| 编号 | 决策 | 结论 |
| --- | --- | --- |
| D-001 | 仓库职责 | Protocol 保存稳定契约、Schema、样例、SDK 和契约测试 |
| D-002 | 框架关系 | 协议与 Go、Python、Agno、Codex、cc-connect 等实现无关 |
| D-003 | 注册中心 | 借鉴 etcd/Nacos 语义，由金运自行实现，不依赖 etcd/Nacos |
| D-004 | 控制面 | 所有新调用必须经过 Control Plane 授权和创建 Run |
| D-005 | 数据面 | 获得 Dispatch Ticket 后，可信调用方可直接请求 Agent |
| D-006 | 交付模式 | 支持 Direct、Proxy 和 Worker Pull |
| D-007 | 流式 | Protocol v1 原生支持结构化流式事件 |
| D-008 | 第一阶段传输 | HTTP/JSON、SSE、HTTP Event Batch，不主动引入 gRPC |
| D-009 | 可靠性 | 至少一次交付；通过幂等、Attempt 和 Fencing 保证正确性 |
| D-010 | 大文件 | 文件本体走对象存储，协议只传 AssetRef |
| D-011 | 追踪 | 所有调用和事件传播 W3C Trace Context |
| D-012 | Web/Bot | Web 与飞书 Bot 是同一 Agent 的渠道，不重复注册 Agent |
| D-013 | SDK 位置 | 协议 SDK 位于 Protocol 仓，不放入 Console |
| D-014 | 公共定位 | 项目按供应商中立的开放 Agent Runtime Operations 协议设计，金运只是首个实现方 |
| D-015 | 项目名称 | 公共名称为 Agent Runtime Operations Protocol，简称 AROP；仓库名为 `agent-runtime-operations-protocol` |
| D-016 | 生态关系 | 补充而不替代 MCP、A2A、ARD/AI Catalog、CloudEvents 和 OpenTelemetry |
| D-017 | 协议分层 | 采用 Core、Runtime Management、Delivery Profile、Streaming 和 Governance Extension |
| D-018 | 独立实现 | 第三方不安装金运 Console 也必须能够运行 Quickstart、SDK 和 Conformance |
| D-019 | 发现边界 | Runtime Registry 管理域内实例；跨域资源发现通过 ARD/AI Catalog 和 A2A 互操作 |
| D-020 | ID 规范 | 人类可读的 `agent_id`/`skill_id` 使用稳定 slug；系统资源 ID 使用资源前缀加 UUIDv7；`instance_id` 稳定持久化，`session_id` 每次启动重建 |
| D-021 | Delta Offset | 文本 Delta 的 `offset` 固定表示当前完整文本的 UTF-8 字节偏移；修改历史输出必须使用 Snapshot 或 Reset |
| D-022 | 事件字段命名 | CloudEvents wire 扩展属性使用无下划线小写名；各语言 SDK 使用本语言惯用命名并由 Schema 生成固定映射 |
| D-023 | Manifest Digest | Manifest 先校验，再按 RFC 8785 JCS 规范化为 JSON，排除 Digest/签名字段后计算 SHA-256，格式为 `sha256:<lowercase-hex>` |
| D-024 | Capability 标识 | Core 保持最小；Direct、Proxy、Worker Pull 等使用标准 Profile；第三方 Extension 使用反向域名命名空间 |
| D-025 | Lease 默认值 | Runtime Lease 默认 TTL 30 秒、Keepalive 10 秒并带随机抖动；服务端返回实际参数；到期立即退出路由但保留历史审计 |
| D-026 | Token 与密钥 | Run Token 默认使用 ES256 非对称签名和 JWKS 发现，生命周期 1～5 分钟；密钥轮换至少重叠两个 Token 生命周期；mTLS 为高安全 Profile |
| D-027 | 浏览器 Direct | v1 浏览器只能通过 BFF/Proxy；可信 Bot Gateway、后端和编排器可 Direct；浏览器直连以后作为独立 Extension |
| D-028 | Inbox/Outbox | 生产级 Provider 必须使用 Durable Inbox/Outbox；内存实现仅限实验等级；`write/irreversible` 副作用必须有稳定 `effect_id` |
| D-029 | Skill 权限 | v1 正式支持 AgentDefinition 与 Skill 两级授权；简单 Agent 可只配置 Agent 级权限；Skill 级 Deny/高风险约束优先 |
| D-030 | Usage | Meter 使用整数累计值；货币使用 ISO 4217 币种和整数 `amount_micros`；终态携带最终 Usage Snapshot，并以 `provider_verified/agent_reported/estimated` 标记来源可信度 |
| D-031 | 数据保留 | 保留期由部署策略配置并由服务端公布；默认 Registry Event 24 小时、Run Delta 24 小时、Final Result 90 天、Audit 180 天 |
| D-032 | Reference Control Plane | 提供最小独立实现，覆盖开发身份、Manifest、Registry、Run/Dispatch、Event/SSE、查询/取消和 Conformance；不复制 Console 业务能力 |
| D-033 | Reference 存储 | Quickstart 默认单进程 SQLite；生产/多节点参考部署使用 PostgreSQL；两者必须通过相同协议 Fixture |
| D-034 | A2A 兼容 | 目标协议线为 A2A 1.0，首批 Fixture 固定到 v1.0.1；映射必须标记 `exact/extended/lossy/unsupported` 并保留原始对象引用 |
| D-035 | SDK 与 CLI 命名 | Go Module 使用 `github.com/gmslll/agent-runtime-operations-protocol`；Python Distribution 使用 `arop-sdk`、Import 使用 `arop`；npm 使用 `@arop/sdk`；CLI 使用 `arop` |
| D-036 | 开源许可证 | 协议文本、Schema、SDK、Conformance 和 Reference Implementation 统一采用 Apache-2.0 |
| D-037 | 公共治理 | 规范性变更走公开 RFC 并需两名 Maintainer 审核；初期使用 DCO，不使用 CLA；安全漏洞走私密报告渠道 |
| D-038 | 仓库归属 | 初期 GitHub 仓库为私有的 `gmslll/agent-runtime-operations-protocol`；C-002～C-004 完成后、公共 v0.1 前转为公开；未来迁移组织应保留 GitHub Redirect 与 Go Module 兼容策略 |
| D-039 | 发布安全 | GitHub 强制 2FA 和分支保护；正式包使用 OIDC Trusted Publishing；Release 使用签名 Tag；发布与恢复权限至少由两人持有 |
| D-040 | 公共命名空间 | Schema `$id`、CloudEvents Type Prefix 和 Extension Namespace 使用项目控制域名；域名确定前保留占位符且不得发布稳定包 |
| D-041 | 后端语言 | Reference Control Plane、Registry、Dispatcher、Run/Event Ledger 和服务端 Conformance Harness 使用 Go 实现；Python 主要用于 Agent Provider SDK，TypeScript 主要用于 Web/BFF Consumer |

# 2. 实现默认值

以下是不会改变核心 wire 语义、可以通过兼容配置调整的实现默认值：

| 编号 | 默认值 |
| --- | --- |
| P-001 | Registry 使用全局递增 `revision` |
| P-002 | 运行实例使用 `lease_id + session_id + generation` |
| P-003 | 事件使用 CloudEvents 兼容信封 |
| P-004 | Agent 到 Control Plane 使用小批量事件上报和 ACK |
| P-005 | Control Plane 到 Web/Gateway 使用 SSE |
| P-006 | Go 与 Python 首批提供完整 SDK；TypeScript 在公共 v0.1 提供生成模型和流式 Consumer，完整 Provider 随后实现 |
| P-007 | Reference Control Plane 的本地 Quickstart 使用 SQLite，生产/多节点模式使用 PostgreSQL |
| P-008 | 简单 Agent 默认授权到 AgentDefinition，高风险 Skill 使用 Skill 级覆盖规则 |
| P-009 | 浏览器默认使用 Proxy/BFF；可信服务优先 Direct |
| P-010 | Agent Manifest 支持一个 AgentVersion 包含多个 Skill |
| P-011 | 公共 v0.1 同时提供 Python Provider、Go SDK 和 TypeScript Streaming Consumer |
| P-012 | 提供最小 Reference Control Plane 和 Docker Compose Quickstart |
| P-013 | Registry 和 Run/Event Ledger 的实时通知只用于降低延迟，持久化 Ledger 才是真值 |

# 3. 发布配置待填写项

架构和 wire 语义已经冻结。以下项目不是设计分歧，只是需要在创建公共仓库、域名和发布账户时填写真实值：

| 编号 | 待填写值 | 当前约束 |
| --- | --- | --- |
| C-002 | 项目控制的文档域名 | 必须由项目长期控制；决定 Schema `$id`、Event Type Prefix 和 Extension Namespace |
| C-003 | PyPI/npm 实际注册结果 | 首选 `arop-sdk` 与 `@arop/sdk`；正式发布前必须完成占用检查和所有权验证 |
| C-004 | 初始 Maintainer、Reviewer 和安全邮箱 | 正式公开贡献前至少两名有恢复权限的维护者，并提供私密安全报告入口 |

在 C-002～C-004 填写完成前可以开发和运行本地 Fixture，但不得发布稳定包或宣称公共命名空间已经永久冻结。

# 4. 与现有上层架构文档的差异

工作区早期架构曾将以下内容列为第一阶段暂缓：

- SSE 全链路流式协议。
- 调用方直接访问 Agent。

本设计讨论已形成新的方向：

- Protocol v1 包含流式事件。
- “必须经过 Control Plane”指必须经过授权和 Run 创建；获得 Ticket 后可以 Direct。

在开始跨仓实现前，应同步更新工作区 `AI中台/ARCHITECTURE.md`，避免两个仓库持有冲突描述。

# 5. 公共化后的文档边界

- 公共定位、开放治理和采用路径以 `PUBLIC_PROJECT_AND_ADOPTION.md` 为准。
- MCP、A2A、ARD、CloudEvents 和 OpenTelemetry 映射以 `INTEROPERABILITY.md` 为准。
- 具体外部标准版本必须在发布时进入 Compatibility Matrix，不在核心字段中暗含。
