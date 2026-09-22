#!/usr/bin/env node

import { readFile, stat } from "node:fs/promises";
import path from "node:path";

import Ajv2020 from "ajv/dist/2020.js";
import canonicalize from "canonicalize";
import { createHash } from "node:crypto";

import {
  loadStructuredFile,
  findPublicV01Violations,
  parseJSONWithUniqueKeys,
  publicV01ViolationsInText,
  repositoryRoot,
  walkFiles,
} from "./lib/repository.mjs";
import { actualCommand, writeCheckReport } from "./lib/report.mjs";

const reportDirectory = "build/reports/P01";
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

function formatAjvErrors(validationErrors = []) {
  return validationErrors
    .map((error) => `${error.instancePath || "/"} ${error.message}`)
    .join("; ");
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

const planningSchemaBindings = [
  {
    schema: "spec/schemas/artifact-manifest.schema.json",
    document: "spec/artifact-manifest.yaml",
    value: artifactManifest,
  },
  {
    schema: "spec/schemas/requirements.schema.json",
    document: "spec/requirements.yaml",
    value: requirementsDocument,
  },
  {
    schema: "spec/schemas/conflicts.schema.json",
    document: "spec/conflicts.yaml",
    value: conflictsDocument,
  },
];
const planningSchemaProblems = [];
const planningAjv = new Ajv2020({ allErrors: true, strict: true });
for (const binding of planningSchemaBindings) {
  try {
    const schema = await loadStructuredFile(path.join(repositoryRoot, binding.schema));
    if (!planningAjv.validateSchema(schema)) {
      planningSchemaProblems.push(
        `${binding.schema}: ${formatAjvErrors(planningAjv.errors)}`,
      );
      continue;
    }
    const validate = planningAjv.compile(schema);
    if (!validate(binding.value)) {
      planningSchemaProblems.push(
        `${binding.document}: ${formatAjvErrors(validate.errors)}`,
      );
    }
  } catch (error) {
    planningSchemaProblems.push(`${binding.schema}: ${error.message}`);
  }
}
record(
  "planning-metadata-json-schema",
  planningSchemaProblems.length === 0,
  planningSchemaProblems.length === 0
    ? "artifact, requirement and conflict metadata pass executable JSON Schemas"
    : planningSchemaProblems.join("; "),
);

const planningMetaSchemaFiles = await walkFiles(
  path.join(repositoryRoot, "spec/schemas"),
  (filePath) => filePath.endsWith(".schema.json"),
);
const planningMetaSchemaProblems = [];
for (const schemaFile of planningMetaSchemaFiles) {
  try {
    const schema = await loadStructuredFile(schemaFile);
    if (!planningAjv.validateSchema(schema)) {
      planningMetaSchemaProblems.push(
        `${relative(schemaFile)}: ${formatAjvErrors(planningAjv.errors)}`,
      );
    }
  } catch (error) {
    planningMetaSchemaProblems.push(`${relative(schemaFile)}: ${error.message}`);
  }
}
record(
  "planning-meta-schema-validity",
  planningMetaSchemaProblems.length === 0,
  planningMetaSchemaProblems.length === 0
    ? `${planningMetaSchemaFiles.length} planning/evidence schemas are valid Draft 2020-12 schemas`
    : planningMetaSchemaProblems.join("; "),
);

const yamlFiles = (
  await walkFiles(repositoryRoot, (filePath) =>
    [".yaml", ".yml"].includes(path.extname(filePath)),
  )
).filter((filePath) => !relative(filePath).startsWith("build/"));
const yamlProblems = [];
for (const yamlFile of yamlFiles) {
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

const jsonFiles = (
  await walkFiles(repositoryRoot, (filePath) => filePath.endsWith(".json"))
).filter((filePath) => !relative(filePath).startsWith("build/"));
const jsonProblems = [];
for (const jsonFile of jsonFiles) {
  try {
    await loadStructuredFile(jsonFile);
  } catch (error) {
    jsonProblems.push(`${relative(jsonFile)}: ${error.message}`);
  }
}
record(
  "json-syntax-and-unique-keys",
  jsonProblems.length === 0,
  jsonProblems.length === 0
    ? `${jsonFiles.length} JSON files parse without duplicate object keys`
    : jsonProblems.join("; "),
);

let duplicateKeyProbePassed = false;
try {
  parseJSONWithUniqueKeys('{"outer":{"same":1,"same":2}}', "duplicate-key-probe");
} catch (error) {
  duplicateKeyProbePassed = /duplicate object key/u.test(error.message);
}
record(
  "json-duplicate-key-negative-probe",
  duplicateKeyProbePassed,
  duplicateKeyProbePassed
    ? "the shared JSON loader rejects nested duplicate keys before JSON.parse"
    : "the shared JSON loader accepted a duplicate key or returned the wrong failure",
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

const textFiles = (
  await walkFiles(repositoryRoot, (filePath) =>
    [".md", ".yaml", ".yml"].includes(path.extname(filePath)),
  )
).filter((filePath) => !relative(filePath).startsWith("build/"));
const unknownDecisionReferences = [];
for (const filePath of textFiles) {
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
const artifactPaths = new Map();
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
  if (artifactPaths.has(artifact.path)) {
    artifactProblems.push(
      `${artifact.id} duplicates path ${artifact.path} already owned by ${artifactPaths.get(artifact.path)}`,
    );
  } else {
    artifactPaths.set(artifact.path, artifact.id);
  }
}
for (const artifact of artifacts) {
  for (const parent of [...(artifact.derives_from ?? []), ...(artifact.runtime_inputs ?? [])]) {
    if (!artifactIDs.has(parent)) {
      artifactProblems.push(`${artifact.id} references unknown artifact: ${parent}`);
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
const artifactByID = new Map(artifacts.map((artifact) => [artifact.id, artifact]));
const lifecycleFields = ["owner_phase", "completion_phase", "producer_phase"];
const phaseNumber = (phaseID) => Number(phaseID?.slice(1));
const availability = (artifact) => {
  const field = lifecycleFields.find((candidate) => artifact?.[candidate]);
  return field ? phaseNumber(artifact[field]) : 0;
};
for (const artifact of artifacts) {
  const lifecycle = lifecycleFields.filter((field) => artifact[field]);
  if (artifact.status === "planned" && lifecycle.length !== 1) {
    artifactProblems.push(`${artifact.id} planned lifecycle must use exactly one of owner/completion/producer phase`);
  }
  if (artifact.owner_phase && artifact.path_role !== "concrete") {
    artifactProblems.push(`${artifact.id} source owner requires path_role=concrete`);
  }
  if (artifact.completion_phase && !["aggregate", "container"].includes(artifact.path_role)) {
    artifactProblems.push(`${artifact.id} completion phase requires aggregate/container path_role`);
  }
  if (artifact.producer_phase && !["machine-reports", "canonical-evidence-summary", "detached-evidence-summary", "detached-evidence-bundle"].includes(artifact.kind)) {
    artifactProblems.push(`${artifact.id} producer phase is reserved for reports/evidence outputs`);
  }
  for (const dependencyID of artifact.derives_from ?? []) {
    const dependency = artifactByID.get(dependencyID);
    if (dependency && availability(dependency) > availability(artifact)) {
      artifactProblems.push(
        `${artifact.id}@${availability(artifact)} depends on future ${dependencyID}@${availability(dependency)}`,
      );
    }
  }
}
const artifactVisiting = new Set();
const artifactVisited = new Set();
const artifactStack = [];
function visitArtifact(artifactID) {
  if (artifactVisiting.has(artifactID)) {
    const cycleStart = artifactStack.indexOf(artifactID);
    artifactProblems.push(
      `artifact dependency cycle: ${[...artifactStack.slice(cycleStart), artifactID].join(" -> ")}`,
    );
    return;
  }
  if (artifactVisited.has(artifactID)) return;
  artifactVisiting.add(artifactID);
  artifactStack.push(artifactID);
  for (const dependency of artifactByID.get(artifactID)?.derives_from ?? []) {
    if (artifactByID.has(dependency)) visitArtifact(dependency);
  }
  artifactStack.pop();
  artifactVisiting.delete(artifactID);
  artifactVisited.add(artifactID);
}
for (const artifactID of artifactIDs) visitArtifact(artifactID);

const languageGeneratedDependencies = new Map([
  ["sdk-go", "generated-models"],
  ["sdk-python", "generated-python-models"],
  ["sdk-typescript", "generated-typescript-models"],
]);
const generatedArtifactIDs = new Set([...languageGeneratedDependencies.values()]);
for (const [sdkID, expectedGeneratedID] of languageGeneratedDependencies) {
  const sdkArtifact = artifactByID.get(sdkID);
  const expectedGeneratedArtifact = artifactByID.get(expectedGeneratedID);
  if (!sdkArtifact || !expectedGeneratedArtifact) {
    artifactProblems.push(`${sdkID} or ${expectedGeneratedID} is missing`);
    continue;
  }
  if (!sdkArtifact.derives_from.includes(expectedGeneratedID)) {
    artifactProblems.push(`${sdkID} must derive from ${expectedGeneratedID}`);
  }
  const wrongGenerated = sdkArtifact.derives_from.filter(
    (dependency) => generatedArtifactIDs.has(dependency) && dependency !== expectedGeneratedID,
  );
  if (wrongGenerated.length > 0) {
    artifactProblems.push(`${sdkID} derives from wrong-language generated artifacts: ${wrongGenerated.join(", ")}`);
  }
  if (!sdkArtifact.language || sdkArtifact.language !== expectedGeneratedArtifact.language) {
    artifactProblems.push(
      `${sdkID} language ${sdkArtifact.language} does not match ${expectedGeneratedID} language ${expectedGeneratedArtifact.language}`,
    );
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
for (const [index, authority] of (artifactManifest?.authority_chain ?? []).entries()) {
  if (authority.rank !== index + 1) {
    artifactProblems.push(`${authority.role} has non-contiguous authority rank ${authority.rank}`);
  }
  for (const sourceID of authority.sources ?? []) {
    if (!artifactIDs.has(sourceID)) {
      artifactProblems.push(`${authority.role} references unknown authority source ${sourceID}`);
    }
  }
}
record(
  "artifact-catalog",
  artifactProblems.length === 0,
  artifactProblems.length === 0
    ? `${artifacts.length} unique paths; authority chain, status, DAG and language derivations valid`
    : artifactProblems.join("; "),
);

const requirementProblems = [];
const requirements = requirementsDocument?.requirements ?? [];
const expectedRequirementIDs = Array.from(
  { length: 14 },
  (_, index) => `IR-${String(index + 1).padStart(2, "0")}`,
);
const expectedRequirementStatements = [
  "先把全量实现规划落到仓库权威文档；之后实现必须按核心文档推进。",
  "协议仓保持厂商中立，不依赖 Console、飞书、cc-connect 或具体 Agent 框架；不得修改 `kinglucky-agent-console`。",
  "后端采用 Go：Reference Control Plane、Registry、Dispatcher、Run/Event Ledger、服务端 Conformance/故障驱动；Node 仅用于 Schema/TS 工具，Python 做 Provider SDK，TypeScript 做 Consumer SDK。",
  "覆盖发布、注册、发现、鉴权、Run/Attempt、Direct/Proxy/Worker Pull、结构化流式、结果、usage、审计、trace、drain/升级全生命周期。",
  "所有新调用先经 Control Plane 鉴权并创建 Run；可信服务拿 Dispatch Ticket 后可 Direct；浏览器 v1 走 BFF/Proxy。",
  "自研 Registry，借鉴 Lease/Revision/Watch/CAS/Fencing，不依赖 etcd/Nacos。",
  "交付语义为 at-least-once，包含幂等、durable inbox/outbox、`effect_id`、终态不可变、final Snapshot。",
  "SDK 放协议仓；Quickstart/Conformance 不依赖 Console 可独立运行。",
  "Core/Profile/Extension 分层；兼容 A2A/MCP/ARD/CloudEvents/OpenTelemetry。",
  "SQLite Quickstart 与 PostgreSQL production reference 语义一致。",
  "最终目录必须清晰分离手写源码、生成代码、公共 SDK、Go 内部后端、迁移、参考实现、conformance、部署与 CI。",
  "对外发布受 domain/package ownership、双 maintainer/security 入口等外部条件门控。",
  "全部验收机器可测，覆盖跨语言、故障注入、HA、replay/out-of-order/cancel/fencing。",
  "不得把外部合作伙伴/独立实现等外部条件伪造为完成。",
];
const approvedRequirementsDigest = "sha256:ae16611e5286beaf050f505dd603f3bc761a96bf03d4c145a27d14d69c89d8fd";
const actualRequirementIDs = requirements.map((requirement) => requirement.id);
if (JSON.stringify(actualRequirementIDs) !== JSON.stringify(expectedRequirementIDs)) {
  requirementProblems.push(
    `requirements must be exactly ${expectedRequirementIDs.join(", ")}`,
  );
}
for (const [index, requirement] of requirements.entries()) {
  if (requirement.statement_original_zh !== expectedRequirementStatements[index]) {
    requirementProblems.push(`${requirement.id} immutable statement changed`);
  }
  if (typeof requirement.translation_en !== "string" || requirement.translation_en.length === 0) {
    requirementProblems.push(`${requirement.id} lacks non-authoritative translation_en`);
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
const canonicalRequirementStatements = canonicalize(
  requirements.map(({ id, statement_original_zh }) => ({ id, statement_original_zh })),
);
const computedRequirementsDigest = `sha256:${createHash("sha256").update(canonicalRequirementStatements).digest("hex")}`;
if (
  requirementsDocument?.statements_digest_algorithm !== "sha256-jcs" ||
  requirementsDocument?.statements_digest !== computedRequirementsDigest ||
  computedRequirementsDigest !== approvedRequirementsDigest
) {
  requirementProblems.push(
    `approved Chinese requirement-set digest mismatch: file=${requirementsDocument?.statements_digest}, computed=${computedRequirementsDigest}, approved=${approvedRequirementsDigest}`,
  );
}
record(
  "immutable-requirements",
  requirementProblems.length === 0,
  requirementProblems.length === 0
    ? `14 original Chinese immutable requirements and approved JCS set digest ${approvedRequirementsDigest} map to artifacts, phases, and tests`
    : requirementProblems.join("; "),
);
record(
  "independent-audit-boundary",
  true,
  "Hard-coded immutable statements detect accidental drift; they do not replace the independent P03 planning audit.",
);

const conflictProblems = [];
const conflicts = conflictsDocument?.conflicts ?? [];
const conflictIDs = new Set();
const verificationCatalog = conflictsDocument?.verification_catalog ?? {};
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
  } else {
    for (const verificationID of conflict.verification) {
      if (!Object.hasOwn(verificationCatalog, verificationID)) {
        conflictProblems.push(`${conflict.id} references unknown verification: ${verificationID}`);
      }
    }
  }
}
for (const [verificationID, verification] of Object.entries(verificationCatalog)) {
  if (!artifactIDs.has(verification.owner_artifact)) {
    conflictProblems.push(
      `${verificationID} owner artifact does not exist: ${verification.owner_artifact}`,
    );
  }
  if (!["present", "planned"].includes(verification.status)) {
    conflictProblems.push(`${verificationID} has invalid status: ${verification.status}`);
  }
}
const implementedVerificationIDs = new Set([
  "canonical-event-batch-binding-check",
  "control-plane-command-binding-check",
  "direct-runtime-command-binding-check",
  "duplicate-key-rejected-test",
  "legacy-event-sink-negative-check",
  "no-cancel-alias-check",
]);
for (const verificationID of implementedVerificationIDs) {
  if (verificationCatalog[verificationID]?.status !== "present") {
    conflictProblems.push(`${verificationID} must be registered as present`);
  }
}
for (const [verificationID, verification] of Object.entries(verificationCatalog)) {
  if (verification.status === "present" && !implementedVerificationIDs.has(verificationID)) {
    conflictProblems.push(`${verificationID} is marked present without an implemented planning check`);
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
    ? `${conflicts.length} conflicts resolved against ${Object.keys(verificationCatalog).length} registered verification IDs; zero unresolved`
    : conflictProblems.join("; "),
);

const protocolSource = await readFile(
  path.join(repositoryRoot, "docs/PROTOCOL_SPECIFICATION.md"),
  "utf8",
);
const runStreamingSource = await readFile(
  path.join(repositoryRoot, "docs/RUN_AND_STREAMING.md"),
  "utf8",
);
const controlledTextRoots = ["docs", "spec", "schemas", "openapi", "asyncapi"];
const controlledTextFiles = [];
for (const controlledRoot of controlledTextRoots) {
  try {
    controlledTextFiles.push(...(await walkFiles(
      path.join(repositoryRoot, controlledRoot),
      (filePath) => /\.(?:md|ya?ml|json)$/iu.test(filePath),
    )));
  } catch {
    // A planned directory may not exist before its implementation phase.
  }
}
const canonicalCorpus = (
  await Promise.all([...new Set(controlledTextFiles)].map((filePath) => readFile(filePath, "utf8")))
).join("\n");
const legacyBindingFindings = (source) => ({
  legacyEventSink:
    /"event_sink"\s*:\s*"[^"\n]*\/v1\/agent-runs\/[^"\n]*\/events(?!:batch)/u.test(source),
  cancelURLField: /\bcancel_url\b/u.test(source),
  cancelHTTPAlias:
    /\b(?:GET|POST|PUT|PATCH|DELETE)\s+\/v1\/(?:agent-)?runs\/\{?[^\s}/]+\}?\/cancel\b/u.test(source),
});
const actualLegacyBindings = legacyBindingFindings(canonicalCorpus);
const canonicalBindingProblems = [];
if (
  !protocolSource.includes(
    '"event_batch_url": "https://control.example/v1/agent-runs/run_01/events:batch"',
  )
) {
  canonicalBindingProblems.push("RunRequest Delivery lacks canonical event_batch_url");
}
if (
  !runStreamingSource.includes(
    '"event_batch_url": "https://control.example/v1/agent-runs/run_01/events:batch"',
  )
) {
  canonicalBindingProblems.push("Event Session response lacks canonical event_batch_url");
}
for (const binding of [
  "POST /v1/agent-runs/{run_id}/events:batch",
  "POST /v1/agent-runs/{run_id}/commands",
  "POST /v1/runs/{run_id}/commands",
]) {
  if (!canonicalCorpus.includes(binding)) canonicalBindingProblems.push(`missing ${binding}`);
}
for (const [finding, found] of Object.entries(actualLegacyBindings)) {
  if (found) canonicalBindingProblems.push(`legacy binding detected: ${finding}`);
}
record(
  "canonical-http-bindings",
  canonicalBindingProblems.length === 0,
  canonicalBindingProblems.length === 0
    ? "event_batch_url and both Command trust-boundary bindings are canonical; legacy event sink and cancel aliases are absent"
    : canonicalBindingProblems.join("; "),
);

const negativeLegacyBindings = legacyBindingFindings(`
{"event_sink":"https://control.invalid/v1/agent-runs/run_1/events"}
{"cancel_url":"/v1/runs/run_1/cancel"}
POST /v1/runs/{run_id}/cancel
`);
record(
  "canonical-binding-negative-probes",
  Object.values(negativeLegacyBindings).every(Boolean),
  Object.values(negativeLegacyBindings).every(Boolean)
    ? "the checker rejects legacy event_sink, cancel_url and HTTP /cancel aliases"
    : `negative probe escaped detection: ${JSON.stringify(negativeLegacyBindings)}`,
);

const publicV01Scan = await findPublicV01Violations();
record(
  "controlled-publication-v01-language",
  publicV01Scan.violations.length === 0,
  publicV01Scan.violations.length === 0
    ? `${publicV01Scan.files.length} controlled publication documents contain no positive public-v0.1 milestone; VERSION/package dev versions and example AgentVersions are outside this policy scan`
    : publicV01Scan.violations.join("; "),
);
const publicV01NegativeProbe = publicV01ViolationsInText(
  "Milestone: publish public v0.1 before the v1 release candidate.",
  "negative-probe.md",
);
record(
  "controlled-publication-v01-negative-probe",
  publicV01NegativeProbe.length === 1,
  publicV01NegativeProbe.length === 1
    ? "the scanner rejects a synthetic positive public-v0.1 milestone"
    : `positive public-v0.1 probe escaped detection: ${JSON.stringify(publicV01NegativeProbe)}`,
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
    const schema = await loadStructuredFile(schemaFile);
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
          targetSchema = await loadStructuredFile(targetPath);
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

const reportInputPaths = [
  ...textFiles.map(relative),
  ...jsonFiles.map(relative),
  "scripts/spec-index-check.mjs",
  "scripts/verify-report.mjs",
  "scripts/test-report-verifier.mjs",
  "scripts/lib/report.mjs",
  "scripts/lib/repository.mjs",
  "spec/schemas/check-report.schema.json",
];
await writeCheckReport({
  reportDirectory,
  suiteName: "arop-spec-index-check",
  className: "arop.spec-index",
  command: actualCommand("scripts/spec-index-check.mjs"),
  checkerPath: "scripts/spec-index-check.mjs",
  inputPaths: reportInputPaths,
  checks,
  errors,
  summary: {
    artifacts: artifacts.length,
    immutable_requirements: requirements.length,
    conflicts: conflicts.length,
    registered_verifications: Object.keys(verificationCatalog).length,
    unresolved_conflicts: conflicts.filter((conflict) => conflict.status !== "resolved")
      .length,
    schemas: schemaFiles.length,
    planning_meta_schemas: planningMetaSchemaFiles.length,
    yaml_files: yamlFiles.length,
    json_files: jsonFiles.length,
    markdown_files: markdownFiles.length,
  },
  auditNote:
    "Hard-coded statements and self-check probes detect repository drift; they cannot replace the independent P03 planning audit.",
});

if (errors.length > 0) {
  console.error(`AROP spec index check failed with ${errors.length} error(s):`);
  for (const error of errors) console.error(`- ${error}`);
  console.error(`Reports: ${reportDirectory}/report.json and junit.xml`);
  process.exit(1);
}

console.log(
  `AROP spec index check passed: ${artifacts.length} artifacts, ` +
    `${requirements.length} immutable requirements, ${conflicts.length} resolved conflicts, ` +
    `${schemaFiles.length} protocol schemas, ${planningMetaSchemaFiles.length} planning/evidence schemas, ` +
    `${markdownFiles.length} Markdown files.`,
);
console.log(`Reports: ${reportDirectory}/report.json and junit.xml`);
