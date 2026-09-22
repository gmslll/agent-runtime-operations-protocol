---
title: Agent Runtime Operations Protocol 实施蓝图
status: frozen-blueprint
updated: 2026-09-22
---

# 1. 蓝图边界

本文档把已冻结的协议决策转换为可实施的组件、依赖、存储、测试和发布约束。规范语义仍以 [DECISIONS](DECISIONS.md)、[领域规范](PROTOCOL_SPECIFICATION.md)与状态机 Fixture 为权威；制品状态以 [`spec/artifact-manifest.yaml`](../spec/artifact-manifest.yaml) 为唯一机器目录；阶段顺序以 [DEVELOPMENT_PLAN](DEVELOPMENT_PLAN.md) 为准。

本阶段只冻结蓝图，不物理搬迁目录、不拆 module、不实现新协议行为。

# 2. 设计不变量

## 2.1 契约解析

- **Authoring strict**：发布者、Schema 作者和仓库生成流程对声明版本执行严格校验，未知字段、重复 JSON Key、未声明 Extension 或不闭包引用必须拒绝。
- **Consumer forward compatible**：同一协议主版本的兼容消费者必须保留或忽略未知可选字段；但影响授权、签名、Digest、幂等、副作用、必需 Extension 或状态迁移的未知语义不得宽松放行。
- **Offline closure**：所有 `$ref` 必须在发布 Bundle 内离线解析；校验 Publisher 内容时禁止网络访问，防止 SSRF、DNS 重绑和不可重现构建。

## 2.2 可靠性不变量

- 原始事件是不可变 append-only 记录；投影、快照和索引可重建，不得反向覆盖原始事件。
- 终态不可逆，必须带最终 Snapshot 或 ResultRef；迟到事件保留审计但不改写终态。
- 跨网络语义是 at-least-once；Run/Attempt/Event/Effect 分别使用稳定幂等键，`write/irreversible` 操作必须传播 `effect_id`。
- Inbox/Outbox 和业务状态同事务持久化；网络发送在提交后执行，不把“调用返回超时”当作“未执行”。
- Audit、Trace、Cancel、Deadline、Usage、`effect_id`、Inbox/Outbox，以及 Clock/ID/Fault seam 必须在它们首次相关的纵向路径内建，不允许留到“最后补运维”。

# 3. 组件和数据/控制面

```text
Author / SDK / CLI
        │ publish bundle
        ▼
Control Plane: Publication → AuthN/AuthZ → Run Service → Dispatcher
                         │                         │
                         │                         ├─ direct ticket → Runtime
Registry → Discovery ┘                         ├─ proxy → Runtime
                                                   └─ claim → Pull Worker
Runtime/Worker → Event Ingest → immutable Event Ledger → SSE projection
                         └→ Usage / Audit / Trace / Outbox
```

**Control plane** 包含发布、身份、授权、注册发现、Run/Attempt 创建、调度、Token/JWKS、用量、审计和查询。所有新 Run 都必须在这里鉴权并持久化。

**Data plane** 是已授权 Attempt 的输入、事件和结果传输。Direct 使用短期、绑定 Deployment/Run/Attempt/Audience/Scope 的 Ticket；Proxy 由 Control Plane 中继；Worker Pull 用 Claim Lease/Fencing。Registry 不代理业务字节。浏览器 v1 仅走 BFF/Proxy。

# 4. Go 双 Module 和导入规则

详细树见 [DIRECTORY_STRUCTURE](DIRECTORY_STRUCTURE.md)。最终只有根公共 module 和 `reference/control-plane` 嵌套 module。

```text
root public module
  generated models ← protocol core ← provider/consumer/registry/worker SDK
        └→ cmd/arop
        └→ cmd/arop-conformance → conformance language-neutral resources

nested reference/control-plane module
  cmd/aropd → internal/app → internal/domain + internal/ports
                                      ↑
                             internal/adapters
  imports exact released root-module version
```

禁止根 module 导入嵌套 module；禁止公共 SDK 导入 SQL/HTTP Server 内部类型；禁止 Domain 导入 Adapter。本地可使用 `go.work`，但发布校验使用临时本地 Go proxy，在 `GOWORK=off` 且嵌套 module 无 `replace` 时验证精确版本依赖。

portable runner 不依赖 Reference `internal`，只导入根公共 SDK 并读取顶层语言中立 Conformance 资源。

# 5. Reference Control Plane 领域、端口和适配器

| 区域 | 职责 | 不允许承担 |
| --- | --- | --- |
| `domain/publication` | AgentVersion Bundle、Digest、发布状态 | HTTP、SQL、Console 审批页 |
| `domain/registry` | Instance/Session/Generation/Lease/Revision/CAS/Drain | 业务请求代理 |
| `domain/run` | Run/Attempt 状态机、Cancel/Deadline、终态 | Channel UI |
| `domain/event` | Event 去重、序列、不可变 Ledger、Snapshot | 改写历史 Event |
| `domain/dispatch` | 选择已授权可用实例、Attempt/Ticket/Fencing | 越过 Control Plane 创建 Run |
| `domain/worker` | Claim/Accept/Renew/Complete、Capacity/Drain | 厂商专属工作区语义 |
| `app` | use case、跨 Aggregate 事务边界、授权编排 | 实现具体 DB/Clock/Token |
| `ports` | Repository、UnitOfWork、Clock、IDSource、Signer、FaultHook、Telemetry | 环境单例 |
| `adapters` | HTTP/SSE、SQLite/Postgres、JWKS、OTel、真/虚拟 Clock/ID/Fault | 重新定义协议语义 |

每个纵向功能先实现 Domain + Port + 两个 Storage Adapter + HTTP Binding + Conformance，而不是先写所有 Handler 再补持久化。

# 6. 存储语义与事务边界

SQLite 是 Quickstart 和单进程参考，PostgreSQL 是多节点生产参考。两者必须在同一套黑盒 Fixture 下表现一致，即“语义一致”而非“SQL 一致”。

| 事务 | 必须原子提交 |
| --- | --- |
| Publish AgentVersion | 验证后 Bundle、Digest、版本记录、Audit |
| Register/Keepalive/Drain | Instance 修订、Lease/Generation、Registry Event、Revision |
| Create Run | Authz 决定快照、Run、初始 Audit、Outbox |
| Dispatch/Claim | Attempt、Fencing Token/Lease、容量预留、Audit |
| Ingest Event | 去重键、原始 Event、Run Sequence、投影/Usage、Outbox |
| Terminal transition | 终态、Final Snapshot/ResultRef、Final Usage、Audit、释放容量 |
| Execute effect | `effect_id` Inbox 声明与业务结果；不许仅靠 Attempt ID |

跨进程通知是加速器，Ledger/Outbox 才是真值。消费者以 inbox 去重后再改变投影；Outbox 按至少一次发送并依赖下游幂等。

# 7. Migration 矩阵

两个数据库的每一个版本都要执行以下矩阵，并生成按 engine/version 分组的 JSON 和 JUnit：

| 场景 | SQLite | PostgreSQL | 通过标准 |
| --- | --- | --- | --- |
| 空库到 N | 必测 | 必测 | 建表、索引、约束和 seed 一致 |
| N-1 到 N | 必测 | 必测 | 保留数据和协议语义 |
| 幂等重跑 | 必测 | 必测 | 不重复执行、不改写 checksum |
| 并发启动 | 进程锁 | advisory/row lock | 仅一个 migrator 提交，其他安全等待/退出 |
| dirty/partial | 必测 | 必测 | 检测并 fail closed，不伪装 ready |
| backup/restore | 文件快照恢复 | `pg_dump/pg_restore` 参考流 | 恢复后同一 Contract Suite 通过 |
| downgrade/rollback | 显式声明 | 显式声明 | 破坏性 migration 只能通过备份恢复，不伪造可逆 |

Readiness 只在 migration checksum、当前版本和必需约束均通过时返回 ready。

# 8. 可测 seam 和故障注入

- `Clock`：服务端时间、Deadline、Lease、Token TTL、Retention 全部注入，不在 Domain 直接读系统时钟。
- `IDSource`：UUIDv7 和资源前缀由端口提供，Fixture 使用确定序列。
- `FaultHook`：只在预定义 checkpoint 注入 crash、delay、drop、duplicate、reorder 和 partition；生产构建默认 no-op 且不暴露远程控制面。
- `TokenSigner/Verifier`：测试键与生产键分离，时钟可控，覆盖 JWKS 旋转重叠期。
- `EventNotifier`：可丢通知，不可丢 Ledger；测试必须证明 Replay/Resync 恢复。

# 9. 安全 seam

HTTP 入口先做请求大小/超时/内容类型限制，再解析与严格校验，然后执行 AuthN/AuthZ。Run Token、Deployment Credential、Event Session Token、Asset Token 和用户 Session 使用不同 audience/scope。Endpoint/Callback/Asset URL 经过统一 SSRF 策略，包含 scheme、DNS/IP 分类、重解析和跳转检查。日志、Trace、Audit 和报告默认脱敏，Asset/Secret 只记 ID 不记凭据。

# 10. Codegen pipeline

代码生成不立即扩展到所有类型。P06 先用一组具有 union、format、`additionalProperties`、循环/外部 `$ref` 风险的代表 Schema 做 Go/Python/TypeScript spike，冻结生成器版本、nullable/optional 映射、命名、自定义 format 和 extension 策略，然后才批量生成。

```text
normative schemas + local ref bundle
 → schema lint / semantic lint / digest
 → pinned generator inputs
 → Go/Python/TypeScript generated directories
 → language compile/type-check
 → golden JSON round-trip + strict/forward-compatible modes
 → regenerate-in-clean-tree (zero diff)
 → provenance: schema digest + generator digest + command + output digest
```

OpenAPI/AsyncAPI 只引用或派生同一结构，不手写第二份 DTO。生成结果可入库以方便消费者，但不是权威源。

# 11. 测试金字塔与报告

1. 文档/Schema lint：链接闭包、本地 `$ref`、决策/需求映射、重新生成无 diff。
2. 领域单测：状态机、序列、Offset、Digest、授权判定、Clock/ID 确定性。
3. 端口契约：所有 Repository/Outbox/Lease/Event 行为同时跑 SQLite 和 PostgreSQL。
4. 传输集成：HTTP/JSON、SSE Resume、Event Batch Partial Failure、JWKS、Asset 安全。
5. 跨语言 Conformance：Go/Python/TypeScript 读取同一 Fixture，portable runner 对外部实现做黑盒测试。
6. 故障与 HA：duplicate/replay/out-of-order/cancel/deadline/fencing/crash/partition、PostgreSQL 多节点、备份恢复。
7. 发布重现：清洁克隆、`GOWORK=off`、临时 Go proxy、包管理器 dry-run、SBOM/签名/来源证明。

每个 P01–P24 的机器验收都必须产生 `build/reports/<phase-or-check>/report.json` 和 `junit.xml`。JSON 至少含 schema version、exact commit、时间、命令、环境/制品 digest、检查项与结论；JUnit 与 JSON 检查数一致。不允许只用「进程退出 0」代替可审计报告。

# 12. 发布 DAG

```text
spec freeze
 → schema/API/fixture digest
 → codegen + SDK build
 → conformance + migration + fault/HA
 → reproducible artifacts + SBOM + provenance
 → P18 code-complete verification
 → P19 external configuration evidence gate
 → P20 regenerate every public-namespace artifact and rerun all checks
 → P21 v0.1 release candidate
 → P22 independent/external evidence bound to RC
 → P23 v1 freeze verification
 → P24 signed v1 delivery
```

Go 发布使用同一 release commit 上的两个 tag：先推送根 `vX.Y.Z`，确认 proxy 可解析；嵌套 `go.mod` 已精确要求该版本且无永久 `replace`；再推送 `reference/control-plane/vX.Y.Z`。两个 tag 必须指向同一 commit。Python/npm/CLI/Container 在同一份制品 manifest 下发布，记录 source/schema/runner/artifact digest，并生成 SBOM 和 provenance。

P19 填入真实域名、包所有权、两名 Maintainer 和安全入口后，必须执行 P20 全量再生成与全套验证，不得直接发 RC。

P22 的每条外部证据必须绑定 exact commit SHA、Schema Bundle Digest、Conformance Runner Digest 和被测制品 Digest，并保存可核验来源。任何规范、Schema、传输绑定、Runner 或安全语义变更都会使相关证据失效，必须回到 P20，重新产生 RC 并重跑 P22，不能在 v1 前补签。

# 13. Console 与下游隔离

`kinglucky-agent-console` 不在本实施范围内，本计划不修改它。Console 未来只通过发布的 Schema/SDK/HTTP 契约消费 AROP，不会被 Reference Control Plane 导入，也不成为 Quickstart/Conformance 前置。Console 专属身份、组织、飞书、审批、UI 和数据库映射留在下游；公共协议不复制这些模型。

# 14. 实施入口与停机点

实施必须按 [P01–P24](DEVELOPMENT_PLAN.md) 顺序推进。P04 是用户明确确认计划的停机 Gate；未通过前不得执行 P05 物理重构。P19 和 P22 是外部证据 Gate，仓库内 Fixture 或自我签字不能代替。
