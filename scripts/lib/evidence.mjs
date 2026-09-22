import { verify } from "node:crypto";
import { execFile } from "node:child_process";
import { readFile } from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";

import canonicalize from "canonicalize";

import { loadStructuredFile, repositoryRoot } from "./repository.mjs";
import { sha256 } from "./report.mjs";

const execFileAsync = promisify(execFile);

export const SUMMARY_FIELDS = Object.freeze({
  "arop-planning-audit": [
    "schema_version",
    "kind",
    "subject",
    "reviewer",
    "result",
    "attested_at",
  ],
  "arop-user-gate": [
    "schema_version",
    "kind",
    "gate",
    "subject",
    "approver",
    "result",
    "attested_at",
  ],
});

export function canonicalSummary(evidence) {
  const fields = SUMMARY_FIELDS[evidence?.kind];
  if (!fields) throw new Error(`unsupported evidence kind: ${evidence?.kind}`);
  const summary = {};
  for (const field of fields) summary[field] = evidence[field];
  const serialized = canonicalize(summary);
  if (serialized === undefined) throw new Error("evidence summary cannot be canonicalized");
  return serialized;
}

export function canonicalSummaryDigest(evidence) {
  return `sha256:${sha256(Buffer.from(canonicalSummary(evidence), "utf8"))}`;
}

export function validateResultCoverage(result, requirementIDs, phaseIDs) {
  const problems = [];
  for (const [label, expected, actual] of [
    ["requirements", requirementIDs, result?.requirements],
    ["phases", phaseIDs, result?.phases],
  ]) {
    const actualIDs = Array.isArray(actual) ? actual.map((item) => item.id) : [];
    if (JSON.stringify(actualIDs) !== JSON.stringify(expected)) {
      problems.push(`${label} must be exactly ${expected.join(", ")} in order`);
    }
    for (const item of actual ?? []) {
      if (item.result !== "PASS") problems.push(`${item.id} result is not PASS`);
    }
  }
  if (result?.must_fix_count !== 0 || result?.must_fix?.length !== 0) {
    problems.push("must_fix_count and must_fix details must both be zero/empty");
  }
  if (result?.should_fix_count !== result?.should_fix?.length) {
    problems.push("should_fix_count does not match should_fix details");
  }
  if (result?.verdict !== "PASS") problems.push("verdict is not PASS");
  return problems;
}

export async function verifyEvidenceSubjectCommit(subjectCommit) {
  if (!/^[0-9a-f]{40}$/u.test(subjectCommit ?? "")) {
    throw new Error(`evidence subject commit is invalid: ${subjectCommit}`);
  }
  const currentHead = (
    await execFileAsync("git", ["rev-parse", "HEAD"], {
      cwd: repositoryRoot,
      encoding: "utf8",
    })
  ).stdout.trim();
  try {
    await execFileAsync("git", ["cat-file", "-e", `${subjectCommit}^{commit}`], {
      cwd: repositoryRoot,
    });
  } catch {
    throw new Error(`evidence subject commit does not exist: ${subjectCommit}`);
  }
  if (subjectCommit !== currentHead) {
    try {
      await execFileAsync("git", ["merge-base", "--is-ancestor", subjectCommit, currentHead], {
        cwd: repositoryRoot,
      });
    } catch {
      throw new Error(
        `evidence subject commit ${subjectCommit} is not an ancestor of current HEAD ${currentHead}`,
      );
    }
  }
  return {
    subject_commit: subjectCommit,
    current_head: currentHead,
    mode: subjectCommit === currentHead ? "current" : "ancestor",
  };
}

function isOutsideRepository(absolutePath) {
  const relative = path.relative(repositoryRoot, absolutePath);
  return relative.startsWith("..") || path.isAbsolute(relative);
}

export async function verifyEvidenceAuthentication({
  evidence,
  expectedRole,
  trustedKeysArgument,
  trustedChannelArgument,
}) {
  const canonical = canonicalSummary(evidence);
  if (trustedKeysArgument) {
    const trustedKeysPath = path.resolve(trustedKeysArgument);
    if (!isOutsideRepository(trustedKeysPath)) {
      throw new Error("TRUSTED_KEYS registry must remain outside the Git repository");
    }
    if (!evidence.attestation) {
      throw new Error("TRUSTED_KEYS was provided but evidence has no Ed25519 attestation");
    }
    const registry = await loadStructuredFile(trustedKeysPath);
    if (registry?.schema_version !== 1 || !Array.isArray(registry.keys)) {
      throw new Error("TRUSTED_KEYS must contain schema_version=1 and a keys array");
    }
    const duplicates = registry.keys.filter(
      (candidate) => candidate.key_id === evidence.attestation.key_id,
    );
    if (duplicates.length !== 1) {
      throw new Error(`trusted key ${evidence.attestation.key_id} must resolve exactly once`);
    }
    const key = duplicates[0];
    if (key.algorithm !== "Ed25519" || evidence.attestation.algorithm !== "Ed25519") {
      throw new Error("attestation and trusted key algorithms must both be Ed25519");
    }
    if (key.role !== expectedRole) {
      throw new Error(`trusted key role must be ${expectedRole}, got ${key.role}`);
    }
    if (typeof key.public_key_pem !== "string" || key.public_key_pem.length === 0) {
      throw new Error("trusted key public_key_pem is missing");
    }
    const valid = verify(
      null,
      Buffer.from(canonical, "utf8"),
      key.public_key_pem,
      Buffer.from(evidence.attestation.signature, "base64"),
    );
    if (!valid) throw new Error("Ed25519 signature does not verify against the canonical summary");
    return {
      mode: "trusted-key-attestation",
      key_id: key.key_id,
      key_role: key.role,
      trusted_keys_sha256: `sha256:${sha256(await readFile(trustedKeysPath))}`,
    };
  }

  if (!trustedChannelArgument) {
    throw new Error(
      "authenticity requires TRUSTED_KEYS with an Ed25519 attestation or a TRUSTED_CHANNEL_CONFIRMATION external record",
    );
  }
  const trustedChannelPath = path.resolve(trustedChannelArgument);
  if (!isOutsideRepository(trustedChannelPath)) {
    throw new Error("TRUSTED_CHANNEL_CONFIRMATION must remain outside the Git repository");
  }
  const confirmation = await readFile(trustedChannelPath);
  if (confirmation.byteLength === 0) throw new Error("trusted-channel confirmation record is empty");
  return {
    mode: "manual-trusted-channel",
    confirmation_sha256: `sha256:${sha256(confirmation)}`,
    limitation:
      "The tool proves only that an external confirmation record was supplied; a human Gate owns channel and actor authenticity.",
  };
}
