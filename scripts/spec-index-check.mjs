#!/usr/bin/env node

import { mkdir, readFile, stat, writeFile } from "node:fs/promises";
import path from "node:path";

import { loadStructuredFile, repositoryRoot, walkFiles } from "./lib/repository.mjs";

const reportDirectory = path.join(
  repositoryRoot,
  "build/reports/spec-index-check",
);
const errors = [];
const checks = [];

function record(name, passed, detail) {
  checks.push({ name, passed, detail });
  if (!passed) {
    errors.push(`${name}: ${detail}`);
  }
}

function relative(filePath) {
  return path.relative(repositoryRoot, filePath).split(path.sep).join("/");
}

function isSafeRepositoryPath(value) {
  return (
    typeof value === "string" &&
    value.length > 0 &&
    !path.isAbsolute(value) &&
    !value.split(/[\\/]/).includes("..")
  );
}

function xmlEscape(value) {
  return String(value)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&apos;");
}

function resolveJsonPointer(document, fragment) {
  if (!fragment || fragment === "#") {
    return document;
  }
  if (!fragment.startsWith("#/")) {
    throw new Error(`unsupported JSON Schema fragment: ${fragment}`);
  }
  return fragment
    .slice(2)
    .split("/")
    .map((token) => token.replaceAll("~1", "/").replaceAll("~0", "~"))
    .reduce((current, token) => {
      if (
        current === null ||
        typeof current !== "object" ||
        !Object.hasOwn(current, token)
      ) {
        throw new Error(`JSON Pointer does not resolve: ${fragment}`);
      }
      return current[token];
    }, document);
}

function collectRefs(value, output = []) {
  if (Array.isArray(value)) {
    for (const item of value) {
      collectRefs(item, output);
    }
    return output;
  }
  if (value && typeof value === "object") {
    if (typeof value.$ref === "string") {
      output.push(value.$ref);
    }
    for (const item of Object.values(value)) {
      collectRefs(item, output);
    }
  }
  return output;
}

let artifactManifest;
let requirementsDocument;
let conflictsDocument;

for (const specFile of [
  "spec/artifact-manifest.yaml",
  "spec/requirements.yaml",
  "spec/conflicts.yaml",
]) {
  try {
    const parsed = await loadStructuredFile(path.join(repositoryRoot, specFile));
    if (specFile.endsWith("artifact-manifest.yaml")) artifactManifest = parsed;
    if (specFile.endsWith("requirements.yaml")) requirementsDocument = parsed;
    if (specFile.endsWith("conflicts.yaml")) conflictsDocument = parsed;
    record(`yaml:${specFile}`, true, "valid YAML with unique keys");
  } catch (error) {
    record(`yaml:${specFile}`, false, error.message);
  }
}

const yamlFiles = await walkFiles(repositoryRoot, (filePath) =>
  [".yaml", ".yml"].includes(path.extname(filePath)),
);
const yamlProblems = [];
for (const yamlFile of yamlFiles) {
  if (relative(yamlFile).startsWith("build/")) continue;
  try {
    await loadStructuredFile(yamlFile);
  } catch (error) {
    yamlProblems.push(`${relative(yamlFile)}: ${error.message}`);
  }
}
record(
  "yaml-syntax",
  yamlProblems.length === 0,
  yamlProblems.length === 0
    ? `${yamlFiles.length} YAML files parse with unique keys`
    : yamlProblems.join("; "),
);

const decisionSource = await readFile(
  path.join(repositoryRoot, "docs/DECISIONS.md"),
  "utf8",
);
const decisionDefinitions = new Map();
for (const line of decisionSource.split(/\r?\n/)) {
  const match = line.match(/^\|\s*([DPC]-\d{3})\s*\|/);
  if (!match) continue;
  decisionDefinitions.set(
    match[1],
    (decisionDefinitions.get(match[1]) ?? 0) + 1,
  );
}
const duplicateDecisions = [...decisionDefinitions.entries()]
  .filter(([, count]) => count !== 1)
  .map(([id]) => id);
record(
  "decision-definitions",
  decisionDefinitions.size > 0 && duplicateDecisions.length === 0,
  duplicateDecisions.length === 0
    ? `${decisionDefinitions.size} unique Decision/Policy/Configuration IDs`
    : `duplicate definitions: ${duplicateDecisions.join(", ")}`,
);

const textFiles = await walkFiles(repositoryRoot, (filePath) =>
  [".md", ".yaml", ".yml"].includes(path.extname(filePath)),
);
const unknownDecisionReferences = [];
for (const filePath of textFiles) {
  if (relative(filePath).startsWith("build/")) continue;
  const source = await readFile(filePath, "utf8");
  for (const match of source.matchAll(/\b([DPC]-\d{3})\b/g)) {
    if (!decisionDefinitions.has(match[1])) {
      unknownDecisionReferences.push(`${relative(filePath)}:${match[1]}`);
    }
  }
}
record(
  "decision-references",
  unknownDecisionReferences.length === 0,
  unknownDecisionReferences.length === 0
    ? "all Decision/Policy/Configuration references resolve"
    : `unknown references: ${unknownDecisionReferences.join(", ")}`,
);

const artifactIDs = new Set();
const artifactProblems = [];
if (!artifactManifest || artifactManifest.schema_version !== 1) {
  artifactProblems.push("artifact manifest schema_version must be 1");
}
const artifacts = artifactManifest?.artifacts ?? [];
for (const artifact of artifacts) {
  if (!artifact?.id || artifactIDs.has(artifact.id)) {
    artifactProblems.push(`missing or duplicate artifact id: ${artifact?.id}`);
    continue;
  }
  artifactIDs.add(artifact.id);
  if (!isSafeRepositoryPath(artifact.path)) {
    artifactProblems.push(`${artifact.id} has unsafe path: ${artifact.path}`);
  }
  if (!["present", "planned"].includes(artifact.status)) {
    artifactProblems.push(`${artifact.id} has invalid status: ${artifact.status}`);
  }
  if (!artifact.kind || !artifact.authority || !Array.isArray(artifact.derives_from)) {
    artifactProblems.push(`${artifact.id} is missing kind, authority, or derives_from`);
  }
}
for (const artifact of artifacts) {
  for (const parent of artifact.derives_from ?? []) {
    if (!artifactIDs.has(parent)) {
      artifactProblems.push(`${artifact.id} derives from unknown artifact: ${parent}`);
    }
  }
  if (artifact.status === "present" && isSafeRepositoryPath(artifact.path)) {
    try {
      await stat(path.join(repositoryRoot, artifact.path));
    } catch {
      artifactProblems.push(`${artifact.id} is present but path is missing: ${artifact.path}`);
    }
  }
}
const expectedAuthorityRoles = [
  "decision_constraints",
  "behavioral_authority",
  "structural_authority",
  "transport_binding",
  "derived_non_authoritative",
];
const actualAuthorityRoles = (artifactManifest?.authority_chain ?? []).map(
  (entry) => entry.role,
);
if (JSON.stringify(actualAuthorityRoles) !== JSON.stringify(expectedAuthorityRoles)) {
  artifactProblems.push(
    `authority chain must be ${expectedAuthorityRoles.join(" -> ")}`,
  );
}
record(
  "artifact-catalog",
  artifactProblems.length === 0,
  artifactProblems.length === 0
    ? `${artifacts.length} artifacts; authority chain and derivations valid`
    : artifactProblems.join("; "),
);

const requirementProblems = [];
const requirements = requirementsDocument?.requirements ?? [];
const expectedRequirementIDs = Array.from(
  { length: 14 },
  (_, index) => `IR-${String(index + 1).padStart(2, "0")}`,
);
const expectedRequirementStatements = [
  "First plan into authoritative docs; later implementation must follow core docs.",
  "Protocol repo vendor-neutral; no dependency on Console/Feishu/cc-connect/framework.",
  "Do not modify kinglucky-agent-console in this implementation.",
  "Go for Reference Control Plane, Registry, Dispatcher, Run/Event Ledger, server Conformance; Node only schema/TS tooling, Python Provider SDK, TS Consumer.",
  "Full lifecycle: publish, register, discover, auth, Run/Attempt, Direct/Proxy/Worker Pull, structured streaming, result, usage, audit, trace, drain/upgrade.",
  "All new calls pass Control Plane auth/create Run; trusted service can Direct after ticket; browser v1 BFF/Proxy.",
  "Self-built registry inspired by Lease/Revision/Watch/CAS/Fencing, no etcd/Nacos dependency.",
  "At-least-once, idempotency, durable inbox/outbox, effect_id, immutable terminal state, final Snapshot.",
  "SDKs in protocol repo, independent Quickstart/Conformance without Console.",
  "Core/Profile/Extension layers; interop A2A/MCP/ARD/CloudEvents/OTel.",
  "SQLite Quickstart and PostgreSQL production reference have identical semantics.",
  "Final layout clearly separates handwritten sources, generated code, public SDKs, internal Go backend, migrations, reference, conformance, deployments, CI.",
  "Public release externally gated by domain, package ownership, two maintainers/security entry.",
  "Machine-test acceptance including cross-language, fault injection, HA, replay/out-of-order/cancel/fencing.",
];
const actualRequirementIDs = requirements.map((requirement) => requirement.id);
if (JSON.stringify(actualRequirementIDs) !== JSON.stringify(expectedRequirementIDs)) {
  requirementProblems.push(
    `requirements must be exactly ${expectedRequirementIDs.join(", ")}`,
  );
}
if (
  requirementsDocument?.external_evidence_gate !==
  "Never fake external partner/independent implementation conditions as complete."
) {
  requirementProblems.push("external evidence gate is missing or changed");
}
for (const [index, requirement] of requirements.entries()) {
  if (requirement.statement !== expectedRequirementStatements[index]) {
    requirementProblems.push(`${requirement.id} immutable statement changed`);
  }
  for (const artifactID of requirement.artifacts ?? []) {
    if (!artifactIDs.has(artifactID)) {
      requirementProblems.push(`${requirement.id} maps unknown artifact: ${artifactID}`);
    }
  }
  for (const field of ["artifacts", "phases", "tests"]) {
    if (!Array.isArray(requirement[field]) || requirement[field].length === 0) {
      requirementProblems.push(`${requirement.id} must map at least one ${field}`);
    }
  }
}
record(
  "immutable-requirements",
  requirementProblems.length === 0,
  requirementProblems.length === 0
    ? "14 immutable requirements map to artifacts, phases, and tests"
    : requirementProblems.join("; "),
);

const conflictProblems = [];
const conflicts = conflictsDocument?.conflicts ?? [];
const conflictIDs = new Set();
const requiredConflictTitles = new Set([
  "strict-authoring-vs-forward-compatible-consumer",
  "schema-declaration-and-reference-closure",
  "required-extensions-schema-and-digest-binding",
  "optional-governance-vs-managed-publication",
  "digest-must-not-run-before-validation",
  "closed-content-part-vs-extension",
  "traceparent-regex-vs-w3c-semantics",
  "worker-claim-path",
  "event-batch-path",
  "cancel-path-vs-command",
  "session-and-attempt-id-prefixes",
  "cancel-requested-run-state",
  "missing-tool-error-heartbeat-event-types",
  "asset-part-vs-asset-ref",
  "remote-schema-ref-and-ssrf",
  "console-cc-connect-and-framework-neutrality",
]);
for (const conflict of conflicts) {
  if (!conflict.id || conflictIDs.has(conflict.id)) {
    conflictProblems.push(`missing or duplicate conflict id: ${conflict.id}`);
  }
  conflictIDs.add(conflict.id);
  requiredConflictTitles.delete(conflict.title);
  if (conflict.status !== "resolved") {
    conflictProblems.push(`${conflict.id} is unresolved: ${conflict.status}`);
  }
  if (!conflict.implementation_state || !conflict.resolution) {
    conflictProblems.push(`${conflict.id} lacks implementation_state or resolution`);
  }
  for (const decisionID of conflict.decisions ?? []) {
    if (!decisionDefinitions.has(decisionID)) {
      conflictProblems.push(`${conflict.id} references unknown decision: ${decisionID}`);
    }
  }
  for (const artifactID of conflict.repair_locations ?? []) {
    if (!artifactIDs.has(artifactID)) {
      conflictProblems.push(`${conflict.id} repairs unknown artifact: ${artifactID}`);
    }
  }
  if (!Array.isArray(conflict.verification) || conflict.verification.length === 0) {
    conflictProblems.push(`${conflict.id} lacks future verification`);
  }
}
if (requiredConflictTitles.size > 0) {
  conflictProblems.push(
    `required conflicts missing: ${[...requiredConflictTitles].join(", ")}`,
  );
}
record(
  "conflict-ledger",
  conflictProblems.length === 0,
  conflictProblems.length === 0
    ? `${conflicts.length} conflicts resolved; zero unresolved`
    : conflictProblems.join("; "),
);

const markdownFiles = await walkFiles(repositoryRoot, (filePath) =>
  filePath.endsWith(".md"),
);
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
  "markdown-local-links",
  markdownProblems.length === 0,
  markdownProblems.length === 0
    ? `${markdownFiles.length} Markdown files have closed local links`
    : `broken links: ${markdownProblems.join(", ")}`,
);

const schemaFiles = await walkFiles(
  path.join(repositoryRoot, "schemas"),
  (filePath) => filePath.endsWith(".schema.json"),
);
const schemaDocuments = new Map();
const schemaByID = new Map();
const schemaProblems = [];
for (const schemaFile of schemaFiles) {
  try {
    const schema = JSON.parse(await readFile(schemaFile, "utf8"));
    schemaDocuments.set(schemaFile, schema);
    if (typeof schema.$id !== "string") {
      schemaProblems.push(`${relative(schemaFile)} has no $id`);
    } else if (schemaByID.has(schema.$id)) {
      schemaProblems.push(`${relative(schemaFile)} duplicates $id ${schema.$id}`);
    } else {
      schemaByID.set(schema.$id, { schema, filePath: schemaFile });
    }
  } catch (error) {
    schemaProblems.push(`${relative(schemaFile)} is invalid JSON: ${error.message}`);
  }
}
for (const [schemaFile, schema] of schemaDocuments) {
  for (const reference of collectRefs(schema)) {
    try {
      const hashIndex = reference.indexOf("#");
      const base = hashIndex >= 0 ? reference.slice(0, hashIndex) : reference;
      const fragment = hashIndex >= 0 ? reference.slice(hashIndex) : "";
      let targetSchema;
      if (!base) {
        targetSchema = schema;
      } else if (/^https?:/i.test(base)) {
        const registered = schemaByID.get(base);
        if (!registered) {
          throw new Error(`remote or unbundled $ref is forbidden: ${reference}`);
        }
        targetSchema = registered.schema;
      } else {
        const targetPath = path.resolve(path.dirname(schemaFile), base);
        const normalizedRoot = `${repositoryRoot}${path.sep}`;
        if (!targetPath.startsWith(normalizedRoot)) {
          throw new Error(`$ref escapes repository: ${reference}`);
        }
        targetSchema = schemaDocuments.get(targetPath);
        if (!targetSchema) {
          targetSchema = JSON.parse(await readFile(targetPath, "utf8"));
        }
      }
      resolveJsonPointer(targetSchema, fragment);
    } catch (error) {
      schemaProblems.push(`${relative(schemaFile)} $ref ${reference}: ${error.message}`);
    }
  }
}
record(
  "json-schema-local-ref-closure",
  schemaProblems.length === 0,
  schemaProblems.length === 0
    ? `${schemaFiles.length} schemas; every $ref resolves from the local bundle`
    : schemaProblems.join("; "),
);

const report = {
  schema_version: 1,
  generated_at: new Date().toISOString(),
  success: errors.length === 0,
  summary: {
    checks: checks.length,
    passed: checks.filter((check) => check.passed).length,
    failed: checks.filter((check) => !check.passed).length,
    artifacts: artifacts.length,
    immutable_requirements: requirements.length,
    conflicts: conflicts.length,
    unresolved_conflicts: conflicts.filter((conflict) => conflict.status !== "resolved")
      .length,
    schemas: schemaFiles.length,
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
    return `  <testcase classname="arop.spec-index" name="${xmlEscape(check.name)}">${failure}</testcase>`;
  })
  .join("\n");
const junit = `<?xml version="1.0" encoding="UTF-8"?>
<testsuite name="arop-spec-index-check" tests="${checks.length}" failures="${errors.length}">
${testCases}
</testsuite>
`;
await writeFile(path.join(reportDirectory, "junit.xml"), junit, "utf8");

if (errors.length > 0) {
  console.error(`AROP spec index check failed with ${errors.length} error(s):`);
  for (const error of errors) console.error(`- ${error}`);
  console.error(`Reports: ${relative(reportDirectory)}/report.json and junit.xml`);
  process.exit(1);
}

console.log(
  `AROP spec index check passed: ${artifacts.length} artifacts, ` +
    `${requirements.length} immutable requirements, ${conflicts.length} resolved conflicts, ` +
    `${schemaFiles.length} schemas, ${markdownFiles.length} Markdown files.`,
);
console.log(`Reports: ${relative(reportDirectory)}/report.json and junit.xml`);
