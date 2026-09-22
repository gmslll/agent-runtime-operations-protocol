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
const artifactByID = new Map((manifest.artifacts ?? []).map((artifact) => [artifact.id, artifact]));

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
  "Components",
  "Artifacts owned",
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
const componentWhitelist = new Set([
  "governance", "planning", "repository", "protocol", "codegen", "control-plane", "operations", "storage",
  "identity", "publication", "assets", "registry", "sdk-go", "run", "dispatch", "events", "delivery",
  "streaming", "worker", "sdk-python", "sdk-typescript", "interop", "conformance", "fault-ha", "quickstart",
  "deployment", "release",
]);
const metadataProblems = [];
const implementationOwners = new Set();
const plannedArtifactOwners = new Map();
for (const phase of phases) {
  for (const key of metadataKeys) {
    if (!phase.metadata[key]) metadataProblems.push(`${phase.id} missing ${key}`);
  }
  if (!allowedTypes.has(phase.metadata.Type)) metadataProblems.push(`${phase.id} illegal type ${phase.metadata.Type}`);
  const acceptance = phase.metadata["Machine acceptance"] ?? "";
  const acceptanceTests = [...acceptance.matchAll(/\bmake ([a-z0-9-]+)/g)].map((match) => `make-${match[1]}`);
  if (!new RegExp(`build/reports/${phase.id}/report\\.json`).test(acceptance) || !acceptance.includes("junit.xml")) {
    metadataProblems.push(`${phase.id} must report to build/reports/${phase.id}/{report.json,junit.xml}`);
  }
  if (acceptanceTests.length !== 1) metadataProblems.push(`${phase.id} must declare exactly one Make acceptance command`);

  const components = (phase.metadata.Components ?? "").split(",").map((value) => value.trim()).filter(Boolean);
  const invalidComponents = components.filter((component) => !componentWhitelist.has(component));
  if (components.length === 0 || invalidComponents.length > 0) {
    metadataProblems.push(`${phase.id} invalid Components: ${invalidComponents.join(", ") || "empty"}`);
  }
  const owned = phase.metadata["Artifacts owned"] === "none"
    ? []
    : (phase.metadata["Artifacts owned"] ?? "").split(",").map((value) => value.trim()).filter(Boolean);
  if (["implement", "refactor"].includes(phase.metadata.Type)) {
    const owner = phase.metadata["Capability owner"];
    if (implementationOwners.has(owner)) metadataProblems.push(`${phase.id} reuses implement/refactor owner ${owner}`);
    implementationOwners.add(owner);
    if (owned.length === 0) metadataProblems.push(`${phase.id} implement/refactor owns no structured artifact`);
  }
  for (const artifactID of owned) {
    const prior = plannedArtifactOwners.get(artifactID);
    if (prior) metadataProblems.push(`${artifactID} is owned by both ${prior} and ${phase.id}`);
    plannedArtifactOwners.set(artifactID, phase.id);
    const artifact = artifactByID.get(artifactID);
    if (!artifact) {
      metadataProblems.push(`${phase.id} owns unknown artifact ${artifactID}`);
      continue;
    }
    if (artifact.owner !== phase.metadata["Capability owner"] || artifact.owner_phase !== phase.id) {
      metadataProblems.push(`${artifactID} owner/owner_phase does not match ${phase.id}/${phase.metadata["Capability owner"]}`);
    }
    if (artifact.acceptance_test !== acceptanceTests[0] || !artifact.exposure) {
      metadataProblems.push(`${artifactID} lacks matching acceptance_test/exposure for ${phase.id}`);
    }
  }
}
record(
  "phase-components-artifact-ownership-and-reports",
  metadataProblems.length === 0,
  metadataProblems.length === 0
    ? "all phases use legal types, controlled components, unique cataloged artifact ownership and canonical reports"
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
  "identity-secret-foundation",
  "publication-contracts",
  "publication-service",
  "asset-broker",
  "registry-core",
  "registry-api-sdk",
  "registry-recovery",
  "registry-verification",
  "run-service",
  "dispatch-security",
  "event-ledger",
  "go-provider-delivery",
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
  "sqlite-quickstart",
  "production-deployment",
  "resilience-verification",
  "release-engineering",
  "release-dry-run-verification",
  "release-readiness-review",
  "public-governance",
  "public-artifact-generation",
  "public-release-verification",
  "v1-rc-delivery",
  "external-conformance-review",
  "v1-freeze-overlay-review",
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
  ownerProblems.length === 0 && phaseByOwner.get("v1-delivery") === phases.at(-1),
  ownerProblems.length === 0 ? `${orderedOwners.length} required capability owners are present in lifecycle order` : ownerProblems.join("; "),
);

function phaseHas(owner, needles) {
  const phase = phaseByOwner.get(owner);
  return phase && needles.every((needle) => phase.body.includes(needle));
}
const firstPathChecks = [
  ["control-plane-platform", ["Clock/ID/Fault", "Audit/Trace"]],
  ["control-plane-storage", ["readiness", "dirty", "backup/restore"]],
  ["identity-secret-foundation", ["Credential", "SecretRef", "不实现 URL"]],
  ["publication-service", ["静态 URL", "离线 `$ref`"]],
  ["asset-broker", ["连接时 DNS/IP", "redirect"]],
  ["run-service", ["Outbox", "Cancel", "Deadline", "Usage", "Audit", "Trace", "effect_id"]],
  ["event-ledger", ["append-only", "Inbox", "终态不可逆", "capacity 释放同事务"]],
  ["go-provider-delivery", ["provider durable Inbox/Outbox", "effect_id", "Cancel/Deadline/Usage/Trace", "crash-before/after-effect", "DNS/IP 重检"]],
  ["worker-service", ["claim 与 Attempt lease 原子", "Inbox/Outbox/effect"]],
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

const migrationOwners = [
  "control-plane-storage", "publication-service", "asset-broker", "registry-core", "registry-recovery",
  "run-service", "dispatch-security", "event-ledger", "go-provider-delivery", "worker-service",
];
const migrationProblems = migrationOwners
  .filter((owner) => !phaseHas(owner, ["SQLite/PostgreSQL", "empty", "N-1→N", "idempotent", "dirty"]))
  .map((owner) => owner);
record(
  "persistence-first-path-migration-submatrices",
  migrationProblems.length === 0,
  migrationProblems.length === 0
    ? "every persistence-owning phase carries dual-database migrations and its own empty/N-1/idempotent/dirty submatrix"
    : `missing migration submatrix in: ${migrationProblems.join(", ")}`,
);

record(
  "bounded-bootstrap-quickstart-deployment-release-scopes",
  phaseHas("repository-layout", ["本地 Go module proxy/bootstrap", "GOWORK=off", "root pseudo-version"]) &&
    phaseHas("sqlite-quickstart", ["只编排", "禁止在本阶段新写四套示例"]) &&
    phaseHas("production-deployment", ["Container build primitive", "不实现语言包打包"]) &&
    phaseHas("release-engineering", ["P05 Go proxy", "P27 Python", "P28 npm", "P37 Container", "不重新实现"]),
  "P05 owns bootstrap, P36 only composes examples, P37 owns deployment/container primitive and P39 only orchestrates prior primitives",
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
    p03.body.includes("IR-01–IR-14") &&
    p03.body.includes("must_fix_count=0") &&
    p03.body.includes("summary_sha256") &&
    p03.body.includes("TRUSTED_KEYS") &&
    p03.body.includes("make planning-audit") &&
    p04?.metadata.Type === "gate" &&
    p04?.metadata.Status === "blocked" &&
    p04.body.includes("summary_sha256") &&
    p04.body.includes("make gate-check") &&
    (dependencyMap.get(p04.id) ?? []).includes(p03.id),
  "P01/P02 are completed implementation work; P03 is pending external review with zero must-fix; P04 is blocked on the explicit user gate",
);

const releaseToolchain = phaseByOwner.get("release-engineering");
const releaseDryRun = phaseByOwner.get("release-dry-run-verification");
record(
  "release-toolchain-before-read-only-dry-run",
  releaseToolchain?.metadata.Type === "implement" &&
    releaseToolchain.body.includes("P05 Go proxy") &&
    releaseToolchain.body.includes("P27 Python") &&
    releaseToolchain.body.includes("P28 npm") &&
    releaseToolchain.body.includes("P37 Container") &&
    releaseToolchain.body.includes("validator/aggregator/freeze/delivery") &&
    releaseToolchain.body.includes("SBOM/provenance") &&
    releaseDryRun?.metadata.Type === "verify · spec" &&
    releaseDryRun.body.includes("只读") &&
    releaseDryRun.body.includes("不得在 verify 阶段补代码") &&
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
const implementedMakeTargets = new Set(
  [...makefile.matchAll(/^([a-z0-9]+(?:-[a-z0-9]+)*):(?:\s|$)/gm)].map((match) => `make-${match[1]}`),
);
const currentPhaseIDs = new Set(["P01", "P02", "P03", "P04"]);
const currentTargetProblems = [];
for (const phaseID of currentPhaseIDs) {
  for (const testID of acceptanceTestsByPhase.get(phaseID) ?? []) {
    if (!implementedMakeTargets.has(testID)) currentTargetProblems.push(`${phaseID} missing implemented ${testID}`);
  }
}
const plannedFutureTargets = new Set(
  phases
    .filter((phase) => !currentPhaseIDs.has(phase.id))
    .flatMap((phase) => acceptanceTestsByPhase.get(phase.id) ?? [])
    .filter((testID) => !implementedMakeTargets.has(testID)),
);
record(
  "implemented-vs-planned-make-targets",
  currentTargetProblems.length === 0,
  currentTargetProblems.length === 0
    ? `${implementedMakeTargets.size} Make targets currently exist; P01-P04 acceptance targets are implemented; ${plannedFutureTargets.size} unique future acceptance targets remain planned`
    : currentTargetProblems.join("; "),
);
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
    ? "IR-01..IR-14 map uniquely to existing phases and declared acceptance target names; future targets are planning references, not claimed as implemented"
    : requirementProblems.join("; "),
);

const contractArtifacts = [
  ["openapi-control-plane", "P11", "public-contract", ["publication", "jwks-discovery", "event-session-exchange", "asset-token-exchange"]],
  ["openapi-agent-runtime", "P21", "public-contract", ["agent-runtime-direct", "agent-runtime-proxy"]],
  ["openapi-registry-runtime", "P15", "public-contract", ["runtime-registration", "lease-keepalive"]],
  ["openapi-discovery-runtime", "P15", "public-contract", ["discovery-snapshot", "discovery-watch"]],
  ["openapi-worker-runtime", "P24", "public-contract", ["worker-claim", "attempt-lease"]],
  ["reference-secret-exchange", "P10", "reference-only", ["secret-ref-resolution"]],
  ["asset-broker-service", "P13", "reference-only", ["asset-storage", "asset-token-exchange", "redirect-dns-ip-recheck"]],
];
const contractProblems = [];
for (const [id, phase, exposure, capabilities] of contractArtifacts) {
  const artifact = artifactByID.get(id);
  if (!artifact) {
    contractProblems.push(`missing ${id}`);
    continue;
  }
  if (artifact.phase !== phase || artifact.owner_phase !== phase || artifact.exposure !== exposure || !artifact.owner || !(artifact.tests?.length > 0)) {
    contractProblems.push(`${id} lacks owner/phase/owner_phase/tests/exposure ownership`);
  }
  const ownerPhase = phases.find((candidate) => candidate.id === phase);
  if (artifact.owner !== ownerPhase?.metadata["Capability owner"]) {
    contractProblems.push(`${id} owner ${artifact.owner} does not match ${phase} owner`);
  }
  if (!(acceptanceTestsByPhase.get(phase) ?? []).some((testID) => artifact.tests?.includes(testID))) {
    contractProblems.push(`${id} tests do not include the Make acceptance of ${phase}`);
  }
  if (!(acceptanceTestsByPhase.get(phase) ?? []).includes(artifact.acceptance_test)) {
    contractProblems.push(`${id} acceptance_test does not match ${phase}`);
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

const plannedExecutableKinds = new Set([
  "generated-code", "sdk", "go-reference-implementation", "reference-port-adapter", "database-migrations",
  "reference-implementations", "deployment", "adapters", "go-cli", "tooling", "release-tooling", "go-package",
  "go-service-component", "sdk-component", "reference-agent", "build-primitive", "adapter", "conformance-driver",
  "release-validator", "release-aggregator",
]);
const executableArtifactProblems = [];
for (const artifact of manifest.artifacts ?? []) {
  if (artifact.status !== "planned" || !plannedExecutableKinds.has(artifact.kind)) continue;
  if (!artifact.owner || !artifact.phase || !artifact.owner_phase || !artifact.tests?.length || !artifact.acceptance_test || !artifact.exposure) {
    executableArtifactProblems.push(`${artifact.id} lacks owner/phase/tests/owner_phase/acceptance_test/exposure`);
    continue;
  }
  if (artifact.phase !== artifact.owner_phase || !artifact.tests.includes(artifact.acceptance_test)) {
    executableArtifactProblems.push(`${artifact.id} phase/tests disagree with owner_phase/acceptance_test`);
  }
  const ownerPhase = phases.find((phase) => phase.id === artifact.owner_phase);
  if (ownerPhase?.metadata["Capability owner"] !== artifact.owner) {
    executableArtifactProblems.push(`${artifact.id} owner does not match ${artifact.owner_phase}`);
  }
  if (!(acceptanceTestsByPhase.get(artifact.owner_phase) ?? []).includes(artifact.acceptance_test)) {
    executableArtifactProblems.push(`${artifact.id} acceptance_test does not match ${artifact.owner_phase}`);
  }
}
record(
  "planned-executable-artifact-metadata",
  executableArtifactProblems.length === 0,
  executableArtifactProblems.length === 0
    ? "all planned executable artifacts have matching owner_phase, acceptance_test and exposure metadata"
    : executableArtifactProblems.join("; "),
);

const releaseArtifactExpectations = [
  ["release-engineering-toolchain", "P39", "release-engineering", "make-build-release-toolchain"],
  ["external-config-evidence-schema", "P39", "release-engineering", "make-build-release-toolchain"],
  ["external-config-validator", "P39", "release-engineering", "make-build-release-toolchain"],
  ["public-release-aggregator", "P39", "release-engineering", "make-build-release-toolchain"],
  ["external-conformance-evidence-schema", "P39", "release-engineering", "make-build-release-toolchain"],
  ["external-conformance-validator", "P39", "release-engineering", "make-build-release-toolchain"],
  ["freeze-overlay-checker", "P39", "release-engineering", "make-build-release-toolchain"],
  ["final-delivery-checker", "P39", "release-engineering", "make-build-release-toolchain"],
  ["release-report-integration", "P39", "release-engineering", "make-build-release-toolchain"],
  ["release-toolchain-reports", "P39", "release-engineering", "make-build-release-toolchain"],
  ["release-dry-run-reports", "P40", "release-dry-run-verification", "make-release-dry-run"],
];
const releaseArtifactProblems = [];
for (const [id, phase, owner, test] of releaseArtifactExpectations) {
  const artifact = artifactByID.get(id);
  if (!artifact) {
    releaseArtifactProblems.push(`missing ${id}`);
    continue;
  }
  if (artifact.phase !== phase || artifact.owner_phase !== phase || artifact.owner !== owner || artifact.acceptance_test !== test || !artifact.tests?.includes(test)) {
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
  "Read-only public namespace verification",
  "Deliver v1 release candidate from commit A",
  "Independent implementation and partner evidence gate",
  "Read-only freeze and deterministic final overlay verification",
  "Deliver exact verified final release commit B",
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
if (v1RC && !["commit A", "nested `go.mod`", "root `v1.0.0-rc.N`"].every((needle) => v1RC.body.includes(needle))) {
  releaseProblems.push("v1 RC stage does not bind nested root RC dependency to commit A");
}
if (evidenceGate && !["commit A", "Schema Bundle", "Runner", "Artifact digest"].every((needle) => evidenceGate.body.includes(needle))) {
  releaseProblems.push("external evidence lacks exact source/schema/runner/artifact binding");
}
if (freeze && !(freeze.metadata.Type === "verify · review" && ["临时树", "commit B tree digest", "payload equivalence", "A→B equivalence attestation", "不关闭 RFC", "不修 Matrix"].every((needle) => freeze.body.includes(needle)))) {
  releaseProblems.push("freeze lacks deterministic final overlay/tree digest and A-to-B attestation");
}
if (v1Deliver && !["release-metadata-only commit B", "B tree digest", "root final→proxy 可解析→nested final", "Conformance/SBOM/provenance"].every((needle) => v1Deliver.body.includes(needle))) {
  releaseProblems.push("v1 delivery lacks exact approved commit B, root-before-nested order or final verification");
}
record(
  "v1-rc-evidence-freeze-delivery-chain",
  releaseProblems.length === 0,
  releaseProblems.length === 0
    ? `${configGate.id}->${regenerate.id}->${finalVerify.id}->${v1RC.id}(A)->${evidenceGate.id}->${freeze.id}(overlay/tree)->${v1Deliver.id}(B) enforces the signed two-commit equivalence chain`
    : releaseProblems.join("; "),
);

const releaseNeedles = [
  "真实 `go.work` 默认不提交",
  "临时 Go proxy",
  "root pseudo-version",
  "GOWORK=off",
  "commit A",
  "commit B",
  "final overlay",
  "root final→proxy 可解析→nested final",
  "A→B equivalence attestation",
  "payload equivalence",
  "SBOM",
  "provenance",
];
const missingReleaseRules = releaseNeedles.filter((needle) => !`${layout}\n${blueprint}`.includes(needle));
record(
  "two-module-release-procedure",
  missingReleaseRules.length === 0 && !blueprint.includes("正式 v1 使用同一 source commit"),
  missingReleaseRules.length === 0 ? "bootstrap proxy and the reviewed commit-A to metadata-only commit-B release chain are explicit" : `missing: ${missingReleaseRules.join(", ")}`,
);

record(
  "first-public-release-is-v1-rc",
  plan.includes("取消独立公共 v0.1") &&
    blueprint.includes("首个公开候选是 P45") &&
    readme.includes("取消独立公共 v0.1") &&
    !`${plan}\n${blueprint}`.includes("## 8.2 公共 v0.1"),
  "P41 and earlier remain private/dev snapshots; the first public candidate is v1.0.0-rc.N",
);

const auditArtifacts = [
  "planning-audit-evidence-schema",
  "user-gate-evidence-schema",
  "planning-audit-validation",
  "planning-audit-reports",
  "planning-audit-canonical-summary",
  "user-gate-validation",
  "user-gate-reports",
  "user-gate-canonical-summary",
  "check-report-meta-schema",
  "machine-report-verifier",
  "evidence-validation-library",
];
const missingAuditArtifacts = auditArtifacts.filter((id) => !artifactByID.has(id));
const expectedAuditPaths = new Map([
  ["planning-audit-evidence-schema", "spec/schemas/planning-audit-evidence.schema.json"],
  ["user-gate-evidence-schema", "spec/schemas/user-gate-evidence.schema.json"],
  ["planning-audit-validation", "scripts/planning-audit.mjs"],
  ["planning-audit-reports", "build/reports/P03"],
  ["planning-audit-canonical-summary", "spec/evidence/P03-planning-audit-summary.json"],
  ["user-gate-validation", "scripts/gate-check.mjs"],
  ["user-gate-reports", "build/reports/P04"],
  ["user-gate-canonical-summary", "spec/evidence/P04-user-gate-summary.json"],
  ["check-report-meta-schema", "spec/schemas/check-report.schema.json"],
  ["machine-report-verifier", "scripts/verify-report.mjs"],
  ["evidence-validation-library", "scripts/lib/evidence.mjs"],
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
    architecture.includes("不在本仓库 P01–P48 实施范围") &&
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
  "可复用的完整三语言 pipeline",
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
  "scripts/verify-report.mjs",
  "scripts/lib/report.mjs",
  "scripts/lib/repository.mjs",
  "spec/schemas/check-report.schema.json",
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
    implemented_make_targets: implementedMakeTargets.size,
    planned_future_make_targets: plannedFutureTargets.size,
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
