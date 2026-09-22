import { createHash } from "node:crypto";
import { execFile } from "node:child_process";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { promisify } from "node:util";

import { repositoryRoot } from "./repository.mjs";

const execFileAsync = promisify(execFile);

function relative(filePath, baseRoot = repositoryRoot) {
  return path.relative(baseRoot, filePath).split(path.sep).join("/");
}

export function sha256(value) {
  return createHash("sha256").update(value).digest("hex");
}

function xmlEscape(value) {
  return String(value)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&apos;");
}

async function commandOutput(command, args, preserveLeadingWhitespace = false) {
  try {
    const { stdout } = await execFileAsync(command, args, {
      cwd: repositoryRoot,
      encoding: "utf8",
    });
    return preserveLeadingWhitespace ? stdout.trimEnd() : stdout.trim();
  } catch (error) {
    return `unavailable: ${error.message}`;
  }
}

export function aggregateFileDigest(files) {
  const aggregate = files.map((file) => `${file.path}\0${file.sha256}\0${file.bytes}\n`).join("");
  return sha256(aggregate);
}

export async function digestFiles(inputPaths, baseRoot = repositoryRoot) {
  const files = [];
  for (const filePath of [...new Set(inputPaths)].sort()) {
    const absolutePath = path.resolve(baseRoot, filePath);
    const content = await readFile(absolutePath);
    files.push({
      path: relative(absolutePath, baseRoot),
      sha256: sha256(content),
      bytes: content.byteLength,
    });
  }
  return { sha256: aggregateFileDigest(files), files };
}

export function actualCommand(fallbackScript) {
  return (
    process.env.AROP_CHECK_COMMAND ??
    [process.execPath, fallbackScript, ...process.argv.slice(2)].join(" ")
  );
}

export async function writeCheckReport({
  reportDirectory,
  suiteName,
  className,
  command,
  checkerPath,
  inputPaths,
  checks,
  errors = [],
  summary,
  auditNote,
}) {
  const [head, status, goVersion, inputs, checkerContent] = await Promise.all([
    commandOutput("git", ["rev-parse", "HEAD"]),
    commandOutput("git", ["status", "--porcelain=v1", "--untracked-files=all"], true),
    commandOutput("go", ["version"]),
    digestFiles(inputPaths),
    readFile(path.resolve(repositoryRoot, checkerPath)),
  ]);
  const dirtyEntries = status.startsWith("unavailable:")
    ? [status]
    : status.split(/\r?\n/).filter(Boolean);
  const provenanceProblems = [];
  if (!/^[0-9a-f]{40}$/u.test(head)) provenanceProblems.push(`invalid git HEAD: ${head}`);
  if (status.startsWith("unavailable:")) provenanceProblems.push(status);
  if (goVersion.startsWith("unavailable:")) provenanceProblems.push(goVersion);
  if (!command) provenanceProblems.push("actual command is empty");
  const provenanceCheck = {
    name: "report-provenance",
    passed: provenanceProblems.length === 0,
    detail:
      provenanceProblems.length === 0
        ? "exact HEAD, dirty state, command, Node/Go/OS, input digest and checker digest captured"
        : provenanceProblems.join("; "),
  };
  checks.push(provenanceCheck);
  if (!provenanceCheck.passed) errors.push(`${provenanceCheck.name}: ${provenanceCheck.detail}`);
  const testcaseCount = checks.length;
  const failureCount = checks.filter((check) => !check.passed).length;
  const report = {
    schema_version: 1,
    generated_at: new Date().toISOString(),
    success: errors.length === 0,
    provenance: {
      git: {
        head,
        dirty: dirtyEntries.length > 0,
        dirty_entries: dirtyEntries,
      },
      command,
      runtime: {
        node: process.version,
        go: goVersion,
        os: {
          platform: os.platform(),
          release: os.release(),
          arch: os.arch(),
        },
      },
      checker: {
        path: checkerPath,
        sha256: sha256(checkerContent),
      },
      inputs,
      testcase_count: testcaseCount,
      audit_note: auditNote,
    },
    summary: {
      ...summary,
      checks: testcaseCount,
      passed: testcaseCount - failureCount,
      failed: failureCount,
      testcase_count: testcaseCount,
    },
    checks,
    errors,
  };

  const absoluteReportDirectory = path.resolve(repositoryRoot, reportDirectory);
  await mkdir(absoluteReportDirectory, { recursive: true });
  await writeFile(
    path.join(absoluteReportDirectory, "report.json"),
    `${JSON.stringify(report, null, 2)}\n`,
    "utf8",
  );

  const testCases = checks
    .map((check) => {
      const failure = check.passed
        ? ""
        : `<failure message="${xmlEscape(check.detail)}"/>`;
      return `  <testcase classname="${xmlEscape(className)}" name="${xmlEscape(check.name)}">${failure}</testcase>`;
    })
    .join("\n");
  const junit = `<?xml version="1.0" encoding="UTF-8"?>
<testsuite name="${xmlEscape(suiteName)}" tests="${testcaseCount}" failures="${failureCount}">
${testCases}
</testsuite>
`;
  await writeFile(path.join(absoluteReportDirectory, "junit.xml"), junit, "utf8");
  return report;
}
