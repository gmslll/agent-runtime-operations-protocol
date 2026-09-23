import { readFile } from "node:fs/promises";
import path from "node:path";

import Ajv2020 from "ajv/dist/2020.js";
import addFormats from "ajv-formats";

import {
  loadStructuredFile,
  manifestFileDigest,
  repositoryRoot,
  walkFiles,
} from "./lib/repository.mjs";

const manifestSchemaId =
  "https://arop.invalid/schemas/v1/manifest/agent-manifest-v1.schema.json";
const errors = [];

function relative(filePath) {
  return path.relative(repositoryRoot, filePath);
}

function formatAjvErrors(validationErrors = []) {
  return validationErrors
    .map((error) => `${error.instancePath || "/"} ${error.message}`)
    .join("; ");
}

const ajv = new Ajv2020({
  allErrors: true,
  strict: true,
  validateFormats: true,
});
addFormats(ajv);

const schemaFiles = await walkFiles(
  path.join(repositoryRoot, "schemas"),
  (filePath) => filePath.endsWith(".schema.json"),
);

for (const schemaFile of schemaFiles) {
  try {
    const schema = await loadStructuredFile(schemaFile);
    if (!ajv.validateSchema(schema)) {
      errors.push(
        `${relative(schemaFile)} is not a valid JSON Schema: ${formatAjvErrors(ajv.errors)}`,
      );
      continue;
    }
    ajv.addSchema(schema);
  } catch (error) {
    errors.push(`${relative(schemaFile)} cannot be loaded: ${error.message}`);
  }
}

try {
  if (!ajv.getSchema(manifestSchemaId)) {
    throw new Error(`schema not registered: ${manifestSchemaId}`);
  }
} catch (error) {
  errors.push(`Manifest schema cannot be compiled: ${error.message}`);
}

async function validateManifestFixture(filePath, expectedValid) {
  try {
    await manifestFileDigest(filePath);
    if (!expectedValid) {
      errors.push(`${relative(filePath)} should be rejected but passed all checks`);
    }
  } catch (error) {
    if (expectedValid) {
      errors.push(`${relative(filePath)} should be valid: ${error.message}`);
    }
  }
}

const fixturePredicate = (filePath) =>
  filePath.endsWith(".yaml") ||
  filePath.endsWith(".yml") ||
  filePath.endsWith(".json");

const validManifestFiles = await walkFiles(
  path.join(repositoryRoot, "examples/manifests/valid"),
  fixturePredicate,
);
const invalidManifestFiles = await walkFiles(
  path.join(repositoryRoot, "examples/manifests/invalid"),
  fixturePredicate,
);

for (const filePath of validManifestFiles) {
  await validateManifestFixture(filePath, true);
}
for (const filePath of invalidManifestFiles) {
  await validateManifestFixture(filePath, false);
}

try {
  const digestFile = path.join(repositoryRoot, "examples/manifests/digests.json");
  const expectedDigests = JSON.parse(await readFile(digestFile, "utf8"));
  for (const [manifestPath, expectedDigest] of Object.entries(expectedDigests)) {
    const actualDigest = await manifestFileDigest(
      path.join(repositoryRoot, manifestPath),
    );
    if (actualDigest !== expectedDigest) {
      errors.push(
        `${manifestPath} digest mismatch: expected ${expectedDigest}, got ${actualDigest}`,
      );
    }
  }
} catch (error) {
  errors.push(`Manifest digest fixtures cannot be checked: ${error.message}`);
}

const markdownFiles = await walkFiles(repositoryRoot, (filePath) =>
  filePath.endsWith(".md"),
);
const markdownLinkPattern = /\[[^\]]+\]\(([^)]+)\)/g;

for (const markdownFile of markdownFiles) {
  const markdown = await readFile(markdownFile, "utf8");
  for (const match of markdown.matchAll(markdownLinkPattern)) {
    const target = match[1].replace(/^<|>$/g, "").split("#", 1)[0];
    if (!target || /^(?:https?:|mailto:)/.test(target)) {
      continue;
    }

    const resolvedTarget = path.resolve(path.dirname(markdownFile), target);
    try {
      await readFile(resolvedTarget);
    } catch {
      errors.push(`${relative(markdownFile)} has broken local link: ${match[1]}`);
    }
  }
}

if (errors.length > 0) {
  console.error(`AROP validation failed with ${errors.length} error(s):`);
  for (const error of errors) {
    console.error(`- ${error}`);
  }
  process.exit(1);
}

console.log(
  `AROP validation passed: ${schemaFiles.length} schemas, ` +
    `${validManifestFiles.length} valid manifests, ` +
    `${invalidManifestFiles.length} negative manifests, ` +
    `${markdownFiles.length} Markdown files.`,
);
