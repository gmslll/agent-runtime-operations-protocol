---
title: 公共项目定位、开放治理与采用策略
status: active-design
updated: 2026-09-22
---

# 1. 决策摘要

本仓库不再只按内部协议设计，而是以可公开发布、供应商中立、可独立实现的 Agent Runtime Operations 协议为目标。

公共项目名称确定为 **Agent Runtime Operations Protocol**，简称 **AROP**；仓库名为 `agent-runtime-operations-protocol`。GitHub/Go Module、Python、TypeScript 和 CLI 首选名称已经冻结；正式发布任何稳定包前，仍需核验包注册表所有权，并用项目控制域名冻结 Schema ID、事件类型和文档站点命名空间。

项目不与 MCP、A2A 或开放发现标准竞争。它解决这些协议之外的企业运行问题：

```text
发布和版本
→ 运行实例注册、租约、健康与容量
→ 授权后的可靠调度
→ Direct / Proxy / Worker Pull
→ Run / Attempt / Event / Result
→ Fencing、幂等、恢复、审计和用量
```

# 2. 公共定位

建议对外描述：

> 一套供应商中立的 Agent 运行与运维互操作规范，让任何框架实现的 Agent 能接入任意兼容 Control Plane，并获得可靠调度、流式事件、故障恢复和全链路可观测能力。

核心承诺：

> Build once, operate anywhere.

不使用以下定位：

- 新的通用 Agent-to-Agent 协议。
- MCP 替代品。
- A2A 替代品。
- 特定 Console 的私有 RPC。
- 强制用户采用某一种 Agent 框架或数据库的运行时。

# 3. 与生态标准的边界

| 标准或层 | 主要职责 | 本项目的关系 |
| --- | --- | --- |
| MCP | Agent/模型访问工具、资源和上下文 | Agent 内部依赖或 Adapter，不重定义 MCP |
| A2A | 独立 Agent 之间的发现、任务、消息和产物互操作 | 提供 Agent Card 与 Task 双向映射 |
| ARD / AI Catalog | 跨组织发布和搜索 Agent、MCP Server、Skill 等资源 | 导出公共发现文档，不代替本地 Runtime Discovery |
| CloudEvents | 事件公共信封和 HTTP 绑定 | 作为 Event Envelope 的基础标准 |
| OpenTelemetry | Trace、Metric、Log 语义 | 作为可观测性输出标准 |
| 本项目 | Agent 运行、注册、调度、可靠交付和治理 | 补齐企业运行控制层 |

本项目中的 Registry 是某个管理域内的运行实例注册表，负责 Lease、Health、Capacity 和 Routing。ARD/AI Catalog 是跨管理域的资源发布与搜索层，两者不得混为一个接口。

# 4. 分层规范

为避免只有完整 Console 才能实现协议，规范分为以下层：

## 4.1 Core

所有实现必须支持：

- Protocol Version 与 Capability Negotiation。
- Agent Manifest 最小字段。
- Run、Attempt、Event、Result 和 Error。
- 幂等、Deadline、Cancel 和最终 Snapshot。
- W3C Trace Context。

## 4.2 Runtime Management Profile

运行和调度实现按需支持：

- RuntimeService、RuntimeInstance 和 AgentBinding。
- Register、Lease、Keepalive、Health、Capacity 和 Drain。
- Session、Generation 和 Fencing。
- Snapshot、Revision、Watch 和 Resync。

## 4.3 Delivery Profiles

实现至少选择一种：

- Direct HTTP。
- Control Plane Proxy。
- Worker Pull。

## 4.4 Streaming Extension

- 结构化 Event Envelope。
- Delta、Snapshot 和 Reset。
- SSE Replay。
- HTTP Event Batch 与 ACK。
- Backpressure 和 Replay Window。

## 4.5 Enterprise Governance Extension

- 风险等级和副作用等级。
- 审批、预算和资源范围。
- Usage、审计、数据区域和凭据代理。

# 5. Conformance 等级

兼容性不能只有“全部实现”一种状态。

| 等级 | 必需能力 | 目标用户 |
| --- | --- | --- |
| Core Provider | Manifest、Run、Result、Error、Cancel、幂等 | 普通 HTTP Agent |
| Streaming Provider | Core + Event Stream + Resume + Snapshot | 对话和生成型 Agent |
| Managed Runtime | Streaming + Register + Lease + Health + Drain | 云端 Agent 服务 |
| Pull Worker | Core + Claim + Attempt Lease + Fencing | 内网、桌面和无入站端点的通用 Worker |
| Control Plane | Authz、Run Ledger、Dispatch、Event Ledger、Discovery | 平台实现方 |
| Production | Durable Inbox/Outbox、Trace、安全和故障测试 | 生产部署 |

每个等级必须有机器可执行的测试套件和公开报告格式。实现方可以声明多个等级，不得只用模糊的“兼容”表述。

# 6. 开源边界

协议流行不要求任何实现公开完整 Console，但必须提供脱离 Console 可运行的公共基线：

- Schema、OpenAPI、AsyncAPI 和黄金样例。
- SDK 和 CLI。
- Conformance Suite。
- Python Reference Agent。
- Go Reference Agent。
- Worker Reference Adapter。
- A2A Adapter 和 MCP Integration Example。
- 轻量 Reference Control Plane。
- Docker Compose Quickstart。
- Web Streaming Demo。

Reference Control Plane 使用 Go 实现，只覆盖验证协议所需的开发身份、Manifest 发布、Registry/Lease、Run/Dispatch、Event Ingest/SSE、Run 查询/取消和 Conformance 接口，不承载特定组织、飞书、业务审批、完整计费或生产 Console UI。

本地 Quickstart 默认使用 SQLite，确保单进程即可启动；生产级、多节点和故障切换参考部署使用 PostgreSQL。两种存储必须运行同一套 Schema、状态机和 Conformance Fixture，不允许形成两套协议语义。Worker Pull 在首个公开 v1 RC 前完成；P44 及之前所有验证制品仅是 private/dev snapshot。

# 7. 开发者体验目标

公共项目的北极星指标：

- 新开发者十分钟内完成首次调用。
- 现有 Python Agent 的核心改造不超过约三十行。
- 不安装特定 Console 也能运行 Quickstart 和 Conformance。
- SDK 默认处理 Token 校验、幂等、Trace、Event Sequence 和重试。
- Schema 是唯一类型源，新语言实现无需复制手写模型。
- 一个 Agent 包可以在两个独立 Control Plane 实现上运行。

建议入口：

```text
安装 SDK
→ 初始化或包装现有 Agent
→ 本地 Reference Control Plane
→ 发起 Run 并查看流式事件
→ 执行 Conformance
→ 导出 A2A Agent Card 和 ARD Entry
```

# 8. 采用路径

## 8.1 内部验证

至少选择三类差异明显的实现：

- 普通 HTTP/Python Agent。
- Go Agent 或服务型 Agent。
- 通用 Pull Worker（可由任意桌面或内网执行器实现）。

它们必须复用同一套 Schema 和 Conformance，而不是各写一条特殊路径。

## 8.2 首个公开 v1 RC

必须包含：

- Core Schema Bundle。
- Python Provider、RuntimeRegistration 和 Worker SDK。
- Go Provider、Consumer、Registry 和 Worker SDK。
- TypeScript Streaming Consumer。
- Reference Control Plane。
- Docker Compose Quickstart。
- 在线或本地 Event Viewer。
- Conformance CLI。
- 可执行 Scenario/Profile（Core、Streaming、Managed、Pull、Control Plane、Production）。
- `arop init/dev/test/publish/register/export/doctor`。

## 8.3 生态适配

优先发布：

- A2A Agent Card 与 Task Adapter。
- MCP 使用示例和依赖声明。
- ARD/AI Catalog 导出器。
- FastAPI/ASGI Adapter。
- 常见 Agent 框架的薄 Adapter。

Adapter 只能翻译边界，不得把框架私有状态写入核心 Schema。

## 8.4 外部设计伙伴

v1.0 前至少需要：

- 三个外部的真实接入团队。
- 两个独立 Agent Runtime 实现。
- 两种语言 Provider。
- 至少一个外部 Control Plane 或独立兼容性验证器。
- 公开记录的互操作问题和修复过程。

## 8.5 外部证据 Gate

域名所有权、PyPI/npm 包所有权、两名维护者与私密安全入口，以及外部伙伴和独立实现，都是只能由真实外部事实满足的发布 Gate。代码库内部 Mock、Reference Implementation、Fixture 或同一团队的第二个示例均不能替代它们。

每个 Gate 必须关联可核验的所有权记录、报告、公开链接或签字评审。证据不存在时状态必须是 `pending-external-evidence` 或 `blocked`，不得为了进入 RC/v1 而标记完成。

原始签名证据的 producer 必须是相应外部主体：P45 的 project owner/maintainer threshold、P50 的独立实现/伙伴 principals、P52 的 release approver。仓库 validator 只验签并输出 detached verified summary，绝不代签、补齐伙伴或把内部模拟写成外部结果。A 内 `compatibility/implementation-matrix.yaml` 只描述门槛/Profile/结果位置，真实兼容结果作为绑定 A/RC 的 release asset、OCI referrer 或 transparency statement；P53 通过 P52 equivalence bridge 绑定 final B。

P49 的 RC subject manifest、P50 compatibility summary、P51 unsigned overlay candidate、P52 verified external attestation summary 与 P53 final release evidence 构成可下载的 detached 证据链，不进入冻结 A/B tree。每个对象绑定 repository/object format、commit/tree、policy/validator/trust-root digest、签名主体、时效和上游 evidence digest。

# 9. 开放治理

开放治理规则已经确定：

- 协议文本、Schema、SDK、Conformance 和 Reference Implementation 统一使用 Apache-2.0。
- 规范性变更走公开 RFC，并要求至少两名 Maintainer 审核。
- 初期贡献签署使用 DCO，不使用 CLA。
- 安全漏洞通过 `SECURITY.md` 公布的私密渠道报告，不先公开 Issue。
- GitHub 强制 2FA、分支保护和签名 Release Tag；包发布使用 OIDC Trusted Publishing。
- OIDC provenance 必须绑定 issuer、repository、protected environment、`job_workflow_ref`/`job_workflow_sha`、workflow blob/lock digest、source commit 与 artifact digest；不得使用 PAT 或静态 registry token 替代。
- 仓库与包的发布、恢复权限至少由两人持有。
- `CONTRIBUTING.md`、`CODE_OF_CONDUCT.md` 和 `SECURITY.md`。
- 版本、兼容性和弃用策略。
- 公开 Roadmap 和 Release Notes。

协议核心字段不得由单一产品需求直接改动。实现方专属能力必须使用扩展命名空间，并通过与第三方相同的扩展机制接入。

# 10. 命名与标识符原则

已经确认：

- 公共名称：Agent Runtime Operations Protocol。
- 简称：AROP。
- 仓库名：`agent-runtime-operations-protocol`。
- 初期仓库位置：私有的 `gmslll/agent-runtime-operations-protocol`；取消独立公共 v0.1，域名、包所有权、维护者和安全渠道就绪后，在首个公开 `v1.0.0-rc.N` 前转为公开。
- Go Module：`github.com/gmslll/agent-runtime-operations-protocol`。
- Python Distribution：`arop-sdk`；Import Package：`arop`。
- TypeScript Package：`@arop/sdk`。
- CLI：`arop`，默认配置目录 `~/.config/arop/`。
- 定位是“运行和运维互操作”，不覆盖所有 Agent 通信。
- 不使用已存在且容易混淆的 Agent Runtime Protocol / ARP 名称。

首个公共版本前仍必须：

- 核验 PyPI/npm 包名和 Scope 的实际所有权。
- 确定项目长期控制的域名，并据此冻结 Schema/Event Namespace。
- 把示例中的占位 URI 和包名迁移为正式标识符。
- 不发布仍带 AROP 或内部实现方命名的公共软件包。

# 11. 成功指标

发布后持续跟踪：

- Time to First Successful Run。
- 接入所需业务代码行数。
- Conformance 通过率和执行时间。
- 外部 Provider、Control Plane 和 Adapter 数量。
- 跨版本无需修改业务 Agent 的比例。
- Issue 首次响应和 RFC 合并时间。
- 外部贡献者和 Maintainer 比例。

GitHub Star、下载量和文章阅读量是传播指标，不代替真实互操作数量。
