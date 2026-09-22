#!/usr/bin/env node

import { execFile } from "node:child_process";
import { readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";

import Ajv2020 from "ajv/dist/2020.js";
import addFormats from "ajv-formats";

import { loadStructuredFile, repositoryRoot } from "./lib/repository.mjs";
import { actualCommand, digestFiles, sha256, writeCheckReport } from "./lib/report.mjs";

const execFileAsync = promisify(execFile);
const checks = [];
const errors = [];
const evidenceArgument = process.env.EVIDENCE ?? process.argv[2];
const subjectPaths = [
  "spec/requirements.yaml",
  "docs/DEVELOPMENT_PLAN.md",
  "docs/IMPLEMENTATION_BLUEPRINT.md",
  "spec/artifact-manifest.yaml",
];

function record(name, passed, detail) {
  checks.push({ name, passed, detail });
  if (!passed) errors.push(`${name}: ${detail}`);
}

let evidence;
let evidenceDigest = "unavailable";
let evidencePath;
if (!evidenceArgument) {
  record("external-evidence-argument", false, "EVIDENCE must point to an external planning-audit JSON/YAML document");
} else {
  evidencePath = path.resolve(evidenceArgument);
  const relative = path.relative(repositoryRoot, evidencePath);
  const outsideRepository = relative.startsWith("..") || path.isAbsolute(relative);
  record(
    "external-evidence-location",
    outsideRepository,
    outsideRepository
      ? "raw review evidence is outside the Git repository"
      : "raw review evidence must remain outside the Git repository",
  );
  try {
    const raw = await readFile(evidencePath);
    evidenceDigest = sha256(raw);
    evidence = await loadStructuredFile(evidencePath);
    record("external-evidence-readable", true, `evidence sha256:${evidenceDigest}; raw content is not copied into reports`);
  } catch (error) {
    record("external-evidence-readable", false, error.message);
  }
}

if (evidence) {
  try {
    const schema = await loadStructuredFile(
      path.join(repositoryRoot, "spec/schemas/planning-audit-evidence.schema.json"),
    );
    const ajv = new Ajv2020({ allErrors: true, strict: true });
    addFormats(ajv);
    const validate = ajv.compile(schema);
    const valid = validate(evidence);
    record(
      "planning-audit-evidence-schema",
      valid,
      valid
        ? "independent reviewer, YES coverage, PASS verdict and zero must-fix are structurally attested"
        : (validate.errors ?? []).map((error) => `${error.instancePath || "/"} ${error.message}`).join("; "),
    );
  } catch (error) {
    record("planning-audit-evidence-schema", false, error.message);
  }
}

let head = "unavailable";
try {
  head = (await execFileAsync("git", ["rev-parse", "HEAD"], { cwd: repositoryRoot })).stdout.trim();
} catch (error) {
  record("subject-commit", false, error.message);
}

try {
  const digests = await digestFiles(subjectPaths);
  const byPath = new Map(digests.files.map((file) => [file.path, file.sha256]));
  const planText = await readFile(path.join(repositoryRoot, "docs/DEVELOPMENT_PLAN.md"), "utf8");
  const planPhases = [...planText.matchAll(/^## (P\d{2}) —/gm)].map((match) => match[1]);
  const expected = {
    commit: head,
    plan_last_phase: planPhases.at(-1),
    requirements_sha256: byPath.get("spec/requirements.yaml"),
    plan_sha256: byPath.get("docs/DEVELOPMENT_PLAN.md"),
    blueprint_sha256: byPath.get("docs/IMPLEMENTATION_BLUEPRINT.md"),
    artifact_manifest_sha256: byPath.get("spec/artifact-manifest.yaml"),
  };
  const mismatches = Object.entries(expected)
    .filter(([key, value]) => evidence?.subject?.[key] !== value)
    .map(([key, value]) => `${key} expected ${value}, got ${evidence?.subject?.[key]}`);
  record(
    "audit-subject-binding",
    mismatches.length === 0,
    mismatches.length === 0
      ? "subject commit and requirements/plan/blueprint/artifact-manifest digests match current inputs"
      : mismatches.join("; "),
  );
} catch (error) {
  record("audit-subject-binding", false, error.message);
}

record(
  "independent-pass-verdict",
  evidence?.reviewer?.independent === true &&
    evidence?.result?.requirements_phase_coverage === "YES" &&
    evidence?.result?.must_fix_count === 0 &&
    evidence?.result?.verdict === "PASS",
  "reviewer must be independent, requirements/phase coverage YES, must_fix_count=0 and verdict PASS",
);

await writeCheckReport({
  reportDirectory: "build/reports/P03",
  suiteName: "arop-planning-audit",
  className: "arop.planning-audit",
  command: actualCommand("scripts/planning-audit.mjs"),
  checkerPath: "scripts/planning-audit.mjs",
  inputPaths: [
    ...subjectPaths,
    "spec/schemas/planning-audit-evidence.schema.json",
    "scripts/planning-audit.mjs",
    "scripts/lib/report.mjs",
    "scripts/lib/repository.mjs",
  ],
  checks,
  errors,
  summary: { evidence_sha256: evidenceDigest, raw_evidence_committed: false },
  auditNote: "Raw independent-review evidence remains outside Git; this report stores only validation results and its digest.",
});

if (errors.length === 0) {
  const sanitizedSummary = {
    schema_version: 1,
    kind: "arop-planning-audit-summary",
    subject: evidence.subject,
    reviewer_id: evidence.reviewer.id,
    result: evidence.result,
    attested_at: evidence.attested_at,
    signed_summary: evidence.signed_summary,
    raw_evidence_sha256: `sha256:${evidenceDigest}`,
  };
  await writeFile(
    path.join(repositoryRoot, "build/reports/P03/attestation-summary.json"),
    `${JSON.stringify(sanitizedSummary, null, 2)}\n`,
    "utf8",
  );
}

if (errors.length > 0) {
  console.error(`AROP planning audit validation failed with ${errors.length} error(s).`);
  process.exit(1);
}
console.log("AROP planning audit evidence passed; sanitized report and attestation candidate are in build/reports/P03/.");
