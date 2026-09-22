---
title: Agent Runtime Operations Protocol 目录规划
status: proposed
updated: 2026-09-22
---

# 1. 目标目录

```text
agent-runtime-operations-protocol/
├── README.md
├── AGENTS.md
├── LICENSE
├── CONTRIBUTING.md
├── CODE_OF_CONDUCT.md
├── SECURITY.md
├── VERSION
├── Makefile
│
├── docs/
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
│   ├── DEVELOPMENT_PLAN.md
│   └── DECISIONS.md
│
├── rfcs/
│   ├── README.md
│   └── template.md
│
├── compatibility/
│   ├── upstream-versions.yaml
│   └── implementation-matrix.yaml
│
├── schemas/
│   ├── common/
│   │   ├── identifiers.schema.json
│   │   ├── error.schema.json
│   │   ├── trace.schema.json
│   │   └── content-part.schema.json
│   ├── manifest/
│   │   └── agent-manifest-v1.schema.json
│   ├── registry/
│   │   ├── runtime-instance-v1.schema.json
│   │   ├── lease-v1.schema.json
│   │   ├── discovery-snapshot-v1.schema.json
│   │   └── registry-event-v1.schema.json
│   ├── runtime/
│   │   ├── run-request-v1.schema.json
│   │   ├── run-status-v1.schema.json
│   │   ├── attempt-v1.schema.json
│   │   ├── command-v1.schema.json
│   │   └── result-v1.schema.json
│   ├── events/
│   │   ├── event-envelope-v1.schema.json
│   │   ├── lifecycle-events-v1.schema.json
│   │   ├── output-events-v1.schema.json
│   │   ├── progress-events-v1.schema.json
│   │   └── usage-events-v1.schema.json
│   └── resources/
│       ├── asset-ref-v1.schema.json
│       ├── data-ref-v1.schema.json
│       └── secret-ref-v1.schema.json
│
├── openapi/
│   ├── agent-runtime-v1.yaml
│   ├── registry-runtime-v1.yaml
│   ├── discovery-runtime-v1.yaml
│   └── worker-runtime-v1.yaml
│
├── asyncapi/
│   └── agent-events-v1.yaml
│
├── examples/
│   ├── manifests/
│   ├── registry/
│   ├── runs/
│   ├── events/
│   ├── errors/
│   └── compatibility/
│
├── conformance/
│   ├── fixtures/
│   ├── provider/
│   ├── consumer/
│   ├── fault-injection/
│   └── reports/
│
├── sdk/
│   ├── go/
│   │   ├── generated/
│   │   ├── protocol/
│   │   ├── provider/
│   │   ├── consumer/
│   │   ├── registry/
│   │   └── worker/
│   ├── python/
│   │   ├── generated/
│   │   └── src/arop/
│   └── typescript/
│       ├── generated/
│       └── src/
│
├── cli/
│   └── arop/
│
├── reference/
│   ├── go-http-agent/
│   ├── python-agent/
│   ├── pull-worker/
│   ├── cc-connect-adapter/
│   ├── control-plane-lite/
│   ├── web-streaming-demo/
│   └── docker-compose/
│
├── adapters/
│   ├── a2a/
│   ├── mcp/
│   └── ard/
│
├── scripts/
│   ├── generate.sh
│   ├── validate.sh
│   ├── compatibility.sh
│   └── release.sh
│
└── .github/workflows/
    ├── validate.yml
    ├── conformance.yml
    └── release.yml
```

# 2. 权威来源

依赖方向：

```text
JSON Schema
  ├── OpenAPI / AsyncAPI 引用
  ├── SDK Generated Models
  ├── Golden Examples 校验
  └── Conformance Fixtures

Handwritten SDK Runtime
  → 只依赖 Generated Models 和标准库/最小运行依赖
```

# 3. 不应出现的目录

- Console 数据库实现。
- 飞书 Adapter。
- Console Web UI。
- 具体客户配置。
- 真实 Deployment Credential。
- 具体 Agent 业务逻辑。
- 可被 Console 反向导入的 Console Internal Package。
- 金运专属的公共 wire namespace。
- 只有连接金运 Console 才能运行的 Conformance Test。

# 4. 渐进创建原则

目录规划表示最终结构，不要求第一提交创建全部空目录。按开发阶段创建有真实内容的目录，避免空占位文件。

# 5. 包命名建议

公共项目名、仓库名、首选包名和 CLI 已确定。GitHub 用户名、域名和包注册表所有权仍需填写或核验，在此之前不得发布稳定包：

```text
Go: github.com/gmslll/agent-runtime-operations-protocol
Python distribution: arop-sdk
Python import: arop
TypeScript: @arop/sdk
CLI: arop
CLI config: ~/.config/arop/
Schema ID: https://<public-domain>/schemas/...
Event Type: <public.namespace>.agent.<event>.v1
```

初期仓库位于 `gmslll/agent-runtime-operations-protocol`。正式发布前必须确认域名并核验 PyPI/npm 包所有权。项目简称为 AROP，不得缩写为已经被其他项目使用的 ARP。
