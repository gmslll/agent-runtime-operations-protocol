#!/usr/bin/env node

import { execFile } from "node:child_process";
import { mkdtemp, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { promisify } from "node:util";

import Ajv2020 from "ajv/dist/2020.js";

import { PLANNING_INPUTS, verifyEvidenceSubjectCommit } from "./lib/evidence.mjs";
import { sha256 } from "./lib/report.mjs";

const execFileAsync = promisify(execFile);
const results = [];

async function git(root, ...args) {
  return (await execFileAsync("git", args, { cwd: root, encoding: "utf8" })).stdout.trim();
}
async function commit(root, message) {
  await git(root, "add", ".");
  await git(root, "commit", "--quiet", "-m", message);
  return git(root, "rev-parse", "HEAD");
}
async function subject(root, commit) {
  const value = { commit };
  for (const [filePath, field] of PLANNING_INPUTS) {
    value[field] = sha256(await readFile(path.join(root, filePath)));
  }
  return value;
}
async function expect(name, action, pass, needle = "") {
  try {
    await action();
    if (!pass) throw new Error(`${name} unexpectedly passed`);
    results.push(`${name}: PASS`);
  } catch (error) {
    if (pass) throw error;
    if (needle && !error.message.includes(needle)) throw new Error(`${name} failed for wrong reason: ${error.message}`);
    results.push(`${name}: expected FAIL`);
  }
}

const temporaryRoot = await mkdtemp(path.join(os.tmpdir(), "arop-evidence-lineage-"));
try {
  await git(temporaryRoot, "init", "--quiet");
  await git(temporaryRoot, "config", "user.name", "AROP Evidence Test");
  await git(temporaryRoot, "config", "user.email", "evidence-test@invalid.example");
  for (const [filePath] of PLANNING_INPUTS) {
    await mkdir(path.dirname(path.join(temporaryRoot, filePath)), { recursive: true });
    await writeFile(path.join(temporaryRoot, filePath), `${filePath} baseline\n`);
  }
  const baseline = await commit(temporaryRoot, "baseline");
  const signed = await subject(temporaryRoot, baseline);
  await writeFile(path.join(temporaryRoot, "unrelated.txt"), "unrelated\n");
  await commit(temporaryRoot, "unrelated descendant");
  await expect("unchanged ancestor closure", () => verifyEvidenceSubjectCommit(baseline, signed, temporaryRoot), true);

  await expect("missing subject commit", () => verifyEvidenceSubjectCommit("0".repeat(40), signed, temporaryRoot), false, "does not exist");

  const wrongDigest = { ...signed, plan_sha256: "0".repeat(64) };
  await expect("signed digest versus ancestor blob", () => verifyEvidenceSubjectCommit(baseline, wrongDigest, temporaryRoot), false, "signed plan_sha256");

  await writeFile(path.join(temporaryRoot, "docs/DEVELOPMENT_PLAN.md"), "changed\n");
  await commit(temporaryRoot, "change planning input");
  await expect("changed ancestor", () => verifyEvidenceSubjectCommit(baseline, signed, temporaryRoot), false, "current planning input");
  await writeFile(path.join(temporaryRoot, "docs/DEVELOPMENT_PLAN.md"), "docs/DEVELOPMENT_PLAN.md baseline\n");
  await commit(temporaryRoot, "revert planning input");
  await expect("changed then reverted replay", () => verifyEvidenceSubjectCommit(baseline, signed, temporaryRoot), false, "changed-then-reverted");

  const manifestPath = path.join(temporaryRoot, "spec/artifact-manifest.yaml");
  const manifestContent = await readFile(manifestPath);
  await writeFile(manifestPath, Buffer.concat([manifestContent, Buffer.from("manifest-only worktree drift\n")]));
  await expect("artifact-manifest-only drift", () => verifyEvidenceSubjectCommit(baseline, signed, temporaryRoot), false, "artifact-manifest.yaml differs");
  await writeFile(manifestPath, manifestContent);

  const deletionRoot = path.join(temporaryRoot, "deleted-case");
  await git(temporaryRoot, "worktree", "add", "--quiet", "-b", "deleted-case", deletionRoot, baseline);
  await git(deletionRoot, "rm", "--quiet", "spec/artifact-manifest.yaml");
  const deletionCommit = await commit(deletionRoot, "delete planning blob");
  await expect("deleted planning blob", () => verifyEvidenceSubjectCommit(baseline, signed, deletionRoot), false, "current planning input");
  const missingAtSubject = { ...signed, commit: deletionCommit };
  await expect("subject commit missing blob", () => verifyEvidenceSubjectCommit(deletionCommit, missingAtSubject, deletionRoot), false, "absent at evidence subject");
  await expect("non-ancestor subject", () => verifyEvidenceSubjectCommit(deletionCommit, missingAtSubject, temporaryRoot), false, "is not an ancestor");
  await git(temporaryRoot, "worktree", "remove", "--force", deletionRoot);

  const gateSchema = JSON.parse(await readFile(path.join(process.cwd(), "spec/schemas/user-gate-evidence.schema.json"), "utf8"));
  const validateGate = new Ajv2020({ strict: true, validateFormats: false }).compile(gateSchema);
  const missingManifestEvidence = {
    schema_version: 1,
    kind: "arop-user-gate",
    gate: "P04",
    subject: {
      commit: baseline,
      plan_last_phase: "P53",
      requirements_sha256: "0".repeat(64),
      plan_sha256: "0".repeat(64),
      blueprint_sha256: "0".repeat(64),
      planning_audit_summary_sha256: `sha256:${"0".repeat(64)}`,
    },
    approver: { id: "fixture" },
    result: { requirements: [], phases: [], must_fix_count: 0, must_fix: [], should_fix_count: 0, should_fix: [], verdict: "PASS" },
    attested_at: "2026-09-22T00:00:00Z",
    summary_sha256: `sha256:${"0".repeat(64)}`,
  };
  if (validateGate(missingManifestEvidence) || !(validateGate.errors || []).some((error) => error.params?.missingProperty === "artifact_manifest_sha256")) {
    throw new Error("P04 schema did not reject missing artifact_manifest_sha256");
  }
  results.push("P04 missing artifact manifest digest: expected FAIL");
} finally {
  await rm(temporaryRoot, { recursive: true, force: true });
}

console.log(`AROP evidence lineage tests passed (${results.length} scenarios):`);
for (const result of results) console.log(`- ${result}`);
