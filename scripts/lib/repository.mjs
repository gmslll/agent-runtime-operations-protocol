import { createHash } from "node:crypto";
import { readFile, readdir } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

import canonicalize from "canonicalize";
import { parseDocument } from "yaml";

export const repositoryRoot = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "../..",
);

export async function walkFiles(directory, predicate = () => true) {
  const entries = await readdir(directory, { withFileTypes: true });
  const files = [];
  const ignoredDirectories = new Set([".git", "node_modules", "dist", "coverage"]);

  for (const entry of entries.sort((left, right) => left.name.localeCompare(right.name))) {
    const entryPath = path.join(directory, entry.name);
    if (entry.isDirectory() && !ignoredDirectories.has(entry.name)) {
      files.push(...(await walkFiles(entryPath, predicate)));
    } else if (entry.isFile() && predicate(entryPath)) {
      files.push(entryPath);
    }
  }

  return files;
}

export async function findPublicV01Violations(baseRoot = repositoryRoot) {
  const topLevel = ["README.md", "SECURITY.md", "CONTRIBUTING.md"].map((filePath) =>
    path.join(baseRoot, filePath),
  );
  const documentation = await walkFiles(
    path.join(baseRoot, "docs"),
    (filePath) => filePath.endsWith(".md"),
  );
  const files = [...topLevel, ...documentation];
  const violations = [];
  for (const filePath of files) {
    const source = await readFile(filePath, "utf8");
    violations.push(
      ...publicV01ViolationsInText(
        source,
        path.relative(baseRoot, filePath).split(path.sep).join("/"),
      ),
    );
  }
  return { files, violations };
}

export function publicV01ViolationsInText(source, label = "<text>") {
  const allowedNegativePolicy = /(?:取消独立公共 v0\.1|不用 v0\.1|不得用 v0\.1|no (?:separate )?public v0\.1)/iu;
  const violations = [];
  for (const [index, line] of source.split(/\r?\n/u).entries()) {
    if (/\bv0\.1\b/iu.test(line) && !allowedNegativePolicy.test(line)) {
      violations.push(`${label}:${index + 1}: ${line.trim()}`);
    }
  }
  return violations;
}

export async function loadStructuredFile(filePath) {
  const source = await readFile(filePath, "utf8");
  if (filePath.endsWith(".json")) {
    return parseJSONWithUniqueKeys(source, filePath);
  }

  const document = parseDocument(source, {
    prettyErrors: true,
    strict: true,
    uniqueKeys: true,
  });
  if (document.errors.length > 0) {
    throw new Error(document.errors.map((error) => error.message).join("\n"));
  }
  return document.toJS({ maxAliasCount: 0 });
}

export function parseJSONWithUniqueKeys(source, label = "JSON input") {
  let index = 0;

  function fail(message) {
    throw new SyntaxError(`${label}:${index}: ${message}`);
  }

  function skipWhitespace() {
    while (/\s/u.test(source[index] ?? "")) index += 1;
  }

  function parseString() {
    if (source[index] !== '"') fail("expected string");
    const start = index;
    index += 1;
    let escaped = false;
    while (index < source.length) {
      const character = source[index];
      index += 1;
      if (escaped) {
        escaped = false;
        continue;
      }
      if (character === "\\") {
        escaped = true;
        continue;
      }
      if (character === '"') {
        return JSON.parse(source.slice(start, index));
      }
    }
    fail("unterminated string");
  }

  function parseArray() {
    index += 1;
    skipWhitespace();
    if (source[index] === "]") {
      index += 1;
      return;
    }
    while (index < source.length) {
      parseValue();
      skipWhitespace();
      if (source[index] === "]") {
        index += 1;
        return;
      }
      if (source[index] !== ",") fail("expected ',' or ']' in array");
      index += 1;
      skipWhitespace();
    }
    fail("unterminated array");
  }

  function parseObject() {
    index += 1;
    skipWhitespace();
    const keys = new Set();
    if (source[index] === "}") {
      index += 1;
      return;
    }
    while (index < source.length) {
      const key = parseString();
      if (keys.has(key)) fail(`duplicate object key ${JSON.stringify(key)}`);
      keys.add(key);
      skipWhitespace();
      if (source[index] !== ":") fail("expected ':' after object key");
      index += 1;
      parseValue();
      skipWhitespace();
      if (source[index] === "}") {
        index += 1;
        return;
      }
      if (source[index] !== ",") fail("expected ',' or '}' in object");
      index += 1;
      skipWhitespace();
    }
    fail("unterminated object");
  }

  function parsePrimitive() {
    const start = index;
    while (index < source.length && !/[\s,\]}]/u.test(source[index])) index += 1;
    if (start === index) fail("expected JSON value");
  }

  function parseValue() {
    skipWhitespace();
    if (source[index] === "{") parseObject();
    else if (source[index] === "[") parseArray();
    else if (source[index] === '"') parseString();
    else parsePrimitive();
  }

  parseValue();
  skipWhitespace();
  if (index !== source.length) fail("unexpected trailing content");

  return JSON.parse(source);
}

export function manifestDigest(manifest) {
  const digestInput = structuredClone(manifest);
  delete digestInput.manifest_digest;
  delete digestInput.signature;
  delete digestInput.signatures;

  const canonical = canonicalize(digestInput);
  if (canonical === undefined) {
    throw new Error("Manifest cannot be represented as RFC 8785 canonical JSON");
  }

  return `sha256:${createHash("sha256").update(canonical, "utf8").digest("hex")}`;
}

export function manifestSemanticErrors(manifest) {
  const errors = [];
  const skillIds = new Set();

  for (const skill of manifest.skills ?? []) {
    if (skillIds.has(skill.id)) {
      errors.push(`duplicate skill id: ${skill.id}`);
    }
    skillIds.add(skill.id);
  }

  const execution = manifest.execution;
  if (
    execution &&
    execution.default_timeout_seconds > execution.max_timeout_seconds
  ) {
    errors.push("default_timeout_seconds exceeds max_timeout_seconds");
  }

  const requiredProfiles = new Set(
    execution?.capabilities?.required_profiles ?? [],
  );
  for (const capability of execution?.capabilities?.optional_profiles ?? []) {
    if (requiredProfiles.has(capability)) {
      errors.push(`capability cannot be both required and optional: ${capability}`);
    }
  }

  return errors;
}
