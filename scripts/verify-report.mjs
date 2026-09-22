#!/usr/bin/env node

import { execFile } from "node:child_process";
import { readFile } from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";

import Ajv2020 from "ajv/dist/2020.js";
import addFormats from "ajv-formats";

import { loadStructuredFile, parseJSONWithUniqueKeys, repositoryRoot } from "./lib/repository.mjs";
import { aggregateFileDigest, digestFiles, sha256 } from "./lib/report.mjs";

const execFileAsync = promisify(execFile);
const cliArguments = process.argv.slice(2);
const allowAncestor =
  process.env.ALLOW_ANCESTOR === "1" || cliArguments.includes("--allow-ancestor");
const positionalArguments = cliArguments.filter((argument) => argument !== "--allow-ancestor");
const reportArgument = process.env.REPORT ?? positionalArguments[0];
const verificationRoot = path.resolve(
  process.env.AROP_VERIFY_REPOSITORY_ROOT ?? repositoryRoot,
);
const problems = [];

function fail(message) {
  problems.push(message);
}

function xmlUnescape(value) {
  return value
    .replaceAll("&quot;", '"')
    .replaceAll("&apos;", "'")
    .replaceAll("&lt;", "<")
    .replaceAll("&gt;", ">")
    .replaceAll("&amp;", "&");
}

function repositoryPath(value, label) {
  if (
    typeof value !== "string" ||
    path.isAbsolute(value) ||
    value.includes(":") ||
    value.split(/[\\/]/u).includes("..")
  ) {
    fail(`${label} is not a safe repository-relative path: ${value}`);
    return undefined;
  }
  return value;
}

async function gitText(arguments_) {
  const { stdout } = await execFileAsync("git", arguments_, {
    cwd: verificationRoot,
    encoding: "utf8",
    maxBuffer: 16 * 1024 * 1024,
  });
  return stdout.trim();
}

async function gitBlob(commit, filePath) {
  const { stdout } = await execFileAsync("git", ["show", `${commit}:${filePath}`], {
    cwd: verificationRoot,
    encoding: null,
    maxBuffer: 64 * 1024 * 1024,
  });
  return Buffer.isBuffer(stdout) ? stdout : Buffer.from(stdout);
}

async function digestFilesAtCommit(inputPaths, commit) {
  const files = [];
  for (const filePath of [...new Set(inputPaths)].sort()) {
    const content = await gitBlob(commit, filePath);
    files.push({
      path: filePath,
      sha256: sha256(content),
      bytes: content.byteLength,
    });
  }
  return { sha256: aggregateFileDigest(files), files };
}

function compareDigestSet(actual, expected, label) {
  if (actual.sha256 !== expected?.sha256) fail(`${label} aggregate input digest mismatch`);
  const expectedByPath = new Map(expected?.files?.map((file) => [file.path, file]));
  for (const file of actual.files) {
    const expectedFile = expectedByPath.get(file.path);
    if (
      !expectedFile ||
      expectedFile.sha256 !== file.sha256 ||
      expectedFile.bytes !== file.bytes
    ) {
      fail(`${label} input digest/size mismatch for ${file.path}`);
    }
  }
}

if (!reportArgument) {
  console.error(
    "Usage: node scripts/verify-report.mjs [--allow-ancestor] <build/reports/.../report.json>",
  );
  process.exit(2);
}

const reportPath = path.isAbsolute(reportArgument)
  ? reportArgument
  : path.resolve(verificationRoot, reportArgument);
const reportRelative = path.relative(verificationRoot, reportPath).split(path.sep).join("/");
if (reportRelative.startsWith("..") || path.isAbsolute(reportRelative)) {
  fail("report must be inside the repository so its input paths can be verified");
}

let report;
try {
  const source = await readFile(reportPath, "utf8");
  report = parseJSONWithUniqueKeys(source, reportPath);
} catch (error) {
  fail(`report is unreadable: ${error.message}`);
}

let currentHead = "unavailable";
let verificationMode = "current-worktree";
let claimedCommitUsable = false;
if (report) {
  try {
    const schema = await loadStructuredFile(
      path.join(verificationRoot, "spec/schemas/check-report.schema.json"),
    );
    const ajv = new Ajv2020({ allErrors: true, strict: true });
    addFormats(ajv);
    const validate = ajv.compile(schema);
    if (!validate(report)) {
      fail(
        `report schema: ${(validate.errors ?? [])
          .map((error) => `${error.instancePath || "/"} ${error.message}`)
          .join("; ")}`,
      );
    }
  } catch (error) {
    fail(`report schema cannot be applied: ${error.message}`);
  }

  const checkNames = report.checks?.map((check) => check.name) ?? [];
  if (new Set(checkNames).size !== checkNames.length) fail("JSON check names are not unique");
  const failedChecks = report.checks?.filter((check) => !check.passed) ?? [];
  const counts = [
    ["provenance.testcase_count", report.provenance?.testcase_count, report.checks?.length],
    ["summary.checks", report.summary?.checks, report.checks?.length],
    ["summary.testcase_count", report.summary?.testcase_count, report.checks?.length],
    ["summary.failed", report.summary?.failed, failedChecks.length],
    ["summary.passed", report.summary?.passed, (report.checks?.length ?? 0) - failedChecks.length],
    ["errors.length", report.errors?.length, failedChecks.length],
  ];
  for (const [label, actual, expected] of counts) {
    if (actual !== expected) fail(`${label} expected ${expected}, got ${actual}`);
  }
  if (report.success !== (failedChecks.length === 0 && report.errors?.length === 0)) {
    fail("success does not match JSON failure/error counts");
  }
  for (const failed of failedChecks) {
    if (!report.errors?.some((message) => message.startsWith(`${failed.name}:`))) {
      fail(`failed check ${failed.name} has no corresponding errors entry`);
    }
  }

  try {
    const junit = await readFile(path.join(path.dirname(reportPath), "junit.xml"), "utf8");
    const suite = junit.match(
      /<testsuite\b[^>]*\btests="(\d+)"[^>]*\bfailures="(\d+)"[^>]*>/u,
    );
    if (!suite) {
      fail("JUnit testsuite counters are missing");
    } else {
      const junitTests = Number(suite[1]);
      const junitFailures = Number(suite[2]);
      const testcaseCount = [...junit.matchAll(/<testcase\b/gu)].length;
      const failureCount = [...junit.matchAll(/<failure\b/gu)].length;
      if (junitTests !== report.checks.length || testcaseCount !== report.checks.length) {
        fail(
          `JUnit test count ${junitTests}/${testcaseCount} does not match JSON ${report.checks.length}`,
        );
      }
      if (junitFailures !== failedChecks.length || failureCount !== failedChecks.length) {
        fail(
          `JUnit failure count ${junitFailures}/${failureCount} does not match JSON ${failedChecks.length}`,
        );
      }
      const junitCases = [
        ...junit.matchAll(/<testcase\b([^>]*)>([\s\S]*?)<\/testcase>/gu),
      ].map((match) => ({
        name: xmlUnescape(match[1].match(/\bname="([^"]*)"/u)?.[1] ?? ""),
        failed: /<failure\b/u.test(match[2]),
      }));
      for (const [index, check] of (report.checks ?? []).entries()) {
        const junitCase = junitCases[index];
        if (junitCase?.name !== check.name) {
          fail(`JUnit testcase ${index} name ${junitCase?.name} does not match JSON ${check.name}`);
        }
        if (junitCase?.failed !== !check.passed) {
          fail(`JUnit testcase ${check.name} failure state does not match JSON passed=${check.passed}`);
        }
      }
    }
  } catch (error) {
    fail(`JUnit is unreadable: ${error.message}`);
  }

  const claimedHead = report.provenance?.git?.head;
  try {
    currentHead = await gitText(["rev-parse", "HEAD"]);
    if (!/^[0-9a-f]{40}$/u.test(claimedHead ?? "")) {
      fail(`report claimed HEAD is invalid: ${claimedHead}`);
    } else {
      try {
        await gitText(["cat-file", "-e", `${claimedHead}^{commit}`]);
        claimedCommitUsable = true;
      } catch {
        fail(`report claimed HEAD does not exist as a commit: ${claimedHead}`);
      }
    }
    if (claimedCommitUsable && claimedHead !== currentHead) {
      verificationMode = "ancestor-commit";
      if (!allowAncestor) {
        fail(
          `report HEAD ${claimedHead} is not current HEAD ${currentHead}; rerun the report or opt in with ALLOW_ANCESTOR=1`,
        );
        claimedCommitUsable = false;
      } else {
        try {
          await execFileAsync("git", ["merge-base", "--is-ancestor", claimedHead, currentHead], {
            cwd: verificationRoot,
          });
        } catch {
          fail(`report claimed HEAD ${claimedHead} is not an ancestor of current HEAD ${currentHead}`);
          claimedCommitUsable = false;
        }
      }
    }
  } catch (error) {
    fail(`cannot verify report commit lineage: ${error.message}`);
  }

  const checkerPath = repositoryPath(report.provenance?.checker?.path, "checker path");
  const inputPaths = [];
  for (const input of report.provenance?.inputs?.files ?? []) {
    const safePath = repositoryPath(input.path, "input path");
    if (safePath) inputPaths.push(safePath);
  }
  if (new Set(inputPaths).size !== inputPaths.length) fail("report input paths are not unique");
  if (JSON.stringify(inputPaths) !== JSON.stringify([...inputPaths].sort())) {
    fail("report input paths are not in canonical sorted order");
  }

  if (claimedCommitUsable && verificationMode === "ancestor-commit") {
    if (report.provenance?.git?.dirty !== false || report.provenance?.git?.dirty_entries?.length !== 0) {
      fail("historical report reuse requires a clean claimed-commit report");
    }
    try {
      if (checkerPath) {
        const checkerDigest = sha256(await gitBlob(report.provenance.git.head, checkerPath));
        if (checkerDigest !== report.provenance.checker.sha256) {
          fail(`claimed-commit checker digest mismatch for ${checkerPath}`);
        }
      }
      const committedInputs = await digestFilesAtCommit(
        inputPaths,
        report.provenance.git.head,
      );
      compareDigestSet(
        committedInputs,
        report.provenance.inputs,
        `claimed commit ${report.provenance.git.head}`,
      );
    } catch (error) {
      fail(`cannot verify claimed-commit blobs: ${error.message}`);
    }

    try {
      if (checkerPath) {
        const currentCheckerDigest = sha256(
          await readFile(path.join(verificationRoot, checkerPath)),
        );
        if (currentCheckerDigest !== report.provenance.checker.sha256) {
          fail(
            `current checker ${checkerPath} changed since claimed commit; historical report must be rerun`,
          );
        }
      }
      const currentInputs = await digestFiles(inputPaths, verificationRoot);
      compareDigestSet(
        currentInputs,
        report.provenance.inputs,
        "current reuse eligibility",
      );
    } catch (error) {
      fail(`cannot verify current-input reuse eligibility: ${error.message}`);
    }
  } else if (claimedCommitUsable) {
    try {
      const status = (
        await execFileAsync("git", ["status", "--porcelain=v1", "--untracked-files=all"], {
          cwd: verificationRoot,
          encoding: "utf8",
        })
      ).stdout.trimEnd();
      const dirtyEntries = status.split(/\r?\n/u).filter(Boolean);
      if (report.provenance?.git?.dirty !== (dirtyEntries.length > 0)) {
        fail("report dirty flag does not match the current worktree");
      }
      if (
        JSON.stringify(report.provenance?.git?.dirty_entries) !==
        JSON.stringify(dirtyEntries)
      ) {
        fail("report dirty_entries do not match the current worktree");
      }
    } catch (error) {
      fail(`cannot verify current dirty state: ${error.message}`);
    }

    if (checkerPath) {
      try {
        const checkerDigest = sha256(
          await readFile(path.join(verificationRoot, checkerPath)),
        );
        if (checkerDigest !== report.provenance.checker.sha256) {
          fail(`checker digest mismatch for ${checkerPath}`);
        }
      } catch (error) {
        fail(`cannot verify checker ${checkerPath}: ${error.message}`);
      }
    }
    try {
      const actual = await digestFiles(inputPaths, verificationRoot);
      compareDigestSet(actual, report.provenance.inputs, "current worktree");
    } catch (error) {
      fail(`cannot verify report inputs: ${error.message}`);
    }
  }
}

if (problems.length > 0) {
  console.error(`AROP report verification failed with ${problems.length} error(s):`);
  for (const problem of problems) console.error(`- ${problem}`);
  process.exit(1);
}
console.log(
  `AROP report verified: ${reportRelative}; mode=${verificationMode}; claimed=${report.provenance.git.head}; current=${currentHead}; ` +
    `${report.checks.length} JSON/JUnit testcases, ${report.summary.failed} failures, lineage and digests match.` +
    (verificationMode === "ancestor-commit"
      ? " Historical mode proves archival integrity only; aggregation still requires an isolated rerun or trusted CI/OIDC/Sigstore provenance."
      : ""),
);
