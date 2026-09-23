import { createHash } from "node:crypto";
import { lstat, readFile, readdir } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

import Ajv2020 from "ajv/dist/2020.js";
import addFormats from "ajv-formats";
import canonicalize from "canonicalize";
import { isAlias, isMap, isScalar, isSeq, parseDocument } from "yaml";

export const repositoryRoot = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "../..",
);

const jsonNumberPattern = /^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$/u;
const maximumSafeInteger = 9_007_199_254_740_991;

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
  validateJSONSurrogateEscapes(source, label);
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

function digestValidatedManifest(manifest) {
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

const manifestSchemaId =
  "https://arop.invalid/schemas/v1/manifest/agent-manifest-v1.schema.json";
const packageSchemaBase = "https://arop.package.invalid/";
const draft202012Schema = "https://json-schema.org/draft/2020-12/schema";
let manifestValidatorPromise;

function formatAjvErrors(validationErrors = []) {
  return validationErrors
    .map((error) => `${error.instancePath || "/"} ${error.message}`)
    .join("; ");
}

function createAjv() {
  const ajv = new Ajv2020({
    allErrors: true,
    loadSchema: async (uri) => {
      throw new Error(`offline schema loader rejected unresolved reference ${JSON.stringify(uri)}`);
    },
    // Protocol acceptance is Draft 2020-12, not Ajv's opinionated strict
    // subset (strictTypes/strictTuples/ignored-keyword diagnostics). The
    // shared walker independently rejects unknown keywords, wrong dialects,
    // and unsafe package references before Publisher Schema compilation.
    strict: false,
    // Parsed authoring objects are cloned before validation. Never let
    // inherited Object.prototype names satisfy required/properties semantics.
    ownProperties: true,
    validateFormats: true,
  });
  addFormats(ajv);
  ajv.addFormat("date-time", {
    type: "string",
    validate: validatePortableDateTime,
  });
  return ajv;
}

async function manifestValidator() {
  if (!manifestValidatorPromise) {
    manifestValidatorPromise = (async () => {
      const ajv = createAjv();
      const schemaFiles = await walkFiles(
        path.join(repositoryRoot, "schemas"),
        (filePath) => filePath.endsWith(".schema.json"),
      );
      for (const schemaFile of schemaFiles) {
        const schema = await loadStructuredFile(schemaFile);
        if (!ajv.validateSchema(schema)) {
          throw new Error(
            `${path.relative(repositoryRoot, schemaFile)} is not a valid JSON Schema: ` +
              formatAjvErrors(ajv.errors),
          );
        }
        ajv.addSchema(schema);
      }
      const validate = ajv.getSchema(manifestSchemaId);
      if (!validate) {
        throw new Error(`schema not registered: ${manifestSchemaId}`);
      }
      return validate;
    })();
  }
  return manifestValidatorPromise;
}

function manifestWithoutPublicationMetadata(manifest) {
  const digestInput = structuredClone(manifest);
  delete digestInput.manifest_digest;
  delete digestInput.signature;
  delete digestInput.signatures;
  return digestInput;
}

/**
 * Validate a parsed Manifest before any digest is exposed. A packageRoot and
 * manifestPath are required when a declared input/output schema has a relative
 * reference; byte-only callers fail closed because no trustworthy boundary is
 * available.
 */
export async function validateManifestDocument(
  manifest,
  { packageRoot, manifestPath = "manifest.schema.json" } = {},
) {
  rejectDangerousObjectKeys(manifest);
  if (!manifest || typeof manifest !== "object" || Array.isArray(manifest)) {
    throw new Error("manifest root must be an object");
  }
  const value = manifestWithoutPublicationMetadata(manifest);
  const validate = await manifestValidator();
  if (!validate(value)) {
    throw new Error(`manifest schema validation failed: ${formatAjvErrors(validate.errors)}`);
  }
  const semanticErrors = manifestSemanticErrors(value);
  if (semanticErrors.length > 0) {
    throw new Error(`manifest semantic validation failed: ${semanticErrors.join("; ")}`);
  }

  for (const [index, skill] of (value.skills ?? []).entries()) {
    for (const field of ["input_schema", "output_schema"]) {
      try {
        await compileOfflineSchema(skill[field], { packageRoot, manifestPath });
      } catch (error) {
        throw new Error(`skills[${index}].${field}: ${error.message}`);
      }
    }
  }
  const extensionIds = Object.keys(value.extensions ?? {}).sort();
  for (const extensionId of extensionIds) {
    const envelope = value.extensions[extensionId];
    try {
      const declaration = Object.create(null);
      declaration.$ref = envelope.schema_ref;
      const { validate, documents } = await compileOfflineSchema(declaration, {
        packageRoot,
        manifestPath,
      });
      if (documents.size !== 2) {
        throw new Error("extension schemas may only use document-local fragment references");
      }
      const { location, fragmentOnly } = resolveSchemaUri(
        `${packageSchemaBase}${manifestPath}`,
        envelope.schema_ref,
        false,
      );
      if (fragmentOnly || !documents.has(location)) {
        throw new Error("schema_ref must name a package schema file");
      }
      const extensionSchema = documents.get(location);
      validateDocumentLocalReferences(extensionSchema);
      const actualDigest = canonicalValueDigest(extensionSchema);
      if (envelope.schema_digest !== actualDigest) {
        throw new Error(
          `schema_digest is ${JSON.stringify(envelope.schema_digest)}, want ${JSON.stringify(actualDigest)}`,
        );
      }
      if (!validate(envelope.data)) {
        throw new Error(`data failed schema validation: ${formatAjvErrors(validate.errors)}`);
      }
    } catch (error) {
      throw new Error(`extensions[${JSON.stringify(extensionId)}]: ${error.message}`);
    }
  }
  return value;
}

function validateDocumentLocalReferences(value) {
  if (typeof value === "boolean") return;
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("JSON Schema must be an object or boolean");
  }
  for (const keyword of ["$ref", "$dynamicRef"]) {
    if (Object.hasOwn(value, keyword)) {
      if (typeof value[keyword] !== "string" || !value[keyword].startsWith("#")) {
        throw new Error(`Extension Schema ${keyword} must be a document-local fragment reference`);
      }
    }
  }
  for (const [keyword, child] of Object.entries(value)) {
    if (schemaObjectKeywords.has(keyword)) {
      validateDocumentLocalReferences(child);
    } else if (schemaArrayKeywords.has(keyword)) {
      for (const item of child) validateDocumentLocalReferences(item);
    } else if (schemaMapKeywords.has(keyword)) {
      for (const name of Object.keys(child).sort()) {
        validateDocumentLocalReferences(child[name]);
      }
    }
  }
}

export async function manifestFileDigest(filePath, { packageRoot } = {}) {
  const root = await secureDirectory(packageRoot ?? path.dirname(filePath));
  const relativePath = packageRoot
    ? normalizePackageRelativePath(
        path.relative(root, path.resolve(filePath)).split(path.sep).join("/"),
      )
    : normalizePackageRelativePath(path.basename(filePath));
  const manifest = await loadManifestStructuredFileSecure(root, relativePath);
  await validateManifestDocument(manifest, {
    packageRoot: root,
    manifestPath: relativePath,
  });
  return digestValidatedManifest(manifest);
}

export async function resolveRepositoryFile(requestedPath) {
  const relativePath = normalizePackageRelativePath(requestedPath);
  const root = await secureDirectory(repositoryRoot);
  await assertSecurePackagePath(root, relativePath);
  return path.join(root, ...relativePath.split("/"));
}

async function compileOfflineSchema(schema, { packageRoot, manifestPath }) {
  rejectDangerousObjectKeys(schema);
  const ajv = createAjv();
  const rootLocation = `${packageSchemaBase}${manifestPath}`;
  const closure = new Map([[rootLocation, schema]]);
  const owners = new Map([[rootLocation, `${manifestPath}#`]]);
  indexSchemaIds(schema, {
    owners,
    baseLocation: rootLocation,
    owner: `${manifestPath}#`,
  });
  await walkOfflineSchema(schema, {
    closure,
    owners,
    baseLocation: rootLocation,
    owner: `${manifestPath}#`,
    packageRoot,
  });
  for (const [location, document] of closure) {
    // Ajv does not consistently retain the retrieval URI as the base when a
    // loaded document declares a relative $id. Compile a private copy with
    // absolute identifiers while preserving closure's original values for
    // schema_digest and Manifest digest computation.
    const compilationDocument = structuredClone(document);
    absolutizeSchemaIdentifiers(compilationDocument, location);
    if (!ajv.validateSchema(compilationDocument)) {
      throw new Error(
        `invalid JSON Schema at ${location}: ${formatAjvErrors(ajv.errors)}`,
      );
    }
    ajv.addSchema(compilationDocument, location);
  }
  try {
    const validate = ajv.getSchema(rootLocation);
    if (!validate) {
      throw new Error("root schema was not registered");
    }
    return { validate, documents: closure };
  } catch (error) {
    throw new Error(`compile offline schema closure: ${error.message}`);
  }
}

// Register all embedded Schema Resources in a physical document before
// resolving any reference. `$ref` lookup is independent of JSON member order,
// so a sibling may target a `$id` declared later in the document.
function indexSchemaIds(value, { owners, baseLocation, owner }) {
  if (typeof value === "boolean") return;
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("JSON Schema must be an object or boolean");
  }
  let currentBase = baseLocation;
  if (Object.hasOwn(value, "$id")) {
    if (typeof value.$id !== "string") throw new Error("$id must be a string");
    const resolved = resolveSchemaUri(baseLocation, value.$id, true).location;
    const previous = owners.get(resolved);
    if (previous !== undefined && previous !== owner) {
      throw new Error(
        `duplicate resolved $id ${JSON.stringify(resolved)} in ${previous} and ${owner}`,
      );
    }
    owners.set(resolved, owner);
    currentBase = resolved;
  }
  for (const [keyword, child] of Object.entries(value)) {
    if (schemaObjectKeywords.has(keyword)) {
      indexSchemaIds(child, {
        owners,
        baseLocation: currentBase,
        owner: `${owner}/${keyword}`,
      });
    } else if (schemaArrayKeywords.has(keyword)) {
      if (!Array.isArray(child)) {
        throw new Error(`JSON Schema keyword ${JSON.stringify(keyword)} must be an array`);
      }
      for (const [index, item] of child.entries()) {
        indexSchemaIds(item, {
          owners,
          baseLocation: currentBase,
          owner: `${owner}/${keyword}/${index}`,
        });
      }
    } else if (schemaMapKeywords.has(keyword)) {
      if (!child || typeof child !== "object" || Array.isArray(child)) {
        throw new Error(`JSON Schema keyword ${JSON.stringify(keyword)} must be an object`);
      }
      for (const name of Object.keys(child).sort()) {
        indexSchemaIds(child[name], {
          owners,
          baseLocation: currentBase,
          owner: `${owner}/${keyword}/${name}`,
        });
      }
    }
  }
}

function absolutizeSchemaIdentifiers(value, baseLocation) {
  if (typeof value === "boolean") return;
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("JSON Schema must be an object or boolean");
  }

  let currentBase = baseLocation;
  if (Object.hasOwn(value, "$id")) {
    currentBase = resolveSchemaUri(baseLocation, value.$id, true).location;
    value.$id = currentBase;
  }
  if (Object.hasOwn(value, "pattern")) {
    value.pattern = compilePortablePatternSource(value.pattern);
  }

  for (const [keyword, child] of Object.entries(value)) {
    if (schemaObjectKeywords.has(keyword)) {
      absolutizeSchemaIdentifiers(child, currentBase);
    } else if (schemaArrayKeywords.has(keyword)) {
      for (const item of child) absolutizeSchemaIdentifiers(item, currentBase);
    } else if (schemaMapKeywords.has(keyword)) {
      for (const name of Object.keys(child).sort()) {
        absolutizeSchemaIdentifiers(child[name], currentBase);
      }
    }
  }
}

async function walkOfflineSchema(
  value,
  { closure, owners, baseLocation, owner, packageRoot },
) {
  if (typeof value === "boolean") return;
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("JSON Schema must be an object or boolean");
  }

  for (const keyword of Object.keys(value)) {
    if (!allowedSchemaKeywords.has(keyword)) {
      throw new Error(`unknown JSON Schema keyword ${JSON.stringify(keyword)}`);
    }
  }
  if (Object.hasOwn(value, "$schema") && value.$schema !== draft202012Schema) {
    throw new Error(`$schema must be exactly ${JSON.stringify(draft202012Schema)}`);
  }
  if (Object.hasOwn(value, "format") && value.format !== "date-time") {
    throw new Error("Publisher Schema format must be exactly date-time");
  }
  if (Object.hasOwn(value, "pattern")) validatePortablePattern(value.pattern);
  rejectDangerousSchemaPropertyNames(value);

  let currentBase = baseLocation;
  if (Object.hasOwn(value, "$id")) {
    if (typeof value.$id !== "string") throw new Error("$id must be a string");
    const resolved = resolveSchemaUri(baseLocation, value.$id, true).location;
    const previous = owners.get(resolved);
    if (previous !== undefined && previous !== owner) {
      throw new Error(
        `duplicate resolved $id ${JSON.stringify(resolved)} in ${previous} and ${owner}`,
      );
    }
    owners.set(resolved, owner);
    currentBase = resolved;
  }

  for (const keyword of ["$ref", "$dynamicRef"]) {
    if (!Object.hasOwn(value, keyword)) continue;
    const reference = value[keyword];
    if (typeof reference !== "string") throw new Error(`${keyword} must be a string`);
    const { location, fragmentOnly } = resolveSchemaUri(currentBase, reference, false);
    if (fragmentOnly) continue;
    if (!packageRoot) {
      throw new Error(
        `relative schema reference ${JSON.stringify(reference)} requires a package file API`,
      );
    }
    if (closure.has(location) || owners.has(location)) continue;
    const targetUrl = new URL(location);
    const targetPath = normalizePackageRelativePath(targetUrl.pathname.slice(1));
    const document = await loadManifestStructuredFileSecure(packageRoot, targetPath);
    rejectDangerousObjectKeys(document);
    closure.set(location, document);
    owners.set(location, `${targetPath}#`);
    indexSchemaIds(document, {
      owners,
      baseLocation: location,
      owner: `${targetPath}#`,
    });
    await walkOfflineSchema(document, {
      closure,
      owners,
      baseLocation: location,
      owner: `${targetPath}#`,
      packageRoot,
    });
  }

  for (const [keyword, child] of Object.entries(value)) {
    if (schemaObjectKeywords.has(keyword)) {
      await walkOfflineSchema(child, {
        closure,
        owners,
        baseLocation: currentBase,
        owner: `${owner}/${keyword}`,
        packageRoot,
      });
    } else if (schemaArrayKeywords.has(keyword)) {
      if (!Array.isArray(child)) throw new Error(`JSON Schema keyword ${JSON.stringify(keyword)} must be an array`);
      for (const [index, item] of child.entries()) {
        await walkOfflineSchema(item, {
          closure,
          owners,
          baseLocation: currentBase,
          owner: `${owner}/${keyword}/${index}`,
          packageRoot,
        });
      }
    } else if (schemaMapKeywords.has(keyword)) {
      if (!child || typeof child !== "object" || Array.isArray(child)) {
        throw new Error(`JSON Schema keyword ${JSON.stringify(keyword)} must be an object`);
      }
      for (const name of Object.keys(child).sort()) {
        await walkOfflineSchema(child[name], {
          closure,
          owners,
          baseLocation: currentBase,
          owner: `${owner}/${keyword}/${name}`,
          packageRoot,
        });
      }
    }
  }
}

const allowedSchemaKeywords = new Set([
  "$schema", "$id", "$ref", "$anchor", "$dynamicRef", "$dynamicAnchor",
  "$vocabulary", "$comment", "$defs", "type", "enum", "const",
  "maximum", "exclusiveMaximum", "minimum", "exclusiveMinimum",
  "maxLength", "minLength", "pattern", "maxItems", "minItems", "uniqueItems",
  "maxContains", "minContains", "items", "prefixItems", "contains", "unevaluatedItems",
  "maxProperties", "minProperties", "required", "dependentRequired", "dependentSchemas",
  "properties", "additionalProperties", "unevaluatedProperties", "propertyNames",
  "allOf", "anyOf", "oneOf", "not", "if", "then", "else",
  "format", "contentEncoding", "contentMediaType", "contentSchema",
  "title", "description", "default", "deprecated", "readOnly", "writeOnly", "examples",
]);
const schemaObjectKeywords = new Set([
  "items", "contains", "unevaluatedItems", "additionalProperties", "unevaluatedProperties",
  "propertyNames", "not", "if", "then", "else", "contentSchema",
]);
const schemaArrayKeywords = new Set(["prefixItems", "allOf", "anyOf", "oneOf"]);
const schemaMapKeywords = new Set(["$defs", "properties", "dependentSchemas"]);

function resolveSchemaUri(baseLocation, reference, identifier) {
  const label = identifier ? "schema $id" : "schema reference";
  if (typeof reference !== "string") throw new Error(`${label} must be a string`);
  if (identifier && reference === "") throw new Error("schema $id must not be empty");
  if (/[^\x21-\x7e]/u.test(reference)) {
    throw new Error(`${label} ${JSON.stringify(reference)} contains characters outside visible ASCII`);
  }
  if (reference.includes("\\") || reference.includes("%")) {
    throw new Error(`${label} ${JSON.stringify(reference)} uses encoded or backslash path syntax`);
  }
  if (/[?]/u.test(reference)) throw new Error(`${label} ${JSON.stringify(reference)} contains a forbidden query`);
  if (identifier && reference.includes("#")) {
    throw new Error(`${label} ${JSON.stringify(reference)} contains a forbidden fragment`);
  }
  const hashIndex = reference.indexOf("#");
  const rawPath = hashIndex === -1 ? reference : reference.slice(0, hashIndex);
  const rawFragment = hashIndex === -1 ? "" : reference.slice(hashIndex + 1);
  if (rawFragment.includes("#")) {
    throw new Error(`${label} ${JSON.stringify(reference)} contains multiple fragments`);
  }
  if (/^[A-Za-z][A-Za-z0-9+.-]*:/u.test(rawPath) || rawPath.startsWith("//") || rawPath.startsWith("/")) {
    throw new Error(`${label} ${JSON.stringify(reference)} is not package-relative`);
  }
  if (rawPath !== "") normalizePackageRelativePath(rawPath);
  validateSchemaFragment(rawFragment);
  let resolved;
  try {
    resolved = new URL(reference, baseLocation);
  } catch (error) {
    throw new Error(`parse ${label} ${JSON.stringify(reference)}: ${error.message}`);
  }
  if (resolved.protocol !== "https:" || resolved.hostname !== "arop.package.invalid" || resolved.username || resolved.password || resolved.search) {
    throw new Error(`${label} ${JSON.stringify(reference)} escapes the package`);
  }
  const fragmentOnly = rawPath === "";
  resolved.hash = "";
  normalizePackageRelativePath(resolved.pathname.slice(1));
  return { location: resolved.href, fragmentOnly };
}

function canonicalValueDigest(value) {
  const canonical = canonicalize(value);
  if (canonical === undefined) throw new Error("value cannot be represented as RFC 8785 canonical JSON");
  return `sha256:${createHash("sha256").update(canonical, "utf8").digest("hex")}`;
}

function normalizePackageRelativePath(relativePath) {
  if (typeof relativePath !== "string" || !relativePath) {
    throw new Error("package path must be a non-empty string");
  }
  if (relativePath.includes("\\")) throw new Error("backslashes are forbidden");
  if (path.posix.isAbsolute(relativePath) || path.isAbsolute(relativePath)) {
    throw new Error("absolute paths are forbidden");
  }
  if (/^[A-Za-z]:/u.test(relativePath)) throw new Error("absolute paths are forbidden");
  const components = relativePath.split("/");
  if (components.some((component) => component === "..")) {
    throw new Error("path traversal is forbidden");
  }
  if (components.some((component) => component === "")) {
    throw new Error("empty path components are forbidden");
  }
  for (const [index, component] of components.entries()) {
    if (component !== "." && !/^[A-Za-z0-9._~-]+$/u.test(component)) {
      throw new Error(`path component ${JSON.stringify(component)} is outside the portable grammar [A-Za-z0-9._~-]+`);
    }
    if (component === "." && index !== 0) {
      throw new Error("dot path component is only allowed as a leading ./ prefix");
    }
  }
  const normalized = path.posix.normalize(relativePath);
  if (normalized === "." || normalized.startsWith("../")) {
    throw new Error("path must name a package file");
  }
  return normalized;
}

function validateSchemaFragment(fragment) {
  if (fragment === "") return;
  if (fragment.startsWith("/")) {
    if (!/^[A-Za-z0-9._~$/-]*$/u.test(fragment)) {
      throw new Error("JSON Pointer fragment contains non-portable characters");
    }
    for (let index = 0; index < fragment.length; index += 1) {
      if (fragment[index] === "~" && !["0", "1"].includes(fragment[index + 1])) {
        throw new Error("JSON Pointer fragment contains an invalid ~ escape");
      }
    }
    return;
  }
  if (!/^[A-Za-z_][A-Za-z0-9._:-]*$/u.test(fragment)) {
    throw new Error("anchor fragment is outside the portable grammar");
  }
}

async function secureDirectory(directory) {
  const absolute = path.resolve(directory);
  const parsed = path.parse(absolute);
  let current = parsed.root;
  for (const component of absolute.slice(parsed.root.length).split(path.sep).filter(Boolean)) {
    current = path.join(current, component);
    const ancestor = await lstat(current);
    if (ancestor.isSymbolicLink()) {
      throw new Error(`package root lexical ancestor must not be a symlink: ${current}`);
    }
  }
  const info = await lstat(absolute);
  if (!info.isDirectory()) {
    throw new Error("package root must be a real directory, not a symlink");
  }
  return absolute;
}

async function assertSecurePackagePath(root, relativePath) {
  const normalized = normalizePackageRelativePath(relativePath);
  let current = root;
  const components = normalized.split("/");
  for (const [index, component] of components.entries()) {
    await assertExactDirectoryEntry(current, component);
    current = path.join(current, component);
    const info = await lstat(current);
    if (info.isSymbolicLink()) {
      throw new Error(
        `symlink path component is forbidden: ${components.slice(0, index + 1).join("/")}`,
      );
    }
    if (index < components.length - 1 && !info.isDirectory()) {
      throw new Error(
        `non-directory path component: ${components.slice(0, index + 1).join("/")}`,
      );
    }
    if (index === components.length - 1 && !info.isFile()) {
      throw new Error(`package reference is not a regular file: ${normalized}`);
    }
  }
}

async function assertExactDirectoryEntry(parent, component) {
  const entries = await readdir(parent);
  if (entries.includes(component)) return;
  const folded = entries.find(
    (entry) => entry.localeCompare(component, "en", { sensitivity: "accent" }) === 0,
  );
  if (folded !== undefined) {
    throw new Error(
      `path component ${JSON.stringify(component)} does not exactly match on-disk name ${JSON.stringify(folded)}`,
    );
  }
  throw new Error(
    `path component ${JSON.stringify(component)} does not exist below ${JSON.stringify(parent)}`,
  );
}

async function loadStructuredFileSecure(root, relativePath) {
  const normalized = normalizePackageRelativePath(relativePath);
  await assertSecurePackagePath(root, normalized);
  return loadStructuredFile(path.join(root, ...normalized.split("/")));
}

async function loadManifestStructuredFileSecure(root, relativePath) {
  const normalized = normalizePackageRelativePath(relativePath);
  await assertSecurePackagePath(root, normalized);
  const filePath = path.join(root, ...normalized.split("/"));
  const bytes = await readFile(filePath);
  const source = decodeUTF8(bytes, normalized);
  if (normalized.toLowerCase().endsWith(".json")) {
    return parseManifestJSON(source, normalized);
  }
  return parseManifestYAML(source, normalized);
}

function decodeUTF8(bytes, label) {
  for (let index = 0; index < bytes.length; index += 1) {
    const first = bytes[index];
    if (first <= 0x7f) continue;
    let length;
    let minimum;
    if (first >= 0xc2 && first <= 0xdf) {
      length = 2;
      minimum = 0x80;
    } else if (first >= 0xe0 && first <= 0xef) {
      length = 3;
      minimum = 0x800;
    } else if (first >= 0xf0 && first <= 0xf4) {
      length = 4;
      minimum = 0x10000;
    } else {
      throw new Error(`${label} is not valid UTF-8`);
    }
    if (index + length > bytes.length) throw new Error(`${label} is not valid UTF-8`);
    let codePoint = first & (0x7f >> length);
    for (let offset = 1; offset < length; offset += 1) {
      const continuation = bytes[index + offset];
      if ((continuation & 0xc0) !== 0x80) throw new Error(`${label} is not valid UTF-8`);
      codePoint = (codePoint << 6) | (continuation & 0x3f);
    }
    if (
      codePoint < minimum ||
      (codePoint >= 0xd800 && codePoint <= 0xdfff) ||
      codePoint > 0x10ffff
    ) {
      throw new Error(`${label} is not valid UTF-8`);
    }
    index += length - 1;
  }
  return bytes.toString("utf8");
}

function parseManifestJSON(source, label) {
  validateJSONSurrogateEscapes(source, label);
  let index = 0;
  const fail = (message) => {
    throw new SyntaxError(`${label}:${index}: ${message}`);
  };
  const skipWhitespace = () => {
    while (/[\t\n\r ]/u.test(source[index] ?? "")) index += 1;
  };
  const parseString = () => {
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
        try {
          return JSON.parse(source.slice(start, index));
        } catch (error) {
          fail(error.message);
        }
      }
    }
    fail("unterminated string");
  };
  const parseValue = () => {
    skipWhitespace();
    if (source[index] === "{") return parseObject();
    if (source[index] === "[") return parseArray();
    if (source[index] === '"') return parseString();
    const start = index;
    while (index < source.length && !/[\t\n\r ,\]}]/u.test(source[index])) index += 1;
    const token = source.slice(start, index);
    if (token === "true") return true;
    if (token === "false") return false;
    if (token === "null") return null;
    if (jsonNumberPattern.test(token)) return normalizeJSONNumber(token);
    fail(`invalid JSON value ${JSON.stringify(token)}`);
  };
  const parseArray = () => {
    index += 1;
    skipWhitespace();
    const value = [];
    if (source[index] === "]") {
      index += 1;
      return value;
    }
    while (index < source.length) {
      value.push(parseValue());
      skipWhitespace();
      if (source[index] === "]") {
        index += 1;
        return value;
      }
      if (source[index] !== ",") fail("expected ',' or ']' in array");
      index += 1;
    }
    fail("unterminated array");
  };
  const parseObject = () => {
    index += 1;
    skipWhitespace();
    const value = Object.create(null);
    const keys = new Set();
    if (source[index] === "}") {
      index += 1;
      return value;
    }
    while (index < source.length) {
      const key = parseString();
      if (keys.has(key)) fail(`duplicate object key ${JSON.stringify(key)}`);
      keys.add(key);
      skipWhitespace();
      if (source[index] !== ":") fail("expected ':' after object key");
      index += 1;
      value[key] = parseValue();
      skipWhitespace();
      if (source[index] === "}") {
        index += 1;
        return value;
      }
      if (source[index] !== ",") fail("expected ',' or '}' in object");
      index += 1;
      skipWhitespace();
    }
    fail("unterminated object");
  };
  const value = parseValue();
  skipWhitespace();
  if (index !== source.length) fail("unexpected trailing content");
  return value;
}

function validateJSONSurrogateEscapes(source, label) {
  for (let index = 0; index < source.length; index += 1) {
    if (source[index] !== '"') continue;
    index += 1;
    while (index < source.length && source[index] !== '"') {
      if (source[index] !== "\\") {
        index += 1;
        continue;
      }
      if (source[index + 1] !== "u") {
        index += 2;
        continue;
      }
      const unit = parseJSONUTF16Unit(source, index, label);
      if (unit >= 0xd800 && unit <= 0xdbff) {
        const next = index + 6;
        if (source[next] !== "\\" || source[next + 1] !== "u") {
          throw new SyntaxError(`${label}:${index}: unpaired high surrogate escape`);
        }
        const low = parseJSONUTF16Unit(source, next, label);
        if (low < 0xdc00 || low > 0xdfff) {
          throw new SyntaxError(`${label}:${index}: unpaired high surrogate escape`);
        }
        index = next + 6;
        continue;
      }
      if (unit >= 0xdc00 && unit <= 0xdfff) {
        throw new SyntaxError(`${label}:${index}: unpaired low surrogate escape`);
      }
      index += 6;
    }
  }
}

function parseJSONUTF16Unit(source, escapeIndex, label) {
  const digits = source.slice(escapeIndex + 2, escapeIndex + 6);
  if (!/^[0-9A-Fa-f]{4}$/u.test(digits)) {
    throw new SyntaxError(`${label}:${escapeIndex}: invalid JSON unicode escape`);
  }
  return Number.parseInt(digits, 16);
}

function parseManifestYAML(source, label) {
  const document = parseDocument(source, {
    prettyErrors: true,
    schema: "failsafe",
    strict: true,
    uniqueKeys: true,
  });
  if (document.errors.length > 0) {
    throw new Error(document.errors.map((error) => error.message).join("\n"));
  }
  if (document.contents === null) throw new Error(`${label} is empty`);
  return yamlJSONValue(document.contents);
}

function yamlJSONValue(node) {
  if (isAlias(node) || node?.anchor) {
    throw new Error("YAML aliases and anchors are forbidden");
  }
  if (node?.tag && !jsonCompatibleYAMLTag(node.tag)) {
    throw new Error(`explicit non-JSON YAML tag ${JSON.stringify(node.tag)} is forbidden`);
  }
  if (isMap(node)) {
    if (node.tag && !["!!map", "tag:yaml.org,2002:map"].includes(node.tag)) {
      throw new Error(`mapping has incompatible explicit tag ${JSON.stringify(node.tag)}`);
    }
    const value = Object.create(null);
    const keys = new Set();
    for (const pair of node.items) {
      const key = yamlJSONValue(pair.key);
      if (typeof key !== "string") throw new Error("manifest mapping keys must be strings");
      if (keys.has(key)) throw new Error(`duplicate mapping key ${JSON.stringify(key)}`);
      keys.add(key);
      value[key] = yamlJSONValue(pair.value);
    }
    return value;
  }
  if (isSeq(node)) {
    if (node.tag && !["!!seq", "tag:yaml.org,2002:seq"].includes(node.tag)) {
      throw new Error(`sequence has incompatible explicit tag ${JSON.stringify(node.tag)}`);
    }
    return node.items.map((item) => yamlJSONValue(item));
  }
  if (!isScalar(node)) throw new Error("unsupported YAML node");
  return yamlJSONScalar(node);
}

function yamlJSONScalar(node) {
  const raw = node.source ?? String(node.value ?? "");
  if (node.tag) {
    if (["!!str", "tag:yaml.org,2002:str"].includes(node.tag)) return String(node.value);
    if (["!!null", "tag:yaml.org,2002:null"].includes(node.tag)) {
      if (raw !== "null") throw new Error(`explicit null must use the JSON literal null, got ${JSON.stringify(raw)}`);
      return null;
    }
    if (["!!bool", "tag:yaml.org,2002:bool"].includes(node.tag)) {
      if (raw === "true") return true;
      if (raw === "false") return false;
      throw new Error(`explicit boolean must use true or false, got ${JSON.stringify(raw)}`);
    }
    if (["!!int", "!!float", "tag:yaml.org,2002:int", "tag:yaml.org,2002:float"].includes(node.tag)) {
      return normalizeJSONNumber(raw);
    }
  }
  if (node.type && node.type !== "PLAIN") return node.value;
  if (raw === "null") return null;
  if (raw === "true") return true;
  if (raw === "false") return false;
  if (jsonNumberPattern.test(raw)) return normalizeJSONNumber(raw);
  return raw;
}

function jsonCompatibleYAMLTag(tag) {
  return new Set([
    "!!map", "tag:yaml.org,2002:map",
    "!!seq", "tag:yaml.org,2002:seq",
    "!!str", "tag:yaml.org,2002:str",
    "!!null", "tag:yaml.org,2002:null",
    "!!bool", "tag:yaml.org,2002:bool",
    "!!int", "tag:yaml.org,2002:int",
    "!!float", "tag:yaml.org,2002:float",
  ]).has(tag);
}

function normalizeJSONNumber(raw) {
  if (!jsonNumberPattern.test(raw)) {
    throw new Error(`non-JSON numeric scalar ${JSON.stringify(raw)} is not allowed by an explicit numeric tag`);
  }
  const value = Number(raw);
  if (!Number.isFinite(value)) throw new Error(`number ${JSON.stringify(raw)} is not finite`);
  const significand = raw.split(/[eE]/u, 1)[0];
  if (value === 0 && /[1-9]/u.test(significand)) {
    throw new Error(`number ${JSON.stringify(raw)} underflows the interoperable IEEE-754 range`);
  }
  if (Number.isInteger(value) && Math.abs(value) > maximumSafeInteger) {
    throw new Error(`integer ${JSON.stringify(raw)} exceeds the interoperable safe range`);
  }
  return value;
}

function validatePortablePattern(pattern) {
  if (
    typeof pattern !== "string" ||
    pattern.length < 2 ||
    pattern.length > 512 ||
    !pattern.startsWith("^") ||
    !pattern.endsWith("$")
  ) {
    throw new Error("pattern must be a full-match ASCII expression (^...$) of at most 512 bytes");
  }
  if (/[^\x20-\x7e]/u.test(pattern)) {
    throw new Error("pattern must contain visible ASCII only");
  }
  const body = pattern.slice(1, -1);
  for (let index = 0; index < body.length; ) {
    if (body[index] === "[") {
      const end = body.indexOf("]", index + 1);
      if (end === -1) throw new Error("pattern contains an unterminated character class");
      validatePortableCharacterClass(body.slice(index + 1, end));
      index = end + 1;
    } else {
      if (!/[A-Za-z0-9 _:/@,\-]/u.test(body[index])) {
        throw new Error(`pattern contains forbidden token ${JSON.stringify(body[index])}`);
      }
      index += 1;
    }
    if (body[index] === "{") {
      const end = body.indexOf("}", index + 1);
      if (end === -1) throw new Error("pattern contains an unterminated fixed repetition");
      const digits = body.slice(index + 1, end);
      const count = Number(digits);
      if (!/^(?:[1-9]|[1-9][0-9]|1[0-9]{2}|2[0-4][0-9]|25[0-6])$/u.test(digits) || count < 1 || count > 256) {
        throw new Error("pattern repetition must be canonical {n} with 1 <= n <= 256");
      }
      index = end + 1;
    }
  }
}

function validatePortableCharacterClass(characterClass) {
  if (characterClass === "" || characterClass.startsWith("^")) {
    throw new Error("pattern character class must be non-empty and non-negated");
  }
  for (let index = 0; index < characterClass.length; index += 1) {
    const character = characterClass[index];
    if (/[A-Za-z0-9 _:/@,]/u.test(character)) continue;
    if (character === "-") {
      if (index === 0 || index === characterClass.length - 1) continue;
      const left = characterClass[index - 1];
      const right = characterClass[index + 1];
      if (samePortableRangeClass(left, right) && left.codePointAt(0) <= right.codePointAt(0)) continue;
    }
    throw new Error(`pattern character class contains forbidden token ${JSON.stringify(character)}`);
  }
}

function samePortableRangeClass(left, right) {
  return (
    (/^[0-9]$/u.test(left) && /^[0-9]$/u.test(right)) ||
    (/^[A-Z]$/u.test(left) && /^[A-Z]$/u.test(right)) ||
    (/^[a-z]$/u.test(left) && /^[a-z]$/u.test(right))
  );
}

function compilePortablePatternSource(pattern) {
  validatePortablePattern(pattern);
  return `^(?:${pattern.slice(1, -1)})$(?![\\s\\S])`;
}

function validatePortableDateTime(value) {
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.\d{1,9})?(?:Z|[+-](\d{2}):(\d{2}))$/u.exec(value);
  if (!match) return false;
  const numbers = match.slice(1).map((item) => (item === undefined ? 0 : Number(item)));
  const [year, month, day, hour, minute, second, offsetHour, offsetMinute] = numbers;
  return (
    month >= 1 && month <= 12 &&
    day >= 1 && day <= daysInGregorianMonth(year, month) &&
    hour <= 23 && minute <= 59 && second <= 59 &&
    offsetHour <= 23 && offsetMinute <= 59
  );
}

function daysInGregorianMonth(year, month) {
  if (month === 2 && (year % 400 === 0 || (year % 4 === 0 && year % 100 !== 0))) return 29;
  return [0, 31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31][month] ?? 0;
}

function rejectDangerousObjectKeys(value) {
  if (Array.isArray(value)) {
    for (const child of value) rejectDangerousObjectKeys(child);
    return;
  }
  if (!value || typeof value !== "object") return;
  for (const key of Object.keys(value)) {
    if (["__proto__", "prototype", "constructor"].includes(key)) {
      throw new Error(`dangerous object key ${JSON.stringify(key)} is forbidden`);
    }
    rejectDangerousObjectKeys(value[key]);
  }
}

function isDangerousObjectKey(key) {
  return ["__proto__", "prototype", "constructor"].includes(key);
}

function rejectDangerousSchemaPropertyNames(schema) {
  for (const value of schema.required ?? []) {
    if (isDangerousObjectKey(value)) {
      throw new Error(`dangerous schema property name ${JSON.stringify(value)} is forbidden in required`);
    }
  }
  for (const values of Object.values(schema.dependentRequired ?? {})) {
    for (const value of values) {
      if (isDangerousObjectKey(value)) {
        throw new Error(`dangerous schema property name ${JSON.stringify(value)} is forbidden in dependentRequired`);
      }
    }
  }
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

  const extensions = manifest.extensions ?? {};
  for (const extension of manifest.required_extensions ?? []) {
    if (!Object.hasOwn(extensions, extension)) {
      errors.push(`required extension has no payload: ${extension}`);
    }
  }

  return errors;
}
