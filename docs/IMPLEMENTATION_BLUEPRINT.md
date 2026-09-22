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

每个 Control Plane 纵向功能先实现 Domain + Port + 两个 Storage Adapter + HTTP Binding + 增量 Fixture，而不是先写所有 Handler 再补持久化。P09、P10、P12、P13、P14、P18、P19、P20、P24 每次新增 Control Plane 持久化都在所属阶段同步 SQLite/PostgreSQL migration，并立即运行 `empty/N-1→N/idempotent/dirty` 子矩阵；P16 明确复用 P14 ledger/watermark，不新增 migration；P38 只读聚合这些证据。

公共 Go/Python Provider SDK 各自只定义 transactional `DurableStore` port，不导入数据库 driver。Reference Go/Python HTTP Agent 可在各自 `reference/agents/<agent>/` subtree 内提供本地 SQLite adapter 和 migration，以证明 Inbox/Outbox/`effect_id` crash recovery；它们不建立第三 Go Module，也不属于 Control Plane 双库 migration 矩阵。因此数据库规则是：公共 SDK 无 DB driver；Control Plane DB 只在 Control Plane subtree；Reference Agent 本地 adapter 只在自身 subtree。

## 5.1 公共 API 与 Reference-only 归属

| 能力 | 制品/暴露 | 归属 | 首次阶段 | 机器验收 |
| --- | --- | --- | --- | --- |
| Publication、Run create/query/command | `openapi/fragments/control-plane/*.yaml` → `openapi/control-plane-v1.yaml` | public Control Plane contract aggregate | P11/P18/P20 completion | `make test-publication-contracts`、`make test-run-lifecycle`、`make test-event-ledger` |
| JWKS discovery | Control Plane dispatch/JWKS fragment → aggregate | public Control Plane contract | P19/P20 completion | `make test-dispatch-ticket`、`make test-event-ledger` |
| Event Session exchange | Control Plane Event Session fragment → aggregate | public Agent↔Control Plane contract | P20 | `make test-event-ledger` |
| Asset upload/download token exchange | Control Plane Asset fragment + Asset broker | public Control Plane contract/service | P13/P20 completion | `make test-asset-broker`、`make test-event-ledger` |
| SecretRef resolution/exchange | `reference/control-plane/internal/ports` + deployment adapter | reference-only，不定义通用 Secret value HTTP API | P10 | `make test-identity-secrets` |
| Agent Runtime Direct/Proxy | `openapi/agent-runtime-v1.yaml` | public Runtime contract | P21 | `make test-direct-proxy-provider` |
| Registry/Discovery | 两份 Registry/Discovery OpenAPI + Go Registry SDK | public managed-runtime contract | P15 | `make test-registry-api` |
| Worker Pull | `openapi/worker-runtime-v1.yaml` | public Worker contract | P24 | `make test-worker-service` |

Token 只传递受限授权，不传递 Secret value。Secret Manager/Vault/KMS 适配是 Reference deployment 责任，不得因为参考实现有某条内部路径就将其宣称为 AROP 公共协议。

# 6. 存储语义与事务边界

SQLite 是 Quickstart 和单进程参考，PostgreSQL 是多节点生产参考。两者必须在同一套黑盒 Fixture 下表现一致，即“语义一致”而非“SQL 一致”。

| 事务 | 必须原子提交 |
| --- | --- | --- |
| Publish AgentVersion | 验证后 Bundle、Digest、版本记录、Audit |
| Register/Keepalive/Drain | Instance 修订、Lease/Generation、Registry Event、Revision |
| Create Run | Authz 决定快照、Run、初始 Audit、Outbox |
| Dispatch/Claim | Attempt、Fencing Token/Lease、容量预留、Audit |
| Ingest Event | 去重键、原始 Event、Run Sequence、投影/Usage、Outbox |
| Terminal transition | 终态、Final Snapshot/ResultRef、Final Usage、Audit、释放容量 |
| Execute effect | `effect_id` Inbox 声明与业务结果；不许仅靠 Attempt ID |
| Provider Direct/Proxy effect | Provider durable Inbox、稳定 `effect_id`、业务结果和 Outbox；crash/retry 后不得重复副作用 |

P19 的 ES256 私钥只能通过 `Signer`/KMS/`SecretRef` port 访问；数据库只保存 key metadata、public JWKS 和 rotation state，不保存可导出的私钥材料。

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

P07 交付可复用的三语言 pipeline：先用具有 union、format、`additionalProperties`、循环/外部 `$ref` 风险的代表 Schema 做受控 spike，冻结生成器版本和映射；它不得声称已经生成尚未存在的未来 Contract。P11/P15/P18/P19/P20/P22/P24 各自在新增 Schema/OpenAPI/AsyncAPI 时调用同一 pipeline，提交本域 Go/Python/TypeScript 增量输出与 provenance；P42 实现并冻结 public namespace/version wrapper，P43 用保留域名验证，P46 只把 P45 已验真实值交给同一工具全量再生成，不临时发明第二套 generator。

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

聚合 Make 目标在执行前声明预期子报告 ID/数量；聚合时拒绝缺报告、多报告、失败报告或 schema/input/checker/artifact digest 不一致。普通报告按 claimed commit 验证；历史普通 success 报告即使通过 ancestry、Git blob 和 current-input 无漂移检查，也只完成存档完整性验证，不能直接作为聚合 success；必须在隔离 checkout 重跑固定 checker，或验证受信 CI/OIDC/Sigstore provenance。P03/P04 的外部规划签署是独立类型：只要 subject commit 仍属于受控 ancestry、planning input closure 的 canonical digest 未漂移，可保留原签署，新 commit 仍重跑当前机器检查，不要求外部人员对无关后续变更重复签署。

P44 等混合 commit 聚合不要求所有报告来自同一 commit，但每份报告都必须独立满足其 phase policy；受控 phase policy 先固定 phase→Make target→checker path→required input closure 与 runner policy，绝不信任 report 自报的 command/input 集。历史 success 只能在隔离 checkout 中以固定 argv、净化环境、无 secrets 重跑（严禁 eval report.command），或验证受信 CI/OIDC/Sigstore provenance 绑定 repository identity、immutable workflow digest、commit、checker、完整 inputs、report digest 和 toolchain。P53 使用 commit A 报告时不得套用普通 ancestor 规则；P52 attestation 是唯一 A→B bridge，必须列出可桥接报告，并证明输入不含 overlay 改动或按批准 normalizer 等价；P53 的 final Conformance/SBOM/provenance 必须在 B 新跑。P03/P04/P45/P50/P52 的原始外部证据保存在仓库外，Git 仅保存脱敏、可重新核验的 canonical 内容摘要与可选签名 attestation。内容 hash 不是签名，检查器不自动生成“已批准”或“独立”证据。

| 聚合目标 | 预期子报告 | 精确数量 |
| --- | --- | --- |
| `make verify-registry` | P14、P15、P16 | 3 |
| `make verify-run-delivery` | P18、P19、P20、P21、P22 | 5 |
| `make verify-operations-security` | P08、P10、P12、P13、P18–P25 | 12 |
| `make verify-resilience` | P09、P10、P12、P13、P14、P16、P18、P19、P20、P21、P24、P27、P34、P35、P37 | 15 |
| `make validate-all` | P01–P43（包含 P03/P04 真实 Gate 报告） | 43 |
| `make verify-public-namespace` | P17、P23、P26、P38、P43、P47 | 6 |
| `make verify-v1-freeze` | P48、P49、P50，另在临时树执行 P51 unsigned overlay/tree-digest/payload-normalization testcase | 3 份子报告 |
| `make deliver-v1` | P33、P38、P43、P48、P49、P50、P51、P52，另加 P53 parent/tree-digest/payload-equivalence/Conformance/SBOM/provenance/journal-resume testcase | 8 份子报告 |

聚合器不允许“就近使用旧报告”。普通 ancestor 报告只证明其 claimed commit，必须通过受控 input closure、隔离重跑或受信 provenance 后才可复用；任何对应输入变化都必须重跑。RC commit A 到 metadata-only commit B 必须由 P52 专用 equivalence attestation 桥接，不得用普通 ancestor 复用规则代替。

P05–P38 每个 implement/refactor 报告的静态 `derives_from` 必须覆盖本阶段全部 `Artifacts owned`。P17/P23/P26/P38 的 `runtime_inputs` 必须分别精确等于上述 3/5/12/15 份报告；P44 静态依赖 P41 聚合/provenance 工具，运行时精确接收 P01–P43 共 43 份报告，不得把 P43 当作前 42 份的代理。Checker 对 `derives_from + runtime_inputs` 联合图做时序与无环校验，并以缺 P05、增加 P45、引用未来报告和构造 runtime cycle 的负例证明失败闭合。

P03/P04 验证成功时，工具先在忽略的 `build/reports/<phase>/canonical-summary.json` 产生脱敏候选；经评审后才可将候选提升为 `spec/evidence/` 内的 canonical 内容摘要，并在确实存在时附带 Ed25519 attestation。canonical summary 的逻辑字段顺序为 P03 `schema_version, kind, subject, reviewer, result, attested_at`，P04 `schema_version, kind, gate, subject, approver, result, attested_at`；实际序列化按 RFC 8785/JCS 确定键顺序，`summary_sha256` 和 `attestation` 不进入被摘要内容。任何原始评审正文、账户证明、凭据或私密联系信息都不得进入 Git。

`TRUSTED_KEYS` 指向仓库外 JSON/YAML registry，格式为 `schema_version: 1` 和 `keys[]`；每个 key 必须有 `key_id`、`algorithm: Ed25519`、`role: independent_reviewer|project_owner` 与 `public_key_pem`。`attestation.signature` 是对 canonical summary UTF-8 字节的 Base64 Ed25519 签名。没有受信 key registry 时，只能由人工可信渠道 Gate 确认，并以 `TRUSTED_CHANNEL_CONFIRMATION` 引用仓库外记录；工具只记录该文件的内容摘要，不声称自动验证了渠道或人员身份。

# 12. 发布 DAG

```text
spec freeze
 → schema/API/fixture digest
 → codegen + SDK build
 → conformance + migration + fault/HA
 → P39 package/container/SBOM/provenance/signing orchestration
 → P40 external evidence schemas/validators/trusted roles
 → P41 report provenance/lineage/aggregate tooling
 → P42 public namespace/version freeze + final-overlay/equivalence/delivery tooling
 → P43 read-only reproducible release dry-run with the same frozen generator
 → P44 private code-complete verification
 → P45 external public-configuration gate
 → P46 deliver deterministic public RC source tree with P42 tooling
 → P47 create and freeze clean RC source commit A
 → P48 strictly read-only verification on A
 → P49 publish v1.0.0-rc.N from A without changing tree
 → P50 independent/external evidence bound to A RC
 → P51 verify unsigned canonical final overlay and expected B tree
 → P52 external release-approver signs A→B equivalence attestation
 → P53 apply approved overlay, create metadata-only B and deliver v1
```

开发期 P05 的本地 proxy/bootstrap 仅用于未发布 root 时验证 nested，不是发布打包 primitive。确定性 Go module/CLI、Python、npm、Container primitive 分别由 P36/P27/P28/P37 实现；P39 只编排这些能力、跨生态版本映射、OIDC、SBOM/provenance/signing 与 journal，P40 专门实现 detached evidence Schema/validator/trusted roles，P41 专门实现 lineage/aggregate，P42 实现并冻结唯一 public namespace/version generator 与 RC freeze/final overlay/equivalence/delivery 工具，四者均不重写 build primitive。

| 实现 owner | 后期工具 | 使用阶段 |
| --- | --- |
| P39 | version policy/mapper、package/container orchestration、OIDC、SBOM/provenance/signing、journal | P43、P49、P53 |
| P40 | detached envelope、external config/conformance validator、trusted role registry | P45、P50、P52、P53 |
| P41 | public report aggregator、cross-commit lineage/provenance verifier | P44、P48、P53 |
| P42 | public namespace/version regeneration、RC freeze、final-overlay/tree-digest/payload-equivalence | P43、P46、P47、P51、P52 |
| P42 | approved-overlay/final-delivery checker | P53 |

上述 Gate 的原始证据始终在仓库外。外部 project owner/maintainer、独立实现/伙伴和 `release_approver` 分别是 P45/P50/P52 原始 signed bundle 的 producer；仓库阶段只是 first consumer/validator，只输出 `build/evidence/Pxx` 中的 verified summary，绝不替外部主体代签。P52 只接受受信 Ed25519 key 或允许的 Sigstore identity，P53 的 final release evidence 是 publisher/CI 自身的发布 provenance，不替代 P52 安全批准。

P44 及之前所有制品只允许 private/dev snapshot；取消独立公共 v0.1 里程碑。P42 冻结 generator，P43 用保留域名和临时 registries 验 deterministic RC tree、二次零 diff、placeholder 清零、双 Go module/多语言 metadata、非法配置失败和全生态 pack/install。P45 验真实外部配置；自 P45 后禁止 implement/refactor。P46 只把已验值交给同一工具，P47 创建 clean A，P48 只读验证。首个公开候选是 P49 从 A 发布的 `v1.0.0-rc.N`。

P46 输入唯一无 `v` 逻辑版本并在真实配置下生成 RC source tree，不创建 commit/tag。P47 从该树创建 A；P48 只读验证。P49 用 mapper 分别发布 root/nested Go tag、Python/npm/CLI/OCI/Schema Bundle 并生成 RC subject manifest，绑定 A/tree、P45 config digest、tag-object、artifact/SBOM/provenance/Schema/API/Fixture/Runner digests。P50 只验证外部 principals 已签 compatibility bundle，生成 detached verified summary，绝不更新 A 内 `compatibility/implementation-matrix.yaml` 或预填伙伴。

P51 的 overlay 白名单精确到 `VERSION`、Python `pyproject`/lock、npm metadata/lock、OCI/CLI/Schema Bundle metadata、nested `go.mod` RC→final 与必要 checksum。P52 外部 attestation 除 A/tree/overlay/expected-B/policy/time/bridged reports 外，必须绑定 version-policy digest 和 P45 config、P49 RC subject、P50 compatibility digests。P53 用同一无 `v` 逻辑 final version分别映射各生态，不能盲传统一 VERSION；创建单父 B 后在 B 重跑 Conformance/SBOM/provenance，并发布不进入 B tree 的 final detached evidence bundle。

跨生态版本策略只有一个逻辑输入：`1.0.0-rc.1`、`1.0.0-rc.10`、`1.0.0`。Go root/nested tags 加 `v`；Python 映射为 `1.0.0rc1`、`1.0.0rc10`、`1.0.0`；npm/OCI/CLI/Schema Bundle 保持逻辑值。Mapper 拒绝输入 `v` 前缀、PEP 440 形式、RC leading zero、build metadata；policy digest 必须进入 P39 journal/SBOM/provenance、P52 attestation 与 P53 final bundle。

# 13. Console 与下游隔离

`kinglucky-agent-console` 不在本实施范围内，本计划不修改它。Console 未来只通过发布的 Schema/SDK/HTTP 契约消费 AROP，不会被 Reference Control Plane 导入，也不成为 Quickstart/Conformance 前置。Console 专属身份、组织、飞书、审批、UI 和数据库映射留在下游；公共协议不复制这些模型。

# 14. 实施入口与停机点

实施必须按 [P01–P53](DEVELOPMENT_PLAN.md) 的 DAG 推进。P04 是用户明确确认计划的停机 Gate；未通过前不得执行 P05 物理重构。P45、P50 和 P52 是外部证据 Gate，仓库内 Fixture、自我签字或裸内容 hash 不能代替。
