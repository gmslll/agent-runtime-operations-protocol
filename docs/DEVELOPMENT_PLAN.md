---
title: Agent Runtime Operations Protocol 开发计划
status: proposed
updated: 2026-09-22
---

# 1. 最终目标

交付一套可被任意兼容 Control Plane、普通 HTTP Agent、cc-connect/Codex Worker、Web、Bot 和外部 A2A Agent 共同使用的开放企业 Agent 运行协议：

```text
定义 Manifest
→ 本地验证
→ 发布 AgentVersion
→ Runtime 注册和续租
→ 用户授权发现
→ Control Plane 创建 Run
→ Direct / Proxy / Worker Pull 执行
→ 结构化流式输出
→ 最终结果、Usage、审计和追踪
```

协议仓先稳定机器可校验的契约，再实现 SDK、Conformance 和参考运行时。金运 Console 只能作为一个实现消费协议，不得维护第二份协议定义或成为唯一测试环境。

# 2. 发布分层与完成定义

## 2.1 公共 v0.1

公共 v0.1 用来验证接入体验和外部边界，必须包含：

- Core Manifest、Run、Attempt、Event、Result 和 Error Schema。
- Agent Runtime OpenAPI 和 Streaming AsyncAPI。
- Python Provider SDK 和 Reference Agent。
- Go SDK 和 Go Reference Control Plane；Registry、Dispatcher、Run/Event Ledger 等参考后端不使用 Python/Node 实现。
- TypeScript Generated Models、SSE Consumer 和 Event Reducer。
- Docker Compose Quickstart、Event Viewer 和 Conformance CLI。
- A2A Agent Card/Task Adapter、ARD Exporter 和 MCP Integration Example。
- 明确的实验性版本、兼容性和不稳定字段标记。

v0.1 的成功标准：全新环境十分钟内完成首次流式 Run，不安装金运 Console也能运行。

## 2.2 Protocol v1.0

Protocol v1.0 满足以下条件才可以冻结：

- Manifest、Run、Attempt、Event、Registry、Worker 和公共类型 Schema 完整。
- Agent Runtime、Registry、Worker、Asset 和 Event Ingest OpenAPI 完整。
- 流式事件 AsyncAPI 完整。
- Go 和 Python 有独立兼容实现，TypeScript 可以消费 Run 和流式事件。
- Direct、Proxy 和 Worker Pull 使用同一套 Run/Event 语义。
- Registry Lease、Generation、Revision、Watch 和 Resync 通过契约测试。
- 重复、断线、乱序、重试、取消、超时和 Fencing 有测试向量。
- 普通 HTTP Agent 和 cc-connect/Codex Worker 完成端到端接入。
- Web 与飞书 Bot 能调用同一个 AgentDefinition，并获得一致权限结果。
- 至少三个金运以外的设计伙伴完成真实接入。
- 至少两个独立 Runtime 实现和一个非金运 Control Plane/Validator 通过 Conformance。
- 公共名称、许可证、治理、命名空间和安全响应流程稳定。

# 3. 阶段 0：决策冻结与仓库基线

目标：把已经确认的跨语言和 wire format 决策落实为仓库基线，并填写公共发布所需的真实账户配置。

任务：

- 在 `gmslll/agent-runtime-operations-protocol` 创建初始仓库；不等待公共组织成立。
- 为协议文本、Schema、SDK、Conformance 和 Reference Implementation 添加 Apache-2.0。
- 落地 RFC、双 Maintainer 审核、DCO 和私密安全报告流程。
- 按 D-020～D-040 落地 UUIDv7、UTF-8 Offset、CloudEvents 命名、JCS Digest、Lease、Token、Usage 和 Capability 规则。
- 核验 `arop-sdk`、`@arop/sdk` 和 `arop` CLI 的注册表所有权；Go Module 使用最终个人 GitHub 地址。
- 填写项目域名并冻结 Schema `$id`、Event Type Prefix 和 Extension Namespace。
- 首批 A2A Fixture 固定到 v1.0.1；其他外部标准在 Compatibility Matrix 中固定精确版本或 commit。
- 建立 Git 基线、变更审查和兼容性检查规则。
- 建立 `make validate`、lint、Schema 校验和文档链接检查。

验收：

- `docs/DECISIONS.md` 中影响阶段 1～3 的架构和 wire 问题均有结论。
- 全新克隆可运行仓库验证命令。
- 个人 GitHub Remote、包名、域名与 Registry 完成冲突和所有权核验。
- 仓库具备 `CONTRIBUTING.md`、`CODE_OF_CONDUCT.md`、`SECURITY.md` 和 RFC 模板。

# 4. 阶段 1：Core 公共模型与 Manifest

目标：开发者能提交机器可校验、可发布的 Agent 能力定义。

交付：

```text
schemas/common/identifiers.schema.json
schemas/common/error.schema.json
schemas/common/trace.schema.json
schemas/common/content-part.schema.json
schemas/resources/asset-ref-v1.schema.json
schemas/resources/data-ref-v1.schema.json
schemas/resources/secret-ref-v1.schema.json
schemas/manifest/agent-manifest-v1.schema.json
examples/manifest/
```

覆盖：

- AgentDefinition、AgentVersion 和 AgentSkill。
- 参数与对话调用模式。
- Input/Output Schema 和 ContentPart。
- Asset、Data、Secret 和网络能力。
- Session、Timeout、并发和流式能力。
- 副作用、幂等和 Usage Meter。
- Owner、Visibility、Maturity 和 Risk。

验收：

- Go、Python 对同一 Fixture 得到相同 Digest。
- 非法 Endpoint、Secret、飞书字段和本机路径不能进入 Manifest。
- 最小、完整和非法黄金样例通过预期校验。
- Manifest 可以确定性导出 A2A Agent Card 和 ARD Entry 的基础字段。

# 5. 阶段 2：Run、Attempt 与事件

目标：冻结调用和流式通信的核心语义。

交付：

- RunRequest、RunAccepted、RunStatus 和 RunResult。
- Attempt、DispatchTicket 和 DeliveryPlan。
- Event Envelope 与首期 Event Data。
- Command、Cancel、InputRequired 和 Approval。
- Usage、Error、ResultRef 和 Output Snapshot。
- Run/Attempt 状态迁移 Fixture。

事件至少覆盖：

```text
run.accepted / run.started / run.waiting_input
run.succeeded / run.failed / run.cancelled / run.timed_out
output.started / output.delta / output.snapshot / output.reset / output.completed
progress.updated / step.started / step.completed
tool.started / tool.completed / asset.created
usage.updated / error.raised / heartbeat
```

验收：

- `run_id` 跨重试稳定，每次执行使用新的 `attempt_id`。
- Producer Sequence 与 Run Sequence 分离。
- 终态不可逆并带最终 Snapshot 或 ResultRef。
- Event 冲突、迟到终态、Cancel 竞态和 Effect 幂等有测试。
- 中英文文本 Offset 跨语言一致。

# 6. 阶段 3：Agent Runtime OpenAPI

目标：普通 HTTP Agent 不理解 Console 内部结构也能接入。

Agent 侧：

```text
POST /v1/runs
GET  /v1/runs/{run_id}
GET  /v1/runs/{run_id}/events
POST /v1/runs/{run_id}/commands
GET  /v1/health/live
GET  /v1/health/ready
```

Control Plane 侧：

```text
POST /v1/agent-runs/{run_id}/event-session
POST /v1/agent-runs/{run_id}/events:batch
GET  /v1/agent-runs/{run_id}/events
```

任务：

- 完成 Run Token/JWKS 校验和 Event Session Token 交换。
- 固定 SSE Event ID、Resume 和错误结束语义。
- 固定 Event Batch、ACK、重复和 Partial Failure。
- 完成 Mock Control Plane 和 Mock Agent。

验收：

- Direct 与 Proxy 使用同一 RunRequest。
- Direct Stream 与 Relay Stream 可断点恢复。
- Token Claims 不匹配时拒绝。
- Control Plane 短暂不可用时 Outbox 可重试。

# 7. 阶段 4：注册与发现

目标：实现多个 Dispatcher 可安全消费的 Runtime Discovery 契约。

接口：

```text
PUT    /v1/registry/instances/{instance_id}
POST   /v1/registry/leases/{lease_id}/keepalive
PATCH  /v1/registry/instances/{instance_id}
POST   /v1/registry/instances/{instance_id}/drain
DELETE /v1/registry/instances/{instance_id}
GET    /v1/discovery/agents/{agent_id}/instances
GET    /v1/discovery/changes
```

任务：

- Deployment Credential、Manifest Digest 和 Binding 校验。
- `instance_id + session_id + generation`、Lease 和服务端时间。
- Runtime/Operator Metadata 和正交健康状态。
- Resource Version、ETag、CAS 和全局 Revision。
- Snapshot、Watch、Compaction、Resync 和 Router Cache。

验收：

- 旧 Session 被 Fencing，Lease 到期实例不可发现。
- Watch 丢通知后可 Replay，压缩后可全量 Resync。
- Drain 实例不接收新 Attempt。

# 8. 阶段 5：Worker Pull

目标：让 cc-connect、Mac mini 和内网 Agent 无需入站地址也能接入。

接口：

```text
POST /v1/workers/{worker_id}/claims:next
POST /v1/attempts/{attempt_id}/lease:renew
POST /v1/attempts/{attempt_id}/accept
POST /v1/attempts/{attempt_id}/events:batch
POST /v1/attempts/{attempt_id}/complete
```

任务：

- 长轮询 Claim、Attempt Lease 和 Fencing Token。
- Accept 前后超时、Session Affinity、Capacity 和 Drain。
- cc-connect/Codex Adapter 参考实现。

验收：

- Worker 崩溃后可重新分配 Attempt。
- 旧 Worker 不能覆盖新 Attempt 的终态。
- Pull 和 Direct 产生相同标准事件。

# 9. 阶段 6：公共 v0.1 SDK 与 Quickstart

Go 交付：Generated Models、Validator、Provider/Consumer、Registry、Worker、Token Validator、Inbox/Outbox、SSE、Trace 和 Drain。

Python 交付：Generated Models、FastAPI/ASGI、`Agent.from_manifest()`、`@agent.skill()`、Async Event Emitter、SQLite Inbox/Outbox 和 Pytest Fixture。

TypeScript 交付：Generated Models、Relay SSE Client、Event Reducer、Snapshot/Delta 合并器、Resume 和去重。

公共开发环境交付：

- Go Reference Control Plane。
- Python 和 Go Reference Agent。
- Docker Compose Quickstart。
- Web Streaming Demo。
- Conformance CLI。

验收：

- Python、Go 和 TypeScript 通过同一组公共 Fixture。
- 全新环境十分钟内完成首次流式 Run。
- 现有 Python Agent 的核心业务改造目标不超过约三十行。
- Web 断线重连不丢失事件，最终 Snapshot 能纠正中间 Delta。
- 所有流程不依赖金运 Console。

# 10. 阶段 7：外部标准 Adapter

交付：

- A2A Agent Card Import/Export。
- A2A Task/Message/Artifact Adapter。
- ARD/AI Catalog Exporter 和 Import Review Fixture。
- MCP Dependency Declaration 和 Tool Trace 示例。
- CloudEvents Structured、Binary 和 Batch Fixture。
- OpenTelemetry Trace/Metric/Log 映射。

验收：

- A2A 生命周期、取消、错误和产物映射有自动测试。
- ARD 导出不泄露 RuntimeInstance、Lease 或内网 Endpoint。
- Direct、Proxy、Pull、A2A 和 MCP Adapter 保持 Trace 关联。
- 任何有损映射都能被检测并在报告中说明。

# 11. 阶段 8：外部设计伙伴与独立实现

目标：证明协议不是金运内部接口。

任务：

- 支持至少三个外部团队接入不同类型的 Agent。
- 提供独立实现指南，不要求复制参考实现内部结构。
- 邀请外部团队实现第二个 Runtime 或 Control Plane/Validator。
- 把实际互操作问题形成 RFC、Fixture 和 Compatibility Matrix。
- 统计首次运行时间、业务改造行数和 Conformance 结果。

验收：

- 外部 Agent 可以在 Reference Control Plane 和金运 Console 间迁移。
- 两个独立实现对同一 Schema、Digest、Event 和 Error 解释一致。
- 至少一名非金运贡献者参与协议变更评审。

# 12. 阶段 9：Conformance 与故障注入

测试矩阵：

```text
合法/非法 Manifest、Digest 不匹配、协议版本无交集
重复 Run、重复 Event、Event ID 冲突、Sequence 缺口
Direct/Relay Stream 断线恢复、Outbox 重试
Lease 过期、Generation Fencing、Attempt Fencing、Drain
Cancel 竞态、Deadline、副作用重试、Asset Token 过期
Registry Watch Compaction、Control Plane 多节点切换
A2A 映射、ARD 数据泄露、CloudEvents 三种 HTTP 编码
OpenTelemetry Trace 跨 Adapter 传播、未知 Extension 降级
```

交付 Provider、Consumer、Registry Client 和 Worker Conformance Suite，以及故障代理和测试报告模板。

# 13. 阶段 10：金运生产集成

1. Console 导入并发布 Manifest。
2. Runtime 注册、续租并进入 Discovery View。
3. Web 与飞书发现同一个 Agent。
4. 两端创建 Run 时经过同一权限判断。
5. Dispatcher 选择 Deployment。
6. Direct、Proxy 或 Worker Pull 完成执行。
7. 两端接收流式事件和最终结果。
8. Usage、审计和 Trace 可查询。
9. 撤权后新 Run 立即拒绝。
10. Drain/升级不破坏已有 Run。

金运集成产生的新需求必须先判断是公共能力还是 `x-kinglucky-*` 扩展，不得直接把飞书、组织表或 Console 数据库字段写入核心协议。

# 14. 发布流程

每次发布包含 Schema、OpenAPI、AsyncAPI、黄金样例、SDK、Adapter、兼容矩阵、Changelog、迁移说明、SBOM 和 Conformance Report。

```text
冻结规范和 Schema
→ 生成模型
→ SDK 与参考实现测试
→ Conformance Matrix
→ Release Candidate
→ Reference Control Plane 与独立实现验证
→ 金运跨仓集成验证
→ 正式发布
```

# 15. 延后内容

以下能力保留架构位置，但不阻塞第一个可运行闭环：

- gRPC、WebSocket Transport Binding。
- Portable Session Checkpoint 和 Hedged Execution。
- 多区域调度、公共 Agent 市场和多租户 SaaS。
- 重型工作流引擎。

延后实现不等于删除协议位置；提前实现必须先更新决策文档和兼容性测试。
