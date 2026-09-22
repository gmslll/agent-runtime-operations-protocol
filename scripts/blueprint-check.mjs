#!/usr/bin/env node

import { readFile, stat } from "node:fs/promises";
import path from "node:path";
import {
  findPublicV01Violations,
  loadStructuredFile,
  publicV01ViolationsInText,
  repositoryRoot,
  walkFiles,
} from "./lib/repository.mjs";
import { actualCommand, writeCheckReport } from "./lib/report.mjs";

const reportDirectory = "build/reports/P02";
const checks = [];
const errors = [];
function record(name, passed, detail) {
  checks.push({ name, passed, detail });
  if (!passed) errors.push(name + ": " + detail);
}
function rel(filePath) {
  return path.relative(repositoryRoot, filePath).split(path.sep).join("/");
}
async function source(filePath) {
  return readFile(path.join(repositoryRoot, filePath), "utf8");
}

const loaded = await Promise.all([
  source("docs/DIRECTORY_STRUCTURE.md"),
  source("docs/IMPLEMENTATION_BLUEPRINT.md"),
  source("docs/DEVELOPMENT_PLAN.md"),
  source("README.md"),
  source("AGENTS.md"),
  source("docs/ARCHITECTURE.md"),
  source("docs/SDK_AND_DX.md"),
  source("docs/DECISIONS.md"),
  source("docs/PUBLIC_PROJECT_AND_ADOPTION.md"),
  source("Makefile"),
  loadStructuredFile(path.join(repositoryRoot, "spec/requirements.yaml")),
  loadStructuredFile(path.join(repositoryRoot, "spec/artifact-manifest.yaml")),
]);
const [layout, blueprint, plan, readme, agents, architecture, sdk, decisions, publicAdoption, makefile, requirements, manifest] = loaded;
const artifacts = manifest.artifacts || [];
const artifactByID = new Map(artifacts.map((artifact) => [artifact.id, artifact]));

const treeNeedles = [
  "<!-- blueprint-target-tree:v1 -->",
  "openapi/fragments/control-plane/",
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
  "reference/agents/go-http/",
  "deployments/quickstart/",
  "deployments/production-reference/",
  ".github/workflows/",
];
const missingTree = treeNeedles.filter((needle) => !layout.includes(needle));
record("target-tree-declaration", missingTree.length === 0,
  missingTree.length === 0 ? String(treeNeedles.length) + " target boundaries declared" : "missing " + missingTree.join(", "));
record("review-candidate-status",
  layout.includes("status: review-candidate") && blueprint.includes("status: review-candidate") &&
  blueprint.includes("P04 用户 Gate 前") && readme.includes("不表示规划已冻结"),
  "layout and blueprint remain review candidates before P04");

const moduleDeclarations = [...layout.matchAll(/<!--\s*blueprint-module:\s*([^>]+?)\s*-->/g)]
  .map((match) => match[1].trim());
record("two-module-bootstrap",
  JSON.stringify(moduleDeclarations) === JSON.stringify(["go.mod", "reference/control-plane/go.mod"]) &&
  layout.includes("不建第三个") && layout.includes("真实") && layout.includes("go.work") &&
  layout.includes("临时 Go proxy") && plan.includes("GOWORK=off") && plan.includes("root pseudo-version"),
  "declared modules: " + moduleDeclarations.join(", "));
record("portable-vendor-neutral",
  layout.includes("cmd/arop-conformance") && layout.includes("语言中立") &&
  blueprint.includes("不依赖 Reference") && architecture.includes("不提供厂商专属 Adapter") &&
  !["cc-connect-adapter", "feishu-adapter", "codex-adapter"].some((needle) =>
    (layout + "\n" + plan).toLowerCase().includes(needle)),
  "portable root runner, neutral fixtures and no vendor adapter");

const phasePattern = /^## (P\d{2}) — ([^\n]+)\n([\s\S]*?)(?=^## P\d{2} — |^# 4\.|(?![\s\S]))/gm;
const metadataKeys = [
  "Type", "Status", "Capability owner", "Components", "Artifacts owned", "Goal", "Scope",
  "Dependencies", "First-path invariants", "Machine acceptance", "Rollback point", "Definition of done",
];
const phases = [];
for (const match of plan.matchAll(phasePattern)) {
  const metadata = {};
  for (const key of metadataKeys) {
    metadata[key] = match[3].match(new RegExp("^- \\*\\*" + key + ":\\*\\* (.+)$", "m"))?.[1];
  }
  phases.push({ id: match[1], title: match[2].trim(), body: match[3], metadata });
}
const phaseIDs = phases.map((phase) => phase.id);
const expectedPhaseIDs = phases.map((unused, index) => "P" + String(index + 1).padStart(2, "0"));
const validPhaseIDs = new Set(phaseIDs);
const phaseByID = new Map(phases.map((phase) => [phase.id, phase]));
const phaseByOwner = new Map(phases.map((phase) => [phase.metadata["Capability owner"], phase]));
record("dynamic-phase-sequence",
  phases.length >= 50 && JSON.stringify(phaseIDs) === JSON.stringify(expectedPhaseIDs) &&
  phases.at(-1)?.metadata["Capability owner"] === "v1-delivery",
  String(phases.length) + " continuous phases ending with v1-delivery");

const allowedTypes = new Set(["implement", "refactor", "verify · review", "verify · spec", "gate", "deliver"]);
const componentWhitelist = new Set([
  "governance", "planning", "repository", "protocol", "codegen", "control-plane", "operations", "storage",
  "identity", "publication", "assets", "registry", "sdk-go", "run", "dispatch", "events", "delivery",
  "streaming", "worker", "sdk-python", "sdk-typescript", "interop", "conformance", "fault-ha",
  "quickstart", "deployment", "release",
]);
const metadataProblems = [];
const acceptanceByPhase = new Map();
const planOwnerByArtifact = new Map();
const implementationOwners = new Set();
for (const phase of phases) {
  for (const key of metadataKeys) if (!phase.metadata[key]) metadataProblems.push(phase.id + " missing " + key);
  if (!allowedTypes.has(phase.metadata.Type)) metadataProblems.push(phase.id + " illegal type " + phase.metadata.Type);
  const acceptance = phase.metadata["Machine acceptance"] || "";
  const phaseTests = [...acceptance.matchAll(/\bmake ([a-z0-9-]+)/g)].map((match) => "make-" + match[1]);
  acceptanceByPhase.set(phase.id, phaseTests);
  if (phaseTests.length !== 1) metadataProblems.push(phase.id + " needs exactly one Make command");
  if (!acceptance.includes("build/reports/" + phase.id + "/report.json") || !acceptance.includes("junit.xml")) {
    metadataProblems.push(phase.id + " lacks canonical JSON/JUnit report paths");
  }
  const components = (phase.metadata.Components || "").split(",").map((value) => value.trim()).filter(Boolean);
  if (!components.length) metadataProblems.push(phase.id + " has no component");
  for (const component of components) if (!componentWhitelist.has(component)) metadataProblems.push(phase.id + " invalid component " + component);
  const owned = phase.metadata["Artifacts owned"] === "none" ? [] :
    (phase.metadata["Artifacts owned"] || "").split(",").map((value) => value.trim()).filter(Boolean);
  if (["implement", "refactor"].includes(phase.metadata.Type)) {
    if (implementationOwners.has(phase.metadata["Capability owner"])) metadataProblems.push("duplicate implement owner " + phase.metadata["Capability owner"]);
    implementationOwners.add(phase.metadata["Capability owner"]);
    if (!owned.length) metadataProblems.push(phase.id + " implement/refactor owns no artifact");
  } else if (owned.length) {
    metadataProblems.push(phase.id + " non-implementation phase owns source artifacts");
  }
  for (const id of owned) {
    if (planOwnerByArtifact.has(id)) metadataProblems.push(id + " is plan-owned twice");
    planOwnerByArtifact.set(id, phase.id);
  }
}
record("phase-metadata-and-reports", metadataProblems.length === 0,
  metadataProblems.length === 0 ? "all phases have legal metadata and one report-producing acceptance" : metadataProblems.join("; "));

const dependencyMap = new Map();
const dependencyProblems = [];
for (const phase of phases) {
  const raw = phase.metadata.Dependencies;
  const dependencies = raw === "none" ? [] : [...(raw || "").matchAll(/P\d{2}/g)].map((match) => match[0]);
  if (raw !== "none" && !dependencies.length) dependencyProblems.push(phase.id + " has unparseable dependencies");
  for (const dependency of dependencies) {
    if (!validPhaseIDs.has(dependency)) dependencyProblems.push(phase.id + " unknown dependency " + dependency);
    if (Number(dependency.slice(1)) >= Number(phase.id.slice(1))) dependencyProblems.push(phase.id + " non-prior dependency " + dependency);
  }
  dependencyMap.set(phase.id, dependencies);
}
const visitingPhases = new Set();
const visitedPhases = new Set();
function visitPhase(id) {
  if (visitingPhases.has(id)) { dependencyProblems.push("phase cycle at " + id); return; }
  if (visitedPhases.has(id)) return;
  visitingPhases.add(id);
  for (const dependency of dependencyMap.get(id) || []) visitPhase(dependency);
  visitingPhases.delete(id);
  visitedPhases.add(id);
}
for (const id of phaseIDs) visitPhase(id);
record("phase-dependency-dag", dependencyProblems.length === 0,
  dependencyProblems.length === 0 ? "all phase dependencies are prior and acyclic" : dependencyProblems.join("; "));

const requiredOwners = [
  "spec-governance", "implementation-planning", "independent-reviewer", "project-owner", "repository-layout",
  "protocol-foundation", "code-generation", "control-plane-platform", "control-plane-storage",
  "identity-secret-foundation", "publication-contracts", "publication-service", "asset-broker", "registry-core",
  "registry-api-sdk", "registry-recovery", "registry-verification", "run-service", "dispatch-security",
  "event-ledger", "go-provider-delivery", "streaming-delivery", "run-delivery-verification", "worker-service",
  "go-worker-client", "operations-security-review", "python-provider", "typescript-consumer", "interop-a2a",
  "interop-mcp", "interop-ard", "interop-observability", "portable-conformance", "server-conformance",
  "fault-ha-harness", "sqlite-quickstart", "production-deployment", "resilience-verification",
  "release-supply-chain", "release-evidence-tooling", "release-lineage-tooling", "release-finalization-tooling",
  "release-dry-run-verification", "release-readiness-review", "public-governance",
  "public-artifact-generation", "rc-source-freeze", "public-release-verification", "v1-rc-delivery",
  "external-conformance-review", "v1-freeze-overlay-review", "v1-release-approval", "v1-delivery",
];
const ownerProblems = [];
let lastOwnerIndex = -1;
for (const owner of requiredOwners) {
  const phase = phaseByOwner.get(owner);
  if (!phase) { ownerProblems.push("missing " + owner); continue; }
  const index = phases.indexOf(phase);
  if (index <= lastOwnerIndex) ownerProblems.push(owner + " out of order");
  lastOwnerIndex = index;
}
record("capability-owner-order", ownerProblems.length === 0,
  ownerProblems.length === 0 ? String(requiredOwners.length) + " capability owners are ordered" : ownerProblems.join("; "));

const p01 = phaseByID.get("P01");
const p02 = phaseByID.get("P02");
const p03 = phaseByID.get("P03");
const p04 = phaseByID.get("P04");
record("planning-audit-user-gate",
  p01?.metadata.Type === "implement" && p01.metadata.Status === "complete" &&
  p02?.metadata.Type === "implement" && p02.metadata.Status === "complete" &&
  p03?.metadata.Type === "verify · review" && p03.metadata.Status === "pending-review" &&
  p04?.metadata.Type === "gate" && p04.metadata.Status === "blocked" &&
  p03.body.includes("must_fix_count=0") && p04.body.includes("summary_sha256") &&
  (dependencyMap.get("P04") || []).includes("P03"),
  "P01/P02 complete, P03 pending re-audit and P04 blocked");

function phaseHas(owner, needles) {
  const phase = phaseByOwner.get(owner);
  return Boolean(phase && needles.every((needle) => phase.body.includes(needle)));
}
const firstPathRules = [
  ["control-plane-platform", ["Clock/ID/Fault", "Audit/Trace"]],
  ["control-plane-storage", ["durable Audit", "fixture versions", "readiness"]],
  ["identity-secret-foundation", ["Credential", "SecretRef", "durable Audit"]],
  ["publication-service", ["静态 URL", "离线"]],
  ["asset-broker", ["DNS/IP", "redirect"]],
  ["run-service", ["Outbox", "Cancel", "Deadline", "Usage", "Audit", "Trace", "effect_id"]],
  ["dispatch-security", ["Signer/KMS/SecretRef", "public JWKS", "rotation state"]],
  ["event-ledger", ["append-only", "Inbox", "capacity 释放同事务"]],
  ["go-provider-delivery", ["DurableStore", "SQLite adapter/migration", "effect_id", "crash-before/after-effect", "DNS/IP 重检"]],
  ["worker-service", ["claim 与 Attempt lease 原子", "Inbox/Outbox/effect"]],
];
const firstPathProblems = firstPathRules.filter(([owner, needles]) => !phaseHas(owner, needles)).map(([owner]) => owner);
record("first-path-invariants", firstPathProblems.length === 0,
  firstPathProblems.length === 0 ? "cross-cutting invariants are built at first use" : "missing " + firstPathProblems.join(", "));

const artifactProblems = [];
const lifecycleFields = ["owner_phase", "completion_phase", "producer_phase"];
function phaseNumber(id) { return Number(id?.slice(1)); }
function availability(artifact) {
  const field = lifecycleFields.find((candidate) => artifact?.[candidate]);
  return field ? phaseNumber(artifact[field]) : 0;
}
for (const artifact of artifacts) {
  const roles = lifecycleFields.filter((field) => artifact[field]);
  if (artifact.status === "planned" && roles.length !== 1) artifactProblems.push(artifact.id + " planned lifecycle roles=" + roles.length);
  if (roles.some((field) => !validPhaseIDs.has(artifact[field]))) artifactProblems.push(artifact.id + " unknown lifecycle phase");
  if (artifact.owner_phase) {
    if (!artifact.owner || !artifact.acceptance_test || !artifact.exposure || artifact.path_role !== "concrete") {
      artifactProblems.push(artifact.id + " invalid concrete source metadata");
    }
    const ownerPhase = phaseByID.get(artifact.owner_phase);
    if (ownerPhase?.metadata["Capability owner"] !== artifact.owner) artifactProblems.push(artifact.id + " owner mismatch");
    if (acceptanceByPhase.get(artifact.owner_phase)?.[0] !== artifact.acceptance_test) artifactProblems.push(artifact.id + " acceptance mismatch");
    if (planOwnerByArtifact.get(artifact.id) !== artifact.owner_phase) artifactProblems.push(artifact.id + " missing plan ownership");
  }
  if (artifact.completion_phase && !["aggregate", "container"].includes(artifact.path_role)) artifactProblems.push(artifact.id + " completion is not aggregate/container");
  if (artifact.producer_phase && !["machine-reports", "canonical-evidence-summary"].includes(artifact.kind)) artifactProblems.push(artifact.id + " invalid producer kind");
  for (const dependencyID of artifact.derives_from || []) {
    const dependency = artifactByID.get(dependencyID);
    if (!dependency) { artifactProblems.push(artifact.id + " unknown dependency " + dependencyID); continue; }
    if (availability(dependency) > availability(artifact)) {
      artifactProblems.push(artifact.id + "@" + availability(artifact) + " depends on future " + dependencyID + "@" + availability(dependency));
    }
  }
  for (const inputID of artifact.runtime_inputs || []) if (!artifactByID.has(inputID)) artifactProblems.push(artifact.id + " unknown runtime input " + inputID);
}
for (const [id, phaseID] of planOwnerByArtifact) {
  const artifact = artifactByID.get(id);
  if (!artifact) artifactProblems.push(phaseID + " owns unknown " + id);
  else if (artifact.owner_phase !== phaseID) artifactProblems.push(id + " plan-to-manifest mismatch");
}
record("artifact-lifecycle-temporal-bidirectional", artifactProblems.length === 0,
  artifactProblems.length === 0 ? String(artifacts.length) + " artifacts satisfy lifecycle, temporal and owner closure" : artifactProblems.join("; "));

const artifactDAGProblems = [];
const visitingArtifacts = new Set();
const visitedArtifacts = new Set();
function visitArtifact(id) {
  if (visitingArtifacts.has(id)) { artifactDAGProblems.push("artifact cycle at " + id); return; }
  if (visitedArtifacts.has(id)) return;
  visitingArtifacts.add(id);
  for (const dependency of artifactByID.get(id)?.derives_from || []) visitArtifact(dependency);
  visitingArtifacts.delete(id);
  visitedArtifacts.add(id);
}
for (const id of artifactByID.keys()) visitArtifact(id);
record("artifact-dag-all-kinds", artifactDAGProblems.length === 0,
  artifactDAGProblems.length === 0 ? "artifact derives graph is acyclic" : artifactDAGProblems.join("; "));

const pathProblems = [];
for (let leftIndex = 0; leftIndex < artifacts.length; leftIndex += 1) {
  for (let rightIndex = leftIndex + 1; rightIndex < artifacts.length; rightIndex += 1) {
    const left = artifacts[leftIndex];
    const right = artifacts[rightIndex];
    if (left.path === right.path) { pathProblems.push("duplicate " + left.path); continue; }
    if (right.path.startsWith(left.path + "/") && !["aggregate", "container"].includes(left.path_role)) pathProblems.push(left.id + " covers " + right.id);
    if (left.path.startsWith(right.path + "/") && !["aggregate", "container"].includes(right.path_role)) pathProblems.push(right.id + " covers " + left.id);
  }
}
record("artifact-path-overlap", pathProblems.length === 0,
  pathProblems.length === 0 ? "only aggregate/container paths contain children" : pathProblems.join("; "));

const criticalArtifacts = [
  "sdk-go-protocol-core", "base-state-machine-fixtures", "conformance-harness-base", "codegen-pipeline",
  "codegen-representative-spike", "reference-control-plane-server", "audit-trace-ports",
  "audit-trace-memory-bootstrap", "migration-engine", "migration-engine-fixture-versions",
  "sqlite-uow-storage-adapter", "postgres-uow-storage-adapter", "durable-audit-storage",
  "identity-service", "credential-store", "reference-secret-exchange", "registry-api-service",
  "direct-proxy-delivery-service", "provider-durable-store-port", "reference-provider-sqlite-adapter",
  "reference-provider-sqlite-migration", "streaming-service", "go-streaming-client",
  "typescript-streaming-client", "release-package-orchestrator", "supply-chain-orchestrator",
  "trusted-release-role-registry", "cross-commit-lineage-verifier", "rc-source-freeze-checker",
  "final-equivalence-attestation-schema",
  "freeze-overlay-checker", "payload-equivalence-checker", "final-delivery-checker",
];
const missingArtifacts = criticalArtifacts.filter((id) => !artifactByID.has(id));
record("critical-artifact-owners", missingArtifacts.length === 0,
  missingArtifacts.length === 0 ? String(criticalArtifacts.length) + " critical artifacts cataloged" : "missing " + missingArtifacts.join(", "));

const generatedDomains = ["control-plane", "asset", "registry", "run", "dispatch", "event", "streaming", "worker"];
const codegenProblems = [];
for (const domain of generatedDomains) {
  for (const language of ["go", "python", "typescript"]) {
    const artifact = artifactByID.get("generated-" + domain + "-" + language);
    if (!artifact?.derives_from?.includes("codegen-pipeline")) codegenProblems.push(domain + "/" + language);
  }
}
if ((artifactByID.get("codegen-pipeline")?.derives_from || []).some((id) => id.startsWith("generated-"))) codegenProblems.push("pipeline reverse dependency");
record("schema-fixture-codegen-increments", codegenProblems.length === 0,
  codegenProblems.length === 0 ? "all contract domains own three-language increments from one pipeline" : codegenProblems.join("; "));

const migrationDomains = [
  ["base", "P09"], ["identity", "P10"], ["publication", "P12"], ["asset", "P13"], ["registry", "P14"],
  ["run", "P18"], ["dispatch", "P19"], ["event", "P20"], ["worker", "P24"],
];
const migrationProblems = [];
for (const [domain, phaseID] of migrationDomains) {
  for (const engine of ["sqlite", "postgres"]) {
    if (artifactByID.get(engine + "-migration-" + domain)?.owner_phase !== phaseID) migrationProblems.push(engine + "/" + domain);
  }
  const body = phaseByID.get(phaseID)?.body || "";
  if (!["empty", "N-1→N", "idempotent", "dirty"].every((needle) => body.includes(needle))) migrationProblems.push(phaseID + " matrix");
}
if (!phaseByID.get("P16")?.body.includes("不新增 migration")) migrationProblems.push("P16 no-migration rule");
if ((phaseByID.get("P21")?.metadata["Artifacts owned"] || "").includes("postgres")) migrationProblems.push("P21 provider postgres");
record("concrete-migration-increments", migrationProblems.length === 0,
  migrationProblems.length === 0 ? "nine CP stages own dual migrations; P16 reuses and Provider remains local" : migrationProblems.join("; "));

record("provider-storage-boundary",
  artifactByID.get("provider-durable-store-port")?.path === "sdk/go/provider/durable_store.go" &&
  artifactByID.get("reference-provider-sqlite-adapter")?.path === "reference/agents/go-http/internal/storage/sqlite" &&
  artifactByID.get("reference-provider-sqlite-migration")?.path.startsWith("reference/agents/go-http/migrations/sqlite/") &&
  layout.includes("DurableStore") && sdk.includes("不属于 Control Plane 双库矩阵"),
  "public SDK ports are driver-free and reference agents keep local SQLite in their subtree");

const reportProblems = [];
for (const phase of phases) {
  const report = artifacts.find((artifact) => artifact.kind === "machine-reports" && artifact.path === "build/reports/" + phase.id);
  if (!report || report.producer_phase !== phase.id || report.owner_phase ||
      report.acceptance_test !== acceptanceByPhase.get(phase.id)?.[0]) {
    reportProblems.push(phase.id);
  }
}
record("all-phase-report-producers", reportProblems.length === 0,
  reportProblems.length === 0 ? "all phases have producer_phase report artifacts" : "invalid report artifacts " + reportProblems.join(", "));

const releaseSequence = [
  ["release-supply-chain", "P39", "implement"], ["release-evidence-tooling", "P40", "implement"],
  ["release-lineage-tooling", "P41", "implement"], ["release-finalization-tooling", "P42", "implement"],
  ["release-dry-run-verification", "P43", "verify · spec"],
  ["release-readiness-review", "P44", "verify · review"], ["public-governance", "P45", "gate"],
  ["public-artifact-generation", "P46", "implement"], ["rc-source-freeze", "P47", "deliver"],
  ["public-release-verification", "P48", "verify · review"], ["v1-rc-delivery", "P49", "deliver"],
  ["external-conformance-review", "P50", "gate"], ["v1-freeze-overlay-review", "P51", "verify · review"],
  ["v1-release-approval", "P52", "gate"], ["v1-delivery", "P53", "deliver"],
];
const releaseProblems = [];
for (let index = 0; index < releaseSequence.length; index += 1) {
  const [owner, id, type] = releaseSequence[index];
  const phase = phaseByOwner.get(owner);
  if (phase?.id !== id || phase?.metadata.Type !== type) releaseProblems.push(owner + " mapping");
  if (index && !(dependencyMap.get(id) || []).includes(releaseSequence[index - 1][1])) releaseProblems.push(id + " dependency");
}
const releaseNeedles = [
  ["P39", ["P05 Go proxy/bootstrap", "P27 Python", "P28 npm", "P37 Container", "不重新实现"]],
  ["P40", ["root→timestamp→snapshot→targets", "TRUST_ROOT", "protected/pinned root", "Sigstore", "dry-run key", "不自动生成"]],
  ["P41", ["隔离 checkout", "CI provenance", "current inputs", "A 到 B"]],
  ["P42", ["unsigned canonical candidate", "parent=A", "release_approver", "bridged_reports"]],
  ["P43", ["只读", "不得在 verify 阶段补代码", "private/dev snapshot"]],
  ["P44", ["private code-complete", "不伪造外部配置/伙伴证据", "P49 v1 RC"]],
  ["P45", ["pinned trust root", "两名 Maintainer", "任意 CLI registry", "rollback/freeze"]],
  ["P46", ["RC source tree", "不创建 commit/tag", "nested `go.mod` 精确 root RC dependency"]],
  ["P47", ["clean RC source commit A", "expected tree digest", "不创 tag/package"]],
  ["P48", ["clean commit A", "只读", "不创建 commit/tag", "tracked tree 前后保持同一 digest"]],
  ["P49", ["reference/control-plane/v1.0.0-rc.N", "create-only", "annotated tag-object", "incident-blocked", "同一 A/digest", "绝不重标旧 A"]],
  ["P50", ["exact A commit/tree", "Schema Bundle", "Runner", "Artifact digests", "distinct `principal_id`"]],
  ["P51", ["unsigned canonical final overlay", "expected B tree", "parent=A", "不包含自签 attestation"]],
  ["P52", ["release_approver", "Sigstore", "expected B tree", "phase-policy digest", "bridged_reports", "dry-run key"]],
  ["P53", ["metadata-only commit B", "parent=A", "expected-previous CAS", "同一 B/同一 digest", "incident-blocked", "Conformance/SBOM/provenance"]],
];
for (const [id, needles] of releaseNeedles) if (!needles.every((needle) => phaseByID.get(id)?.body.includes(needle))) releaseProblems.push(id + " invariants");
record("release-a-b-chain", releaseProblems.length === 0,
  releaseProblems.length === 0 ? "P39-P53 enforces tool ownership, clean A, public RC, external evidence, signed overlay and exact B" : releaseProblems.join("; "));

record("cross-commit-report-policy",
  blueprint.includes("历史普通 success 报告") && blueprint.includes("隔离 checkout") &&
  blueprint.includes("受信 CI/OIDC/Sigstore provenance") && blueprint.includes("current-input") &&
  blueprint.includes("P03/P04 的外部规划签署是独立类型") &&
  blueprint.includes("混合 commit 聚合不要求所有报告来自同一 commit") &&
  blueprint.includes("P53 使用 commit A 报告") && blueprint.includes("P52 attestation 是唯一 A→B bridge") &&
  decisions.includes("P03/P04 历史规划签署与机器报告分类") &&
  decisions.includes("混合 commit 聚合") && decisions.includes("P52 专用外部签名 equivalence attestation"),
  "ordinary reports, planning signatures, mixed commits and A-to-B bridge are distinct");
record("first-public-v1-rc",
  plan.includes("取消独立公共 v0.1") && plan.includes("P44 前所有产物都是 private/dev snapshot") &&
  blueprint.includes("P44 及之前所有制品只允许 private/dev snapshot") && blueprint.includes("首个公开候选是 P49") &&
  readme.includes("P44 及之前均为 private/dev snapshot") && readme.includes("首个公开候选是 P49") &&
  publicAdoption.includes("P44 及之前所有验证制品仅是 private/dev snapshot"),
  "P44 and earlier are private/dev and first public candidate is P49 v1 RC");

const publicV01Scan = await findPublicV01Violations();
record("controlled-v01-language", publicV01Scan.violations.length === 0,
  publicV01Scan.violations.length === 0 ? String(publicV01Scan.files.length) + " controlled files scanned" : publicV01Scan.violations.join("; "));
const publicV01NegativeProbe = publicV01ViolationsInText(
  "Release plan: publish public v0.1 before v1.0.0-rc.1.",
  "negative-probe.md",
);
record("controlled-v01-negative-probe", publicV01NegativeProbe.length === 1,
  publicV01NegativeProbe.length === 1
    ? "synthetic positive public-v0.1 milestone is rejected"
    : "positive public-v0.1 probe escaped detection");

const implementedMakeTargets = new Set(
  [...makefile.matchAll(/^([a-z0-9]+(?:-[a-z0-9]+)*):(?:\s|$)/gm)].map((match) => "make-" + match[1]));
const makeProblems = [];
for (const phaseID of ["P01", "P02", "P03", "P04"]) {
  for (const test of acceptanceByPhase.get(phaseID) || []) if (!implementedMakeTargets.has(test)) makeProblems.push(phaseID + " missing " + test);
}
record("current-make-targets", makeProblems.length === 0,
  makeProblems.length === 0 ? "P01-P04 Make targets are implemented" : makeProblems.join("; "));

const requirementProblems = [];
for (const requirement of requirements.requirements || []) {
  const allowedTests = new Set((requirement.phases || []).flatMap((phaseID) => acceptanceByPhase.get(phaseID) || []));
  for (const phaseID of requirement.phases || []) {
    if (!validPhaseIDs.has(phaseID)) requirementProblems.push(requirement.id + " unknown " + phaseID);
    if (!(acceptanceByPhase.get(phaseID) || []).some((test) => requirement.tests?.includes(test))) requirementProblems.push(requirement.id + " lacks " + phaseID + " acceptance");
  }
  for (const test of requirement.tests || []) if (!allowedTests.has(test)) requirementProblems.push(requirement.id + " non-phase test " + test);
  for (const id of requirement.artifacts || []) if (!artifactByID.has(id)) requirementProblems.push(requirement.id + " unknown artifact " + id);
}
record("requirement-traceability", requirements.requirements?.length === 14 && requirementProblems.length === 0,
  requirementProblems.length === 0 ? "IR-01..IR-14 map to valid artifacts/phases/tests" : requirementProblems.join("; "));

record("console-isolation",
  blueprint.includes("kinglucky-agent-console") && blueprint.includes("不在本实施范围内") &&
  blueprint.includes("P01–P53") && architecture.includes("P01–P53") &&
  plan.includes("本仓库 P01–P53 不修改 `kinglucky-agent-console`") && readme.includes("不依赖 Console") &&
  agents.includes("kinglucky-agent-console") && !layout.includes("kinglucky-agent-console/"),
  "Console is a downstream consumer outside P01-P53");
const semanticNeedles = [
  "Authoring strict", "Consumer forward compatible", "Offline closure", "原始事件是不可变 append-only",
  "语义一致”而非“SQL 一致", "P16 明确复用 P14 ledger/watermark",
  "P07 交付可复用的三语言 pipeline", "预期子报告 ID/数量", "release_approver", "Sigstore identity",
];
const missingSemantics = semanticNeedles.filter((needle) => !(blueprint + "\n" + sdk).includes(needle));
record("blueprint-cross-cutting-rules", missingSemantics.length === 0,
  missingSemantics.length === 0 ? "contract/storage/codegen/report/signing rules explicit" : "missing " + missingSemantics.join(", "));

const markdownFiles = await walkFiles(repositoryRoot, (filePath) => filePath.endsWith(".md"));
const markdownProblems = [];
const linkPattern = /\[[^\]]+\]\(([^)]+)\)/g;
for (const markdownFile of markdownFiles) {
  const markdown = await readFile(markdownFile, "utf8");
  for (const match of markdown.matchAll(linkPattern)) {
    let target = match[1].trim().replace(/^<|>$/g, "");
    target = target.split(/\s+["']/u, 1)[0];
    const localTarget = target.split("#", 1)[0].split("?", 1)[0];
    if (!localTarget || /^(?:https?:|mailto:)/i.test(localTarget)) continue;
    try { await stat(path.resolve(path.dirname(markdownFile), decodeURIComponent(localTarget))); }
    catch { markdownProblems.push(rel(markdownFile) + " -> " + target); }
  }
}
record("markdown-local-link-closure", markdownProblems.length === 0,
  markdownProblems.length === 0 ? String(markdownFiles.length) + " Markdown files checked" : markdownProblems.join(", "));

await writeCheckReport({
  reportDirectory,
  suiteName: "arop-blueprint-check",
  className: "arop.blueprint",
  command: actualCommand("scripts/blueprint-check.mjs"),
  checkerPath: "scripts/blueprint-check.mjs",
  inputPaths: [
    "README.md", "AGENTS.md", "Makefile", "docs/ARCHITECTURE.md", "docs/DECISIONS.md",
    "docs/DEVELOPMENT_PLAN.md", "docs/DIRECTORY_STRUCTURE.md", "docs/IMPLEMENTATION_BLUEPRINT.md",
    "docs/PUBLIC_PROJECT_AND_ADOPTION.md", "docs/SDK_AND_DX.md",
    ...publicV01Scan.files.map(rel), "spec/artifact-manifest.yaml", "spec/requirements.yaml",
    "spec/schemas/artifact-manifest.schema.json", "scripts/blueprint-check.mjs",
    "scripts/verify-report.mjs", "scripts/test-report-verifier.mjs", "scripts/lib/report.mjs",
    "scripts/lib/repository.mjs", "spec/schemas/check-report.schema.json",
  ],
  checks,
  errors,
  summary: {
    phases: phases.length,
    artifacts: artifacts.length,
    checks: checks.length,
    failures: errors.length,
    implement_or_refactor_phases: phases.filter((phase) => ["implement", "refactor"].includes(phase.metadata.Type)).length,
    verify_phases: phases.filter((phase) => phase.metadata.Type?.startsWith("verify ·")).length,
    gates: phases.filter((phase) => phase.metadata.Type === "gate").length,
    deliveries: phases.filter((phase) => phase.metadata.Type === "deliver").length,
  },
  auditNote:
    "Machine blueprint checks enforce declared structure and negative probes; they do not replace the independent P03 planning audit or external release gates.",
});
if (errors.length) {
  console.error("AROP blueprint check failed with " + errors.length + " issue(s):");
  for (const error of errors) console.error("- " + error);
  process.exit(1);
}
console.log("AROP blueprint check passed: " + phases.length + " phases, " + artifacts.length + " artifacts, " + checks.length + " checks.");
