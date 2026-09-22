---
title: Agent Runtime Operations Protocol 仓库布局
status: review-candidate
updated: 2026-09-22
---

# 1. 用途与权威

本文档是最终目标布局的评审候选人类视图，P04 前内容受控但尚未用户批准，用来约束后续物理重构、代码生成和发布。唯一机器可读制品目录是 [`spec/artifact-manifest.yaml`](../spec/artifact-manifest.yaml)；两者不一致时必须先停止实现并修复蓝图，不能在代码中自行选择。

<!-- blueprint-target-tree:v1 -->

# 2. 最终目标树

```text
agent-runtime-operations-protocol/
├── go.mod                              # 公共 Go module：SDK、生成模型、CLI/portable runner
├── go.sum
├── go.work.example                     # 仅本地联调模板，发布不依赖
├── package.json                         # Schema/TypeScript/文档工具链，不是后端运行时
├── Makefile
├── VERSION
├── scripts/
│   ├── validate.mjs                     # 仅 Schema/spec/manifest validation
│   ├── spec-index-check.mjs             # 仅结构化 Schema/spec/catalog 检查结果；Go 写报告
│   ├── manifest-digest.mjs              # P06 repair-retain 的 Schema/Manifest 工具
│   ├── generate.mjs                     # P07 Schema-driven 三语言 codegen
│   └── lib/repository.mjs               # 仅 Node Schema/YAML/JSON loader/walker
├── internal/tooling/                    # 根 module 私有 Go 治理与发布工具
│   ├── cmd/                             # blueprint/report/planning/Gate/proxy/build/release 私有入口
│   ├── structuredfile/                  # Go JSON/YAML/Git-safe repository helpers
│   ├── schema/                          # 离线 Draft 2020-12 + format assertion validator
│   ├── report/                          # machine report writer/verifier/tests
│   ├── evidence/                        # planning/Gate evidence 与 lineage tests
│   ├── specindex/                       # 通用 governance/index/traceability/link closure
│   ├── blueprint/                       # 阶段、制品、发布链与 Node 边界 checker/probes
│   └── release/                         # P39 supply；P40 evidence；P41 lineage；P42 finalize
│
├── docs/                                # 手写规范、决策、蓝图
│   ├── ARCHITECTURE.md
│   ├── PROTOCOL_SPECIFICATION.md
│   ├── REGISTRY_AND_DISCOVERY.md
│   ├── RUN_AND_STREAMING.md
│   ├── SECURITY_AND_GOVERNANCE.md
│   ├── RELIABILITY_AND_OPERATIONS.md
│   ├── SDK_AND_DX.md
│   ├── INTEROPERABILITY.md
│   ├── PUBLIC_PROJECT_AND_ADOPTION.md
│   ├── DIRECTORY_STRUCTURE.md
│   ├── IMPLEMENTATION_BLUEPRINT.md
│   ├── DEVELOPMENT_PLAN.md
│   └── DECISIONS.md
├── spec/                                # 机器可读制品索引、需求和冲突台账
│   ├── release/                          # 跨生态版本/Go artifact layout 等发布策略
│   ├── evidence/                         # 仅规划期脱敏 canonical 摘要；原始外部证据不入库
│   └── schemas/                          # report/evidence/trusted-key/catalog/requirements 元 Schema
├── rfcs/
├── compatibility/
│
├── schemas/                             # 手写结构权威；所有 $ref 离线闭包
├── openapi/                             # 手写/受控生成的 HTTP 绑定
│   ├── fragments/control-plane/           # 各领域阶段拥有的增量源
│   ├── control-plane-v1.yaml              # Publication/Run/JWKS/Event Session/Asset Exchange
│   ├── agent-runtime-v1.yaml               # Direct/Proxy Runtime
│   ├── registry-runtime-v1.yaml
│   ├── discovery-runtime-v1.yaml
│   └── worker-runtime-v1.yaml
├── asyncapi/                            # 手写/受控生成的事件与流式绑定
├── examples/                            # 黄金样例，不含真实凭据
│
├── sdk/
│   ├── go/
│   │   ├── generated/                 # 生成；禁止手改
│   │   ├── protocol/                  # 手写核心、验证、状态机
│   │   ├── provider/
│   │   ├── consumer/
│   │   ├── registry/
│   │   └── worker/
│   ├── python/
│   │   ├── pyproject.toml               # arop-sdk PEP 517 metadata
│   │   └── src/arop/
│   │       ├── generated/              # 生成；禁止手改
│   │       ├── provider/
│   │       ├── registry/                # RuntimeRegistration/keepalive/drain
│   │       ├── worker/                  # claim/renew/complete/fencing
│   │       └── protocol/
│   └── typescript/
│       ├── package.json                 # @arop/sdk exports/types/files metadata
│       └── src/
│           ├── generated/                  # 生成；禁止手改
│           ├── consumer/
│           └── reducer/
│
├── cmd/
│   ├── arop/                               # init/dev/test/publish/register/export/doctor
│   └── arop-conformance/                   # portable runner，不依赖 Reference CP internal
│
├── conformance/                         # 语言中立；不允许 go.mod
│   ├── fixtures/
│   ├── scenarios/                       # Core + fault/HA；唯一 ID/闭包/required 不可跳
│   ├── profiles/                        # Core/Provider/Streaming/Managed/Pull/CP/Production
│   ├── fault-injection/
│   └── reports/                            # 公开认证报告与摘要
│
├── adapters/                            # 仅开放标准互操适配器
│   ├── a2a/
│   ├── mcp/
│   └── ard/
│
├── reference/
│   ├── control-plane/
│   │   ├── go.mod                       # 唯一嵌套 Go module
│   │   ├── go.sum
│   │   ├── cmd/aropd/
│   │   ├── internal/
│   │   │   ├── app/                      # use case / transaction orchestration
│   │   │   ├── domain/                   # publication/registry/run/event/worker
│   │   │   ├── ports/                    # storage/clock/id/token/event interfaces
│   │   │   └── adapters/                 # HTTP、storage、auth、telemetry、fault seam
│   │   ├── migrations/
│   │   │   ├── sqlite/
│   │   │   └── postgres/
│   │   └── tests/                      # integration/storage/HA/server-conformance driver
│   └── agents/
│       ├── go-http/
│       │   ├── internal/storage/sqlite/   # Reference Agent 本地 DurableStore adapter
│       │   └── migrations/sqlite/         # 仅该 Agent；非 CP 双库矩阵且不建 module
│       ├── python-http/
│       │   ├── arop_agent/storage/sqlite.py # Reference Python Agent 本地 DurableStore adapter
│       │   └── migrations/sqlite/          # 仅该 Agent；非 CP 双库矩阵
│       └── pull-worker/                    # 通用 Worker Pull，无厂商专属适配
│
├── deployments/
│   ├── quickstart/                          # SQLite 单进程
│   └── production-reference/                # PostgreSQL 多节点参考部署
├── build/reports/                       # CI 产生，默认不入库
├── build/evidence/                      # ignored detached summaries/candidates/final bundle
└── .github/workflows/                   # validate/conformance/release/provenance + release lock
```

## 2.1 冻结路径锚点

以下是检查器用的路径锚点，是上述树的平铺表达，不是第二份制品目录：

```text
openapi/control-plane-v1.yaml
openapi/fragments/control-plane/
spec/evidence/
sdk/go/generated/
sdk/python/src/arop/
sdk/typescript/
cmd/arop-conformance/
conformance/
reference/control-plane/
reference/control-plane/internal/domain/
reference/control-plane/internal/ports/
reference/control-plane/migrations/sqlite/
reference/control-plane/migrations/postgres/
reference/agents/go-http/
deployments/quickstart/
deployments/production-reference/
.github/workflows/
```

# 3. Go Module 冻结

<!-- blueprint-module: go.mod -->
<!-- blueprint-module: reference/control-plane/go.mod -->

仓库最终只允许两个 Go module：

| Module | 职责 | 允许导入 | 禁止 |
| --- | --- | --- | --- |
| `github.com/gmslll/agent-runtime-operations-protocol` | 公共 Go SDK、生成类型、`arop`、`arop-conformance` | 标准库和审核过的公共依赖 | 不得导入 Reference CP `internal` 或数据库驱动 |
| `github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane` | Go Reference Control Plane 和服务端 Conformance Driver | 精确版本的根公共 module | 不得被根 module 或 SDK 反向导入 |

`conformance/` 只包含语言中立的 Fixture、Scenario 和 Profile，不建第三个 `go.mod`。根 `cmd/arop-conformance` 读取这些资源；Reference Control Plane 的服务端驱动留在嵌套 module 内。仓库只提交 `go.work.example`，开发者的真实 `go.work` 默认不提交；CI/发布必须在 `GOWORK=off` 下通过，并通过临时 Go proxy 先放入根伪版本/RC 候选，再验证 nested module 的精确版本解析。

# 4. 分层和依赖方向

```text
DECISIONS + domain specifications + state-machine fixtures
                         ↓
JSON Schema (structural authority, offline reference closure)
                         ↓
OpenAPI / AsyncAPI bindings
                         ↓
generated models (never handwritten)
                         ↓
handwritten public SDKs / portable conformance runner
                         ↓
Reference Control Plane application and adapters
```

嵌套 Control Plane 内部依赖只能从 Adapter 指向 Port，从 App 指向 Domain/Port；Domain 不导入 HTTP、SQL、具体 Clock/ID 或 Fault 实现。完整边界和事务见 [IMPLEMENTATION_BLUEPRINT.md](IMPLEMENTATION_BLUEPRINT.md)。

# 5. 手写、生成与存储边界

- `schemas/`、领域规范和事务语义是手写与评审对象。
- 各 SDK 的 `generated/` 只能由锁定版本的生成器产生；CI 验证重新生成无 diff。
- 公共 Go/Python SDK 各自只暴露 driver-free transactional `DurableStore` port，不导入 SQLite/PostgreSQL driver；Control Plane 数据库代码仅在 `reference/control-plane/`，Go/Python Reference Agent 的本地 SQLite adapter/migration 只允许放在各自 `reference/agents/<agent>/` subtree。
- SQLite 与 PostgreSQL 各有独立 migration 目录，实现相同领域语义，不要求 SQL 文本相同。
- `build/reports/` 是机器验收输出；公开 Conformance 摘要经脱敏后进入 `conformance/reports/`。

# 6. 不允许的布局

- 第三个 Go module，或为 `conformance/` 单独建 module。
- Console 数据库、Web UI、组织模型或内部权限实现。
- 任何渠道、桌面执行器、Agent 框架或具体产品的厂商专属 Adapter；它们应通过通用 HTTP/Worker/A2A/MCP 边界接入。
- 金运专属 wire namespace、真实 Credential 或客户数据。
- SDK 导入 Reference Control Plane `internal` 包，或将 Reference 内部模型当作公共协议类型。
- 只有连接金运 Console 才能运行的 Quickstart 或 Conformance。

# 7. 渐进建立

目标树不要求创建空目录。P05 才执行物理重构，每个后续阶段只在存在真实制品时创建路径。`reference/control-plane-lite` 在 P05 `migrate-retire`；现有 Go Manifest/Node digest baseline 在 P06 `repair-retain`；`cmd/arop` baseline 在 P36 `extend-retain`。future transition 不改变现存制品的当前可用阶段，也不形成第三个长期实现。

# 8. 包名与发布前置

```text
Go root module: github.com/gmslll/agent-runtime-operations-protocol
Go nested module: github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane
Python distribution: arop-sdk
Python import: arop
TypeScript: @arop/sdk
CLI: arop
CLI config: ~/.config/arop/
```

上述是预发布命名。项目域名、PyPI/npm 所有权与两名 Maintainer/安全入口在 P45 必须用外部证据确认。`internal/tooling/release/finalize/regenerate.go` 在 P42 实现并冻结，P43 用保留域名和临时 registry 验其可重现性；P46 仅把 P45 已验真实值交给该工具生成 RC tree，P47 再创建 clean commit A。P45 后禁止 implement/refactor。P44 及之前只允许 private/dev snapshot；取消独立公共 v0.1，首个公开候选是 P49 的 `v1.0.0-rc.N`。

发布工具唯一逻辑版本输入不带 `v`：`1.0.0-rc.N`/`1.0.0` 映射到 Go `v...`、Python `1.0.0rcN`/`1.0.0`，npm/OCI/CLI/Schema Bundle 保持逻辑值。P51 final overlay 只允许 `VERSION`、Python/npm metadata/lock、OCI/CLI/Schema metadata、nested `go.mod` 和必要 checksum 的 RC→final 变化。

P45/P50/P52 的原始签名外部证据永不写入 A/B tree；仓库工具只在 ignored `build/evidence/` 生成验签摘要。P49 生成 RC subject manifest，P50 验证伙伴 compatibility bundle，P51 生成 unsigned candidate，P52 验外部 approver attestation，P53 生成 final detached evidence bundle；这些作为 release asset、OCI referrer 或 transparency statement 发布。
