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

export async function loadStructuredFile(filePath) {
  const source = await readFile(filePath, "utf8");
  if (filePath.endsWith(".json")) {
    return JSON.parse(source);
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
