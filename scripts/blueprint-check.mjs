#!/usr/bin/env node

import { readFile, stat } from "node:fs/promises";
import path from "node:path";

import { loadStructuredFile, repositoryRoot, walkFiles } from "./lib/repository.mjs";
import { actualCommand, writeCheckReport } from "./lib/report.mjs";

const reportDirectory = "build/reports/P02";
const checks = [];
const errors = [];

function record(name, passed, detail) {
  checks.push({ name, passed, detail });
  if (!passed) errors.push(`${name}: ${detail}`);
}

function relative(filePath) {
  return path.relative(repositoryRoot, filePath).split(path.sep).join("/");
}

async function source(relativePath) {
  return readFile(path.join(repositoryRoot, relativePath), "utf8");
}

const [layout, blueprint, plan, readme, agents, architecture, sdk, makefile, requirements, manifest] =
  await Promise.all([
    source("docs/DIRECTORY_STRUCTURE.md"),
    source("docs/IMPLEMENTATION_BLUEPRINT.md"),
    source("docs/DEVELOPMENT_PLAN.md"),
    source("README.md"),
    source("AGENTS.md"),
    source("docs/ARCHITECTURE.md"),
    source("docs/SDK_AND_DX.md"),
    source("Makefile"),
    loadStructuredFile(path.join(repositoryRoot, "spec/requirements.yaml")),
    loadStructuredFile(path.join(repositoryRoot, "spec/artifact-manifest.yaml")),
  ]);

const targetTreeNeedles = [
  "<!-- blueprint-target-tree:v1 -->",
  "openapi/control-plane-v1.yaml",
  "sdk/go/generated/",
  "sdk/python/src/arop/",
  "sdk/typescript/",
  "cmd/arop-conformance/",
  "conformance/",
  "reference/control-plane/",
  "internal/domain/",
  "internal/ports/",
  "migrations/sqlite/",
  "migrations/postgres/",
  "deployments/quickstart/",
  "deployments/production-reference/",
  ".github/workflows/",
];
const missingTree = targetTreeNeedles.filter((needle) => !layout.includes(needle));
record(
  "target-tree-declaration",
  missingTree.length === 0,
  missingTree.length === 0 ? `${targetTreeNeedles.length} target boundaries declared` : `missing: ${missingTree.join(", ")}`,
);

record(
  "review-candidate-status-before-user-gate",
  layout.includes("status: review-candidate") &&
    blueprint.includes("status: review-candidate") &&
    blueprint.includes("P04 用户 Gate 前") &&
    readme.includes("不表示规划已冻结"),
  "layout and blueprint remain controlled review candidates until the P04 user gate",
);

const moduleDeclarations = [...layout.matchAll(/<!--\s*blueprint-module:\s*([^>]+?)\s*-->/g)].map(
  (match) => match[1].trim(),
);
const expectedModules = ["go.mod", "reference/control-plane/go.mod"];
record(
  "two-go-modules",
  JSON.stringify(moduleDeclarations) === JSON.stringify(expectedModules) &&
    layout.includes("不建第三个 `go.mod`") &&
    layout.includes("conformance/` 只包含语言中立") &&
    layout.includes("真实 `go.work` 默认不提交") &&
    layout.includes("临时 Go proxy"),
  `declared modules: ${moduleDeclarations.join(", ") || "none"}`,
);

record(
  "portable-and-neutral-boundaries",
  layout.includes("cmd/arop-conformance") &&
    blueprint.includes("不依赖 Reference `internal`") &&
    !["cc-connect-adapter", "codex-adapter", "feishu-adapter"].some((needle) => layout.toLowerCase().includes(needle)) &&
    architecture.includes("不提供厂商专属 Adapter"),
  "portable runner is root-owned, conformance is language-neutral and no vendor adapter path is planned",
);

const phasePattern = /^## (P\d{2}) — ([^\n]+)\n([\s\S]*?)(?=^## P\d{2} — |^# 4\.|(?![\s\S]))/gm;
const metadataKeys = [
  "Type",
  "Status",
  "Capability owner",
  "Goal",
  "Scope",
  "Dependencies",
  "First-path invariants",
  "Machine acceptance",
  "Rollback point",
  "Definition of done",
];
const phases = [];
for (const match of plan.matchAll(phasePattern)) {
  const metadata = {};
  for (const key of metadataKeys) {
    metadata[key] = match[3].match(new RegExp(`^- \\*\\*${key}:\\*\\* (.+)$`, "m"))?.[1];
  }
  phases.push({ id: match[1], title: match[2].trim(), body: match[3], metadata });
}
const expectedPhaseIDs = phases.map((_, index) => `P${String(index + 1).padStart(2, "0")}`);
const actualPhaseIDs = phases.map((phase) => phase.id);
record(
  "dynamic-phase-sequence",
  phases.length >= 38 && JSON.stringify(actualPhaseIDs) === JSON.stringify(expectedPhaseIDs),
  `${phases.length} phases parsed; expected a continuous P01..Pn sequence with at least 38 schedulable phases`,
);

const allowedTypes = new Set(["implement", "refactor", "verify · review", "verify · spec", "gate", "deliver"]);
const metadataProblems = [];
const implementationOwners = new Set();
for (const phase of phases) {
  for (const key of metadataKeys) {
    if (!phase.metadata[key]) metadataProblems.push(`${phase.id} missing ${key}`);
  }
  if (!allowedTypes.has(phase.metadata.Type)) metadataProblems.push(`${phase.id} illegal type ${phase.metadata.Type}`);
  if (["implement", "refactor"].includes(phase.metadata.Type)) {
    const owner = phase.metadata["Capability owner"];
    if (implementationOwners.has(owner)) metadataProblems.push(`${phase.id} reuses implement/refactor owner ${owner}`);
    implementationOwners.add(owner);
    if ((phase.metadata.Scope ?? "").length < 30) metadataProblems.push(`${phase.id} scope is not concrete enough`);
  }
  const acceptance = phase.metadata["Machine acceptance"] ?? "";
  if (!new RegExp(`build/reports/${phase.id}/report\\.json`).test(acceptance) || !acceptance.includes("junit.xml")) {
    metadataProblems.push(`${phase.id} must report to build/reports/${phase.id}/{report.json,junit.xml}`);
  }
  if (![...acceptance.matchAll(/\bmake ([a-z0-9-]+)/g)].length) {
    metadataProblems.push(`${phase.id} has no Make acceptance command`);
  }
}
record(
  "phase-types-scope-and-reports",
  metadataProblems.length === 0,
  metadataProblems.length === 0
    ? "all phases use paseo-epic types, bounded owners/scopes and canonical JSON/JUnit reports"
    : metadataProblems.join("; "),
);

const phaseIDs = new Set(actualPhaseIDs);
const dependencyMap = new Map();
const dependencyProblems = [];
for (const phase of phases) {
  const raw = phase.metadata.Dependencies;
  const dependencies = raw === "none" ? [] : [...(raw ?? "").matchAll(/P\d{2}/g)].map((match) => match[0]);
  if (raw !== "none" && dependencies.length === 0) dependencyProblems.push(`${phase.id} has unparseable dependencies`);
  for (const dependency of dependencies) {
    if (!phaseIDs.has(dependency)) dependencyProblems.push(`${phase.id} references unknown ${dependency}`);
    if (dependency >= phase.id) dependencyProblems.push(`${phase.id} depends on non-prior ${dependency}`);
  }
  dependencyMap.set(phase.id, dependencies);
}
const visiting = new Set();
const visited = new Set();
function visit(phaseID) {
  if (visiting.has(phaseID)) {
    dependencyProblems.push(`cycle reaches ${phaseID}`);
    return;
  }
  if (visited.has(phaseID)) return;
  visiting.add(phaseID);
  for (const dependency of dependencyMap.get(phaseID) ?? []) visit(dependency);
  visiting.delete(phaseID);
  visited.add(phaseID);
}
for (const phaseID of phaseIDs) visit(phaseID);
record(
  "phase-dependency-dag",
  dependencyProblems.length === 0,
  dependencyProblems.length === 0 ? "all dependencies resolve to prior phases; DAG is acyclic" : dependencyProblems.join("; "),
);

const phaseByOwner = new Map(phases.map((phase) => [phase.metadata["Capability owner"], phase]));
const orderedOwners = [
  "spec-governance",
  "implementation-planning",
  "independent-reviewer",
  "project-owner",
  "repository-layout",
  "protocol-foundation",
  "code-generation",
  "control-plane-platform",
  "control-plane-storage",
  "control-plane-security-foundation",
  "publication-contracts",
  "publication-service",
  "registry-core",
  "registry-api",
  "registry-recovery",
  "registry-verification",
  "run-service",
  "dispatch-security",
  "event-ledger",
  "delivery-http",
  "streaming-delivery",
  "run-delivery-verification",
  "worker-service",
  "go-worker-client",
  "operations-security-review",
  "python-provider",
  "typescript-consumer",
  "interop-a2a",
  "interop-mcp",
  "interop-ard",
  "interop-observability",
  "portable-conformance",
  "server-conformance",
  "fault-ha-harness",
  "developer-experience",
  "resilience-verification",
  "release-engineering",
  "release-dry-run-verification",
  "release-readiness-review",
  "public-governance",
  "public-artifact-generation",
  "public-release-verification",
  "v1-rc-delivery",
  "external-conformance-review",
  "v1-freeze-review",
  "v1-delivery",
];
const ownerProblems = [];
let previousIndex = -1;
for (const owner of orderedOwners) {
  const phase = phaseByOwner.get(owner);
  if (!phase) {
    ownerProblems.push(`missing capability owner ${owner}`);
    continue;
  }
  const index = phases.indexOf(phase);
  if (index <= previousIndex) ownerProblems.push(`${owner} is out of order at ${phase.id}`);
  previousIndex = index;
}
record(
  "capability-owner-sequence",
  ownerProblems.length === 0,
  ownerProblems.length === 0 ? `${orderedOwners.length} required capability owners are present in lifecycle order` : ownerProblems.join("; "),
);

function phaseHas(owner, needles) {
  const phase = phaseByOwner.get(owner);
  return phase && needles.every((needle) => phase.body.includes(needle));
}
const firstPathChecks = [
  ["control-plane-platform", ["Clock/ID/Fault", "Trace/Audit"]],
  ["control-plane-storage", ["readiness", "dirty", "backup-restore"]],
  ["control-plane-security-foundation", ["Credential", "静态 scheme/host/IP", "连接时 DNS/IP", "redirect"]],
  ["run-service", ["Outbox", "Cancel", "Deadline", "Usage", "Audit", "Trace", "effect_id"]],
  ["event-ledger", ["append-only", "Inbox", "终态不可逆", "Final Usage"]],
  ["delivery-http", ["浏览器 v1 仅 BFF/Proxy", "DNS/IP 重检"]],
  ["worker-service", ["P19 Event/Inbox/Outbox/effect_id"]],
];
const firstPathProblems = firstPathChecks
  .filter(([owner, needles]) => !phaseHas(owner, needles))
  .map(([owner]) => owner);
record(
  "first-path-cross-cutting-invariants",
  firstPathProblems.length === 0,
  firstPathProblems.length === 0
    ? "Clock/ID/Fault, migration readiness, credentials/endpoint security, audit/trace/cancel/deadline/usage/effect/inbox/outbox are built at first use"
    : `missing first-path invariants in: ${firstPathProblems.join(", ")}`,
);

const p03 = phaseByOwner.get("independent-reviewer");
const p04 = phaseByOwner.get("project-owner");
const p01 = phaseByOwner.get("spec-governance");
const p02 = phaseByOwner.get("implementation-planning");
record(
  "planning-audit-and-user-gate",
  p01?.metadata.Type === "implement" &&
    p01?.metadata.Status === "complete" &&
    p02?.metadata.Type === "implement" &&
    p02?.metadata.Status === "complete" &&
    p03?.metadata.Type === "verify · review" &&
    p03?.metadata.Status === "pending-review" &&
    p03.body.includes("requirements_phase_coverage=YES") &&
    p03.body.includes("must_fix_count=0") &&
    p03.body.includes("make planning-audit") &&
    p04?.metadata.Type === "gate" &&
    p04?.metadata.Status === "blocked" &&
    p04.body.includes("make gate-check") &&
    (dependencyMap.get(p04.id) ?? []).includes(p03.id),
  "P01/P02 are completed implementation work; P03 is pending external review with zero must-fix; P04 is blocked on the explicit user gate",
);

const releaseToolchain = phaseByOwner.get("release-engineering");
const releaseDryRun = phaseByOwner.get("release-dry-run-verification");
record(
  "release-toolchain-before-read-only-dry-run",
  releaseToolchain?.metadata.Type === "implement" &&
    releaseToolchain.body.includes("双 Go Module 临时 proxy") &&
    releaseToolchain.body.includes("SBOM/provenance") &&
    releaseDryRun?.metadata.Type === "verify · spec" &&
    releaseDryRun.body.includes("只读") &&
    releaseDryRun.body.includes("不在验证阶段补代码") &&
    (dependencyMap.get(releaseDryRun.id) ?? []).includes(releaseToolchain.id),
  "release tooling is implemented by one owner before a dependent read-only reproducibility verification",
);

const acceptanceTestsByPhase = new Map();
for (const phase of phases) {
  acceptanceTestsByPhase.set(
    phase.id,
    [...phase.metadata["Machine acceptance"].matchAll(/\bmake ([a-z0-9-]+)/g)].map((match) => `make-${match[1]}`),
  );
}
const requirementProblems = [];
for (const requirement of requirements.requirements ?? []) {
  const allowedTests = new Set(
    (requirement.phases ?? []).flatMap((phaseID) => acceptanceTestsByPhase.get(phaseID) ?? []),
  );
  for (const phaseID of requirement.phases ?? []) {
    if (!phaseIDs.has(phaseID)) requirementProblems.push(`${requirement.id} maps unknown ${phaseID}`);
    if (!(acceptanceTestsByPhase.get(phaseID) ?? []).some((testID) => requirement.tests.includes(testID))) {
      requirementProblems.push(`${requirement.id} lacks acceptance for ${phaseID}`);
    }
  }
  for (const testID of requirement.tests ?? []) {
    if (!allowedTests.has(testID)) requirementProblems.push(`${requirement.id} maps non-phase test ${testID}`);
  }
}
record(
  "requirement-phase-test-traceability",
  (requirements.requirements ?? []).length === 14 && requirementProblems.length === 0,
  requirementProblems.length === 0
    ? "IR-01..IR-14 map only to existing phases and their exact Make acceptance targets"
    : requirementProblems.join("; "),
);

const artifactByID = new Map((manifest.artifacts ?? []).map((artifact) => [artifact.id, artifact]));
const contractArtifacts = [
  ["openapi-control-plane", "P11", "public-contract", ["publication", "jwks-discovery", "event-session-exchange", "asset-token-exchange"]],
  ["openapi-agent-runtime", "P20", "public-contract", ["agent-runtime-direct", "agent-runtime-proxy"]],
  ["openapi-registry-runtime", "P14", "public-contract", ["runtime-registration", "lease-keepalive"]],
  ["openapi-discovery-runtime", "P14", "public-contract", ["discovery-snapshot", "discovery-watch"]],
  ["openapi-worker-runtime", "P23", "public-contract", ["worker-claim", "attempt-lease"]],
  ["reference-secret-exchange", "P10", "reference-only", ["secret-ref-resolution"]],
];
const contractProblems = [];
for (const [id, phase, exposure, capabilities] of contractArtifacts) {
  const artifact = artifactByID.get(id);
  if (!artifact) {
    contractProblems.push(`missing ${id}`);
    continue;
  }
  if (artifact.phase !== phase || artifact.exposure !== exposure || !artifact.owner || !(artifact.tests?.length > 0)) {
    contractProblems.push(`${id} lacks owner/phase/tests/exposure ownership`);
  }
  const ownerPhase = phases.find((candidate) => candidate.id === phase);
  if (artifact.owner !== ownerPhase?.metadata["Capability owner"]) {
    contractProblems.push(`${id} owner ${artifact.owner} does not match ${phase} owner`);
  }
  if (!(acceptanceTestsByPhase.get(phase) ?? []).some((testID) => artifact.tests?.includes(testID))) {
    contractProblems.push(`${id} tests do not include the Make acceptance of ${phase}`);
  }
  for (const capability of capabilities) {
    if (!artifact.capabilities?.includes(capability)) contractProblems.push(`${id} lacks capability ${capability}`);
  }
}
record(
  "public-reference-api-ownership",
  contractProblems.length === 0 && blueprint.includes("SecretRef resolution/exchange"),
  contractProblems.length === 0
    ? "Publication/Control Plane, JWKS, Event Session, Asset and reference-only Secret ownership is explicit"
    : contractProblems.join("; "),
);

const releaseArtifactExpectations = [
  ["release-engineering-toolchain", "P37", "release-engineering", "make-build-release-toolchain"],
  ["release-toolchain-reports", "P37", "release-engineering", "make-build-release-toolchain"],
  ["release-dry-run-reports", "P38", "release-dry-run-verification", "make-release-dry-run"],
];
const releaseArtifactProblems = [];
for (const [id, phase, owner, test] of releaseArtifactExpectations) {
  const artifact = artifactByID.get(id);
  if (!artifact) {
    releaseArtifactProblems.push(`missing ${id}`);
    continue;
  }
  if (artifact.phase !== phase || artifact.owner !== owner || !artifact.tests?.includes(test)) {
    releaseArtifactProblems.push(`${id} must be owned by ${owner}/${phase} and tested by ${test}`);
  }
}
record(
  "release-toolchain-artifact-ownership",
  releaseArtifactProblems.length === 0,
  releaseArtifactProblems.length === 0
    ? "release implementation and read-only dry-run reports have explicit artifact owner/phase/test mappings"
    : releaseArtifactProblems.join("; "),
);

const releaseTitles = [
  "External public configuration gate",
  "Real public namespace regeneration",
  "Public namespace final verification",
  "Deliver v1 release candidate",
  "Independent implementation and partner evidence gate",
  "Strictly read-only v1 freeze verification",
  "Deliver v1 from the approved source commit",
];
const releasePhases = releaseTitles.map((title) => phases.find((phase) => phase.title === title));
const releaseProblems = [];
for (const [index, phase] of releasePhases.entries()) {
  if (!phase) releaseProblems.push(`missing ${releaseTitles[index]}`);
  if (index > 0 && phase && !(dependencyMap.get(phase.id) ?? []).includes(releasePhases[index - 1]?.id)) {
    releaseProblems.push(`${phase.id} does not directly depend on prior release-chain phase`);
  }
}
const [configGate, regenerate, finalVerify, v1RC, evidenceGate, freeze, v1Deliver] = releasePhases;
if (v1RC && !v1RC.body.includes("v1.0.0-rc.N")) {
  releaseProblems.push("v1 RC stage is not final-v1-RC specific");
}
if (evidenceGate && !["exact source commit", "Schema Bundle Digest", "Runner Digest", "Artifact Digest"].every((needle) => evidenceGate.body.includes(needle))) {
  releaseProblems.push("external evidence lacks exact source/schema/runner/artifact binding");
}
if (freeze && !(freeze.metadata.Type === "verify · review" && freeze.body.includes("严格只读") && freeze.body.includes("不关闭 RFC") && freeze.body.includes("不改 Compatibility Matrix"))) {
  releaseProblems.push("freeze is not strictly read-only");
}
if (v1Deliver && !(v1Deliver.body.includes("同一 source commit") && v1Deliver.body.includes("payload-equivalence") && v1Deliver.body.includes("Conformance/SBOM/provenance"))) {
  releaseProblems.push("v1 delivery lacks same-commit payload equivalence and final verification");
}
record(
  "v1-rc-evidence-freeze-delivery-chain",
  releaseProblems.length === 0,
  releaseProblems.length === 0
    ? `${configGate.id}->${regenerate.id}->${finalVerify.id}->${v1RC.id}->${evidenceGate.id}->${freeze.id}->${v1Deliver.id} enforces final v1 RC evidence and equivalent v1 delivery`
    : releaseProblems.join("; "),
);

const releaseNeedles = [
  "真实 `go.work` 默认不提交",
  "临时 Go proxy",
  "根 module 伪版本/RC 候选",
  "GOWORK=off",
  "先推送根 `vX.Y.Z`",
  "再推送 `reference/control-plane/vX.Y.Z`",
  "同一 source commit",
  "payload-equivalence",
  "SBOM",
  "provenance",
];
const missingReleaseRules = releaseNeedles.filter((needle) => !`${layout}\n${blueprint}`.includes(needle));
record(
  "two-module-release-procedure",
  missingReleaseRules.length === 0,
  missingReleaseRules.length === 0 ? "development workspace, temporary proxy, tag order and payload equivalence are frozen" : `missing: ${missingReleaseRules.join(", ")}`,
);

const auditArtifacts = [
  "planning-audit-evidence-schema",
  "user-gate-evidence-schema",
  "planning-audit-validation",
  "planning-audit-reports",
  "planning-audit-signed-summary",
  "user-gate-validation",
  "user-gate-reports",
  "user-gate-signed-summary",
];
const missingAuditArtifacts = auditArtifacts.filter((id) => !artifactByID.has(id));
const expectedAuditPaths = new Map([
  ["planning-audit-evidence-schema", "spec/schemas/planning-audit-evidence.schema.json"],
  ["user-gate-evidence-schema", "spec/schemas/user-gate-evidence.schema.json"],
  ["planning-audit-validation", "scripts/planning-audit.mjs"],
  ["planning-audit-reports", "build/reports/P03"],
  ["planning-audit-signed-summary", "spec/evidence/P03-planning-audit-summary.json"],
  ["user-gate-validation", "scripts/gate-check.mjs"],
  ["user-gate-reports", "build/reports/P04"],
  ["user-gate-signed-summary", "spec/evidence/P04-user-gate-summary.json"],
]);
const wrongAuditPaths = [...expectedAuditPaths].filter(
  ([id, expectedPath]) => artifactByID.get(id)?.path !== expectedPath,
);
record(
  "audit-gate-tooling-catalog",
  missingAuditArtifacts.length === 0 &&
    wrongAuditPaths.length === 0 &&
    makefile.includes("planning-audit:") &&
    makefile.includes("gate-check:") &&
    plan.includes("原始外部证据") &&
    agents.includes("原始外部证据不入 Git"),
  missingAuditArtifacts.length === 0 && wrongAuditPaths.length === 0
    ? "P03/P04 schemas, validators and planned reports are cataloged; raw evidence remains outside Git"
    : `missing: ${missingAuditArtifacts.join(", ")}; wrong paths: ${wrongAuditPaths.map(([id]) => id).join(", ")}`,
);

record(
  "console-isolation",
  blueprint.includes("`kinglucky-agent-console` 不在本实施范围内") &&
    architecture.includes("不在本仓库 P01–P46 实施范围") &&
    readme.includes("不依赖 Console") &&
    agents.includes("不得依赖 `kinglucky-agent-console`") &&
    !layout.includes("kinglucky-agent-console/"),
  "Console remains a downstream consumer and outside all implementation phases",
);

const semanticNeedles = [
  "Authoring strict",
  "Consumer forward compatible",
  "Offline closure",
  "原始事件是不可变 append-only",
  "语义一致”而非“SQL 一致",
  "全量 codegen 前必须先执行代表 Schema spike",
  "预期子报告 ID/数量",
  "原始外部证据保存在仓库外",
];
const missingSemantics = semanticNeedles.filter((needle) => !`${blueprint}\n${sdk}`.includes(needle));
record(
  "blueprint-cross-cutting-rules",
  missingSemantics.length === 0,
  missingSemantics.length === 0 ? "contract, event, storage, codegen, aggregate-report and evidence rules are explicit" : `missing: ${missingSemantics.join(", ")}`,
);

const markdownFiles = await walkFiles(repositoryRoot, (filePath) => filePath.endsWith(".md"));
const markdownProblems = [];
const markdownLinkPattern = /\[[^\]]+\]\(([^)]+)\)/g;
for (const markdownFile of markdownFiles) {
  const markdown = await readFile(markdownFile, "utf8");
  for (const match of markdown.matchAll(markdownLinkPattern)) {
    let target = match[1].trim().replace(/^<|>$/g, "");
    target = target.split(/\s+["']/u, 1)[0];
    const localTarget = target.split("#", 1)[0].split("?", 1)[0];
    if (!localTarget || /^(?:https?:|mailto:)/i.test(localTarget)) continue;
    try {
      await stat(path.resolve(path.dirname(markdownFile), decodeURIComponent(localTarget)));
    } catch {
      markdownProblems.push(`${relative(markdownFile)} -> ${target}`);
    }
  }
}
record(
  "markdown-local-link-closure",
  markdownProblems.length === 0,
  markdownProblems.length === 0 ? `${markdownFiles.length} Markdown files have closed local links` : markdownProblems.join(", "),
);

await writeCheckReport({
  reportDirectory,
  suiteName: "arop-blueprint-check",
  className: "arop.blueprint",
  command: actualCommand("scripts/blueprint-check.mjs"),
  checkerPath: "scripts/blueprint-check.mjs",
  inputPaths: [
    "README.md",
    "AGENTS.md",
    "Makefile",
    "docs/ARCHITECTURE.md",
    "docs/DEVELOPMENT_PLAN.md",
    "docs/DIRECTORY_STRUCTURE.md",
    "docs/IMPLEMENTATION_BLUEPRINT.md",
    "docs/SDK_AND_DX.md",
    "spec/artifact-manifest.yaml",
    "spec/requirements.yaml",
    "scripts/blueprint-check.mjs",
    "scripts/lib/report.mjs",
    "scripts/lib/repository.mjs",
  ],
  checks,
  errors,
  summary: {
    phases: phases.length,
    implement_or_refactor_phases: phases.filter((phase) => ["implement", "refactor"].includes(phase.metadata.Type)).length,
    verify_phases: phases.filter((phase) => phase.metadata.Type?.startsWith("verify ·")).length,
    gates: phases.filter((phase) => phase.metadata.Type === "gate").length,
    deliveries: phases.filter((phase) => phase.metadata.Type === "deliver").length,
    immutable_requirements: requirements.requirements?.length ?? 0,
    declared_go_modules: moduleDeclarations.length,
    markdown_files: markdownFiles.length,
  },
  auditNote: "This repeatable self-check cannot replace the independent P03 planning audit or the explicit P04 user gate.",
});

if (errors.length > 0) {
  console.error(`AROP blueprint check failed with ${errors.length} error(s):`);
  for (const error of errors) console.error(`- ${error}`);
  console.error(`Reports: ${reportDirectory}/report.json and junit.xml`);
  process.exit(1);
}
console.log(`AROP blueprint check passed: ${phases.length} continuous phases, ${checks.length} checks and ${moduleDeclarations.length} Go modules.`);
console.log(`Reports: ${reportDirectory}/report.json and junit.xml`);
