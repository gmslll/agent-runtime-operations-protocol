---
title: Agent Runtime Operations Protocol 实施蓝图
status: review-candidate
updated: 2026-09-22
---

# 1. 蓝图边界

本文档把受控但尚待用户批准的协议决策转换为可实施的组件、依赖、存储、测试和发布约束。规范语义仍以 [DECISIONS](DECISIONS.md)、[领域规范](PROTOCOL_SPECIFICATION.md)与状态机 Fixture 为权威；制品状态以 [`spec/artifact-manifest.yaml`](../spec/artifact-manifest.yaml) 为唯一机器目录；阶段顺序以 [DEVELOPMENT_PLAN](DEVELOPMENT_PLAN.md) 为准。

P04 用户 Gate 前本蓝图状态是 `review-candidate`：内容受控但尚未冻结；此时不物理搬迁目录、不拆 module、不实现新协议行为。

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

禁止根 module 导入嵌套 module；禁止公共 SDK 导入 SQL/HTTP Server 内部类型；禁止 Domain 导入 Adapter。开发期可使用本地 `go.work`，但默认不提交真实工作区文件，只提交 `go.work.example`。预发布校验先在临时本地 Go proxy 放入根 module 伪版本/RC 候选，再在 `GOWORK=off` 且嵌套 module 无 `replace` 时验证其精确版本依赖。

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

每个纵向功能先实现 Domain + Port + 两个 Storage Adapter + HTTP Binding + 增量 Fixture，而不是先写所有 Handler 再补持久化。P09、P12、P13、P14、P16、P18、P19、P20、P21、P24 每次新增持久化都在所属阶段同步 SQLite/PostgreSQL migration，并立即运行 `empty/N-1→N/idempotent/dirty` 子矩阵；P38 只读聚合这些证据。

## 5.1 公共 API 与 Reference-only 归属

| 能力 | 制品/暴露 | 归属 | 首次阶段 | 机器验收 |
| --- | --- | --- | --- | --- |
| Publication、Run create/query/command | `openapi/control-plane-v1.yaml` | public Control Plane contract | P11/P18 | `make test-publication-contracts`、`make test-run-lifecycle` |
| JWKS discovery | `openapi/control-plane-v1.yaml` | public Control Plane contract | P11/P19 | `make test-publication-contracts`、`make test-dispatch-ticket` |
| Event Session exchange | `openapi/control-plane-v1.yaml` | public Agent↔Control Plane contract | P11/P20 | `make test-publication-contracts`、`make test-event-ledger` |
| Asset upload/download token exchange | `openapi/control-plane-v1.yaml` + Asset broker | public Control Plane contract/service | P11/P13 | `make test-publication-contracts`、`make test-asset-broker` |
| SecretRef resolution/exchange | `reference/control-plane/internal/ports` + deployment adapter | reference-only，不定义通用 Secret value HTTP API | P10 | `make test-identity-secrets` |
| Agent Runtime Direct/Proxy | `openapi/agent-runtime-v1.yaml` | public Runtime contract | P21 | `make test-direct-proxy-provider` |
| Registry/Discovery | 两份 Registry/Discovery OpenAPI + Go Registry SDK | public managed-runtime contract | P15 | `make test-registry-api` |
| Worker Pull | `openapi/worker-runtime-v1.yaml` | public Worker contract | P24 | `make test-worker-service` |

Token 只传递受限授权，不传递 Secret value。Secret Manager/Vault/KMS 适配是 Reference deployment 责任，不得因为参考实现有某条内部路径就将其宣称为 AROP 公共协议。

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
| Provider Direct/Proxy effect | Provider durable Inbox、稳定 `effect_id`、业务结果和 Outbox；crash/retry 后不得重复副作用 |

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

HTTP 入口先做请求大小/超时/内容类型限制，再解析与严格校验，然后执行 AuthN/AuthZ。Run Token、Deployment Credential、Event Session Token、Asset Token 和用户 Session 使用不同 audience/scope。P12 在 Manifest 发布时完成 Endpoint 静态 scheme/host/IP policy；P13 Asset broker 和 P21 Provider 调用在每次实际连接及每个 redirect hop 重新解析、分类并校验 DNS/IP。日志、Trace、Audit 和报告默认脱敏，Asset/Secret 只记 ID 不记凭据。

# 10. Codegen pipeline

P07 交付可复用的完整三语言 pipeline：先用具有 union、format、`additionalProperties`、循环/外部 `$ref` 风险的代表 Schema 做受控 spike，冻结生成器版本和映射；同一阶段再把已验证 pipeline 扩展到全量 Schema。后续 P43 只调用它做真实 namespace 全量再生成，不临时发明第二套 generator。

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

每个 P01..Pn 的机器验收都必须产生 `build/reports/<phase>/report.json` 和 `junit.xml`。JSON 至少含 schema version、exact commit/dirty 状态、实际命令、Node/Go/OS runtime、输入集与 Checker Digest、testcase 数和结论；JUnit 与 JSON 的 testcase/failure 数必须一致。不允许只用「进程退出 0」代替可审计报告。

聚合 Make 目标在执行前声明预期子报告 ID/数量；聚合时拒绝缺报告、多报告、失败报告、commit 不一致或 schema/input/checker/artifact digest 不一致。P03/P04/P42/P46 的原始外部证据保存在仓库外，Git 仅保存脱敏、可重新核验的 canonical 内容摘要与可选 Ed25519 attestation。内容 hash 不是签名，检查器不自动生成“已批准”或“独立”证据。

| 聚合目标 | 预期子报告 | 精确数量 |
| --- | --- | --- |
| `make verify-registry` | P14、P15、P16 | 3 |
| `make verify-run-delivery` | P18、P19、P20、P21、P22 | 5 |
| `make verify-operations-security` | P08、P10、P12、P13、P18–P25 | 12 |
| `make verify-resilience` | P09、P12、P13、P14、P16、P18、P19、P20、P21、P24、P34、P35、P37 | 13 |
| `make validate-all` | P01–P40（包含 P03/P04 真实 Gate 报告） | 40 |
| `make verify-public-namespace` | P17、P23、P26、P38、P40、P43 | 6 |
| `make verify-v1-freeze` | P44、P45、P46，另在临时树执行 P47 overlay/tree-digest testcase | 3 份子报告 |
| `make deliver-v1` | P33、P38、P40、P44、P45、P46、P47，另加 P48 tree-digest/payload-equivalence/Conformance/SBOM/provenance testcase | 7 份子报告 |

聚合器不允许“就近使用旧报告”；上表所有子报告必须属于本次 exact commit 和本次声明的输入/制品 digest。

P03/P04 验证成功时，工具先在忽略的 `build/reports/<phase>/canonical-summary.json` 产生脱敏候选；经评审后才可将候选提升为 `spec/evidence/` 内的 canonical 内容摘要，并在确实存在时附带 Ed25519 attestation。canonical summary 的逻辑字段顺序为 P03 `schema_version, kind, subject, reviewer, result, attested_at`，P04 `schema_version, kind, gate, subject, approver, result, attested_at`；实际序列化按 RFC 8785/JCS 确定键顺序，`summary_sha256` 和 `attestation` 不进入被摘要内容。任何原始评审正文、账户证明、凭据或私密联系信息都不得进入 Git。

`TRUSTED_KEYS` 指向仓库外 JSON/YAML registry，格式为 `schema_version: 1` 和 `keys[]`；每个 key 必须有 `key_id`、`algorithm: Ed25519`、`role: independent_reviewer|project_owner` 与 `public_key_pem`。`attestation.signature` 是对 canonical summary UTF-8 字节的 Base64 Ed25519 签名。没有受信 key registry 时，只能由人工可信渠道 Gate 确认，并以 `TRUSTED_CHANNEL_CONFIRMATION` 引用仓库外记录；工具只记录该文件的内容摘要，不声称自动验证了渠道或人员身份。

# 12. 发布 DAG

```text
spec freeze
 → schema/API/fixture digest
 → codegen + SDK build
 → conformance + migration + fault/HA
 → P39 implement release orchestration/evidence toolchain
 → P40 read-only reproducible release dry-run
 → P41 private code-complete verification
 → P42 external public-configuration gate
 → P43 regenerate every public-namespace artifact
 → P44 final public-namespace verification
 → P45 deliver v1.0.0-rc.N from commit A
 → P46 independent/external evidence bound to commit A RC
 → P47 read-only freeze + deterministic final overlay/tree digest
 → P48 exact approved overlay creates metadata-only commit B and delivers v1
```

开发期 P05 的本地 proxy/bootstrap 先装入 root pseudo-version，再以 `GOWORK=off` 验证 nested。语言打包 primitive 分别由 P27/P28 实现，Container primitive 由 P37 实现；P39 只编排这些能力并实现 SBOM/provenance/signing、外部证据 validator、聚合器与 freeze/delivery checker，不重写 build primitive。

| P39 实现的后期工具 | 使用阶段 |
| --- | --- |
| external-config evidence Schema + validator | P42 |
| public namespace report aggregator | P44 |
| external-conformance evidence Schema + validator | P46 |
| deterministic freeze/final-overlay/tree-digest checker | P47 |
| approved-overlay/payload-equivalence/final-delivery checker | P48 |
| 通用 report schema/verifier 集成、SBOM/provenance/signing orchestration | P40–P48 |

上述 Gate 的原始证据始终在仓库外；validator 只能接受受信 key registry 中对应角色的有效签名，或显式人工可信渠道记录，不能把自报身份或裸内容 hash 当作批准。

P41 前所有制品只允许 private/dev snapshot；取消独立公共 v0.1 里程碑。P42 填入真实域名、包所有权、两名 Maintainer 和安全入口后，必须执行 P43 全量再生成和 P44 只读验证。首个公开候选是 P45 的最终 `v1.0.0-rc.N`，不得用 v0.1 或本地伪制品代替。

P45 RC 对应 source commit A；A 中 nested `go.mod` 精确 require root RC。P46 的每条外部证据必须绑定 commit A、Schema Bundle、Runner 和被测 Artifact Digest。任何 tracked 规范、Schema、传输、Runner、安全或兼容变化都使证据失效，必须回 P43 产生新 RC 并重取证。

P47 在临时树生成确定性 final overlay，仅允许版本/channel/tag 元数据、nested `go.mod` 的 root 依赖从 RC 改为 `v1.0.0` 以及必要 lock/checksum。它审核 overlay digest、预期 commit B tree digest 和 payload equivalence，并用签名 A→B equivalence attestation 连接 P46 的 commit A 证据。P48 只能精确应用该 overlay 创建 release-metadata-only commit B；B tree digest 必须等于 P47 批准值，再按 root final→proxy 可解析→nested final 顺序发布。Schema/API/Runner、生成模型和逻辑不得变化，最终本地制品必须重跑 Conformance/SBOM/provenance。

# 13. Console 与下游隔离

`kinglucky-agent-console` 不在本实施范围内，本计划不修改它。Console 未来只通过发布的 Schema/SDK/HTTP 契约消费 AROP，不会被 Reference Control Plane 导入，也不成为 Quickstart/Conformance 前置。Console 专属身份、组织、飞书、审批、UI 和数据库映射留在下游；公共协议不复制这些模型。

# 14. 实施入口与停机点

实施必须按 [P01–P48](DEVELOPMENT_PLAN.md) 的 DAG 推进。P04 是用户明确确认计划的停机 Gate；未通过前不得执行 P05 物理重构。P42 和 P46 是外部证据 Gate，仓库内 Fixture 或自我签字不能代替。
