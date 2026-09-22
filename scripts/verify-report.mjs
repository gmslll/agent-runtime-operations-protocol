#!/usr/bin/env node

import { execFile } from "node:child_process";
import { readFile } from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";

import Ajv2020 from "ajv/dist/2020.js";
import addFormats from "ajv-formats";

import { loadStructuredFile, parseJSONWithUniqueKeys, repositoryRoot } from "./lib/repository.mjs";
import { digestFiles, sha256 } from "./lib/report.mjs";

const execFileAsync = promisify(execFile);
const reportArgument = process.env.REPORT ?? process.argv[2];
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
  if (typeof value !== "string" || path.isAbsolute(value) || value.split(/[\\/]/u).includes("..")) {
    fail(`${label} is not a safe repository-relative path: ${value}`);
    return undefined;
  }
  return value;
}

if (!reportArgument) {
  console.error("Usage: node scripts/verify-report.mjs <build/reports/.../report.json>");
  process.exit(2);
}

const reportPath = path.resolve(reportArgument);
const reportRelative = path.relative(repositoryRoot, reportPath);
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

if (report) {
  try {
    const schema = await loadStructuredFile(path.join(repositoryRoot, "spec/schemas/check-report.schema.json"));
    const ajv = new Ajv2020({ allErrors: true, strict: true });
    addFormats(ajv);
    const validate = ajv.compile(schema);
    if (!validate(report)) {
      fail(`report schema: ${(validate.errors ?? []).map((error) => `${error.instancePath || "/"} ${error.message}`).join("; ")}`);
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
    const suite = junit.match(/<testsuite\b[^>]*\btests="(\d+)"[^>]*\bfailures="(\d+)"[^>]*>/u);
    if (!suite) {
      fail("JUnit testsuite counters are missing");
    } else {
      const junitTests = Number(suite[1]);
      const junitFailures = Number(suite[2]);
      const testcaseCount = [...junit.matchAll(/<testcase\b/gu)].length;
      const failureCount = [...junit.matchAll(/<failure\b/gu)].length;
      if (junitTests !== report.checks.length || testcaseCount !== report.checks.length) {
        fail(`JUnit test count ${junitTests}/${testcaseCount} does not match JSON ${report.checks.length}`);
      }
      if (junitFailures !== failedChecks.length || failureCount !== failedChecks.length) {
        fail(`JUnit failure count ${junitFailures}/${failureCount} does not match JSON ${failedChecks.length}`);
      }
      const junitCases = [...junit.matchAll(/<testcase\b([^>]*)>([\s\S]*?)<\/testcase>/gu)].map(
        (match) => ({
          name: xmlUnescape(match[1].match(/\bname="([^"]*)"/u)?.[1] ?? ""),
          failed: /<failure\b/u.test(match[2]),
        }),
      );
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

  try {
    const head = (await execFileAsync("git", ["rev-parse", "HEAD"], { cwd: repositoryRoot })).stdout.trim();
    if (report.provenance?.git?.head !== head) {
      fail(`report HEAD ${report.provenance?.git?.head} does not match current HEAD ${head}`);
    }
    const status = (
      await execFileAsync("git", ["status", "--porcelain=v1", "--untracked-files=all"], {
        cwd: repositoryRoot,
      })
    ).stdout.trimEnd();
    const dirtyEntries = status.split(/\r?\n/u).filter(Boolean);
    if (report.provenance?.git?.dirty !== (dirtyEntries.length > 0)) {
      fail("report dirty flag does not match the current worktree");
    }
    if (JSON.stringify(report.provenance?.git?.dirty_entries) !== JSON.stringify(dirtyEntries)) {
      fail("report dirty_entries do not match the current worktree");
    }
  } catch (error) {
    fail(`cannot verify HEAD/dirty state: ${error.message}`);
  }

  const checkerPath = repositoryPath(report.provenance?.checker?.path, "checker path");
  if (checkerPath) {
    try {
      const checkerDigest = sha256(await readFile(path.join(repositoryRoot, checkerPath)));
      if (checkerDigest !== report.provenance.checker.sha256) {
        fail(`checker digest mismatch for ${checkerPath}`);
      }
    } catch (error) {
      fail(`cannot verify checker ${checkerPath}: ${error.message}`);
    }
  }

  const inputPaths = [];
  for (const input of report.provenance?.inputs?.files ?? []) {
    const safePath = repositoryPath(input.path, "input path");
    if (safePath) inputPaths.push(safePath);
  }
  if (new Set(inputPaths).size !== inputPaths.length) fail("report input paths are not unique");
  if (JSON.stringify(inputPaths) !== JSON.stringify([...inputPaths].sort())) {
    fail("report input paths are not in canonical sorted order");
  }
  try {
    const actual = await digestFiles(inputPaths);
    if (actual.sha256 !== report.provenance?.inputs?.sha256) fail("aggregate input digest mismatch");
    const expectedByPath = new Map(report.provenance?.inputs?.files.map((file) => [file.path, file]));
    for (const file of actual.files) {
      const expected = expectedByPath.get(file.path);
      if (!expected || expected.sha256 !== file.sha256 || expected.bytes !== file.bytes) {
        fail(`input digest/size mismatch for ${file.path}`);
      }
    }
  } catch (error) {
    fail(`cannot verify report inputs: ${error.message}`);
  }
}

if (problems.length > 0) {
  console.error(`AROP report verification failed with ${problems.length} error(s):`);
  for (const problem of problems) console.error(`- ${problem}`);
  process.exit(1);
}
console.log(`AROP report verified: ${reportRelative}; ${report.checks.length} JSON/JUnit testcases, ${report.summary.failed} failures, HEAD and digests match.`);
