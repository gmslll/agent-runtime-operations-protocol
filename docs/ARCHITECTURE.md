---
title: Agent Runtime Operations Protocol 总体架构
status: active-design
updated: 2026-09-23
---

# 1. 产品定位

Agent Runtime Operations Protocol（AROP）是企业 Agent 能力接入任意兼容 Control Plane 的稳定边界。

它把员工或团队创建的 LLM Agent、普通 HTTP Agent、本地/桌面 Agent Worker、内网 Worker 和后续外部 A2A Agent，统一为可发布、可发现、可授权、可调用、可追踪的企业能力。

协议不负责实现 Agent 的推理、工作流和业务逻辑，也不替代 MCP、A2A、ARD、Agent 框架或模型 API。它负责定义这些实现如何进入一个可注册、可调度、可恢复、可追踪和可治理的运行平面。

金运 AI 中台是首个实现和验证环境，不是协议的唯一合法实现。第三方 Runtime 和 Control Plane 必须能够只依赖公开契约互操作。

# 2. 核心原则

## 2.1 控制面与数据面分离

每次新调用必须先经过 Control Plane：

```text
身份解析
→ 权限判定
→ 预算与风险检查
→ 创建 run_id
→ 选择 Deployment
→ 签发短期 Dispatch Ticket
```

获得 Ticket 后，实际调用有三种模式：

| 模式 | 数据路径 | 适用场景 |
| --- | --- | --- |
| `direct` | Caller 直接请求 Agent Endpoint | Bot Gateway、可信后端、BFF、低延迟流式调用 |
| `proxy` | Caller 经 Control Plane 转发 | 浏览器默认、强审计、隐藏内网地址 |
| `worker_pull` | Worker 主动从 Control Plane 领取 | 桌面节点、企业内网 Agent、无入站端点的执行器 |

“所有调用经过 Control Plane”表示所有新调用必须经过授权、创建 Run 和签发 Ticket，不表示所有业务字节都经过 Console。

## 2.2 注册发现与业务调用分离

注册发现回答：

```text
哪些 Agent 已发布？
哪些运行实例当前可用？
它们支持什么协议、能力和容量？
```

业务调用回答：

```text
谁调用了哪个 Agent？
是否有权？
路由到了哪个实例？
执行、重试和结果是什么？
```

Registry 不代理业务请求。它提供 Deployment 发现视图，Dispatcher 根据该视图签发 Ticket。

## 2.3 结构化事件是一等协议

Protocol v1 原生支持结构化流式事件。文本增量只是事件类型之一，状态、工具调用、审批、产物、用量和错误都使用同一事件信封。

传输可以是 SSE、HTTP Event Batch，后续也可以增加 WebSocket 或 gRPC；事件语义不随传输改变。

## 2.4 至少一次与幂等

跨网络通信采用至少一次语义：

- Run 投递可能重复。
- Callback/Event Batch 可能重复。
- Worker Claim 可能因租约过期重新分配。
- 客户端可能因超时无法确认请求是否成功。

协议不声称提供“恰好一次”。提供方和消费方必须通过 `Idempotency-Key`、`event_id`、`attempt_id`、`generation` 和 fencing token 实现幂等。

## 2.5 核心、Profile 与 Extension 分离

协议按能力分层：

```text
Core
├── Runtime Management Profile
├── Direct Delivery Profile
├── Proxy Delivery Profile
├── Worker Pull Profile
├── Streaming Extension
└── Enterprise Governance Extension
```

所有兼容实现必须支持 Core，但只需声明其实现的 Profile 和 Extension。协议版本表示语义兼容范围，Capability 表示某个实现实际可用的能力，两者不得混用。

分层的目的，是让一个普通 HTTP Agent 不必实现 Registry Watch 和 Worker Pull，也能成为合格 Provider；同时让生产平台能够组合更高等级的可靠性能力。

# 3. 总体组件

```text
┌────────────────────── 企业入口 ──────────────────────┐
│ Web / Bot Gateway / Backend / Future Channel         │
└─────────────────────────┬─────────────────────────────┘
                          │ 创建 Run / 获取 Ticket
                          ▼
┌──────────────────── Control Plane ────────────────────┐
│ Identity  Authz  Catalog  Registry  Dispatcher        │
│ Run Ledger  Usage  Audit  Event Ingest                │
└───────┬─────────────────┬──────────────────────┬───────┘
        │ Ticket/Proxy    │ Claim                │ Events
        ▼                 ▼                      ▲
  Direct HTTP Agent  Generic Pull Worker         │
        │                 │                      │
        └──────── Direct / Proxy execution ───────┘

Large Assets: Caller / Agent <-> OSS or MinIO by scoped AssetRef
Model Calls: Agent <-> LLM Provider directly
```

# 4. 领域对象

## 4.1 AgentDefinition

用户可发现和授权的业务能力，例如“商品图生成”“客户分析”“周报生成”。它不描述具体进程地址。

## 4.2 AgentVersion

AgentDefinition 的不可变发布版本。包含 Manifest、输入输出 Schema、能力、风险等级和 Manifest Digest。

同一个 Agent 可以同时存在多个版本。新 Run 绑定精确版本；滚动升级不会改变已有 Run。

## 4.3 AgentSkill

AgentVersion 可选地暴露多个 Skill。简单 Agent 只有一个默认 Skill。

Skill 是可调用操作，不等于运行实例。Protocol v1 支持 AgentDefinition 与 Skill 两级授权；简单 Agent 可以只配置 Agent 级授权，高风险 Skill 的独立约束和 Deny 规则优先。

## 4.4 RuntimeService

一个可部署程序或 Adapter，可以承载一个或多个 AgentVersion，例如一个 Python 服务承载多个业务 Agent。

## 4.5 RuntimeInstance

RuntimeService 的具体进程实例。实例具有独立 `instance_id`、启动 `session_id`、租约、健康状态、容量和 Generation。

## 4.6 AgentBinding

声明某个 RuntimeService/RuntimeInstance 可以执行哪个 `agent_id + agent_version + skill_id`。

## 4.7 Run

用户可见的一次业务调用。Run 在所有重试中保持同一个 `run_id`。

## 4.8 Attempt

Run 的一次实际执行投递。重新路由或重试创建新的 `attempt_id`，并签发新的 fencing token。

## 4.9 Session

跨 Run 的会话或工作区关联。Agent 声明会话模式：

- `stateless`：任意实例均可执行。
- `sticky`：必须路由到原实例。
- `portable`：可通过 checkpoint 迁移。

## 4.10 AssetRef、DataRef、SecretRef

- `AssetRef`：文件和长期/短期产物引用。
- `DataRef`：ERP、数据库或业务资源的授权引用。
- `SecretRef`：运行所需凭据的受控引用。

三者生命周期和权限不同，不使用一个万能字符串代替。

# 5. 完整生命周期

## 5.1 创建与发布

```text
开发者创建 Agent
→ SDK 生成 Manifest 和 Schema
→ 本地契约测试
→ 提交 AgentVersion
→ 所有权、风险和安全审核
→ 发布为可配置能力
```

建议生命周期：

```text
draft -> testing -> review -> published -> deprecated -> retired
```

## 5.2 运行实例上线

```text
RuntimeInstance 启动
→ 使用 Deployment Credential 注册
→ 校验 AgentBinding 和 Manifest Digest
→ 获得 lease_id、generation 和 selected_protocol_version
→ readiness 成功
→ 进入发现视图
```

## 5.3 用户发现

用户只能通过 Control Plane 的授权目录发现 Agent。用户目录与 Runtime Discovery 是两个视图：

- 用户视图：已发布、渠道开启且当前主体有权限的能力。
- Router 视图：租约有效、健康、启用、就绪、容量充足且协议兼容的实例。

## 5.4 调用

```text
Caller 创建 Run
→ Control Plane 原子鉴权并绑定 AgentVersion
→ Dispatcher 选择 RuntimeInstance
→ 创建 Attempt
→ 签发 Deployment-bound Run Token
→ direct / proxy / worker_pull 执行
→ 结构化事件和最终快照
→ Usage、审计和渠道呈现
```

## 5.5 下线

实例下线前进入 `draining`，停止接收新 Attempt，等待已有执行结束或达到 Drain Deadline，然后注销租约。

AgentVersion 下线时不影响历史 Run 和审计记录。已创建但未投递的 Run 是否允许继续执行由发布策略明确指定。

# 6. 数据路径

## 6.1 小型结构化数据

参数、消息、状态和事件使用 JSON。

## 6.2 大文件

大文件使用 AssetRef：

```text
Caller 向 Control Plane 申请上传
→ 获得 run/asset scoped 短期地址
→ 直接上传对象存储
→ Run 只传 asset_id
→ Agent 使用短期授权读取
→ Agent 直接上传产物
→ 结果只返回 AssetRef
```

## 6.3 模型流量

Agent 直接调用其模型 Provider。Control Plane 不代理模型请求，只接收必要的结果事件和 Usage。

# 7. 标准与互操作

- HTTP 接口：OpenAPI。
- 事件通道：AsyncAPI。
- 数据结构：JSON Schema。
- 浏览器/服务端流式消费：SSE。
- 分布式追踪：W3C `traceparent`、`tracestate`。
- 事件信封：CloudEvents 兼容字段。
- Agent 间互操作：A2A Agent Card、Task、Message、Artifact 和 Streaming 映射。
- 工具与资源：MCP 原生语义和 Adapter。
- 跨域资源发现：ARD / AI Catalog 导出和导入。
- 可观测性：OpenTelemetry Trace、Metric 和 Log 映射。

协议不要求所有接入方实现完整 A2A、MCP 或 ARD，但公共实现必须说明支持范围和映射损失。详细规则见 [INTEROPERABILITY.md](INTEROPERABILITY.md)。

# 8. 高可用边界

- Control Plane 可多实例部署，共享持久化 Registry、Run Ledger 和 Event Ledger。
- Registry 的实时通知可以丢失，但 Revision Replay 必须补齐。
- 已签发的 Direct Run 在 Control Plane 暂时不可用时可以继续执行。
- 新 Run 在身份、权限或 Control Plane 不可用时默认拒绝。
- Agent Event Outbox 在 Control Plane 不可用时暂存并重试。
- Runtime Lease 使用服务端时间和 Generation，避免旧进程复活。

# 9. 明确非目标

Protocol v1 不承担：

- 通用分布式 KV。
- Raft 或共识集群实现。
- 多租户 SaaS 模型。
- 公共 Agent 市场。
- Agent 自主信任和开放互联网自动组网。
- 重型工作流编排引擎。
- 业务数据库真值同步。
- 完整 MCP/A2A Server 强制实现。

# 10. 公共实现边界

为了证明协议不依赖金运 Console，公共项目至少提供：

- 可单独运行的 Reference Control Plane。
- 两个独立语言的 Reference Agent。
- Provider、Consumer、Registry 和 Worker Conformance Suite。
- A2A Adapter、MCP Integration Example 和 ARD Exporter。
- Docker Compose Quickstart 和 Web Streaming Demo。

Reference Control Plane 只验证协议，不复制金运 Console 的组织、飞书、业务审批和运营后台。公共项目定位、治理和采用要求见 [PUBLIC_PROJECT_AND_ADOPTION.md](PUBLIC_PROJECT_AND_ADOPTION.md)。

Reference Control Plane、Registry、Dispatcher、Run/Event Ledger 和服务端 Conformance Harness 统一使用 Go 实现。通用 report/evidence/planning/Gate/blueprint、Go proxy、Go/Container build 以及 P39–P42 release/lineage/finalization 工具也统一使用根 module 的 `internal/tooling/` Go 包与私有命令。Node.js 只可用于 Schema/spec/manifest validation、Schema-driven codegen、TypeScript SDK 和 npm packaging；Python 主要用于 Agent Provider SDK，并用原生 Python/PEP 517 完成 Python package primitive。Node/Python 均不是参考后端或通用发布治理运行依赖。机器制品以 `implementation_runtime + tool_scope` 显式声明边界，Checker 同时扫描当前/规划路径、依赖、package scripts、Make/workflow 与生产镜像入口。

治理工具共享同一严格输入链：递归拒绝 JSON/YAML 重复键与多文档/尾随值，离线执行 Draft 2020-12（含 format assertion）后才进入 typed model。外部证据/信任材料同时按 separator-aware lexical path 与 EvalSymlinks canonical target 验证必须位于仓库外。机器报告把 source-tree 静态输入、前序阶段运行时输入和仓库外/ignored runtime evidence 分栏；writer 落盘前自验，任何序列化、Git、runtime 探测或 I/O 错误均 fail closed；current-worktree 精确重验 Node/Go/OS，ancestor 仅为 archive-only。Node scope 不允许 child process 或动态执行逃逸。P01 用未缓存 Go tests 实际验证这些边界，而不是把测试列表当作证据。

# 11. 实施边界

参考实现采用根公共 Go module 与唯一 `reference/control-plane` 嵌套 Go module；语言中立 Conformance Fixture 留在顶层，portable runner 留在根 module。详细组件边界、数据库语义、事务、codegen、测试和发布 DAG 见 [IMPLEMENTATION_BLUEPRINT.md](IMPLEMENTATION_BLUEPRINT.md)；最终物理布局见 [DIRECTORY_STRUCTURE.md](DIRECTORY_STRUCTURE.md)；唯一执行顺序见 [DEVELOPMENT_PLAN.md](DEVELOPMENT_PLAN.md)。

`kinglucky-agent-console` 是未来下游实现，不在本仓库 P01–P53 实施范围。Reference Control Plane、Quickstart 和 Conformance 必须独立运行；任何特定 Worker 产品都通过通用 Worker Pull/HTTP/A2A/MCP 边界接入，本仓库不提供厂商专属 Adapter。
