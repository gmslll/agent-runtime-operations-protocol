#!/usr/bin/env node

import { mkdir, readFile, stat, writeFile } from "node:fs/promises";
import path from "node:path";

import { loadStructuredFile, repositoryRoot, walkFiles } from "./lib/repository.mjs";

const reportDirectory = path.join(repositoryRoot, "build/reports/blueprint-check");
const checks = [];
const errors = [];

function record(name, passed, detail) {
  checks.push({ name, passed, detail });
  if (!passed) errors.push(`${name}: ${detail}`);
}

function xmlEscape(value) {
  return String(value)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&apos;");
}

function relative(filePath) {
  return path.relative(repositoryRoot, filePath).split(path.sep).join("/");
}

async function source(relativePath) {
  return readFile(path.join(repositoryRoot, relativePath), "utf8");
}

const [layout, blueprint, plan, readme, agents, architecture, sdk, requirements, manifest] =
  await Promise.all([
    source("docs/DIRECTORY_STRUCTURE.md"),
    source("docs/IMPLEMENTATION_BLUEPRINT.md"),
    source("docs/DEVELOPMENT_PLAN.md"),
    source("README.md"),
    source("AGENTS.md"),
    source("docs/ARCHITECTURE.md"),
    source("docs/SDK_AND_DX.md"),
    loadStructuredFile(path.join(repositoryRoot, "spec/requirements.yaml")),
    loadStructuredFile(path.join(repositoryRoot, "spec/artifact-manifest.yaml")),
  ]);

const targetTreeNeedles = [
  "<!-- blueprint-target-tree:v1 -->",
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
const missingTreeDeclarations = targetTreeNeedles.filter((needle) => !layout.includes(needle));
record(
  "target-tree-declaration",
  missingTreeDeclarations.length === 0,
  missingTreeDeclarations.length === 0
    ? `${targetTreeNeedles.length} required target-tree boundaries declared`
    : `missing: ${missingTreeDeclarations.join(", ")}`,
);

const moduleDeclarations = [...layout.matchAll(/<!--\s*blueprint-module:\s*([^>]+?)\s*-->/g)].map(
  (match) => match[1].trim(),
);
const expectedModules = ["go.mod", "reference/control-plane/go.mod"];
record(
  "two-go-modules",
  JSON.stringify(moduleDeclarations) === JSON.stringify(expectedModules) &&
    layout.includes("不建第三个 `go.mod`") &&
    layout.includes("conformance/` 只包含语言中立"),
  JSON.stringify(moduleDeclarations) === JSON.stringify(expectedModules)
    ? "root public module and the sole nested control-plane module; conformance remains language-neutral"
    : `module declarations must be exactly ${expectedModules.join(", ")}; got ${moduleDeclarations.join(", ")}`,
);

record(
  "portable-runner-boundary",
  layout.includes("cmd/arop-conformance") &&
    layout.includes("portable runner") &&
    blueprint.includes("cmd/arop-conformance") &&
    blueprint.includes("不依赖 Reference `internal`"),
  "portable conformance runner is declared in the root module and independent of reference internals",
);

const forbiddenAdapterPaths = ["cc-connect-adapter", "codex-adapter", "feishu-adapter"];
const foundForbiddenAdapters = forbiddenAdapterPaths.filter((value) =>
  layout.toLowerCase().includes(value),
);
record(
  "no-vendor-adapter",
  foundForbiddenAdapters.length === 0 && layout.includes("无厂商专属适配"),
  foundForbiddenAdapters.length === 0
    ? "target layout uses generic HTTP, Worker Pull, A2A, MCP and ARD boundaries"
    : `forbidden adapter paths: ${foundForbiddenAdapters.join(", ")}`,
);

const releaseNeedles = [
  "先推送根 `vX.Y.Z`",
  "再推送 `reference/control-plane/vX.Y.Z`",
  "两个 tag 必须指向同一 commit",
  "GOWORK=off",
  "无永久 `replace`",
  "临时本地 Go proxy",
  "SBOM",
  "provenance",
];
const missingReleaseRules = releaseNeedles.filter((needle) => !blueprint.includes(needle));
record(
  "module-tag-release-order",
  missingReleaseRules.length === 0,
  missingReleaseRules.length === 0
    ? "root tag, proxy resolution, nested tag, reproducibility, SBOM and provenance order is frozen"
    : `missing release rules: ${missingReleaseRules.join(", ")}`,
);

const phasePattern = /^## (P\d{2}) — ([^\n]+)\n([\s\S]*?)(?=^## P\d{2} — |^# 4\.|(?![\s\S]))/gm;
const phases = [];
for (const match of plan.matchAll(phasePattern)) {
  const body = match[3];
  const metadata = {};
  for (const key of [
    "Type",
    "Status",
    "Goal",
    "Scope",
    "Dependencies",
    "Built-in invariants",
    "Machine acceptance",
    "Rollback point",
    "Definition of done",
  ]) {
    metadata[key] = body.match(new RegExp(`^- \\*\\*${key}:\\*\\* (.+)$`, "m"))?.[1];
  }
  phases.push({ id: match[1], title: match[2].trim(), body, metadata });
}

const expectedPhaseIDs = Array.from(
  { length: 24 },
  (_, index) => `P${String(index + 1).padStart(2, "0")}`,
);
record(
  "phase-sequence",
  JSON.stringify(phases.map((phase) => phase.id)) === JSON.stringify(expectedPhaseIDs),
  `expected P01-P24 once and in order; found ${phases.map((phase) => phase.id).join(", ")}`,
);

const phaseIDs = new Set(phases.map((phase) => phase.id));
const dependencyMap = new Map();
const dependencyProblems = [];
for (const phase of phases) {
  const raw = phase.metadata.Dependencies;
  if (!raw) {
    dependencyProblems.push(`${phase.id} has no Dependencies metadata`);
    continue;
  }
  const dependencies = raw === "none" ? [] : [...raw.matchAll(/P\d{2}/g)].map((match) => match[0]);
  if (raw !== "none" && dependencies.length === 0) {
    dependencyProblems.push(`${phase.id} has unparseable dependencies: ${raw}`);
  }
  for (const dependency of dependencies) {
    if (!phaseIDs.has(dependency)) dependencyProblems.push(`${phase.id} references unknown ${dependency}`);
    if (dependency >= phase.id) dependencyProblems.push(`${phase.id} has non-prior dependency ${dependency}`);
  }
  dependencyMap.set(phase.id, dependencies);
}

const visiting = new Set();
const visited = new Set();
function visit(phaseID) {
  if (visiting.has(phaseID)) {
    dependencyProblems.push(`dependency cycle reaches ${phaseID}`);
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
  dependencyProblems.length === 0
    ? "all phase dependencies resolve to prior phases and the graph is acyclic"
    : dependencyProblems.join("; "),
);

const missingMetadata = [];
for (const phase of phases) {
  for (const [key, value] of Object.entries(phase.metadata)) {
    if (!value) missingMetadata.push(`${phase.id}:${key}`);
  }
  const acceptance = phase.metadata["Machine acceptance"] ?? "";
  if (!acceptance.includes("report.json") || !acceptance.includes("junit.xml")) {
    missingMetadata.push(`${phase.id}:JSON/JUnit report paths`);
  }
  if (!/build\/reports\/(?:P\d{2}|[a-z0-9-]+)\/report\.json/.test(acceptance)) {
    missingMetadata.push(`${phase.id}:canonical build/reports path`);
  }
}
record(
  "phase-machine-acceptance",
  missingMetadata.length === 0 && blueprint.includes("不允许只用「进程退出 0」"),
  missingMetadata.length === 0
    ? "every phase declares type, status, goal, scope, dependencies, invariants, JSON/JUnit acceptance, rollback and done"
    : `missing or invalid: ${missingMetadata.join(", ")}`,
);

const p04 = phases.find((phase) => phase.id === "P04");
record(
  "P04-user-gate",
  p04?.metadata.Type === "user-gate" &&
    p04?.metadata.Status === "blocked-user-evidence" &&
    p04.body.includes("用户") &&
    p04.body.includes("明确确认") &&
    p04.body.includes("P05 必须保持未开始"),
  "P04 requires explicit user evidence before P05 physical refactor",
);

function hasDependency(phaseID, dependency) {
  return (dependencyMap.get(phaseID) ?? []).includes(dependency);
}
record(
  "public-rc-gate-order",
  hasDependency("P20", "P19") &&
    hasDependency("P21", "P20") &&
    blueprint.includes("P19 external configuration evidence gate") &&
    blueprint.includes("P20 regenerate every public-namespace artifact and rerun all checks") &&
    blueprint.includes("P21 v0.1 release candidate"),
  "P19 external configuration gates P20 full regeneration, which gates P21 RC",
);

const p22 = phases.find((phase) => phase.id === "P22")?.body ?? "";
const p22Needles = [
  "exact commit SHA",
  "Schema Bundle Digest",
  "Runner Digest",
  "被测制品 Digest",
  "必须回 P20",
  "产生新 RC",
  "重跑 P22",
];
const missingP22 = p22Needles.filter((needle) => !p22.includes(needle));
record(
  "external-evidence-binding",
  hasDependency("P22", "P21") && missingP22.length === 0,
  missingP22.length === 0
    ? "P22 evidence binds exact source/schema/runner/artifact digests and specification changes restart RC evidence"
    : `P22 missing: ${missingP22.join(", ")}`,
);

const validPhaseIDs = new Set(expectedPhaseIDs);
const requirementProblems = [];
const acceptanceTestsByPhase = new Map();
for (const phase of phases) {
  const commands = [
    ...(phase.metadata["Machine acceptance"] ?? "").matchAll(/\bmake ([a-z0-9-]+)/g),
  ].map((match) => `make-${match[1]}`);
  acceptanceTestsByPhase.set(phase.id, commands);
}
const expectedRequirementIDs = Array.from(
  { length: 14 },
  (_, index) => `IR-${String(index + 1).padStart(2, "0")}`,
);
if (
  JSON.stringify((requirements.requirements ?? []).map((requirement) => requirement.id)) !==
  JSON.stringify(expectedRequirementIDs)
) {
  requirementProblems.push("requirements must be exactly IR-01 through IR-14");
}
for (const requirement of requirements.requirements ?? []) {
  const mappedTests = new Set(
    (requirement.phases ?? []).flatMap((phaseID) => acceptanceTestsByPhase.get(phaseID) ?? []),
  );
  for (const phaseID of requirement.phases ?? []) {
    if (!validPhaseIDs.has(phaseID)) requirementProblems.push(`${requirement.id} maps invalid phase ${phaseID}`);
    const phaseTests = acceptanceTestsByPhase.get(phaseID) ?? [];
    if (!phaseTests.some((testID) => (requirement.tests ?? []).includes(testID))) {
      requirementProblems.push(`${requirement.id} has no machine acceptance mapped for ${phaseID}`);
    }
  }
  if (!(requirement.phases?.length > 0)) requirementProblems.push(`${requirement.id} has no phase mapping`);
  for (const testID of requirement.tests ?? []) {
    if (!mappedTests.has(testID)) {
      requirementProblems.push(`${requirement.id} test ${testID} is not an acceptance command of its phases`);
    }
  }
}
record(
  "immutable-requirement-phase-map",
  requirementProblems.length === 0,
  requirementProblems.length === 0
    ? "IR-01 through IR-14 map only to valid P01-P24 phases and their exact acceptance commands"
    : requirementProblems.join("; "),
);

const artifactByID = new Map((manifest.artifacts ?? []).map((artifact) => [artifact.id, artifact]));
const expectedArtifacts = new Map([
  ["implementation-blueprint", "docs/IMPLEMENTATION_BLUEPRINT.md"],
  ["repository-layout", "docs/DIRECTORY_STRUCTURE.md"],
  ["development-plan", "docs/DEVELOPMENT_PLAN.md"],
  ["blueprint-validation", "scripts/blueprint-check.mjs"],
  ["blueprint-check-reports", "build/reports/blueprint-check"],
  ["portable-conformance-runner", "cmd/arop-conformance"],
  ["sqlite-migrations", "reference/control-plane/migrations/sqlite"],
  ["postgres-migrations", "reference/control-plane/migrations/postgres"],
  ["quickstart", "deployments/quickstart"],
  ["production-reference-deployment", "deployments/production-reference"],
]);
const artifactProblems = [];
for (const [id, expectedPath] of expectedArtifacts) {
  const artifact = artifactByID.get(id);
  if (!artifact) artifactProblems.push(`missing ${id}`);
  else if (artifact.path !== expectedPath) artifactProblems.push(`${id} path is ${artifact.path}`);
}
record(
  "blueprint-artifact-catalog",
  artifactProblems.length === 0,
  artifactProblems.length === 0
    ? `${expectedArtifacts.size} Phase-2 and target-layout artifacts use frozen paths`
    : artifactProblems.join("; "),
);

const isolationCorpus = `${layout}\n${blueprint}\n${architecture}`;
record(
  "console-isolation",
  isolationCorpus.includes("`kinglucky-agent-console` 不在本实施范围内") &&
    isolationCorpus.includes("本计划不修改它") &&
    isolationCorpus.includes("不在本仓库 P01–P24 实施范围") &&
    readme.includes("不依赖 Console") &&
    agents.includes("不得依赖 `kinglucky-agent-console`") &&
    !layout.includes("kinglucky-agent-console/"),
  "Console is a downstream consumer, not a module, runtime dependency or implementation target",
);

const semanticNeedles = [
  "Authoring strict",
  "Consumer forward compatible",
  "Offline closure",
  "原始事件是不可变 append-only",
  "SQLite 是 Quickstart",
  "语义一致”而非“SQL 一致",
  "Audit、Trace、Cancel、Deadline、Usage、`effect_id`、Inbox/Outbox",
  "Clock/ID/Fault seam",
  "全量 codegen 前必须先执行代表 Schema spike",
];
const semanticCorpus = `${blueprint}\n${sdk}`;
const missingSemantics = semanticNeedles.filter((needle) => !semanticCorpus.includes(needle));
record(
  "cross-cutting-invariants",
  missingSemantics.length === 0,
  missingSemantics.length === 0
    ? "authoring/consumer, offline refs, immutable events, dual storage, reliability seams and codegen spike are frozen"
    : `missing: ${missingSemantics.join(", ")}`,
);

const markdownFiles = await walkFiles(repositoryRoot, (filePath) => filePath.endsWith(".md"));
const markdownProblems = [];
const markdownLinkPattern = /\[[^\]]+\]\(([^)]+)\)/g;
for (const markdownFile of markdownFiles) {
  const markdown = await readFile(markdownFile, "utf8");
  for (const match of markdown.matchAll(markdownLinkPattern)) {
    let target = match[1].trim().replace(/^<|>$/g, "");
    target = target.split(/\s+["']/u, 1)[0];
    const withoutFragment = target.split("#", 1)[0].split("?", 1)[0];
    if (!withoutFragment || /^(?:https?:|mailto:)/i.test(withoutFragment)) continue;
    try {
      await stat(path.resolve(path.dirname(markdownFile), decodeURIComponent(withoutFragment)));
    } catch {
      markdownProblems.push(`${relative(markdownFile)} -> ${target}`);
    }
  }
}
record(
  "markdown-local-link-closure",
  markdownProblems.length === 0,
  markdownProblems.length === 0
    ? `${markdownFiles.length} Markdown files have closed local links`
    : `broken links: ${markdownProblems.join(", ")}`,
);

const report = {
  schema_version: 1,
  generated_at: new Date().toISOString(),
  success: errors.length === 0,
  summary: {
    checks: checks.length,
    passed: checks.filter((check) => check.passed).length,
    failed: checks.filter((check) => !check.passed).length,
    phases: phases.length,
    immutable_requirements: requirements.requirements?.length ?? 0,
    declared_go_modules: moduleDeclarations.length,
    markdown_files: markdownFiles.length,
  },
  checks,
  errors,
};

await mkdir(reportDirectory, { recursive: true });
await writeFile(
  path.join(reportDirectory, "report.json"),
  `${JSON.stringify(report, null, 2)}\n`,
  "utf8",
);

const testCases = checks
  .map((check) => {
    const failure = check.passed
      ? ""
      : `<failure message="${xmlEscape(check.detail)}"/>`;
    return `  <testcase classname="arop.blueprint" name="${xmlEscape(check.name)}">${failure}</testcase>`;
  })
  .join("\n");
const junit = `<?xml version="1.0" encoding="UTF-8"?>
<testsuite name="arop-blueprint-check" tests="${checks.length}" failures="${errors.length}">
${testCases}
</testsuite>
`;
await writeFile(path.join(reportDirectory, "junit.xml"), junit, "utf8");

if (errors.length > 0) {
  console.error(`AROP blueprint check failed with ${errors.length} error(s):`);
  for (const error of errors) console.error(`- ${error}`);
  console.error(`Reports: ${relative(reportDirectory)}/report.json and junit.xml`);
  process.exit(1);
}

console.log(
  `AROP blueprint check passed: ${phases.length} phases, ${checks.length} checks, ` +
    `${requirements.requirements?.length ?? 0} immutable requirements and ${moduleDeclarations.length} Go modules.`,
);
console.log(`Reports: ${relative(reportDirectory)}/report.json and junit.xml`);
