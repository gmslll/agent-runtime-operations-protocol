---
title: Agent Runtime Operations Protocol 开发与发布计划
status: planning-candidate
updated: 2026-09-23
---

# 1. 执行约定

本计划是连续 P01..Pn 的单 Agent 阶段 DAG。阶段类型只使用 paseo-epic 支持的 `implement`、`refactor`、`verify · review`、`verify · spec`、`gate` 和 `deliver`。`Components` 必须来自受控集合；`Artifacts owned` 必须引用 [`spec/artifact-manifest.yaml`](../spec/artifact-manifest.yaml) 中由该阶段唯一拥有的关键制品。机器检查依赖真实边界，不再把 Scope 字数当作“单 Agent 可完成”的证明。

每个阶段的 Make 验收必须生成 `build/reports/<phase>/report.json` 与 `junit.xml`；聚合目标的子报告数量、输入/Checker Digest 必须一致。普通报告按 claimed commit 验证；历史成功报告只能在隔离 checkout 对 claimed commit 重跑同一 checker，或验证受信 CI/OIDC/Sigstore 对 report digest、commit、checker 与 inputs 的 provenance attestation，并确认当前相关输入未漂移；否则必须重跑。P03/P04 的历史规划签署在 planning inputs 未漂移时可复用；commit A→B 只能由专用 equivalence attestation 桥接。P03/P04/P45/P50/P52 只校验真实外部证据，不自动伪造证据。原始外部证据保存在仓库外；Git 仅接受脱敏 canonical 内容摘要、可选验签 attestation 和机器报告，未验签的 SHA-256 只能称为内容摘要。

所有新增 Control Plane 持久化阶段必须同时提交 SQLite/PostgreSQL migration，并在本阶段运行 `empty`、`N-1→N`、`idempotent`、`dirty` 子矩阵；P38 只聚合既有结果，不能首次补 migration。Reference Agent 的本地 SQLite adapter 属于自身进程可靠性边界，不进入 Control Plane 双库矩阵。P44 前所有产物都是 private/dev snapshot；取消独立公共 v0.1 里程碑，首个公开候选仅为 P49 的 `v1.0.0-rc.N`。

# 2. 状态和里程碑

| 区间 | 里程碑 | 当前含义 |
| --- | --- | --- |
| P01–P04 | 规划、独立审计、用户 Gate | P01/P02 候选完成；等待 P03 重新审计和 P04 用户确认 |
| P05–P37 | 可运行实现 | 两 Module、Control Plane、SDK、互操作、Conformance 和部署 |
| P38–P44 | 私有发布工具与验证 | resilience、四段发布工具、release dry-run、code-complete；仍不是公共发行 |
| P45–P49 | public v1 RC | 外部配置 Gate、生成 RC 树、单独创建/冻结 clean commit A、只读验证、从 A 发布 `v1.0.0-rc.N` |
| P50–P53 | 外部证据与 v1 | 证据绑定 RC commit A，生成 unsigned canonical final overlay candidate，外部签名批准，再从直接单父 parent=A 的 metadata-only commit B 发布 v1 |

# 3. 可调度阶段

## P01 — Contract authority and metadata integrity

- **Type:** implement
- **Status:** complete
- **Capability owner:** spec-governance
- **Components:** governance
- **Artifacts owned:** spec-index-validation, spec-index-report-orchestrator, spec-index-governance, spec-index-governance-tests, go-structured-file-tools, go-structured-file-tools-tests, go-schema-validator, go-schema-validator-tests, check-report-meta-schema, p01-fixed-test-inventory-schema, p01-fixed-test-inventory, p01-fixed-test-runner, p01-fixed-test-runner-tests, planning-canonical-evidence-summary-schema, planning-trusted-key-registry-schema, controlled-input-manifest-library, controlled-input-manifest-tests, machine-report-writer, machine-report-verifier-library, machine-report-verifier, machine-report-verifier-tests, evidence-lineage-tests, evidence-validation-tests, planning-audit-evidence-schema, user-gate-evidence-schema, evidence-validation-library, planning-audit-validation, user-gate-validation
- **Goal:** 建立 Decision 权威链、14 条不可变 `statement_original_zh`、冲突台账、制品 DAG 和规划 Meta-Schema 候选。
- **Scope:** `docs/DECISIONS.md`、`spec/*`、Node 仅执行无进程逃逸的 Schema/spec/manifest 校验，Go 编排并生成公共 report、evidence、planning/Gate 验证；P01 使用受 Schema 和 artifact manifest 管理的 exact package+test ID inventory，净化 Go 选测/缓存环境并解析 `go test -json`；统一执行 separator-aware、symlink-aware 路径边界和 current runtime 复核；不改业务实现。
- **Dependencies:** none
- **First-path invariants:** Authoring strict/Consumer forward compatible；离线 `$ref`；先校验再 Digest；制品路径唯一、DAG 无环、语言派生正确。
- **Machine acceptance:** `make spec-index-check` → `build/reports/P01/report.json` 和 `junit.xml`。
- **Rollback point:** 恢复上一份规划元数据；冲突或 DAG 错误时不进入 P02。
- **Definition of done:** Meta-Schema、路径、DAG、Decision/链接/引用闭包和反例探针全绿；P01 实际 package/test 终态集合与 tracked inventory 完全一致且全部 pass，fail/skip/cache/no-tests 均为零，`GOFLAGS=-run`、TestMain/no-tests、cache、缺测试和多测试负例失败闭合；报告带 commit/dirty/command/runtime/input/checker digest，current-worktree 精确重验 Node/Go/OS，ancestor 只标记 archive-only；allowlisted Node 工具仍拒绝 child process、动态加载和 eval/Function。

## P02 — Schedulable implementation blueprint

- **Type:** implement
- **Status:** complete
- **Capability owner:** implementation-planning
- **Components:** planning
- **Artifacts owned:** blueprint-validation-library, blueprint-validation, blueprint-validation-tests
- **Goal:** 形成双 Go Module、组件边界、单 Agent 阶段 DAG、API 归属、机器验收和 v1 发布闭环候选。
- **Scope:** `DIRECTORY_STRUCTURE.md`、`IMPLEMENTATION_BLUEPRINT.md`、本计划、README/AGENTS/ARCHITECTURE/SDK 交叉引用和 Go blueprint checker；执行 `implementation_runtime + tool_scope` 边界及恶意别名/路径负例。
- **Dependencies:** P01
- **First-path invariants:** 根公共 module + 唯一 Reference CP 嵌套 module；Conformance 语言中立；Console/厂商 Adapter 隔离。
- **Machine acceptance:** `make blueprint-check` → `build/reports/P02/report.json` 和 `junit.xml`。
- **Rollback point:** 仅回退规划文档/检查器，不物理搬迁目录。
- **Definition of done:** P01..Pn 连续无环，类型、组件、关键制品唯一 owner 和真实 Make 验收可追溯；P01–P04 目标当前存在，P05 后新增目标在实现前为 planned，已存在 baseline 可为 present 并显式标记未来 owner/refactor。

## P03 — Independent planning audit

- **Type:** verify · review
- **Status:** pending-review
- **Capability owner:** independent-reviewer
- **Components:** governance
- **Artifacts owned:** none
- **Goal:** 由未参与 P01/P02 撰写的审核者验证需求覆盖、可调度性和必修项归零。
- **Scope:** 只读核心文档/规划元数据；输入为仓库外审计证据，仓库只生成脱敏 canonical 内容摘要报告和可选验签结果。
- **Dependencies:** P02
- **First-path invariants:** IR-01–IR-14 与 P01–最后阶段逐条 `PASS`；count 必须是非负、可表示的严格整数，`must_fix_count=0` 且明细为空；subject commit 是当前 HEAD 或其祖先，requirements/plan/blueprint/artifact-manifest 四份证据 digest、`git show subject:path` blob 与 current HEAD/worktree 三方精确匹配；subject→HEAD ancestry 中修改后回退或删除任一 planning blob 都拒绝；`summary_sha256` 重算一致。EVIDENCE、TRUSTED_KEYS、TRUSTED_CHANNEL_CONFIRMATION 的词法路径和 EvalSymlinks 后真实路径都必须在仓库外。
- **Machine acceptance:** `make planning-audit EVIDENCE=<outside-repo-review> TRUSTED_KEYS=<outside-repo-key-registry>`，或人工 Gate 提供 `TRUSTED_CHANNEL_CONFIRMATION=<outside-repo-record>` → `build/reports/P03/report.json` 和 `junit.xml`。
- **Rollback point:** must-fix 非零时回 P01/P02 修复并重新独立审计，不编辑证据伪造通过。
- **Definition of done:** 14 条需求和全部阶段结果完整、digest 匹配、must-fix 为零；审核者独立性由受信 key 角色与 Ed25519 签名或人工可信渠道确认。

## P04 — User implementation gate

- **Type:** gate
- **Status:** blocked
- **Capability owner:** project-owner
- **Components:** governance
- **Artifacts owned:** none
- **Goal:** 在任何物理重构和新协议行为前获得用户对已审计计划的明确确认。
- **Scope:** P03 成功报告与稳定 canonical summary Digest、计划 subject commit/digest、仓库外用户批准证据。
- **Dependencies:** P03
- **First-path invariants:** Gate 绑定已审计版本的 P03 canonical summary 及 promoted envelope 的 JCS/file digest；P04 重新 strict/schema 验证 P03 原始 evidence 和 reviewer trust/confirmation，重跑 `Authenticate`，并将 raw/trust/P03 report/promoted envelope 逐项交叉核对；P03 checker path、command、required checks、static input closure、runtime inputs/evidence 使用固定政策；P04 同样对 requirements/plan/blueprint/artifact-manifest 做证据字段、subject Git blob、current closure 三方校验，并拒绝 changed→reverted/deleted ancestry；IR-01–IR-14 与 P01–最后阶段逐条 `PASS`，严格整数 count、零 must-fix，`summary_sha256` 可重算；三类外部材料执行同一 lexical/canonical containment，工具只验证而不自动批准。
- **Machine acceptance:** `make gate-check GATE=P04 EVIDENCE=<outside-repo-approval> TRUSTED_KEYS=<outside-repo-owner-key-registry> P03_EVIDENCE=<outside-repo-review> P03_TRUSTED_KEYS=<outside-repo-reviewer-key-registry>`，两个角色均可分别改用对应的 `*_TRUSTED_CHANNEL_CONFIRMATION=<outside-repo-record>` → `build/reports/P04/report.json` 和 `junit.xml`。
- **Rollback point:** 用户要求修改时回 P02/P03；任一受审输入新 Digest 使旧批准失效，仅 commit 前进且输入不变时不要求重复批准。
- **Definition of done:** canonical hash 可重算，批准者由受信 `project_owner` key 或人工可信渠道确认；此前 P05 保持未开始。

## P05 — Physical two-module refactor and local Go proxy bootstrap

- **Type:** refactor
- **Status:** pending
- **Capability owner:** repository-layout
- **Components:** repository, release
- **Artifacts owned:** root-go-module, root-go-sum, nested-control-plane-go-module, go-work-example, ci-workspace-validation, go-module-proxy-bootstrap
- **Baselines transitioned:** reference-control-plane-lite-baseline(migrate-retire)
- **Goal:** 保留并收口已有 root module，新增唯一 nested module，并让未发布 root module 时 nested module 仍可在 `GOWORK=off` 下验证。
- **Scope:** 物理搬迁并退休 `reference/control-plane-lite` 基线、验证已有根 `go.mod`、新增 `reference/control-plane/go.mod`、`go.work.example`、最小本地 Go module proxy/bootstrap harness 和 CI 骨架；不改变 wire 行为。
- **Dependencies:** P04
- **First-path invariants:** 仅两个 `go.mod`；真实 `go.work` 默认不提交；bootstrap 先把 root pseudo-version 写入临时 proxy，再让 nested 精确 require 并以 `GOWORK=off` 测试；无永久 `replace`。
- **Machine acceptance:** `make test-go-workspace` → `build/reports/P05/report.json` 和 `junit.xml`。
- **Rollback point:** 回到迁移前 commit 并销毁临时 proxy，不长期维护双目录。
- **Definition of done:** root/nested 分别离线测试，nested 在 root 尚未公开时从本地 proxy 解析，仓库不存在第三 Module。

## P06 — Protocol foundation and base state-machine fixtures

- **Type:** implement
- **Status:** pending
- **Capability owner:** protocol-foundation
- **Components:** protocol
- **Artifacts owned:** error-fixtures, base-state-machine-fixtures, conformance-harness-base, sdk-go-protocol-core
- **Baselines transitioned:** go-manifest-library-baseline(repair-retain), manifest-digest-cli-baseline(repair-retain)
- **Goal:** 完成 strict/forward 解析、Digest、错误、UTF-8 offset 和基础状态机 Fixture。
- **Scope:** `sdk/go/protocol`、通用 Fixture/Digest corpus、重复 JSON Key、错误码和基础迁移；不含生成模型。
- **Dependencies:** P05
- **First-path invariants:** Authoring strict 与 Consumer forward compatible 分 API；所有 `$ref` 离线；把现有 Manifest library 的 digest-before-validation 技术债改为先验证再 Digest；基础 Fixture 是行为权威。
- **Machine acceptance:** `make test-protocol-foundation` → `build/reports/P06/report.json` 和 `junit.xml`。
- **Rollback point:** 任一跨语言 digest 或状态迁移分歧时停在 P06。
- **Definition of done:** 黄金/反例、JCS、中英文 offset、错误码和基础状态机结论一致。

## P07 — Reusable three-language codegen pipeline

- **Type:** implement
- **Status:** pending
- **Capability owner:** code-generation
- **Components:** codegen
- **Artifacts owned:** codegen-pipeline, codegen-representative-spike
- **Goal:** 交付可复用、固定版本、可重复执行的 Go/Python/TypeScript codegen pipeline 和代表性 spike，而不是预生成未来尚不存在的 Contract。
- **Scope:** 对当时已存在且覆盖 union/nullable/format/Extension/离线 `$ref` 的代表 Schema 做生成、编译、round-trip、drift 和 provenance；后续每个 Contract 阶段必须调用同一 pipeline 生成自己的增量输出。
- **Dependencies:** P06
- **First-path invariants:** 生成代码不手改；先验证 union/nullable/format/Extension/离线 `$ref`；生成器/命令/输入输出 digest 可追踪。
- **Machine acceptance:** `make test-codegen-pipeline` → `build/reports/P07/report.json` 和 `junit.xml`。
- **Rollback point:** 生成器失败回代表 Schema 配置，不保留部分全量生成物。
- **Definition of done:** 代表 spike 的三语言生成、编译/类型检查、round-trip 和 clean regenerate 零 diff；未来 Schema 的生成物仍由其所属阶段负责。

## P08 — Control Plane HTTP/application and observability foundation

- **Type:** implement
- **Status:** pending
- **Capability owner:** control-plane-platform
- **Components:** control-plane, operations
- **Artifacts owned:** reference-control-plane-server, control-plane-platform-foundation, audit-trace-ports, audit-trace-memory-bootstrap
- **Goal:** 建立 Go Control Plane 配置、HTTP 装配、UoW、Clock/ID/Fault seam 与 base Audit/Trace ports/storage。
- **Scope:** `reference/control-plane/internal/{app,ports,adapters/http,observability}`；不含业务领域和 Credential/URL policy。
- **Dependencies:** P07
- **First-path invariants:** handler→app→domain/ports→adapter；request ID/trace/audit 从第一条路径存在；Clock/ID/Fault 可确定注入；readiness 不伪造。
- **Machine acceptance:** `make test-control-plane-platform` → `build/reports/P08/report.json` 和 `junit.xml`。
- **Rollback point:** 保留 P05 可启动骨架；装配失败不替换基线。
- **Definition of done:** 配置失败闭合，Audit/Trace 可查询且脱敏，Clock/ID/Fault seam 有确定性测试。

## P09 — Dual-database migrations and truthful readiness

- **Type:** implement
- **Status:** pending
- **Capability owner:** control-plane-storage
- **Components:** storage
- **Artifacts owned:** migration-engine, migration-engine-fixture-versions, sqlite-uow-storage-adapter, postgres-uow-storage-adapter, durable-audit-storage, sqlite-migration-base, postgres-migration-base
- **Goal:** 建立 SQLite/PostgreSQL 语义一致的 migration engine、UoW backend 和真实 readiness。
- **Scope:** 双库 runner、版本/dirty 状态、backup/restore hooks、并发互斥、durable Audit base 和启动检查；`N-1→N` 使用 migration-engine 自身的 fixture versions，不冒充未来业务升级。
- **Dependencies:** P08
- **First-path invariants:** 语义一致而非 SQL 一致；本阶段运行双库 `empty/N-1→N/idempotent/dirty` 子矩阵；dirty 或不兼容必须 not-ready。
- **Machine acceptance:** `make test-storage-migrations` → `build/reports/P09/report.json` 和 `junit.xml`。
- **Rollback point:** migration 不可逆时先恢复备份；未证明恢复前不升级。
- **Definition of done:** 双库子矩阵、并发互斥、backup/restore 和 readiness 故障用例全绿。

## P10 — Development identity, credential lifecycle and SecretRef resolver

- **Type:** implement
- **Status:** pending
- **Capability owner:** identity-secret-foundation
- **Components:** identity
- **Artifacts owned:** identity-service, credential-store, secret-resolver-port, reference-secret-exchange, sqlite-migration-identity, postgres-migration-identity
- **Goal:** 只实现 dev identity、Credential 发行/轮换/撤销和 reference-only SecretRef resolver adapter。
- **Scope:** AuthN middleware、Credential store/cache invalidation、SecretRef port/deployment adapter、SQLite/PostgreSQL credential migration；不实现 URL、Asset 或通用 Secret value HTTP API。
- **Dependencies:** P09
- **First-path invariants:** Credential 短期、最小权限、可撤销；Secret 值不进协议/日志；Credential 事件写入 P09 durable Audit base，Audit/Trace port 复用 P08；运行双库 `empty/N-1→N/idempotent/dirty` 子矩阵。
- **Machine acceptance:** `make test-identity-secrets` → `build/reports/P10/report.json` 和 `junit.xml`。
- **Rollback point:** 撤销测试 Credential 并停用 resolver，不留下明文。
- **Definition of done:** 发行/轮换/撤销/cache 失效、双库 credential migration 和 SecretRef deny-by-default 通过；无 URL/Asset 职责混入。

## P11 — Publication and Control Plane public contracts

- **Type:** implement
- **Status:** pending
- **Capability owner:** publication-contracts
- **Components:** publication
- **Artifacts owned:** openapi-control-plane-foundation, publication-contract-fixtures, generated-control-plane-go, generated-control-plane-python, generated-control-plane-typescript
- **Goal:** 冻结 Publication foundation 与 Control Plane 公共 API 的分片/聚合规则，为后续领域 Contract 留出受控增量点。
- **Scope:** Publication fragment、公共错误/鉴权/Fixture 与聚合规范；Run/JWKS/Event Session/Asset 结构分别由 P13/P18/P19/P20 首次落地，不写 handler/store。
- **Dependencies:** P10
- **First-path invariants:** 公共和 reference-only API 分离；Secret value 不成为公共 API；所有 `$ref` 离线；未知安全语义失败闭合。
- **Machine acceptance:** `make test-publication-contracts` → `build/reports/P11/report.json` 和 `junit.xml`。
- **Rollback point:** 只回退合同源，不留下部分生成 SDK。
- **Definition of done:** OpenAPI lint、离线 bundle、正反例、授权矩阵和三语言 compile probe 通过。

## P12 — Publication service and Manifest endpoint security

- **Type:** implement
- **Status:** pending
- **Capability owner:** publication-service
- **Components:** publication, storage
- **Artifacts owned:** publication-fixtures, publication-service, sqlite-migration-publication, postgres-migration-publication, arop-cli-publication-command
- **Goal:** 实现 Publication 纵向路径及 Manifest endpoint 静态 URL/离线 `$ref` 安全。
- **Scope:** domain/app/http/storage、Bundle/Digest、SQLite/PostgreSQL migration、scheme/host/IP 静态 allowlist，以及调用同一公共 SDK 的 `arop publish`；不做网络连接和 Asset broker。
- **Dependencies:** P11
- **First-path invariants:** strict→offline `$ref`→Digest→immutable version；静态拒绝 loopback/link-local/private/metadata/credential-in-URL；运行双库 `empty/N-1→N/idempotent/dirty` 子矩阵。
- **Machine acceptance:** `make test-publication-service` → `build/reports/P12/report.json` 和 `junit.xml`。
- **Rollback point:** 禁用新发布，不删除已审计 Bundle；migration 失败按 P09 恢复。
- **Definition of done:** 发布/读取/幂等/并发、静态 URL policy、双库子矩阵和 Digest 一致性通过。

## P13 — Asset broker and token exchange service

- **Type:** implement
- **Status:** pending
- **Capability owner:** asset-broker
- **Components:** assets, storage
- **Artifacts owned:** schema-asset-exchange, openapi-control-plane-asset-fragment, generated-asset-go, generated-asset-python, generated-asset-typescript, asset-fixtures, asset-broker-service, sqlite-migration-asset, postgres-migration-asset
- **Goal:** 独立实现 Asset handler/storage/token exchange 和实际连接安全。
- **Scope:** AssetRef、上传/下载 token、storage adapter、SQLite/PostgreSQL migration、redirect hop 与连接时 DNS/IP 重检；不承载 SecretRef。
- **Dependencies:** P12
- **First-path invariants:** token 短期最小权限；每次连接及 redirect 重新校验 DNS/IP；大小/MIME/digest 限制；调用 P07 同一 pinned pipeline 生成三语言增量；运行双库 `empty/N-1→N/idempotent/dirty` 子矩阵。
- **Machine acceptance:** `make test-asset-broker` → `build/reports/P13/report.json` 和 `junit.xml`。
- **Rollback point:** 撤销 token、隔离未确认对象，migration 按 P09 恢复。
- **Definition of done:** handler/storage/token 生命周期、SSRF/DNS rebinding/redirect、双库子矩阵和审计全绿；Go/Python/TypeScript compile/typecheck、provenance 与 clean-regenerate 零 diff 通过。

## P14 — Registry domain, fixtures and persistence core

- **Type:** implement
- **Status:** pending
- **Capability owner:** registry-core
- **Components:** registry, storage
- **Artifacts owned:** schema-registry-runtime-instance, schema-registry-lease, schema-registry-event, registry-fixtures, registry-core-service, sqlite-migration-registry, postgres-migration-registry
- **Goal:** 实现 Lease/Revision/CAS/Fencing 领域核心、权威 Fixture 和双库存储。
- **Scope:** domain/repository、Instance/Session/Generation、append-only Registry Event、compaction watermark、SQLite/PostgreSQL migration；不含 HTTP/SDK。
- **Dependencies:** P13
- **First-path invariants:** 服务端 Clock；revision、事件和 compaction watermark 与状态同事务；旧 generation 被 fencing；运行双库 `empty/N-1→N/idempotent/dirty` 子矩阵。
- **Machine acceptance:** `make test-registry-core` → `build/reports/P14/report.json` 和 `junit.xml`。
- **Rollback point:** Registry Event 不删不改；状态不确定时退出发现。
- **Definition of done:** 领域 Fixture、CAS/fencing、租约过期和双库子矩阵通过。

## P15 — Registry API, drain and Go Registry SDK

- **Type:** implement
- **Status:** pending
- **Capability owner:** registry-api-sdk
- **Components:** registry, sdk-go
- **Artifacts owned:** schema-discovery-snapshot, openapi-registry-runtime, openapi-discovery-runtime, registry-api-service, go-registry-sdk, generated-registry-go, generated-registry-python, generated-registry-typescript, arop-cli-registration-command
- **Goal:** 交付注册、续租、发现、Drain API 和 Go Registry SDK。
- **Scope:** Registry/Discovery OpenAPI、handler/app、Go client/keepalive/drain helper 与 `arop register`；不含 Watch/HA。
- **Dependencies:** P14
- **First-path invariants:** Credential 复用 P10；健康字段正交；Lease 失效立即不可发现；Drain 禁止新 Attempt；SDK 遵守 generation fencing。
- **Machine acceptance:** `make test-registry-api` → `build/reports/P15/report.json` 和 `junit.xml`。
- **Rollback point:** 停止新注册并 drain，保留租约审计。
- **Definition of done:** 黑盒 API、Go SDK、错误码、Drain 与过期可见性通过。

## P16 — Registry Watch, HA and recovery

- **Type:** implement
- **Status:** pending
- **Capability owner:** registry-recovery
- **Components:** registry, storage
- **Artifacts owned:** registry-recovery-service, registry-watch-fixtures
- **Goal:** 实现 Watch/Revision/Compaction/Resync 和 PostgreSQL 多节点恢复。
- **Scope:** watch app/http、compaction watermark、leader/lock、backup/restore；复用 P14 已落库的 append-only Registry Event 与 watermark，不新增 migration，不改 P14 事件语义。
- **Dependencies:** P15
- **First-path invariants:** snapshot+watch 无缝；compact 后 resync；旧主不能复活；P14 ledger/watermark 的双库恢复语义保持一致。
- **Machine acceptance:** `make test-registry-recovery` → `build/reports/P16/report.json` 和 `junit.xml`。
- **Rollback point:** 关闭 Watch 回全量 Snapshot；不回退 revision/event。
- **Definition of done:** duplicate/reorder/partition/compaction/restore 通过，并证明 P16 没有隐藏的 schema drift 或新增 migration。

## P17 — Read-only Registry verification

- **Type:** verify · spec
- **Status:** pending
- **Capability owner:** registry-verification
- **Components:** registry
- **Artifacts owned:** none
- **Goal:** 只读验证 P14–P16 的领域、API、SDK、Watch、HA 和恢复证据。
- **Scope:** 既有制品与报告、独立黑盒 Registry suite；不修代码/Schema。
- **Dependencies:** P16
- **First-path invariants:** commit/input/checker/artifact digest 一致；失败回首个实现阶段。
- **Machine acceptance:** `make verify-registry` → `build/reports/P17/report.json` 和 `junit.xml`。
- **Rollback point:** 失败回 P14–P16 修复并重跑后续报告。
- **Definition of done:** 双库 Registry suite 与 migration 子矩阵证据完整，tracked tree 不变。

## P18 — Run authorization and durable lifecycle

- **Type:** implement
- **Status:** pending
- **Capability owner:** run-service
- **Components:** run, storage
- **Artifacts owned:** schema-run-request, schema-run-status, schema-command, schema-result, openapi-control-plane-run-fragment, run-fixtures, run-lifecycle-service, sqlite-migration-run, postgres-migration-run, generated-run-go, generated-run-python, generated-run-typescript
- **Goal:** 实现 Run/Auth、durable Outbox、Cancel、Deadline、Usage、Audit、Trace 和 effect 语义。
- **Scope:** domain/app/http/storage、authz snapshot、outbox/effect records、SQLite/PostgreSQL migration；不含 Dispatcher。
- **Dependencies:** P17
- **First-path invariants:** Run 与 Outbox/Audit 同事务；Cancel/Deadline/Usage/Trace 首次路径内建；稳定 `effect_id`；运行双库 `empty/N-1→N/idempotent/dirty` 子矩阵。
- **Machine acceptance:** `make test-run-lifecycle` → `build/reports/P18/report.json` 和 `junit.xml`。
- **Rollback point:** 投递失败保留 Run/Audit/Outbox 可重试状态，不删历史。
- **Definition of done:** 授权拒绝、幂等创建、cancel/deadline race、effect 去重和双库子矩阵通过。

## P19 — Dispatcher, Attempt, Dispatch Ticket and JWKS

- **Type:** implement
- **Status:** pending
- **Capability owner:** dispatch-security
- **Components:** dispatch, storage
- **Artifacts owned:** schema-attempt, schema-dispatch-ticket, schema-jwks-metadata, openapi-control-plane-dispatch-fragment, dispatch-fixtures, dispatcher-ticket-service, sqlite-migration-dispatch, postgres-migration-dispatch, generated-dispatch-go, generated-dispatch-python, generated-dispatch-typescript
- **Goal:** 实现候选选择、Attempt、短期 Dispatch Ticket 和 JWKS 生命周期。
- **Scope:** dispatcher/app/storage、attempt lease、ticket issuer/verifier、JWKS rotation、SQLite/PostgreSQL migration；不含 Event ingest。
- **Dependencies:** P18
- **First-path invariants:** ticket 绑定 run/attempt/audience/endpoint/mode/expiry；ES256 私钥只能经 Signer/KMS/SecretRef port 使用，数据库仅存 key metadata、public JWKS 和 rotation state；key rotation overlap；Attempt 递增；运行双库 `empty/N-1→N/idempotent/dirty` 子矩阵。
- **Machine acceptance:** `make test-dispatch-ticket` → `build/reports/P19/report.json` 和 `junit.xml`。
- **Rollback point:** 停止签发并撤销 key，保留 Attempt/Audit。
- **Definition of done:** 选择/重试、过期/错 audience、轮换和双库子矩阵通过。

## P20 — Immutable Event Ledger, Event Session and capacity release

- **Type:** implement
- **Status:** pending
- **Capability owner:** event-ledger
- **Components:** events, storage
- **Artifacts owned:** schema-event-envelope, schema-lifecycle-events, schema-output-events, schema-progress-events, schema-usage-events, schema-event-session, openapi-control-plane-event-fragment, event-fixtures, event-ledger-service, sqlite-migration-event, postgres-migration-event, generated-event-go, generated-event-python, generated-event-typescript
- **Goal:** 实现 Event Session、ingest、durable Inbox、序列/终态投影，并保证终态与 capacity 释放同事务。
- **Scope:** event domain/app/http/storage、session token、producer/run sequence、SQLite/PostgreSQL migration；不含 SSE。
- **Dependencies:** P19
- **First-path invariants:** Event append-only；Inbox 去重与投影同事务；终态不可逆且含 Final Snapshot/ResultRef/Usage；终态写入与 Runtime capacity 释放同一 UoW；运行双库 `empty/N-1→N/idempotent/dirty` 子矩阵。
- **Machine acceptance:** `make test-event-ledger` → `build/reports/P20/report.json` 和 `junit.xml`。
- **Rollback point:** 投影可重建，Event 不删不改；容量不确定时不接新 Attempt。
- **Definition of done:** duplicate/out-of-order/late terminal、session scope、Final Usage、capacity 同事务释放和双库子矩阵通过。

## P21 — Direct/Proxy delivery, Go Provider SDK and reference agent

- **Type:** implement
- **Status:** pending
- **Capability owner:** go-provider-delivery
- **Components:** delivery, sdk-go, storage
- **Artifacts owned:** openapi-agent-runtime, direct-proxy-delivery-service, provider-durable-store-contract, go-provider-sdk, go-consumer-core, provider-durable-store-port, reference-http-agent, reference-provider-sqlite-adapter, reference-provider-sqlite-migration, provider-reliability-fixtures
- **Goal:** 交付 Direct/Proxy、Go Provider SDK、可信 Bot/Gateway Go Consumer 和通用参考 Agent 的端到端可靠路径。
- **Scope:** Control Plane delivery handler、公共 Go Provider SDK、可信 Bot/Gateway 使用的 Go Consumer Direct/Proxy client、transactional `DurableStore` port、Reference Go HTTP Agent 及其自身 subtree 内 SQLite adapter/migration、连接/redirect DNS/IP 重检；公共 SDK 不带 DB driver，不含 SSE，也不把 Reference Agent 纳入 Control Plane 双库矩阵。
- **Dependencies:** P20
- **First-path invariants:** 浏览器仅 BFF/Proxy；Consumer create/query/command 传播 idempotency/trace，ticket 过期必须回 Control Plane redispatch 而非复用；每次连接及 redirect DNS/IP 重检；provider durable Inbox/Outbox、稳定 `effect_id` 去重、Cancel/Deadline/Usage/Trace、crash-before/after-effect retry 首次内建；Reference Agent SQLite migration 有空库/幂等/dirty/crash-recovery 测试，但不伪称 PostgreSQL Provider migration。
- **Machine acceptance:** `make test-direct-proxy-provider` → `build/reports/P21/report.json` 和 `junit.xml`。
- **Rollback point:** 禁用 Direct 回 Proxy；副作用不确定时不自动重试 write/irreversible。
- **Definition of done:** Provider 与 Consumer 的 Direct/Proxy/redispatch、create/query/command、SSRF/DNS/redirect、provider crash/retry/effect 去重、cancel/deadline/usage/trace 与 Reference Agent SQLite 恢复矩阵通过。

## P22 — Structured streaming and AsyncAPI

- **Type:** implement
- **Status:** pending
- **Capability owner:** streaming-delivery
- **Components:** streaming
- **Artifacts owned:** asyncapi-agent-events, streaming-service, go-streaming-client, typescript-streaming-client, streaming-fixtures, generated-streaming-go, generated-streaming-python, generated-streaming-typescript
- **Goal:** 交付结构化事件 AsyncAPI、SSE、batch ingest/ACK 和断线续传。
- **Scope:** AsyncAPI、SSE handler/client、cursor/retention/resume、partial failure；复用 P20 Ledger 和 P21 Provider SDK。
- **Dependencies:** P21
- **First-path invariants:** Producer Sequence 与 Run Sequence 分离；Offset 统一；resume 超窗口返回明确错误；最终状态不靠 Delta 重建。
- **Machine acceptance:** `make test-streaming-resume` → `build/reports/P22/report.json` 和 `junit.xml`。
- **Rollback point:** 关闭 streaming 回同步 Result/Snapshot，不丢 Ledger。
- **Definition of done:** reconnect/replay/gap/duplicate/partial batch、慢消费者和终态恢复通过。

## P23 — Read-only Run and Delivery verification

- **Type:** verify · spec
- **Status:** pending
- **Capability owner:** run-delivery-verification
- **Components:** run, delivery, streaming
- **Artifacts owned:** none
- **Goal:** 只读验证 P18–P22 的 Run/Attempt/Ticket/Event/Provider/Direct/Proxy/Streaming 闭环。
- **Scope:** 既有报告和独立黑盒 suite；不修实现、Schema 或 migrations。
- **Dependencies:** P22
- **First-path invariants:** digest 一致；终态/capacity、effect、provider crash 证据不可缺失。
- **Machine acceptance:** `make verify-run-delivery` → `build/reports/P23/report.json` 和 `junit.xml`。
- **Rollback point:** 失败回 P18–P22 首个实现阶段并使后续报告失效。
- **Definition of done:** Direct/Proxy、流式恢复、双库与 Provider 可靠性证据全绿，tracked tree 不变。

## P24 — Worker Pull service

- **Type:** implement
- **Status:** pending
- **Capability owner:** worker-service
- **Components:** worker, storage
- **Artifacts owned:** schema-worker-claim, schema-worker-complete, openapi-worker-runtime, worker-fixtures, worker-pull-service, sqlite-migration-worker, postgres-migration-worker, generated-worker-go, generated-worker-python, generated-worker-typescript
- **Goal:** 实现 Worker claim/lease/renew/complete/requeue 服务及双库存储。
- **Scope:** Worker OpenAPI、service/http/storage、SQLite/PostgreSQL migration；不含客户端 SDK。
- **Dependencies:** P23
- **First-path invariants:** claim 与 Attempt lease 原子；复用 P20 Inbox/Outbox/effect；旧 Worker 不覆盖新 Attempt；运行双库 `empty/N-1→N/idempotent/dirty` 子矩阵。
- **Machine acceptance:** `make test-worker-service` → `build/reports/P24/report.json` 和 `junit.xml`。
- **Rollback point:** 停止新 claim，让租约过期回收；不手工改 Attempt。
- **Definition of done:** crash-before/after-accept、lease reclaim、重复 complete 和双库子矩阵通过。

## P25 — Go Worker SDK and generic reference worker

- **Type:** implement
- **Status:** pending
- **Capability owner:** go-worker-client
- **Components:** worker, sdk-go
- **Artifacts owned:** go-worker-sdk, reference-worker
- **Goal:** 交付 Go Worker SDK 和无厂商依赖的参考 Worker。
- **Scope:** claim loop、renew/backoff、cancel/deadline、event/effect helper、graceful drain；复用 P24 存储契约，不新增表。
- **Dependencies:** P24
- **First-path invariants:** SDK 不导入 Reference internal；at-least-once 明示；退出前 drain；effect_id 跨 Attempt 稳定。
- **Machine acceptance:** `make test-go-worker-sdk` → `build/reports/P25/report.json` 和 `junit.xml`。
- **Rollback point:** 停止参考 Worker，服务端租约自动回收。
- **Definition of done:** 并发、backpressure、crash/restart、cancel/deadline、drain 和重复完成通过。

## P26 — Read-only operations and security verification

- **Type:** verify · review
- **Status:** pending
- **Capability owner:** operations-security-review
- **Components:** operations
- **Artifacts owned:** none
- **Goal:** 只读验证前序首次内建的安全、审计、恢复和可靠性能力，不在本阶段补能力。
- **Scope:** P08–P25 报告、Credential/Secret、URL/Asset/Provider SSRF、Audit/Trace、备份恢复和限额套件。
- **Dependencies:** P25
- **First-path invariants:** 只读；缺失回对应首次实现阶段；敏感值不进报告。
- **Machine acceptance:** `make verify-operations-security` → `build/reports/P26/report.json` 和 `junit.xml`。
- **Rollback point:** 阻断发布并回缺陷 owner；不在 verify 中打补丁。
- **Definition of done:** Credential、DNS/IP/redirect、审计/追踪、恢复和权限回归全绿。

## P27 — Python Provider SDK, ASGI runtime and package primitive

- **Type:** implement
- **Status:** pending
- **Capability owner:** python-provider
- **Components:** sdk-python
- **Artifacts owned:** python-provider-durable-store-port, python-provider-sdk, python-runtime-registration-client, python-worker-client, python-asgi-runtime, reference-python-http-agent, reference-python-sqlite-adapter, reference-python-sqlite-migration, python-package-metadata, python-package-primitive
- **Goal:** 交付 Python v1 SDK distribution：Provider/ASGI、RuntimeRegistration、Worker Client、P21/P24 可靠语义绑定和供 P39 调用的 Python 打包 primitive。
- **Scope:** generated models、provider middleware、ASGI SSE、RuntimeRegistration/keepalive/drain、Worker claim/renew/complete、durable store port，以及由 `pyproject.toml` 驱动的 wheel/sdist primitive；不发布包。
- **Dependencies:** P26
- **First-path invariants:** strict/forward API 分离；Registration 覆盖 register→lease/generation→keepalive→断线重注册→SIGTERM drain/deregister，Worker 覆盖 long-poll/renew/fencing/backoff/cancel/deadline/outbox/crash restart；公共 Python SDK 只定义 driver-free transactional `DurableStore` port；Reference Python Agent SQLite adapter/migration 只在自身 subtree，复用 Inbox/Outbox/`effect_id`；`pyproject.toml` 固定 name/version/license/requires-python；primitive 输出 deterministic manifest，不含 registry 凭据。
- **Machine acceptance:** `make test-python-provider` → `build/reports/P27/report.json` 和 `junit.xml`。
- **Rollback point:** 删除候选 wheel/sdist，不影响 Go 路径。
- **Definition of done:** Python Provider、RuntimeRegistration、Worker、crash-resume、SQLite migration/retry 矩阵全绿；PEP 517 wheel/sdist 两次 clean build digest 一致，METADATA/RECORD/allowlist 合法，clean venv install/import 通过且无 secret/绝对路径。

## P28 — TypeScript Consumer, BFF/Web and npm package primitive

- **Type:** implement
- **Status:** pending
- **Capability owner:** typescript-consumer
- **Components:** sdk-typescript
- **Artifacts owned:** typescript-consumer-sdk, typescript-bff-web, typescript-package-metadata, npm-package-primitive
- **Goal:** 交付 TypeScript Consumer/reducer、BFF/Web 示例和供 P39 调用的 npm 打包 primitive。
- **Scope:** generated models、SSE reducer/resume、BFF ticket shielding、Web sample、`package.json` exports/types/files metadata 与 npm pack primitive；不实现浏览器 Direct，不发布包。
- **Dependencies:** P27
- **First-path invariants:** 浏览器不持有 Dispatch/Event Token；未知可选事件前向兼容；primitive 可重现且不含 registry 凭据。
- **Machine acceptance:** `make test-typescript-consumer` → `build/reports/P28/report.json` 和 `junit.xml`。
- **Rollback point:** 删除候选 tarball/Web build，不影响服务端。
- **Definition of done:** reducer replay、BFF auth、Web recovery通过；`@arop/sdk` name/version/files/exports/types（以及声明时的 require）正确，tarball 不含 dev/test/secret，npm pack 两次 digest 一致并可在空项目安装。

## P29 — A2A interoperability adapter

- **Type:** implement
- **Status:** pending
- **Capability owner:** interop-a2a
- **Components:** interop
- **Artifacts owned:** a2a-adapter, arop-cli-export-a2a-command
- **Goal:** 实现 A2A task/message/artifact 与 AROP Run/Event 的显式映射。
- **Scope:** adapter、fixtures、version pin、loss table 与 `arop export a2a`；不改变 Core。
- **Dependencies:** P28
- **First-path invariants:** 不伪造无损映射；identity/effect/cancel/terminal 差异显式记录。
- **Machine acceptance:** `make test-interop-a2a` → `build/reports/P29/report.json` 和 `junit.xml`。
- **Rollback point:** 移除可选 adapter，不改 Core wire。
- **Definition of done:** 固定 upstream 版本 Fixture 与 loss/negative tests 通过。

## P30 — MCP interoperability example

- **Type:** implement
- **Status:** pending
- **Capability owner:** interop-mcp
- **Components:** interop
- **Artifacts owned:** mcp-adapter
- **Goal:** 实现 MCP 工具/资源调用与 AROP tool/effect/trace 的边界示例。
- **Scope:** adapter、fixtures、authorization/trace mapping；MCP 不承担 Runtime 注册发现。
- **Dependencies:** P29
- **First-path invariants:** MCP session 不冒充 Run/Attempt；write/irreversible 传播稳定 effect_id。
- **Machine acceptance:** `make test-interop-mcp` → `build/reports/P30/report.json` 和 `junit.xml`。
- **Rollback point:** 禁用 adapter，不影响协议执行。
- **Definition of done:** read/write 工具、资源、错误/cancel/trace 与副作用去重用例通过。

## P31 — ARD catalog interoperability

- **Type:** implement
- **Status:** pending
- **Capability owner:** interop-ard
- **Components:** interop
- **Artifacts owned:** ard-adapter, arop-cli-export-ard-command
- **Goal:** 实现 ARD/AI Catalog 描述到 AROP Definition/Version 的导入导出。
- **Scope:** adapter、fixtures、capability/security/version loss table 与 `arop export ard`；不引入公共市场。
- **Dependencies:** P30
- **First-path invariants:** Catalog discovery 不等于 Runtime readiness；不可表达字段进入 Extension/loss report。
- **Machine acceptance:** `make test-interop-ard` → `build/reports/P31/report.json` 和 `junit.xml`。
- **Rollback point:** 停止同步，不删除本地版本历史。
- **Definition of done:** round-trip、版本冲突、未知字段和 loss report 通过。

## P32 — CloudEvents and OpenTelemetry mapping

- **Type:** implement
- **Status:** pending
- **Capability owner:** interop-observability
- **Components:** interop, operations
- **Artifacts owned:** cloudevents-otel-mapping
- **Goal:** 固化 CloudEvents envelope 归属与 OpenTelemetry trace/metric 语义。
- **Scope:** mapping library、semantic conventions、fixtures/exporter tests；不建立第二套 Event 权威。
- **Dependencies:** P31
- **First-path invariants:** AROP Event ID/sequence/terminal 仍由 AROP 定义；Trace 与 Audit 相关但不互相替代。
- **Machine acceptance:** `make test-interop-observability` → `build/reports/P32/report.json` 和 `junit.xml`。
- **Rollback point:** 禁用 exporter，不丢 Event/Audit。
- **Definition of done:** envelope round-trip、context propagation、sampling/redaction 和 metric cardinality 通过。

## P33 — Portable root Go conformance runner

- **Type:** implement
- **Status:** pending
- **Capability owner:** portable-conformance
- **Components:** conformance
- **Artifacts owned:** portable-conformance-runner, conformance-scenario-schema, conformance-profile-schema, conformance-core-scenarios, conformance-v1-profiles, arop-cli-test-command
- **Baselines transitioned:** compatibility-matrix(extend-retain)
- **Goal:** 在 root module 交付语言中立 Runner、Scenario/Profile 和 `arop test`；Fixture 从 P06 起随领域增量形成。
- **Scope:** `cmd/arop-conformance`、profile/scenario loader、JSON/JUnit reporter 与只调用同一 runner 的 `arop test`；不导入 Reference internal。
- **Dependencies:** P32
- **First-path invariants:** 离线运行；Fixture digest 固定；第三方可复用；Runner 不成为行为权威。
- **Machine acceptance:** `make test-portable-conformance` → `build/reports/P33/report.json` 和 `junit.xml`。
- **Rollback point:** 回退 runner，不删历史 Fixture。
- **Definition of done:** Scenario/Profile 唯一 ID、引用存在且无环、profile closure、required scenario 不可 skip、target/profile/scenario/fixture digest 进入 JSON/JUnit；多 profile 正反例、过滤、超时与 `arop test` 通过。

## P34 — Nested Control Plane conformance driver

- **Type:** implement
- **Status:** pending
- **Capability owner:** server-conformance
- **Components:** conformance, control-plane
- **Artifacts owned:** server-conformance-driver
- **Goal:** 在唯一 nested module 交付 Reference Control Plane 黑盒 driver。
- **Scope:** server lifecycle、SQLite/PostgreSQL fixtures、root runner invocation；不复制 portable logic。
- **Dependencies:** P33
- **First-path invariants:** root runner 不导入 nested；driver 对双库执行相同 scenario；测试密钥与生产分离。
- **Machine acceptance:** `make test-server-conformance` → `build/reports/P34/report.json` 和 `junit.xml`。
- **Rollback point:** 回退 driver，不改 Fixture 语义。
- **Definition of done:** 两数据库 Control Plane Profile 黑盒通过，无第三 Module。

## P35 — Fault and HA drivers

- **Type:** implement
- **Status:** pending
- **Capability owner:** fault-ha-harness
- **Components:** fault-ha, conformance
- **Artifacts owned:** fault-ha-harness, conformance-fault-ha-scenarios
- **Goal:** 交付可确定注入 duplicate/drop/reorder/delay/crash/partition 和 PostgreSQL 多节点切换的 driver。
- **Scope:** fault scenarios、Reference FaultHook adapter、故障代理/多节点 harness；生产默认 no-op。
- **Dependencies:** P34
- **First-path invariants:** FaultHook 仅预定义 checkpoint；生产不暴露远程 fault API；seed/Clock/ID 可重现。
- **Machine acceptance:** `make test-fault-ha-drivers` → `build/reports/P35/report.json` 和 `junit.xml`。
- **Rollback point:** 禁用 harness/build tag，不影响生产二进制。
- **Definition of done:** 故障场景可同 seed 复现并生成时间线/节点/digest 报告。

## P36 — SQLite Quickstart composition

- **Type:** implement
- **Status:** pending
- **Capability owner:** sqlite-quickstart
- **Components:** quickstart
- **Artifacts owned:** quickstart, arop-cli-development-commands, go-release-layout, go-release-primitive
- **Baselines transitioned:** arop-cli-baseline(extend-retain)
- **Goal:** 只编排前序已有 Go/Python/Worker/Web 示例形成 SQLite 单进程 Quickstart。
- **Scope:** compose/config/sample manifest/start-stop/smoke 与 `arop init/dev/doctor`；完成既有 CLI command tree；提供 `GOWORK=off`、`-trimpath`、固定 mtime/order/mode、跨平台 archive/checksum 的 deterministic Go module/CLI release primitive；禁止在本阶段新写四套示例或协议行为。
- **Dependencies:** P35
- **First-path invariants:** 无真实 Credential；组件引用 P21/P25/P27/P28 制品；默认安全不降级。
- **Machine acceptance:** `make quickstart-smoke` → `build/reports/P36/report.json` 和 `junit.xml`。
- **Rollback point:** 销毁临时进程/SQLite/测试凭据，不触及用户数据。
- **Definition of done:** 空临时工作区内 init 确定且无 secret、dev 生命周期可清理、doctor JSON/非零诊断准确；十分钟内完成 publish/register/run/stream；Go module/source zip 与 CLI archives 两次 clean clone build digest 一致且无绝对路径。

## P37 — PostgreSQL production reference deployment

- **Type:** implement
- **Status:** pending
- **Capability owner:** production-deployment
- **Components:** deployment
- **Artifacts owned:** production-reference-manifests, production-upgrade-recovery-runbook, production-conformance-profile, container-build-primitive
- **Goal:** 交付 PostgreSQL production reference、Container build primitive 和升级/恢复 runbook。
- **Scope:** deployment manifests、probes、Production Conformance Profile、container primitive、migration/backup/restore/rollback runbook；不实现语言包打包或 release orchestration。
- **Dependencies:** P36
- **First-path invariants:** 使用前序 migration；镜像非 root、最小能力、固定 base digest；升级前备份和恢复演练。
- **Machine acceptance:** `make production-reference-smoke` → `build/reports/P37/report.json` 和 `junit.xml`。
- **Rollback point:** 回滚 deployment revision/镜像并按 runbook 恢复备份。
- **Definition of done:** PostgreSQL 部署、container clean build、滚动升级、备份恢复和回滚演练通过；Production Profile 在实际部署运行且 required scenario 不可 skip。

## P38 — Read-only resilience verification

- **Type:** verify · review
- **Status:** pending
- **Capability owner:** resilience-verification
- **Components:** fault-ha, storage
- **Artifacts owned:** none
- **Goal:** 只读聚合所有 migration 子矩阵、故障注入、双库、备份恢复和 HA 证据。
- **Scope:** P09/P10/P12/P13/P14/P18/P19/P20/P24 的 Control Plane 双库子矩阵、P16 恢复、P21 Go Reference Agent SQLite、P27 Python Reference Agent SQLite、P34/P35/P37 报告；不改实现或 migration。
- **Dependencies:** P37
- **First-path invariants:** 报告 digest 一致；不能在本阶段首次补 `empty/N-1→N/idempotent/dirty`。
- **Machine acceptance:** `make verify-resilience` → `build/reports/P38/report.json` 和 `junit.xml`。
- **Rollback point:** 回首个缺陷阶段并使后续聚合失效。
- **Definition of done:** 全部持久化阶段双库子矩阵、replay/cancel/fencing/partition/HA/backup-restore 证据全绿，tracked tree 不变。

## P39 — Package, container and supply-chain orchestration

- **Type:** implement
- **Status:** pending
- **Capability owner:** release-supply-chain
- **Components:** release
- **Artifacts owned:** release-version-policy-schema, release-version-policy, release-version-mapper, release-package-orchestrator, supply-chain-orchestrator, release-journal, oidc-release-workflow, oidc-release-workflow-lock, oidc-release-workflow-policy
- **Goal:** 编排语言包、Container、SBOM、provenance、signing 和可恢复发布 journal，不重新实现 build primitive。
- **Scope:** 唯一逻辑版本输入不带 `v`；machine-readable policy/mapper 映射 Go root/nested tag、Python PEP 440、npm/OCI/CLI/Schema Bundle。只调用 P36 Go、P27 Python、P28 npm、P37 Container primitive；实现 deterministic artifact manifest、SBOM/provenance/signing、受保护 OIDC workflow/lock/policy 和 durable/idempotent journal。Journal key 至少绑定 `source commit/tree + logical version + version-policy digest + destination + operation + artifact digest`，记录原子持久、校验和并使用单写锁。
- **Dependencies:** P38
- **First-path invariants:** `1.0.0-rc.1`/`1.0.0-rc.10`/`1.0.0` 映射为 Go `v...`、Python `1.0.0rc1`/`1.0.0rc10`/`1.0.0`，npm/OCI/CLI/Schema Bundle 保持逻辑值；拒绝输入 `v` 前缀、PEP 440 输入、RC leading zero、build metadata 和非规范形式。OIDC workflow 只允许 pinned full-SHA actions、protected environment、`id-token:write`/`contents:read` 最小权限、source/tag/concurrency 约束且不得含 PAT/static registry token；workflow blob digest/lock、job identity、version-policy digest 进入 journal/SBOM/provenance。重启先 remote reconcile；不可变制品先发、mutable channel 最后 CAS；P39 不把 P05 proxy bootstrap 当打包 primitive，也不重写 P27/P28/P36/P37 primitive。
- **Machine acceptance:** `make test-release-supply-chain` → `build/reports/P39/report.json` 和 `junit.xml`。
- **Rollback point:** 保留 journal 和已发布不可变制品，销毁临时 registry/test key，从未完成 step 恢复。
- **Definition of done:** RC1/RC10/final 正例与 v-prefix/PEP-input/leading-zero/build-metadata 负例、四类 primitive 统一编排、OIDC workflow identity/digest、并发 publisher、journal 截断/篡改、remote success/local crash、tag/registry/channel 冲突及 before/after failure injection 全绿。

## P40 — External evidence schemas, validators and trust bootstrap

- **Type:** implement
- **Status:** pending
- **Capability owner:** release-evidence-tooling
- **Components:** release, governance
- **Artifacts owned:** external-config-evidence-schema, external-config-validator, external-conformance-evidence-schema, external-conformance-validator, detached-release-evidence-envelope-schema, detached-release-evidence-verifier, trusted-release-role-registry-schema, trusted-release-role-registry
- **Goal:** 实现外部证据 validator 和不可由调用者替换的 trust bootstrap。
- **Scope:** P45/P50/P52/P53 Gate 输入与 detached release evidence envelope；registry 可位于仓库外，但 root 必须来自 CI protected config 内 pinned root key，或由 project_owner/maintainer threshold 签名的 registry；完整验证 TUF `root→timestamp→snapshot→targets`；定义职责分离角色，并在 protected state 保存 monotonic checkpoint。Detached envelope 固定 repository URI/object format、subject commit/tree、kind/schema/policy/validator digests、issued/expires、principal/role、trust-root/role-registry/checkpoint、payload digest 与 Ed25519/DSSE Sigstore verification material。P45 配置证据还必须绑定 OIDC issuer/repository/workflow path/environment/workflow-lock digest。
- **Dependencies:** P39
- **First-path invariants:** 正式 Gate 不接受任意命令行 registry 或人工渠道 fallback 作为信任根；`TRUST_ROOT` 只能引用 protected/pinned root，并与保护配置中的 digest/checkpoint 匹配，不得成为调用方换根入口；以 TUF root→timestamp→snapshot→targets 和 protected last-seen checkpoint 验 version/expiry/revocation 并防 rollback/freeze，targets 必须精确绑定 trusted role registry digest；threshold 按 distinct `principal_id` 计数，同一 principal 多 key 不得占多席；Sigstore 失败闭合验 exact issuer+subject/SAN+repository+workflow identity/ref/event、trusted-root/TUF、Fulcio chain、Rekor inclusion+signed checkpoint+integrated time，integratedTime 必须晚于 candidate 产生时间且落在 identity/registry 有效窗口内，DSSE payload 必须绑定 artifact digest；禁止缺根 offline bundle 和 `--insecure-ignore-tlog`；release_approver 不得与 builder/orchestrator/publisher/CI build identity 同 principal，dry-run key 不得进入正式 registry；validator 不自动生成“批准”或“独立”证据。
- **Machine acceptance:** `make test-release-evidence-tooling` → `build/reports/P40/report.json` 和 `junit.xml`。
- **Rollback point:** 撤销测试 key/identity，不改外部原始证据或 pinned root。
- **Definition of done:** root substitution、last-seen checkpoint 丢失/倒退、registry rollback/freeze、同 principal 多 key 占阈值、过期/撤销、Sigstore 缺链/缺 tlog/时间越界/DSSE digest 不符、角色混同与敏感信息泄露反例全部失败闭合。

## P41 — Report provenance, lineage and aggregation tooling

- **Type:** implement
- **Status:** pending
- **Capability owner:** release-lineage-tooling
- **Components:** release, governance
- **Artifacts owned:** public-release-aggregator, cross-commit-lineage-verifier, report-provenance-verifier, release-report-integration
- **Goal:** 实现普通报告的 commit 系谱、执行来源与可验证聚合，不处理 final overlay。
- **Scope:** P03/P04 历史规划报告、普通 ancestor 报告、隔离 checkout 重跑、受信 CI/OIDC/Sigstore provenance 和通用 report verifier 集成。
- **Dependencies:** P40
- **First-path invariants:** 报告必须验 claimed commit/blob/checker/inputs；历史 success 只能在隔离 checkout 重跑 claimed checker，或验受信 CI provenance 对 report digest+commit+checker+inputs 的签名；OIDC provenance 必须核对 `job_workflow_ref`、`job_workflow_sha`、workflow blob/lock digest、source commit 与 artifact digest，不信任报告自报；current inputs 漂移必须重跑；聚合允许受控 ancestry 不强求同 commit，但不用普通 ancestor 规则桥接 A 到 B。
- **Machine acceptance:** `make test-release-lineage-tooling` → `build/reports/P41/report.json` 和 `junit.xml`。
- **Rollback point:** 删除聚合候选报告，不改变受审 source tree。
- **Definition of done:** 混合 ancestry、虚假历史 success、Git blob 不符、输入漂移和伪造 CI identity 反例全部失败闭合。

## P42 — Final overlay, equivalence and delivery tooling

- **Type:** implement
- **Status:** pending
- **Capability owner:** release-finalization-tooling
- **Components:** release, governance
- **Artifacts owned:** public-namespace-regeneration, rc-source-freeze-checker, final-equivalence-attestation-schema, freeze-overlay-checker, payload-equivalence-checker, final-delivery-checker
- **Goal:** 实现唯一 deterministic public namespace/版本冻结器、final overlay candidate、A→B equivalence 验签、单父 commit 与 final delivery 检查器。
- **Scope:** `internal/tooling/release/finalize/regenerate.go` 接受已验证配置与无 `v` 逻辑版本，统一生成 public namespace、跨生态版本和 nested root 依赖；final overlay 仅允许 `VERSION`、Python/npm/OCI/CLI/package metadata、locks/checksums、nested `go.mod` 的 RC→final 变化；工具可生成 unsigned canonical candidate，绝不自签或创建正式 B。
- **Dependencies:** P41
- **First-path invariants:** public freeze 二次执行零 diff，禁止 placeholder/非法配置；attestation payload 绑定 A commit/tree、canonical overlay digest、expected B tree、version-policy digest、payload policy/version、equivalence normalizer/version、phase-policy digest、validity window 和 `bridged_reports[{phase,report_digest,claimed_commit,input_closure_digest}]`；B 必须是直接单父 `parent=A`；Schema/API/Runner/生成模型/逻辑差异失败闭合；必须通过 P40 pinned trust root 验 `release_approver` 且与 publisher 分离。
- **Machine acceptance:** `make test-release-finalization-tooling` → `build/reports/P42/report.json` 和 `junit.xml`。
- **Rollback point:** 删除候选 overlay/attestation，不改 A 或发布表面。
- **Definition of done:** noncanonical overlay、wrong parent/signer/tree/payload/time/policy/normalizer、bridged report 缺失/多余/digest 不符、逻辑 diff 与重放反例全部失败闭合。

## P43 — Read-only reproducible release dry-run

- **Type:** verify · spec
- **Status:** pending
- **Capability owner:** release-dry-run-verification
- **Components:** release
- **Artifacts owned:** none
- **Goal:** 只读使用 P39–P42 工具验证双 Module、多语言包、Container、SBOM/provenance/signing、journal 恢复和 finalization。
- **Scope:** clean clone、`GOWORK=off`、保留域名、临时 Go/PyPI/npm/OCI registries/test key，调用 P42 同一冻结脚本与 P27/P28/P33/P37 primitives；工具源码只读。
- **Dependencies:** P42
- **First-path invariants:** 临时树验证 deterministic RC tree、二次零 diff、placeholder 清零、Schema/Event namespace、双 Go module/package metadata、全生态 pack/install、非法配置与非法版本失败；tracked source tree 零变更；P44 前仅 private/dev snapshot；不得在 verify 阶段补代码；每个 publish step 注入失败并从同 digest journal 断点恢复。
- **Machine acceptance:** `make release-dry-run READ_ONLY=1` → `build/reports/P43/report.json` 和 `junit.xml`。
- **Rollback point:** 销毁临时 proxy/registry/key，不创建公开 tag/package。
- **Definition of done:** clean build digest、nested 解析、安装、SBOM/provenance/signing、partial publish 恢复和报告复验通过。

## P44 — Code-complete verification

- **Type:** verify · review
- **Status:** pending
- **Capability owner:** release-readiness-review
- **Components:** governance, release
- **Artifacts owned:** none
- **Goal:** 聚合仓库内可自证要求，只宣告 private code-complete，不宣告 public/v1 complete。
- **Scope:** 精确聚合 P01–P43 共 43 份报告、IR-01–IR-14 追溯、clean environment 和已知风险；静态依赖 P41 聚合/provenance 工具，不把 P43 当作其余报告的代理；不修实现。
- **Dependencies:** P43
- **First-path invariants:** 历史报告须满足 P41 隔离重跑或受信 provenance；不伪造外部配置/伙伴证据；首个公开候选必须是 P49 v1 RC。
- **Machine acceptance:** `make validate-all` → `build/reports/P44/report.json` 和 `junit.xml`。
- **Rollback point:** 回首个失败阶段并重跑后续报告。
- **Definition of done:** 内部要求全绿，状态仅 `private code-complete`，P45/P50/P52 保持外部阻塞。

## P45 — External public configuration gate

- **Type:** gate
- **Status:** blocked-external-evidence
- **Capability owner:** public-governance
- **Components:** governance
- **Artifacts owned:** none
- **Goal:** 用 P40 validator 验证真实域名、PyPI/npm 所有权、两名 Maintainer/恢复权限和私密安全入口。
- **Scope:** 仓库外证据、pinned trust root、可信 registry 和脱敏 canonical 摘要；不存原始 Credential。
- **Dependencies:** P44
- **First-path invariants:** 外部 project_owner/maintainer threshold 是原始 signed config bundle 的 producer，P45 只验证并生成包含原文 digest、验签结论和非敏感字段的 `build/evidence/P45` summary，绝不代签；bundle 绑定 P44 baseline、canonical public-config digest、domain/package/container claims、两名 distinct maintainer/recovery、安全入口和 protected OIDC environment/workflow-lock；P46 只消费该 verified digest，P47/P49 再把它绑定 A/tree/RC。占位域名、内部 Fixture、任意 CLI registry、单人自签不能通过；证据过期、撤销或 registry rollback/freeze 立即阻塞。
- **Machine acceptance:** `make external-config-gate EVIDENCE_BUNDLE=<outside-repo-signed-bundle> TRUST_ROOT=<protected-pinned-root> CHECKPOINT=<protected-state>` → `build/reports/P45/report.json`、`junit.xml` 与 `build/evidence/P45/verified-summary.json`。
- **Rollback point:** 所有权/安全入口失效即撤销 Gate。
- **Definition of done:** 外部项有来源、threshold/角色验签、时间和非敏感摘要。

## P46 — Deliver deterministic RC source tree

- **Type:** deliver
- **Status:** pending
- **Capability owner:** public-artifact-generation
- **Components:** codegen, release
- **Artifacts owned:** none
- **Goal:** 把 P45 已验证的真实配置交给 P42 已冻结工具并产出 RC source tree，但不创建 commit/tag。
- **Scope:** 输入唯一无 `v` 逻辑版本 `1.0.0-rc.N`，由版本 mapper 写入 Schema `$id`、Event/Extension namespace、OpenAPI/AsyncAPI、三 SDK、Fixture/Docs/Digest、root/Go/Python/npm/OCI/CLI metadata 与 nested `go.mod` 精确 root RC dependency；Compatibility Matrix 不预填未验证伙伴；不手改生成物。
- **Dependencies:** P45
- **First-path invariants:** P45 后禁止 implement/refactor；只执行 P42 已冻结脚本，不在交付阶段修工具；全量生成而非字符串替换；相同输入与 version-policy digest 产生同一 tree digest；本阶段只准备工作树，不僭越创建 A。
- **Machine acceptance:** `make regenerate-public RELEASE_VERSION=1.0.0-rc.N CREATE_COMMIT=0` → `build/reports/P46/report.json` 和 `junit.xml`。
- **Rollback point:** 回 P45 后基线；未创建 commit/tag/package。
- **Definition of done:** 占位 namespace 清零、clean regenerate 零 diff，nested 精确 require root `v1.0.0-rc.N`，RC tree digest 已记录。

## P47 — Create and freeze clean RC source commit A

- **Type:** deliver
- **Status:** pending
- **Capability owner:** rc-source-freeze
- **Components:** release
- **Artifacts owned:** none
- **Goal:** 由 orchestrator 从 P46 已验收 tree 创建并冻结 clean RC source commit A，不发布任何制品。
- **Scope:** 校验 expected tree digest、创建单个 commit A、记录 commit/tree；不生成新内容、不创 tag/package。
- **Dependencies:** P46
- **First-path invariants:** commit A 必须精确对应 P46 tree；创建后 HEAD=A 且工作树 clean；A 含 root/nested `v1.0.0-rc.N` 版本关系，freeze report 绑定 P45 verified config summary digest 与 version-policy digest。
- **Machine acceptance:** `make freeze-rc-source RELEASE_VERSION=1.0.0-rc.N EXPECTED_TREE=<P46-tree>` → `build/reports/P47/report.json` 和 `junit.xml`。
- **Rollback point:** A 不符则废弃该 RC 号，回 P46 产生新树，不重写历史。
- **Definition of done:** clean commit A 已存在、tree digest 精确匹配 P46，尚无公开 tag/package。

## P48 — Strictly read-only verification on commit A

- **Type:** verify · review
- **Status:** pending
- **Capability owner:** public-release-verification
- **Components:** release, conformance
- **Artifacts owned:** none
- **Goal:** 在已存在的 clean commit A 上只读重跑 contracts/conformance/resilience/release 检查。
- **Scope:** commit A、P17/P23/P26/P38/P43 套件和空环境安装；不修生成物、不创建 commit/tag。
- **Dependencies:** P47
- **First-path invariants:** HEAD 必须是 clean A；无 `arop.invalid`、本地 replace/workspace 或 private snapshot 依赖；重算 RC1/RC10/final 跨生态映射并核对 policy digest；tracked tree 前后保持同一 digest。
- **Machine acceptance:** `make verify-public-namespace READ_ONLY=1 SOURCE_COMMIT=A` → `build/reports/P48/report.json` 和 `junit.xml`。
- **Rollback point:** 失败回 P46 并用新 RC 号重建 A；不得在 verify 内修补。
- **Definition of done:** A 的公共命名、安装、Conformance、resilience、SBOM/provenance 全绿，tree 不变。

## P49 — Deliver v1 release candidate from clean commit A

- **Type:** deliver
- **Status:** pending
- **Capability owner:** v1-rc-delivery
- **Components:** release
- **Artifacts owned:** none
- **Goal:** 不改 tree，从 clean commit A 发布首个公开候选 `v1.0.0-rc.N`。
- **Scope:** root tag、nested `reference/control-plane/v1.0.0-rc.N`、Python/npm/CLI/Container、Schema/API/Runner、SBOM/provenance 与 digest manifest；使用 P39 journal。
- **Dependencies:** P48
- **First-path invariants:** 先以 create-only 语义发布所有不可变 root Go/Python/npm/Container/SBOM/provenance RC 制品，校验 tag target 与 annotated tag-object 签名，重启 remote reconcile 且 proxy 可解析后再发不可变 nested `reference/control-plane/v1.0.0-rc.N`，mutable channel/dist-tag 最后用 expected-previous CAS；每步预检 digest、持久记录并可从断点恢复；重试必须是同一 A/同一 digest，remote digest mismatch 或不可变冲突进入 `incident-blocked`，不得推进 mutable channel 或宣告 RC complete；已发布不可变制品不回滚、不重写，只允许同一 A/digest 经 journal/reconcile 续传；deliver 不创建新 commit、不改 tree；不用 v0.1 或本地伪制品取证。
- **Machine acceptance:** `make deliver-v1-rc RELEASE_VERSION=1.0.0-rc.N SOURCE_COMMIT=A RESUME=1` → `build/reports/P49/report.json` 和 `junit.xml`。
- **Rollback point:** 可恢复故障只可按 journal/reconcile 从同一 A/digest 续传，不回滚或重写已发布不可变制品；remote digest 冲突导致 `incident-blocked` 时，必须回 P46 用新 RC 号生成新 tree、P47 创建新 commit A，再重走 P48/P49，绝不重标旧 A。
- **Definition of done:** 所有发布边界 failure injection 可恢复，外部空环境可下载/验签/安装/跑 Conformance；生成 RC subject manifest，绑定 A/tree、P45 config digest、RC root/nested tag-object、全 registry artifacts、Schema/OpenAPI/AsyncAPI/Fixture/Runner、SBOM/provenance digests。

## P50 — Independent implementation and partner evidence gate

- **Type:** gate
- **Status:** blocked-external-evidence
- **Capability owner:** external-conformance-review
- **Components:** governance, conformance
- **Artifacts owned:** none
- **Goal:** 使用 P40 validator 验证独立 Runtime/Control Plane/Validator 和伙伴对 P49 commit A v1 RC 的真实证据。
- **Scope:** 至少两个独立 Runtime、一个非金运 Control Plane/Validator、三个外部伙伴；原始 signed compatibility bundle 由外部 principals 产生并留在仓库外，P50 只验证它并发布/记录 verified detached summary 作为绑定 A/RC 的 release asset，绝不替伙伴代签或修改 A。
- **Dependencies:** P49
- **First-path invariants:** compatibility bundle 引用 P49 RC subject manifest，绑定 exact A commit/tree/object format、RC version/tag-object、Schema Bundle/OpenAPI/AsyncAPI/Fixture/Runner 与被测 Artifact digests/provenance digests；逐条含 external principal、实现来源、runtime/profile/report/result/time；内部 fork/模拟不算，数量按 distinct `principal_id`；信任链从 P40 pinned root 建立，不接受调用者自带 root；`compatibility/implementation-matrix.yaml` 仅是 A 内策略声明，不在 P50 后改写。
- **Machine acceptance:** `make external-evidence-gate RELEASE_VERSION=1.0.0-rc.N SOURCE_COMMIT=A EVIDENCE_BUNDLE=<outside-repo-signed-bundle> TRUST_ROOT=<protected-pinned-root>` → `build/reports/P50/report.json`、`junit.xml` 与 `build/evidence/P50/verified-compatibility-summary.json`。
- **Rollback point:** tracked 规范/Schema/transport/runner/security/compatibility 变化使证据失效，回 P46 产生新 A/RC 并重取证。
- **Definition of done:** 数量/独立性/来源可核验，digests 指向 A v1 RC；公开 detached canonical summary、attestation 与 compatibility bundle 可下载并验签，内部模拟/空 Matrix 不算完成。

## P51 — Verify unsigned canonical final overlay candidate

- **Type:** verify · review
- **Status:** pending
- **Capability owner:** v1-freeze-overlay-review
- **Components:** release, governance
- **Artifacts owned:** none
- **Goal:** 冻结 A，在隔离临时树生成并只读验证 unsigned canonical final overlay candidate 与 expected B tree，不签名。
- **Scope:** 使用 P42 工具；overlay 白名单精确列出 `VERSION`、Python `pyproject`/lock、npm `package.json`/lock、OCI/CLI/Schema Bundle metadata、nested `go.mod` 的 `v1.0.0-rc.N→v1.0.0` 依赖及必要 checksum；A tracked tree 保持不变。
- **Dependencies:** P50
- **First-path invariants:** 不关闭 RFC、不修 Matrix；Schema/API/Runner/生成模型/逻辑不得变；candidate 绑定 A commit/tree、canonical overlay digest、expected B tree、`parent=A` 和 payload policy；不包含自签 attestation。
- **Machine acceptance:** `make verify-v1-freeze READ_ONLY=1 APPROVED_RC=1.0.0-rc.N RELEASE_VERSION=1.0.0 SIGN=0` → `build/reports/P51/report.json` 和 `junit.xml`。
- **Rollback point:** 非白名单 diff 或 digest 不一致即丢弃 candidate；规范变化回 P46–P50 重走 RC/取证。
- **Definition of done:** 临时树可重现同一 overlay/expected-tree digest，payload equivalence 通过，A 工作树未变。

## P52 — External release-approver signature gate

- **Type:** gate
- **Status:** blocked-external-evidence
- **Capability owner:** v1-release-approval
- **Components:** governance, release
- **Artifacts owned:** none
- **Goal:** 由外部 `release_approver` 对 P51 canonical candidate 签发 A→B equivalence attestation，并严格验签。
- **Scope:** 仓库外 Ed25519 或允许的 Sigstore 签名流程；阶段仅验 `ATTESTATION=<outside-repo-attestation>` 与 P40 pinned trust root，不改 A/candidate。
- **Dependencies:** P51
- **First-path invariants:** 外部 `release_approver` 是 signed attestation producer，P52 只验证并记录 summary，绝不代签。Attestation 绑定 A commit/tree、overlay digest、expected B tree、`parent=A`、version-policy digest、payload/equivalence policy、phase-policy digest、有效时间、replay nonce、完整 `bridged_reports` 与 `bound_evidence`；后者至少含 P45 config summary digest、P49 RC subject manifest digest、P50 compatibility summary digest。approver 与 builder/orchestrator/publisher/CI build identity 不得同 principal；Sigstore integratedTime 必须晚于 candidate；dry-run key、任意 CLI root、缺 tlog/缺 trusted root 不能通过。
- **Machine acceptance:** `make approve-v1-overlay CANDIDATE=build/evidence/P51/final-overlay-candidate.json ATTESTATION=<outside-repo-attestation> TRUST_ROOT=<protected-pinned-root>` → `build/reports/P52/report.json`、`junit.xml` 与 `build/evidence/P52/verified-attestation-summary.json`。
- **Rollback point:** signer/trust/time/digest 不符即继续阻塞；不替外部批准者生成签名。
- **Definition of done:** 只接受职责分离、信任链完整且精确绑定 P51 candidate 的有效批准。

## P53 — Deliver exact approved metadata-only commit B and final v1

- **Type:** deliver
- **Status:** pending
- **Capability owner:** v1-delivery
- **Components:** release
- **Artifacts owned:** none
- **Goal:** 验 P52 批准，只应用 canonical overlay，创建直接单父 `parent=A` 的 metadata-only commit B，并从 B 发布 final v1。
- **Scope:** 在冻结 A 的专用 release ref/detached checkout 中验 `release_approver`、应用 approved overlay、核对 B parent/tree、创建 commit/tags；向 mapper 传唯一无 `v` 逻辑版本，分别发布 root/nested Go、Python/npm/CLI/Schema Bundle/OCI，不盲传同一 VERSION；把 P50 detached compatibility bundle 绑定 final B 后发布 final evidence asset；不 reset/重写 main；使用 P39 journal，不允许临场修改 overlay。
- **Dependencies:** P52
- **First-path invariants:** B 必须是直接单父 parent=A 且 tree 等于 attestation expected value；P52 的 P45/P49/P50 bound digests 必须原样核验，A 报告只能通过有效 A→B attestation 的精确 `bridged_reports` 桥接；先以 create-only 语义发布所有不可变 root Go/Python/npm/Container/SBOM/provenance final 制品，tag target/annotated tag signature 校验、proxy 可解析后再发 nested，mutable channel 最后 expected-previous CAS；partial publish 只可从同一 B/同一 digest journal 恢复，remote mismatch 进入 `incident-blocked`；最终在 B 重跑 Conformance/SBOM/provenance，并由 publisher/CI 生成不进入 B tree 的 final detached evidence bundle，绑定 B/A/P52/P50/final artifacts/journal。
- **Machine acceptance:** `make deliver-v1 RELEASE_VERSION=1.0.0 APPROVED_RC=1.0.0-rc.N ATTESTATION=<outside-repo-attestation> TRUST_ROOT=<protected-pinned-root> RESUME=1` → `build/reports/P53/report.json` 和 `junit.xml`。
- **Rollback point:** signer/parent/tree/payload 不符立即停止；部分成功保留 journal 并仅从同 B/digest 续传；metadata 变化回 P51/P52，逻辑变化回 P46 重走 RC/取证。
- **Definition of done:** metadata-only commit B 是 A 的直接单父子 commit 且精确匹配已批准 tree，root/nested final 顺序发布，所有 partial-failure 恢复测试和 final Conformance/SBOM/provenance 全绿。

# 4. 明确延后

gRPC、WebSocket、Portable Session Checkpoint、Hedged Execution、多区域调度、公共 Agent 市场、多租户 SaaS 和通用工作流引擎不阻塞 v1。Console 集成是下游独立计划，本仓库 P01–P53 不修改 `kinglucky-agent-console`。
