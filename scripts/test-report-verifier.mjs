#!/usr/bin/env node

import { execFile } from "node:child_process";
import { cp, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { promisify } from "node:util";

import { repositoryRoot } from "./lib/repository.mjs";
import { aggregateFileDigest, sha256 } from "./lib/report.mjs";

const execFileAsync = promisify(execFile);
const verifierPath = path.join(repositoryRoot, "scripts/verify-report.mjs");
const results = [];

async function command(command, arguments_, options = {}) {
  return execFileAsync(command, arguments_, {
    encoding: "utf8",
    maxBuffer: 64 * 1024 * 1024,
    ...options,
  });
}

async function expectVerifier({ name, root, report, allowAncestor = false, pass, needle }) {
  try {
    const result = await command(process.execPath, [verifierPath, report], {
      cwd: root,
      env: {
        ...process.env,
        AROP_VERIFY_REPOSITORY_ROOT: root,
        ALLOW_ANCESTOR: allowAncestor ? "1" : "0",
      },
    });
    if (!pass) throw new Error(`${name} unexpectedly passed: ${result.stdout.trim()}`);
    if (needle && !result.stdout.includes(needle)) {
      throw new Error(`${name} output did not include ${JSON.stringify(needle)}`);
    }
    results.push(`${name}: PASS`);
  } catch (error) {
    if (pass) {
      throw new Error(`${name} unexpectedly failed: ${(error.stderr || error.message).trim()}`);
    }
    const output = `${error.stdout ?? ""}\n${error.stderr ?? ""}\n${error.message ?? ""}`;
    if (needle && !output.includes(needle)) {
      throw new Error(`${name} failed for the wrong reason: ${output.trim()}`);
    }
    results.push(`${name}: expected FAIL`);
  }
}

function successfulFixture(head, checkerPath, checkerDigest, input) {
  const files = [input];
  return {
    schema_version: 1,
    generated_at: "2026-09-22T00:00:00.000Z",
    success: true,
    provenance: {
      git: { head, dirty: false, dirty_entries: [] },
      command: "report-verifier-historical-fixture",
      runtime: {
        node: process.version,
        go: "go fixture",
        os: { platform: os.platform(), release: os.release(), arch: os.arch() },
      },
      checker: { path: checkerPath, sha256: checkerDigest },
      inputs: { sha256: aggregateFileDigest(files), files },
      testcase_count: 1,
      audit_note: "Synthetic verifier fixture; not release or conformance evidence.",
    },
    summary: { checks: 1, passed: 1, failed: 0, testcase_count: 1 },
    checks: [{ name: "fixture", passed: true, detail: "synthetic integrity fixture" }],
    errors: [],
  };
}

async function writeFixture(directory, report) {
  await mkdir(directory, { recursive: true });
  await writeFile(path.join(directory, "report.json"), `${JSON.stringify(report, null, 2)}\n`);
  await writeFile(
    path.join(directory, "junit.xml"),
    '<?xml version="1.0" encoding="UTF-8"?>\n<testsuite name="fixture" tests="1" failures="0">\n  <testcase classname="fixture" name="fixture"></testcase>\n</testsuite>\n',
  );
  return path.join(directory, "report.json");
}

await expectVerifier({
  name: "current P01 report",
  root: repositoryRoot,
  report: "build/reports/P01/report.json",
  pass: true,
  needle: "mode=current-worktree",
});
await expectVerifier({
  name: "current P02 report",
  root: repositoryRoot,
  report: "build/reports/P02/report.json",
  pass: true,
  needle: "mode=current-worktree",
});

const corruptedDirectory = path.join(repositoryRoot, "build/report-verifier-tests/corrupted");
await mkdir(corruptedDirectory, { recursive: true });
const currentP01 = JSON.parse(
  await readFile(path.join(repositoryRoot, "build/reports/P01/report.json"), "utf8"),
);
currentP01.provenance.inputs.files[0].sha256 = "0".repeat(64);
await writeFile(
  path.join(corruptedDirectory, "report.json"),
  `${JSON.stringify(currentP01, null, 2)}\n`,
);
await cp(
  path.join(repositoryRoot, "build/reports/P01/junit.xml"),
  path.join(corruptedDirectory, "junit.xml"),
);
await expectVerifier({
  name: "corrupted input digest",
  root: repositoryRoot,
  report: path.join(corruptedDirectory, "report.json"),
  pass: false,
  needle: "input digest/size mismatch",
});

const temporaryRoot = await mkdtemp(path.join(os.tmpdir(), "arop-report-verifier-"));
try {
  const cloneRoot = path.join(temporaryRoot, "repo");
  await command("git", ["clone", "--quiet", "--no-local", repositoryRoot, cloneRoot]);
  const currentHead = (await command("git", ["rev-parse", "HEAD"], { cwd: cloneRoot })).stdout.trim();
  const ancestorHead = (await command("git", ["rev-parse", "HEAD^"], { cwd: cloneRoot })).stdout.trim();
  const candidates = ["LICENSE", "CODE_OF_CONDUCT.md", "scripts/validate.mjs"];
  let stablePath;
  let stableBlob;
  for (const candidate of candidates) {
    const ancestorBlob = (
      await command("git", ["show", `${ancestorHead}:${candidate}`], {
        cwd: cloneRoot,
        encoding: null,
      })
    ).stdout;
    const currentBlob = (
      await command("git", ["show", `${currentHead}:${candidate}`], {
        cwd: cloneRoot,
        encoding: null,
      })
    ).stdout;
    const ancestorBuffer = Buffer.isBuffer(ancestorBlob) ? ancestorBlob : Buffer.from(ancestorBlob);
    const currentBuffer = Buffer.isBuffer(currentBlob) ? currentBlob : Buffer.from(currentBlob);
    if (sha256(ancestorBuffer) === sha256(currentBuffer)) {
      stablePath = candidate;
      stableBlob = ancestorBuffer;
      break;
    }
  }
  if (!stablePath) throw new Error("no unchanged ancestor fixture input is available");

  const input = {
    path: stablePath,
    sha256: sha256(stableBlob),
    bytes: stableBlob.byteLength,
  };
  const historicalReport = successfulFixture(
    ancestorHead,
    stablePath,
    input.sha256,
    input,
  );
  const historicalPath = await writeFixture(
    path.join(cloneRoot, "build/report-verifier-tests/historical"),
    historicalReport,
  );
  await expectVerifier({
    name: "historical ancestor archival integrity",
    root: cloneRoot,
    report: historicalPath,
    allowAncestor: true,
    pass: true,
    needle: "Historical mode proves archival integrity only",
  });
  await expectVerifier({
    name: "ancestor requires explicit opt-in",
    root: cloneRoot,
    report: historicalPath,
    pass: false,
    needle: "ALLOW_ANCESTOR=1",
  });

  const stableWorktreePath = path.join(cloneRoot, stablePath);
  const stableWorktreeContent = await readFile(stableWorktreePath);
  await writeFile(
    stableWorktreePath,
    Buffer.concat([stableWorktreeContent, Buffer.from("\nchanged after claimed commit\n")]),
  );
  await expectVerifier({
    name: "ancestor current input changed",
    root: cloneRoot,
    report: historicalPath,
    allowAncestor: true,
    pass: false,
    needle: "current reuse eligibility input digest/size mismatch",
  });
  await writeFile(stableWorktreePath, stableWorktreeContent);

  const missingReport = structuredClone(historicalReport);
  missingReport.provenance.git.head = "0".repeat(40);
  const missingPath = await writeFixture(
    path.join(cloneRoot, "build/report-verifier-tests/missing"),
    missingReport,
  );
  await expectVerifier({
    name: "missing commit",
    root: cloneRoot,
    report: missingPath,
    allowAncestor: true,
    pass: false,
    needle: "does not exist as a commit",
  });

  await command("git", ["config", "user.name", "AROP Verifier Test"], { cwd: cloneRoot });
  await command("git", ["config", "user.email", "verifier-test@invalid.example"], {
    cwd: cloneRoot,
  });
  await command("git", ["checkout", "--quiet", "--detach", ancestorHead], { cwd: cloneRoot });
  await writeFile(path.join(cloneRoot, "nonancestor-fixture.txt"), "non-ancestor fixture\n");
  await command("git", ["add", "nonancestor-fixture.txt"], { cwd: cloneRoot });
  await command("git", ["commit", "--quiet", "-m", "test: non-ancestor verifier fixture"], {
    cwd: cloneRoot,
  });
  const nonAncestorHead = (
    await command("git", ["rev-parse", "HEAD"], { cwd: cloneRoot })
  ).stdout.trim();
  await command("git", ["checkout", "--quiet", "--detach", currentHead], { cwd: cloneRoot });
  const nonAncestorReport = structuredClone(historicalReport);
  nonAncestorReport.provenance.git.head = nonAncestorHead;
  const nonAncestorPath = await writeFixture(
    path.join(cloneRoot, "build/report-verifier-tests/nonancestor"),
    nonAncestorReport,
  );
  await expectVerifier({
    name: "non-ancestor commit",
    root: cloneRoot,
    report: nonAncestorPath,
    allowAncestor: true,
    pass: false,
    needle: "is not an ancestor",
  });
} finally {
  await rm(temporaryRoot, { recursive: true, force: true });
}

console.log(`AROP report verifier tests passed (${results.length} scenarios):`);
for (const result of results) console.log(`- ${result}`);
