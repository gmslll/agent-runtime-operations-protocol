import { createHash } from "node:crypto";
import {
  chmod,
  lstat,
  mkdir,
  readFile,
  rename,
  rm,
  writeFile,
} from "node:fs/promises";
import path from "node:path";

import Ajv2020 from "ajv/dist/2020.js";
import addFormats from "ajv-formats";

import {
  parseJSONWithUniqueKeys,
  repositoryRoot,
} from "./lib/repository.mjs";

const pipelineID = "arop-codegen-v1";
const mappingProfile = "arop-wire-model-v1";
const draft202012 = "https://json-schema.org/draft/2020-12/schema";
const safeIntegerLimit = 9_007_199_254_740_991;
const pythonKeywords = new Set(["False", "None", "True", "and", "as", "assert", "async", "await", "break", "class", "continue", "def", "del", "elif", "else", "except", "finally", "for", "from", "global", "if", "import", "in", "is", "lambda", "nonlocal", "not", "or", "pass", "raise", "return", "try", "while", "with", "yield"]);
const toolFiles = [
  "scripts/generate.mjs",
  "scripts/lib/repository.mjs",
  "package.json",
  "package-lock.json",
];

function fail(message) {
  throw new Error(message);
}

function sha256(data) {
  return createHash("sha256").update(data).digest("hex");
}

function byteCompare(left, right) {
  return Buffer.compare(Buffer.from(left, "utf8"), Buffer.from(right, "utf8"));
}

function slash(value) {
  return value.split(path.sep).join("/");
}

function safeRelative(value, label) {
  if (typeof value !== "string" || value.length === 0 || value.includes("\0")) {
    fail(`${label} must be a non-empty path`);
  }
  if (path.isAbsolute(value) || /^[A-Za-z]:[\\/]/u.test(value)) {
    fail(`${label} must be repository-relative`);
  }
  const normalized = slash(path.normalize(value));
  if (normalized === "." || normalized === ".." || normalized.startsWith("../")) {
    fail(`${label} escapes its root`);
  }
  return normalized;
}

function within(root, candidate, label) {
  const relative = path.relative(root, candidate);
  if (relative === "" || (!relative.startsWith(`..${path.sep}`) && relative !== ".." && !path.isAbsolute(relative))) {
    return;
  }
  fail(`${label} escapes ${slash(root)}`);
}

async function rejectSymlinkPath(absolutePath, allowMissingLeaf = false) {
  const relative = path.relative(repositoryRoot, absolutePath);
  within(repositoryRoot, absolutePath, "path");
  let current = repositoryRoot;
  const components = relative.split(path.sep).filter((component) => component.length > 0);
  for (let index = 0; index < components.length; index += 1) {
    current = path.join(current, components[index]);
    let information;
    try {
      information = await lstat(current);
    } catch (error) {
      if (allowMissingLeaf && error?.code === "ENOENT") return;
      throw error;
    }
    if (information.isSymbolicLink()) fail(`symlink path component is forbidden: ${slash(path.relative(repositoryRoot, current))}`);
    if (index + 1 < components.length && !information.isDirectory()) {
      fail(`non-directory path component: ${slash(path.relative(repositoryRoot, current))}`);
    }
  }
}

async function readRegular(relativePath, label = "input") {
  const safe = safeRelative(relativePath, label);
  const absolute = path.join(repositoryRoot, safe);
  await rejectSymlinkPath(absolute);
  const before = await lstat(absolute);
  if (!before.isFile()) fail(`${label} is not a regular file: ${safe}`);
  const data = await readFile(absolute);
  const after = await lstat(absolute);
  if (!after.isFile() || before.dev !== after.dev || before.ino !== after.ino || before.size !== after.size) {
    fail(`${label} changed while being read: ${safe}`);
  }
  const mode = (before.mode & 0o111) === 0 ? "100644" : "100755";
  return { path: safe, data, bytes: data.length, sha256: sha256(data), mode };
}

function parseStrictJSON(input, label) {
  const text = input.toString("utf8");
  if (Buffer.from(text, "utf8").compare(input) !== 0) fail(`${label} is not valid UTF-8`);
  return parseJSONWithUniqueKeys(text, label);
}

function exactKeys(value, required, optional, label) {
  if (value === null || typeof value !== "object" || Array.isArray(value)) fail(`${label} must be an object`);
  const allowed = new Set([...required, ...optional]);
  for (const key of Object.keys(value)) if (!allowed.has(key)) fail(`${label} has unknown field ${JSON.stringify(key)}`);
  for (const key of required) if (!Object.hasOwn(value, key)) fail(`${label} is missing ${JSON.stringify(key)}`);
}

function parseArguments(argv) {
  const parsed = new Map();
  for (let index = 2; index < argv.length; index += 2) {
    const option = argv[index];
    const value = argv[index + 1];
    if (!["--config", "--output", "--result"].includes(option) || value === undefined) fail(`usage: generate.mjs --config <file> --output <build-dir> --result <file>`);
    if (parsed.has(option)) fail(`duplicate option ${option}`);
    parsed.set(option, value);
  }
  if (parsed.size !== 3) fail("--config, --output and --result are all required");
  return {
    config: safeRelative(parsed.get("--config"), "--config"),
    output: safeRelative(parsed.get("--output"), "--output"),
    result: safeRelative(parsed.get("--result"), "--result"),
  };
}

function validateConfiguration(config) {
  exactKeys(config, ["schema_version", "pipeline_id", "mapping_profile", "resources", "roots", "fixtures", "outputs"], [], "configuration");
  if (config.schema_version !== 1 || config.pipeline_id !== pipelineID || config.mapping_profile !== mappingProfile) fail("configuration version/profile mismatch");
  if (!Array.isArray(config.resources) || config.resources.length === 0) fail("resources must be a non-empty array");
  if (!Array.isArray(config.roots) || config.roots.length !== 1) fail("P07 profile requires exactly one root");
  exactKeys(config.fixtures, ["valid", "forward", "invalid"], [], "fixtures");
  exactKeys(config.outputs, ["go", "python", "typescript"], [], "outputs");
  const resourcePaths = new Set();
  const resourceURIs = new Set();
  for (const [index, resource] of config.resources.entries()) {
    exactKeys(resource, ["path", "uri"], [], `resources[${index}]`);
    resource.path = safeRelative(resource.path, `resources[${index}].path`);
    if (typeof resource.uri !== "string" || !/^https:\/\/[A-Za-z0-9._~-]+\//u.test(resource.uri) || resource.uri.includes("#")) fail(`resources[${index}].uri must be an absolute HTTPS resource URI without fragment`);
    if (resourcePaths.has(resource.path) || resourceURIs.has(resource.uri)) fail("resource path and URI must be unique");
    resourcePaths.add(resource.path);
    resourceURIs.add(resource.uri);
  }
  for (const [index, root] of config.roots.entries()) {
    exactKeys(root, ["ref", "name"], [], `roots[${index}]`);
    if (typeof root.ref !== "string" || typeof root.name !== "string" || !/^[A-Z][A-Za-z0-9]*$/u.test(root.name)) fail("root ref/name is invalid");
  }
  const fixturePaths = new Set();
  for (const fixtureClass of ["valid", "forward", "invalid"]) {
    const fixtures = config.fixtures[fixtureClass];
    if (!Array.isArray(fixtures) || fixtures.length === 0) fail(`${fixtureClass} fixtures must be non-empty`);
    for (const [index, fixture] of fixtures.entries()) {
      exactKeys(fixture, ["id", "path"], [], `${fixtureClass}[${index}]`);
      if (!/^[a-z][a-z0-9-]*$/u.test(fixture.id)) fail(`invalid fixture id ${JSON.stringify(fixture.id)}`);
      fixture.path = safeRelative(fixture.path, `${fixtureClass}[${index}].path`);
      if (fixturePaths.has(fixture.path)) fail(`duplicate fixture path ${fixture.path}`);
      fixturePaths.add(fixture.path);
    }
  }
  for (const language of ["go", "python", "typescript"]) {
    const output = config.outputs[language];
    exactKeys(output, ["model", "probe"], [], `outputs.${language}`);
    output.model = safeRelative(output.model, `outputs.${language}.model`);
    output.probe = safeRelative(output.probe, `outputs.${language}.probe`);
    if (output.model === "provenance.json" || output.probe === "provenance.json") fail("provenance/result path is reserved");
  }
}

function rejectDangerousKeys(value, pointer = "") {
  if (Array.isArray(value)) {
    value.forEach((child, index) => rejectDangerousKeys(child, `${pointer}/${index}`));
    return;
  }
  if (value === null || typeof value !== "object") return;
  for (const key of Object.keys(value)) {
    if (["__proto__", "prototype", "constructor"].includes(key)) fail(`dangerous object key at ${pointer}/${key}`);
    rejectDangerousKeys(value[key], `${pointer}/${key}`);
  }
}

function rejectUnsafeNumbers(value, pointer = "") {
  if (typeof value === "number" && (!Number.isFinite(value) || (Number.isInteger(value) && !Number.isSafeInteger(value)))) fail(`unsafe JSON number at ${pointer || "/"}`);
  if (Array.isArray(value)) value.forEach((child, index) => rejectUnsafeNumbers(child, `${pointer}/${index}`));
  else if (value !== null && typeof value === "object") for (const [key, child] of Object.entries(value)) rejectUnsafeNumbers(child, `${pointer}/${key}`);
}

function rejectUnpairedSurrogates(value, pointer = "") {
  if (typeof value === "string") {
    for (let index = 0; index < value.length; index += 1) {
      const code = value.charCodeAt(index);
      if (code >= 0xD800 && code <= 0xDBFF) {
        const next = value.charCodeAt(index + 1);
        if (!(next >= 0xDC00 && next <= 0xDFFF)) fail(`unpaired Unicode surrogate at ${pointer || "/"}`);
        index += 1;
      } else if (code >= 0xDC00 && code <= 0xDFFF) fail(`unpaired Unicode surrogate at ${pointer || "/"}`);
    }
  } else if (Array.isArray(value)) value.forEach((child, index) => rejectUnpairedSurrogates(child, `${pointer}/${index}`));
  else if (value !== null && typeof value === "object") for (const [key, child] of Object.entries(value)) rejectUnpairedSurrogates(child, `${pointer}/${key}`);
}

const schemaKeywords = new Set([
  "$schema", "$id", "$ref", "$defs", "$comment", "title", "description", "type", "const", "enum",
  "oneOf", "anyOf", "properties", "required", "additionalProperties", "items", "format",
  "minimum", "maximum",
]);

function validateSchemaKeywords(schema, pointer = "") {
  if (schema === true || schema === false) return;
  if (schema === null || typeof schema !== "object" || Array.isArray(schema)) fail(`schema at ${pointer || "/"} must be an object or boolean`);
  for (const key of Object.keys(schema)) if (!schemaKeywords.has(key)) fail(`unknown schema keyword ${key} at ${pointer || "/"}`);
  for (const container of ["properties", "$defs"]) {
    if (schema[container] !== undefined) for (const [key, child] of Object.entries(schema[container])) validateSchemaKeywords(child, `${pointer}/${container}/${key}`);
  }
  for (const key of ["additionalProperties", "items", "not"]) if (schema[key] !== undefined) validateSchemaKeywords(schema[key], `${pointer}/${key}`);
  for (const key of ["oneOf", "anyOf", "allOf"]) if (schema[key] !== undefined) schema[key].forEach((child, index) => validateSchemaKeywords(child, `${pointer}/${key}/${index}`));
}

function strictDateTime(value) {
  if (typeof value !== "string") return false;
  const match = /^([0-9]{4})-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T(?:[01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](?:\.[0-9]+)?(?:Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])$/u.exec(value);
  if (match === null) return false;
  const year = Number(match[1]), month = Number(match[2]), day = Number(match[3]);
  const leap = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
  const days = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  return year > 0 && day <= days[month - 1] && Number.isFinite(Date.parse(value));
}

function decodePointer(document, fragment, reference) {
  if (fragment === "") return document;
  if (!fragment.startsWith("/")) fail(`only JSON Pointer fragments are supported: ${reference}`);
  let current = document;
  for (const encoded of fragment.slice(1).split("/")) {
    if (/~(?:[^01]|$)/u.test(encoded)) fail(`invalid JSON Pointer escape in ${reference}`);
    const token = encoded.replaceAll("~1", "/").replaceAll("~0", "~");
    if (current === null || typeof current !== "object" || !Object.hasOwn(current, token)) fail(`unresolved JSON Pointer ${reference}`);
    current = current[token];
  }
  return current;
}

function splitReference(reference, baseURI) {
  if (typeof reference !== "string" || reference.length === 0 || /^(?:file:|\/|[A-Za-z]:[\\/])/u.test(reference)) fail(`forbidden schema reference ${JSON.stringify(reference)}`);
  const resolved = new URL(reference, baseURI);
  if (resolved.protocol !== "https:") fail(`schema reference must resolve to HTTPS: ${reference}`);
  const fragment = resolved.hash.length === 0 ? "" : decodeURIComponent(resolved.hash.slice(1));
  resolved.hash = "";
  return { uri: resolved.href, fragment };
}

class ModelCompiler {
  constructor(resources, names) {
    this.resources = resources;
    this.names = names;
    this.types = new Map();
    this.referenceNames = new Map();
    this.nameReferences = new Map();
  }

  nameFor(reference, hint) {
    const configured = this.names.get(reference);
    const candidate = configured ?? pascal(hint);
    if (!/^[A-Z][A-Za-z0-9]*$/u.test(candidate)) fail(`invalid generated type name ${JSON.stringify(candidate)}`);
    const prior = this.nameReferences.get(candidate);
    if (prior !== undefined && prior !== reference) fail(`generated type name collision ${candidate}: ${prior} / ${reference}`);
    this.nameReferences.set(candidate, reference);
    this.referenceNames.set(reference, candidate);
    return candidate;
  }

  resolve(reference, baseURI) {
    const { uri, fragment } = splitReference(reference, baseURI);
    const document = this.resources.get(uri);
    if (document === undefined) fail(`offline schema bundle rejected unresolved resource ${uri}`);
    return { schema: decodePointer(document, fragment, reference), uri, fragment, canonical: `${uri}${fragment === "" ? "" : `#${fragment}`}` };
  }

  compileReference(reference, baseURI, hint) {
    const resolved = this.resolve(reference, baseURI);
    const existing = this.referenceNames.get(resolved.canonical);
    if (existing !== undefined) return { kind: "named", name: existing };
    const pointerHint = resolved.fragment.split("/").at(-1) || resolved.schema.title || hint;
    const name = this.nameFor(resolved.canonical, pointerHint);
    this.types.set(name, { kind: "pending" });
    this.types.set(name, this.compileSchema(resolved.schema, resolved.uri, name));
    return { kind: "named", name };
  }

  compileSchema(schema, baseURI, hint) {
    if (schema === true) return { kind: "json" };
    if (schema === false || schema === null || typeof schema !== "object" || Array.isArray(schema)) fail(`unsupported schema at ${hint}`);
    if (Object.hasOwn(schema, "$ref")) {
      const siblings = Object.keys(schema).filter((key) => !["$ref", "title", "description", "$comment"].includes(key));
      if (siblings.length > 0) fail(`$ref siblings are outside ${mappingProfile}: ${siblings.join(",")}`);
      return this.compileReference(schema.$ref, baseURI, hint);
    }
    if (Array.isArray(schema.anyOf)) {
      if (schema.anyOf.length !== 2) fail(`anyOf at ${hint} must express exactly T|null`);
      const nullIndex = schema.anyOf.findIndex((branch) => branch?.type === "null");
      if (nullIndex < 0) fail(`anyOf at ${hint} is not the supported nullable form`);
      return { kind: "nullable", value: this.compileSchema(schema.anyOf[1 - nullIndex], baseURI, hint) };
    }
    if (Array.isArray(schema.oneOf)) {
      const branches = schema.oneOf.map((branch, index) => {
        const resolved = Object.hasOwn(branch, "$ref") ? this.resolve(branch.$ref, baseURI) : { schema: branch, uri: baseURI, canonical: `${baseURI}#inline-${hint}-${index}` };
        const object = resolved.schema;
        if (object?.type !== "object") fail(`oneOf at ${hint} must contain object branches`);
        const candidates = Object.entries(object.properties ?? {})
          .filter(([wire, property]) => object.required?.includes(wire) && typeof property?.const === "string")
          .map(([wire, property]) => ({ wire, tag: property.const }));
        return { branch, resolved, object, candidates };
      });
      const discriminatorCandidates = branches[0].candidates.map((candidate) => candidate.wire).filter((wire) => {
        const tags = branches.map((branch) => branch.candidates.find((candidate) => candidate.wire === wire)?.tag);
        return tags.every((tag) => typeof tag === "string") && new Set(tags).size === tags.length;
      });
      if (discriminatorCandidates.length !== 1) fail(`oneOf at ${hint} must have exactly one shared required string const discriminator`);
      const discriminator = discriminatorCandidates[0];
      const variants = branches.map(({ branch, resolved, object, candidates }) => {
        const tag = candidates.find((candidate) => candidate.wire === discriminator).tag;
        const type = Object.hasOwn(branch, "$ref") ? this.compileReference(branch.$ref, baseURI, `${hint}${pascal(tag)}`) : this.compileInlineNamed(object, resolved.uri, `${hint}${pascal(tag)}`, resolved.canonical);
        return { tag, type };
      });
      const tags = new Set(variants.map((variant) => variant.tag));
      if (tags.size !== variants.length) fail(`oneOf at ${hint} has duplicate discriminator values`);
      const goVariantNames = new Map();
      for (const variant of variants) {
        const identifier = goName(variant.tag);
        const prior = goVariantNames.get(identifier);
        if (prior !== undefined) fail(`generated identifier collision in Go ${hint}: discriminator values ${JSON.stringify(prior)} and ${JSON.stringify(variant.tag)} both map to ${identifier}`);
        goVariantNames.set(identifier, variant.tag);
      }
      const canonical = `${baseURI}#/$arop-codegen/${encodeURIComponent(hint)}`;
      const existing = this.referenceNames.get(canonical);
      if (existing !== undefined) return { kind: "named", name: existing };
      const name = this.nameFor(canonical, hint);
      this.types.set(name, { kind: "union", discriminator, variants });
      return { kind: "named", name };
    }
    if (Array.isArray(schema.type)) {
      if (schema.type.length !== 2 || !schema.type.includes("null")) fail(`type array at ${hint} must express exactly T|null`);
      return { kind: "nullable", value: this.compileSchema({ ...schema, type: schema.type.find((item) => item !== "null") }, baseURI, hint) };
    }
    if (Array.isArray(schema.enum)) {
      if (schema.enum.length === 0 || !schema.enum.every((item) => typeof item === "string")) fail(`enum at ${hint} must contain strings`);
      return { kind: "enum", values: [...schema.enum] };
    }
    if (Object.hasOwn(schema, "const")) {
      if (!["string", "number", "boolean"].includes(typeof schema.const)) fail(`const at ${hint} has unsupported type`);
      return { kind: "const", value: schema.const };
    }
    switch (schema.type) {
      case "string":
        if (schema.format !== undefined && !["date-time", "uri-reference"].includes(schema.format)) fail(`unsupported string format ${schema.format}`);
        return { kind: schema.format === "date-time" ? "date-time" : schema.format === "uri-reference" ? "uri-reference" : "string" };
      case "integer":
        if (!Number.isSafeInteger(schema.minimum) || !Number.isSafeInteger(schema.maximum) || schema.minimum > schema.maximum) fail(`integer bounds at ${hint} must declare finite safe minimum and maximum`);
        return { kind: "integer", minimum: schema.minimum, maximum: schema.maximum };
      case "number":
        if (schema.minimum !== undefined || schema.maximum !== undefined) fail(`number bounds are unsupported at ${hint}`);
        return { kind: "number" };
      case "boolean":
        return { kind: "boolean" };
      case "null":
        return { kind: "null" };
      case "array":
        if (schema.items === undefined || Array.isArray(schema.items)) fail(`array at ${hint} requires one item schema`);
        return { kind: "array", item: this.compileSchema(schema.items, baseURI, `${hint}Item`) };
      case "object": {
        const properties = schema.properties ?? {};
        if (properties === null || typeof properties !== "object" || Array.isArray(properties)) fail(`properties at ${hint} must be an object`);
        const required = new Set(schema.required ?? []);
        for (const key of required) if (!Object.hasOwn(properties, key)) fail(`required property ${key} is not declared at ${hint}`);
        const fields = Object.keys(properties).sort().map((wire) => {
          if (!/^[A-Za-z_][A-Za-z0-9_]*$/u.test(wire) || pythonKeywords.has(wire)) fail(`wire field ${JSON.stringify(wire)} has no portable generated identifier`);
          const fieldHint = `${hint}${pascal(wire)}`;
          const fieldSchema = properties[wire];
          const inlineObject = fieldSchema !== null && typeof fieldSchema === "object" && !Array.isArray(fieldSchema)
            && fieldSchema.type === "object"
            && (Object.keys(fieldSchema.properties ?? {}).length > 0 || fieldSchema.additionalProperties === false);
          const fieldType = inlineObject
            ? this.compileInlineNamed(fieldSchema, baseURI, fieldHint, `${baseURI}#/$arop-codegen/${encodeURIComponent(fieldHint)}`)
            : this.compileSchema(fieldSchema, baseURI, fieldHint);
          return { wire, name: wire, required: required.has(wire), type: fieldType };
        });
        for (const [language, mapper] of [["Go", goName], ["Python", (wire) => wire], ["TypeScript", (wire) => wire]]) {
          const generated = new Map();
          for (const field of fields) {
            const identifier = mapper(field.wire);
            const prior = generated.get(identifier);
            if (prior !== undefined) fail(`generated identifier collision in ${language} ${hint}: wire fields ${JSON.stringify(prior)} and ${JSON.stringify(field.wire)} both map to ${identifier}`);
            generated.set(identifier, field.wire);
          }
        }
        let additional;
        if (schema.additionalProperties === false) additional = false;
        else if (schema.additionalProperties === true || schema.additionalProperties === undefined) additional = { kind: "json" };
        else additional = this.compileSchema(schema.additionalProperties, baseURI, `${hint}Value`);
        if (fields.length === 0 && additional !== false) return { kind: "map", value: additional };
        return { kind: "object", fields, additional };
      }
      default:
        fail(`schema at ${hint} has no supported type/ref/union`);
    }
  }

  compileInlineNamed(schema, baseURI, hint, reference) {
    const name = this.nameFor(reference, hint);
    if (!this.types.has(name)) {
      this.types.set(name, { kind: "pending" });
      this.types.set(name, this.compileSchema(schema, baseURI, name));
    }
    return { kind: "named", name };
  }

  compileRoot(reference, name) {
    const resolved = this.resolve(reference, reference);
    this.names.set(resolved.canonical, name);
    return this.compileReference(reference, reference, name);
  }
}

function pascal(value) {
  const words = String(value).replace(/([a-z0-9])([A-Z])/gu, "$1 $2").split(/[^A-Za-z0-9]+/u).filter(Boolean);
  const result = words.map((word) => word.length === 0 ? "" : word[0].toUpperCase() + word.slice(1)).join("");
  if (result === "") fail(`cannot derive a type name from ${JSON.stringify(value)}`);
  return /^[0-9]/u.test(result) ? `Value${result}` : result;
}

function camel(value) {
  const name = pascal(value);
  const initialisms = new Map([["ID", "id"], ["URI", "uri"], ["URL", "url"], ["SHA256", "sha256"]]);
  for (const [prefix, replacement] of initialisms) if (name.startsWith(prefix)) return replacement + name.slice(prefix.length);
  return name[0].toLowerCase() + name.slice(1);
}

function goName(value) {
  let name = pascal(value);
  name = name.replaceAll("Id", "ID").replaceAll("Uri", "URI").replaceAll("Url", "URL").replaceAll("Sha256", "SHA256");
  if (name === "Type") return "Type";
  return name;
}

function literal(value) {
  return JSON.stringify(value);
}

function goType(type) {
  switch (type.kind) {
    case "named": return type.name;
    case "string": return "string";
    case "date-time": return "DateTime";
    case "uri-reference": return "URIReference";
    case "integer": return "SafeInteger";
    case "number": return "float64";
    case "boolean": return "bool";
    case "json": return "json.RawMessage";
    case "array": return `[]${goType(type.item)}`;
    case "map": return `map[string]${goType(type.value)}`;
    case "enum": return "string";
    case "const": return typeof type.value === "number" ? "int64" : typeof type.value === "boolean" ? "bool" : "string";
    case "nullable": return `Nullable[${type.value.kind === "named" ? `*${goType(type.value)}` : goType(type.value)}]`;
    case "union": fail("anonymous Go unions are forbidden");
    default: fail(`unsupported Go type ${type.kind}`);
  }
}

function pythonType(type) {
  switch (type.kind) {
    case "named": return type.name;
    case "string": case "date-time": case "uri-reference": return "str";
    case "integer": return "int";
    case "number": return "float";
    case "boolean": return "bool";
    case "json": return "JsonValue";
    case "array": return `list[${pythonType(type.item)}]`;
    case "map": return `dict[str, ${pythonType(type.value)}]`;
    case "enum": return `Literal[${type.values.map(literal).join(", ")}]`;
    case "const": return `Literal[${literal(type.value)}]`;
    case "nullable": return `${pythonType(type.value)} | None`;
    case "union": return type.variants.map((variant) => pythonType(variant.type)).join(" | ");
    default: fail(`unsupported Python type ${type.kind}`);
  }
}

function tsType(type) {
  switch (type.kind) {
    case "named": return type.name;
    case "string": return "string";
    case "date-time": return "DateTime";
    case "uri-reference": return "URIReference";
    case "integer": case "number": return "number";
    case "boolean": return "boolean";
    case "json": return "JsonValue";
    case "array": return `ReadonlyArray<${tsType(type.item)}>`;
    case "map": return `Readonly<Record<string, ${tsType(type.value)}>>`;
    case "enum": return type.values.map(literal).join(" | ");
    case "const": return literal(type.value);
    case "nullable": return `${tsType(type.value)} | null`;
    case "union": return type.variants.map((variant) => tsType(variant.type)).join(" | ");
    default: fail(`unsupported TypeScript type ${type.kind}`);
  }
}

function generatedHeader(language, inputDigest) {
  const prefix = language === "python" ? "#" : "//";
  return `${prefix} Code generated by scripts/generate.mjs (${pipelineID}/${mappingProfile}); DO NOT EDIT.\n${prefix} schema-inputs-sha256: ${inputDigest}\n`;
}

function goValidation(type, expression, indent = "\t", sequence = { value: 0 }) {
  const lines = [];
  const add = (line) => lines.push(`${indent}${line}`);
  switch (type.kind) {
    case "named":
      add(`if err := (${expression}).validate(); err != nil { return err }`);
      break;
    case "integer":
      add(`if ${expression} < SafeInteger(${type.minimum}) || ${expression} > SafeInteger(${type.maximum}) { return fmt.Errorf("integer outside declared safe bounds") }`);
      break;
    case "number":
      break;
    case "json":
      add(`if err := validateRawJSON(${expression}); err != nil { return err }`);
      break;
    case "array": {
      const item = `item${sequence.value++}`;
      const child = goValidation(type.item, item, `${indent}\t`, sequence);
      if (child.length > 0) { add(`for _, ${item} := range ${expression} {`); lines.push(...child); add("}"); }
      break;
    }
    case "map": {
      const item = `item${sequence.value++}`;
      const child = goValidation(type.value, item, `${indent}\t`, sequence);
      if (child.length > 0) { add(`for _, ${item} := range ${expression} {`); lines.push(...child); add("}"); }
      break;
    }
    case "enum":
      add(`switch ${expression} { case ${type.values.map(literal).join(", ")}: default: return fmt.Errorf("unexpected enum value") }`);
      break;
    case "const":
      add(`if ${expression} != ${literal(type.value)} { return fmt.Errorf("unexpected const value") }`);
      break;
    case "nullable":
      add(`if ${expression}.Set && !${expression}.Null {`);
      if (type.value.kind === "named") add(`\tif ${expression}.Value == nil { return fmt.Errorf("nullable value is nil") }`);
      lines.push(...goValidation(type.value, `${expression}.Value`, `${indent}\t`, sequence));
      add("}");
      break;
    case "string": case "date-time": case "uri-reference": case "boolean": case "null":
      break;
    default:
      fail(`unsupported Go validation type ${type.kind}`);
  }
  return lines;
}

function buildGoRuntime(types, rootName) {
  const sanitizers = [];
  let helperSequence = 0;
  const sanitizerFor = (type, hint) => {
    if (type.kind === "named") return `sanitize${type.name}`;
    if (type.kind === "json") return "";
    const name = `sanitize${pascal(hint)}${helperSequence++}`;
    if (["string", "date-time", "uri-reference", "integer", "number", "boolean", "enum", "const"].includes(type.kind)) {
      sanitizers.push(`func ${name}(data []byte, forward bool) ([]byte, error) { if bytes.Equal(bytes.TrimSpace(data), []byte("null")) { return nil, fmt.Errorf("non-nullable value is null") }; return data, nil }`);
    } else if (type.kind === "null") {
      sanitizers.push(`func ${name}(data []byte, forward bool) ([]byte, error) { if !bytes.Equal(bytes.TrimSpace(data), []byte("null")) { return nil, fmt.Errorf("expected null") }; return data, nil }`);
    } else if (type.kind === "nullable") {
      const child = sanitizerFor(type.value, `${hint}Value`);
      sanitizers.push(child === "" ? `func ${name}(data []byte, forward bool) ([]byte, error) { return data, nil }` : `func ${name}(data []byte, forward bool) ([]byte, error) { if bytes.Equal(data, []byte("null")) { return data, nil }; return ${child}(data, forward) }`);
    } else if (type.kind === "array") {
      const child = sanitizerFor(type.item, `${hint}Item`);
      const transform = child === "" ? "" : ` for index, raw := range items { clean, err := ${child}(raw, forward); if err != nil { return nil, err }; items[index] = clean };`;
      sanitizers.push(`func ${name}(data []byte, forward bool) ([]byte, error) { if bytes.Equal(bytes.TrimSpace(data), []byte("null")) { return nil, fmt.Errorf("non-nullable array is null") }; var items []json.RawMessage; if err := decodeJSON(data, &items); err != nil { return nil, err };${transform} return json.Marshal(items) }`);
    } else if (type.kind === "map") {
      const child = sanitizerFor(type.value, `${hint}Value`);
      const transform = child === "" ? "" : ` for key, raw := range items { clean, err := ${child}(raw, forward); if err != nil { return nil, err }; items[key] = clean };`;
      sanitizers.push(`func ${name}(data []byte, forward bool) ([]byte, error) { if bytes.Equal(bytes.TrimSpace(data), []byte("null")) { return nil, fmt.Errorf("non-nullable map is null") }; var items map[string]json.RawMessage; if err := decodeJSON(data, &items); err != nil { return nil, err };${transform} return json.Marshal(items) }`);
    } else fail(`unsupported Go sanitizer type ${type.kind}`);
    return name;
  };

  const namedSanitizers = [];
  const validators = [];
  for (const [name, type] of [...types.entries()].sort(([left], [right]) => byteCompare(left, right))) {
    if (type.kind === "object") {
      const known = type.fields.map((field) => literal(field.wire)).join(", ");
      const required = type.fields.filter((field) => field.required).map((field) => ` if _, ok := object[${literal(field.wire)}]; !ok { return nil, fmt.Errorf("${name}.${field.wire} is required") };`).join("");
      const nested = type.fields.map((field) => {
        const child = sanitizerFor(field.type, `${name}${pascal(field.wire)}`);
        return child === "" ? "" : ` if raw, ok := object[${literal(field.wire)}]; ok { clean, err := ${child}(raw, forward); if err != nil { return nil, err }; object[${literal(field.wire)}] = clean };`;
      }).join("");
      const additionalSanitizer = type.additional === false ? "" : sanitizerFor(type.additional, `${name}Additional`);
      const unknown = type.additional === false
        ? `if !forward { return nil, fmt.Errorf("${name} has unknown field %s", key) }; delete(object, key)`
        : additionalSanitizer === ""
          ? "continue"
          : `clean, err := ${additionalSanitizer}(object[key], forward); if err != nil { return nil, err }; object[key] = clean`;
      namedSanitizers.push(`func sanitize${name}(data []byte, forward bool) ([]byte, error) { if bytes.Equal(bytes.TrimSpace(data), []byte("null")) { return nil, fmt.Errorf("non-nullable ${name} is null") }; var object map[string]json.RawMessage; if err := decodeJSON(data, &object); err != nil { return nil, err };${required} known := map[string]bool{${known.split(", ").filter(Boolean).map((item) => `${item}: true`).join(", ")}}; for key := range object { if !known[key] { ${unknown} } };${nested} return json.Marshal(object) }`);
      const statements = [];
      for (const field of type.fields) {
        const fieldExpression = `value.${goName(field.wire)}`;
        if (field.required || field.type.kind === "nullable") statements.push(...goValidation(field.type, fieldExpression));
        else {
          statements.push(`\tif ${fieldExpression} != nil {`);
          statements.push(...goValidation(field.type, `*${fieldExpression}`, "\t\t"));
          statements.push("\t}");
        }
      }
      if (type.additional !== false) {
        const additionalValidation = goValidation(type.additional, "item", "\t\t");
        if (additionalValidation.length > 0) {
          statements.push("\tfor _, item := range value.additionalProperties {");
          statements.push(...additionalValidation);
          statements.push("\t}");
        }
      }
      validators.push(`func (value ${name}) validate() error {\n${statements.join("\n")}\n\treturn nil\n}`);
    } else if (type.kind === "union") {
      const cases = type.variants.map((variant) => `case ${literal(variant.tag)}: return sanitize${variant.type.name}(data, forward)`).join("; ");
      namedSanitizers.push(`func sanitize${name}(data []byte, forward bool) ([]byte, error) { if bytes.Equal(bytes.TrimSpace(data), []byte("null")) { return nil, fmt.Errorf("non-nullable ${name} is null") }; var discriminator struct { Value string \`json:"${type.discriminator}"\` }; if err := json.Unmarshal(data, &discriminator); err != nil { return nil, err }; switch discriminator.Value { ${cases}; default: return nil, fmt.Errorf("unknown ${name} discriminator") } }`);
      const selected = type.variants.map((variant) => `if value.${goName(variant.tag)} != nil { count++; if err := value.${goName(variant.tag)}.validate(); err != nil { return err } }`).join("; ");
      validators.push(`func (value ${name}) validate() error { count := 0; ${selected}; if count != 1 { return fmt.Errorf("${name} requires exactly one variant") }; return nil }`);
    } else {
      namedSanitizers.push(`func sanitize${name}(data []byte, forward bool) ([]byte, error) { return data, nil }`);
      validators.push(`func (value ${name}) validate() error {\n${goValidation(type, "value").join("\n")}\n\treturn nil\n}`);
    }
  }
  const strictJSON = `func hexValue(value byte) (int, bool) { switch { case value >= '0' && value <= '9': return int(value - '0'), true; case value >= 'a' && value <= 'f': return int(value - 'a' + 10), true; case value >= 'A' && value <= 'F': return int(value - 'A' + 10), true; default: return 0, false } }\nfunc escapedCodeUnit(data []byte, offset int) (int, bool) { if offset+4 > len(data) { return 0, false }; value := 0; for index := offset; index < offset+4; index++ { digit, ok := hexValue(data[index]); if !ok { return 0, false }; value = value*16 + digit }; return value, true }\nfunc validateSurrogateEscapes(data []byte) error { inString := false; for index := 0; index < len(data); index++ { if data[index] == '"' { inString = !inString; continue }; if !inString || data[index] != '\\\\' { continue }; index++; if index >= len(data) { return fmt.Errorf("truncated JSON escape") }; if data[index] != 'u' { continue }; first, ok := escapedCodeUnit(data, index+1); if !ok { return fmt.Errorf("invalid Unicode escape") }; index += 4; if first >= 0xD800 && first <= 0xDBFF { if index+6 >= len(data) || data[index+1] != '\\\\' || data[index+2] != 'u' { return fmt.Errorf("unpaired high surrogate") }; second, ok := escapedCodeUnit(data, index+3); if !ok || second < 0xDC00 || second > 0xDFFF { return fmt.Errorf("unpaired high surrogate") }; index += 6 } else if first >= 0xDC00 && first <= 0xDFFF { return fmt.Errorf("unpaired low surrogate") } }; return nil }\nfunc walkStrictJSON(decoder *json.Decoder) error { token, err := decoder.Token(); if err != nil { return err }; delimiter, compound := token.(json.Delim); if !compound { return nil }; switch delimiter { case '{': seen := map[string]bool{}; for decoder.More() { keyToken, err := decoder.Token(); if err != nil { return err }; key, ok := keyToken.(string); if !ok { return fmt.Errorf("object key is not a string") }; if seen[key] { return fmt.Errorf("duplicate JSON key %s", key) }; seen[key] = true; if err := walkStrictJSON(decoder); err != nil { return err } }; closing, err := decoder.Token(); if err != nil || closing != json.Delim('}') { return fmt.Errorf("invalid object close") }; case '[': for decoder.More() { if err := walkStrictJSON(decoder); err != nil { return err } }; closing, err := decoder.Token(); if err != nil || closing != json.Delim(']') { return fmt.Errorf("invalid array close") }; default: return fmt.Errorf("unexpected JSON delimiter") }; return nil }\nfunc validateStrictJSON(data []byte) error { if !utf8.Valid(data) { return fmt.Errorf("JSON is not valid UTF-8") }; if err := validateSurrogateEscapes(data); err != nil { return err }; decoder := json.NewDecoder(bytes.NewReader(data)); decoder.UseNumber(); if err := walkStrictJSON(decoder); err != nil { return err }; if _, err := decoder.Token(); err != io.EOF { if err == nil { return fmt.Errorf("trailing JSON value") }; return err }; return nil }`;
  return `${sanitizers.join("\n")}\n${namedSanitizers.join("\n")}\n${validators.join("\n")}\n${strictJSON}\nfunc DecodeAuthoring(data []byte) (${rootName}, error) { var value ${rootName}; if err := validateStrictJSON(data); err != nil { return value, err }; clean, err := sanitize${rootName}(data, false); if err != nil { return value, err }; if err := decodeExact(clean, &value); err != nil { return value, err }; if err := value.validate(); err != nil { return value, err }; return value, nil }\nfunc DecodeForward(data []byte) (${rootName}, error) { var value ${rootName}; if err := validateStrictJSON(data); err != nil { return value, err }; clean, err := sanitize${rootName}(data, true); if err != nil { return value, err }; if err := decodeExact(clean, &value); err != nil { return value, err }; if err := value.validate(); err != nil { return value, err }; value.forwardWire = append([]byte(nil), data...); return value, nil }`;
}

function emitGo(types, inputDigest, validFixtures, forwardFixtures, invalidFixtures, rootName) {
  const definitions = [];
  for (const [name, type] of [...types.entries()].sort(([left], [right]) => byteCompare(left, right))) {
    if (type.kind === "object") {
      const fields = type.fields.map((field) => {
        let valueType = goType(field.type);
        let options = "";
        if (!field.required && field.type.kind !== "nullable") {
          valueType = `*${valueType}`;
          options = ",omitempty";
        } else if (!field.required && field.type.kind === "nullable") options = ",omitzero";
        return `\t${goName(field.wire)} ${valueType} \`json:"${field.wire}${options}"\``;
      });
      if (type.additional !== false) fields.push(`\tadditionalProperties map[string]${goType(type.additional)}`);
      const internal = name === rootName ? "\n\tforwardWire json.RawMessage" : "";
      let methods = "";
      if (type.additional !== false) {
        const known = type.fields.map((field) => `${literal(field.wire)}: true`).join(", ");
        const rootForward = name === rootName ? "if value.forwardWire != nil { return append([]byte(nil), value.forwardWire...), nil }; " : "";
        methods = `\nfunc (value *${name}) UnmarshalJSON(data []byte) error { type wire ${name}; var decoded wire; if err := decodeExact(data, &decoded); err != nil { return err }; var object map[string]json.RawMessage; if err := decodeJSON(data, &object); err != nil { return err }; known := map[string]bool{${known}}; additional := map[string]${goType(type.additional)}{}; for key, raw := range object { if known[key] { continue }; var item ${goType(type.additional)}; if err := decodeExact(raw, &item); err != nil { return err }; additional[key] = item }; *value = ${name}(decoded); value.additionalProperties = additional; return nil }\nfunc (value ${name}) MarshalJSON() ([]byte, error) { ${rootForward}type wire ${name}; encoded, err := json.Marshal(wire(value)); if err != nil { return nil, err }; var object map[string]json.RawMessage; if err := decodeJSON(encoded, &object); err != nil { return nil, err }; for key, item := range value.additionalProperties { if _, exists := object[key]; exists { return nil, fmt.Errorf("additional property collides with declared field %s", key) }; raw, err := json.Marshal(item); if err != nil { return nil, err }; object[key] = raw }; return json.Marshal(object) }`;
        methods = methods.replace("decodeExact(data, &decoded)", "json.Unmarshal(data, &decoded)");
      } else if (name === rootName) methods = `\nfunc (value ${name}) MarshalJSON() ([]byte, error) { if value.forwardWire != nil { return append([]byte(nil), value.forwardWire...), nil }; type wire ${name}; return json.Marshal(wire(value)) }`;
      definitions.push(`type ${name} struct {\n${fields.join("\n")}${internal}\n}${methods}`);
    } else if (type.kind === "union") {
      const fields = type.variants.map((variant) => `\t${goName(variant.tag)} *${goType(variant.type)}`).join("\n");
      const decodeCases = type.variants.map((variant) => `\tcase ${literal(variant.tag)}:\n\t\tvar candidate ${goType(variant.type)}\n\t\tif err := decodeExact(data, &candidate); err != nil { return err }\n\t\tvalue.${goName(variant.tag)} = &candidate`).join("\n");
      const encodeCases = type.variants.map((variant) => `\tif value.${goName(variant.tag)} != nil { if selected != nil { return nil, fmt.Errorf("${name} has multiple variants") }; selected = value.${goName(variant.tag)} }`).join("\n");
      definitions.push(`type ${name} struct {\n${fields}\n}\nfunc (value *${name}) UnmarshalJSON(data []byte) error {\n\tvar discriminator struct { Value string \`json:"${type.discriminator}"\` }\n\tif err := json.Unmarshal(data, &discriminator); err != nil { return err }\n\t*value = ${name}{}\n\tswitch discriminator.Value {\n${decodeCases}\n\tdefault: return fmt.Errorf("unknown ${name} discriminator %q", discriminator.Value)\n\t}\n\treturn nil\n}\nfunc (value ${name}) MarshalJSON() ([]byte, error) {\n\tvar selected any\n${encodeCases}\n\tif selected == nil { return nil, fmt.Errorf("${name} has no selected variant") }\n\treturn json.Marshal(selected)\n}`);
    } else {
      definitions.push(`type ${name} ${goType(type)}`);
    }
  }
  const model = `${generatedHeader("go", inputDigest)}package codegenspike\n\nimport (\n\t"bytes"\n\t"encoding/json"\n\t"fmt"\n\t"io"\n\t"net/url"\n\t"regexp"\n\t"time"\n)\n\nvar strictDateTime = regexp.MustCompile(\`^[0-9]{4}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12][0-9]|3[01])T(?:[01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](?:\\.[0-9]+)?(?:Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])$\`)\n\ntype DateTime string\nfunc (value *DateTime) UnmarshalJSON(data []byte) error { var wire string; if err := json.Unmarshal(data, &wire); err != nil { return err }; if !strictDateTime.MatchString(wire) { return fmt.Errorf("invalid RFC3339 date-time") }; if _, err := time.Parse(time.RFC3339Nano, wire); err != nil { return err }; *value = DateTime(wire); return nil }\ntype URIReference string\nfunc (value *URIReference) UnmarshalJSON(data []byte) error { var wire string; if err := json.Unmarshal(data, &wire); err != nil { return err }; parsed, err := url.Parse(wire); if err != nil || parsed.String() != wire { return fmt.Errorf("invalid URI-reference") }; *value = URIReference(wire); return nil }\n\ntype Nullable[T any] struct { Set bool; Null bool; Value T }\nfunc (value Nullable[T]) IsZero() bool { return !value.Set }\nfunc (value *Nullable[T]) UnmarshalJSON(data []byte) error {\n\tvalue.Set = true\n\tif bytes.Equal(data, []byte("null")) { value.Null = true; var zero T; value.Value = zero; return nil }\n\tvalue.Null = false\n\treturn decodeExact(data, &value.Value)\n}\nfunc (value Nullable[T]) MarshalJSON() ([]byte, error) {\n\tif !value.Set || value.Null { return []byte("null"), nil }\n\treturn json.Marshal(value.Value)\n}\n\n${definitions.join("\n\n")}\n\nfunc decodeExact(data []byte, destination any) error {\n\tdecoder := json.NewDecoder(bytes.NewReader(data)); decoder.DisallowUnknownFields()\n\tif err := decoder.Decode(destination); err != nil { return err }\n\tvar trailing any\n\tif err := decoder.Decode(&trailing); err != io.EOF { if err == nil { return fmt.Errorf("trailing JSON value") }; return err }\n\treturn nil\n}\nfunc DecodeAuthoring(data []byte) (CodegenSpikeEnvelope, error) {\n\tvar value CodegenSpikeEnvelope\n\tif err := decodeExact(data, &value); err != nil { return value, err }\n\tif value.SchemaVersion != 1 || value.SafeInteger < -9007199254740991 || value.SafeInteger > 9007199254740991 { return value, fmt.Errorf("wire constraints failed") }\n\treturn value, nil\n}\nfunc DecodeForward(data []byte) (CodegenSpikeEnvelope, error) {\n\tvar document map[string]json.RawMessage; if err := decodeJSON(data, &document); err != nil { return CodegenSpikeEnvelope{}, err }\n\tfor key := range document { if !map[string]bool{"schema_version":true,"payload":true,"optional_nullable":true,"occurred_at":true,"labels":true,"attributes":true,"extension":true,"tree":true,"safe_integer":true}[key] { delete(document, key) } }\n\tif raw := document["tree"]; raw != nil { cleaned, err := stripNodeForward(raw); if err != nil { return CodegenSpikeEnvelope{}, err }; document["tree"] = cleaned }\n\tcleaned, err := json.Marshal(document); if err != nil { return CodegenSpikeEnvelope{}, err }; value, err := DecodeAuthoring(cleaned); if err != nil { return value, err }; value.forwardWire = append([]byte(nil), data...); return value, nil\n}\nfunc stripNodeForward(data []byte) ([]byte, error) { var node map[string]json.RawMessage; if err := decodeJSON(data, &node); err != nil { return nil, err }; for key := range node { if key != "name" && key != "children" { delete(node, key) } }; var children []json.RawMessage; if err := json.Unmarshal(node["children"], &children); err != nil { return nil, err }; for index, child := range children { clean, err := stripNodeForward(child); if err != nil { return nil, err }; children[index] = clean }; encoded, err := json.Marshal(children); if err != nil { return nil, err }; node["children"] = encoded; return json.Marshal(node) }\nfunc decodeJSON(data []byte, destination any) error { decoder := json.NewDecoder(bytes.NewReader(data)); decoder.UseNumber(); if err := decoder.Decode(destination); err != nil { return err }; var trailing any; if err := decoder.Decode(&trailing); err != io.EOF { return fmt.Errorf("trailing JSON value") }; return nil }\n`;
  let hardenedModel = model.replaceAll("CodegenSpikeEnvelope", rootName).replaceAll("package codegenspike", "package generatedcodec").replace("\t\"io\"\n", "\t\"io\"\n\t\"math/big\"\n\t\"strings\"\n\t\"unicode/utf8\"\n");
  hardenedModel = hardenedModel.replace(
    "type URIReference string",
    () => "func validURIReference(wire string) bool { for index := 0; index < len(wire); index++ { value := wire[index]; if value > 127 || value < 33 { return false }; if value == '%' { if index+2 >= len(wire) { return false }; if _, ok := hexValue(valueAt(wire, index+1)); !ok { return false }; if _, ok := hexValue(valueAt(wire, index+2)); !ok { return false }; index += 2; continue }; if !strings.ContainsRune(\"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._~!$&'()*+,;=:@/?#[]-\", rune(value)) { return false } }; open, close := strings.IndexByte(wire, '['), strings.IndexByte(wire, ']'); if open < 0 && close < 0 { return true }; if open < 0 || close < 0 || close < open || strings.ContainsAny(wire[open+1:close], \"[]\") { return false }; authority := strings.Index(wire, \"//\"); if authority < 0 || open < authority+2 { return false }; end := len(wire); if relative := strings.IndexAny(wire[authority+2:], \"/?#\"); relative >= 0 { end = authority + 2 + relative }; if close >= end { return false }; return regexp.MustCompile(\`^[0-9A-Fa-f:.]+$\`).MatchString(wire[open+1:close]) }\nfunc valueAt(wire string, index int) byte { return wire[index] }\ntype URIReference string",
  );
  hardenedModel = hardenedModel.replace("if err != nil || parsed.String() != wire", "if err != nil || parsed.String() != wire || !validURIReference(wire)");
  hardenedModel = hardenedModel.replace("if !strictDateTime.MatchString(wire)", "if !strictDateTime.MatchString(wire) || strings.HasPrefix(wire, \"0000-\")");
  hardenedModel = hardenedModel.replace(
    "type Nullable[T any]",
    "type SafeInteger int64\nfunc (value *SafeInteger) UnmarshalJSON(data []byte) error { var number json.Number; if err := decodeJSON(data, &number); err != nil { return err }; rational, ok := new(big.Rat).SetString(number.String()); if !ok || !rational.IsInt() { return fmt.Errorf(\"expected mathematical integer\") }; limit := big.NewInt(9007199254740991); negativeLimit := new(big.Int).Neg(new(big.Int).Set(limit)); if rational.Num().Cmp(limit) > 0 || rational.Num().Cmp(negativeLimit) < 0 { return fmt.Errorf(\"integer outside safe bounds\") }; *value = SafeInteger(rational.Num().Int64()); return nil }\n\ntype Nullable[T any]",
  );
  hardenedModel = hardenedModel.replace(
    "\tif value.SchemaVersion != 1 || value.SafeInteger < -9007199254740991 || value.SafeInteger > 9007199254740991 { return value, fmt.Errorf(\"wire constraints failed\") }\n\treturn value, nil",
    "\tif value.SchemaVersion != 1 || value.SafeInteger < -9007199254740991 || value.SafeInteger > 9007199254740991 { return value, fmt.Errorf(\"wire constraints failed\") }\n\tfor _, raw := range value.Attributes { if err := validateRawJSON(raw); err != nil { return value, err } }\n\tif err := validateRawJSON(value.Extension.Data); err != nil { return value, err }\n\tif value.Payload.Json != nil { if err := validateRawJSON(value.Payload.Json.Value); err != nil { return value, err } }\n\treturn value, nil",
  );
  hardenedModel = hardenedModel.replace(/func DecodeAuthoring[\s\S]*?func decodeJSON/u, `${buildGoRuntime(types, rootName)}\nfunc decodeJSON`);
  hardenedModel = hardenedModel.replace(
    "func DecodeForward(data []byte)",
    "func validateRawJSON(data []byte) error { var value any; if err := decodeJSON(data, &value); err != nil { return err }; return validateJSONValue(value) }\nfunc validateJSONValue(value any) error { switch item := value.(type) { case json.Number: rational, ok := new(big.Rat).SetString(item.String()); if !ok { return fmt.Errorf(\"invalid JSON number\") }; if rational.IsInt() { limit := big.NewInt(9007199254740991); negativeLimit := new(big.Int).Neg(new(big.Int).Set(limit)); if rational.Num().Cmp(limit) > 0 || rational.Num().Cmp(negativeLimit) < 0 { return fmt.Errorf(\"JSON integer outside safe range\") } }; case []any: for _, child := range item { if err := validateJSONValue(child); err != nil { return err } }; case map[string]any: for _, child := range item { if err := validateJSONValue(child); err != nil { return err } } }; return nil }\nfunc DecodeForward(data []byte)",
  );
  const cases = validFixtures.map((fixture) => `\t\t{name: ${literal(fixture.id)}, document: ${JSON.stringify(fixture.text)}},`).join("\n");
  const invalidCases = invalidFixtures.map((fixture) => `\t\t{name: ${literal(fixture.id)}, document: ${JSON.stringify(fixture.text)}},`).join("\n");
  const forwardCases = forwardFixtures.map((fixture) => `\t\t{name: ${literal(fixture.id)}, document: ${JSON.stringify(fixture.text)}, forward: true},`).join("\n");
  const probe = `${generatedHeader("go", inputDigest)}package codegenspike\n\nimport (\n\t"bytes"\n\t"encoding/json"\n\t"reflect"\n\t"testing"\n)\n\nfunc TestCodegenSpike(t *testing.T) {\n\tcases := []struct{name, document string; forward bool}{\n${cases}${forwardCases === "" ? "" : `\n${forwardCases}`}\n\t}\n\tfor _, item := range cases {\n\t\tt.Run(item.name, func(t *testing.T) {\n\t\t\tvar value CodegenSpikeEnvelope; var err error\n\t\t\tif item.forward { if _, strictErr := DecodeAuthoring([]byte(item.document)); strictErr == nil { t.Fatal("authoring accepted forward fields") }; value, err = DecodeForward([]byte(item.document)) } else { value, err = DecodeAuthoring([]byte(item.document)) }; if err != nil { t.Fatal(err) }\n\t\t\tif value.Payload.Text == nil && value.Payload.Json == nil { t.Fatal("typed tagged union was not selected") }\n\t\t\tif len(value.Tree.Children) == 0 { t.Fatal("recursive model was not decoded") }\n\t\t\tif value.Extension.Data == nil { t.Fatal("extension JSON was not retained") }\n\t\t\tswitch item.name { case "minimal": if value.OptionalNullable.Set { t.Fatal("absent nullable became present") }; case "nullable-null": if !value.OptionalNullable.Set || !value.OptionalNullable.Null { t.Fatal("explicit null was lost") }; case "full": if !value.OptionalNullable.Set || value.OptionalNullable.Null || value.OptionalNullable.Value != "present-value" { t.Fatal("nullable value was lost") } }\n\t\t\tencoded, err := json.Marshal(value); if err != nil { t.Fatal(err) }\n\t\t\tleft := decodeComparable(t, []byte(item.document)); right := decodeComparable(t, encoded)\n\t\t\tif !reflect.DeepEqual(left, right) { t.Fatalf("round-trip drift: %s", encoded) }\n\t\t})\n\t}\n}\n\nfunc TestGeneratedDecoderRejectsInvalid(t *testing.T) {\n\tcases := []struct{name, document string}{\n${invalidCases}\n\t}\n\tfor _, item := range cases { if _, err := DecodeAuthoring([]byte(item.document)); err == nil { t.Fatalf("%s was accepted", item.name) } }\n}\n\nfunc decodeComparable(t *testing.T, data []byte) any {\n\tt.Helper(); decoder := json.NewDecoder(bytes.NewReader(data)); decoder.UseNumber(); var value any\n\tif err := decoder.Decode(&value); err != nil { t.Fatal(err) }; return value\n}\n`;
  let hardenedProbe = probe.replaceAll("CodegenSpikeEnvelope", rootName)
    .replaceAll("package codegenspike", "package generatedcodec")
    .replace("TestCodegenSpike", "TestGeneratedCodec")
    .replace("TestGeneratedDecoderRejectsInvalid", "TestGeneratedCodecRejectsInvalid")
    .replace(
    "\tfor _, item := range cases { if _, err := DecodeAuthoring([]byte(item.document)); err == nil { t.Fatalf(\"%s was accepted\", item.name) } }",
    "\tfor _, item := range cases { t.Run(item.name, func(t *testing.T) { if _, err := DecodeAuthoring([]byte(item.document)); err == nil { t.Fatalf(\"%s was accepted\", item.name) } }) }",
  );
  hardenedProbe = hardenedProbe.replace("\t\t\tif value.Payload.Text == nil && value.Payload.Json == nil { t.Fatal(\"typed tagged union was not selected\") }\n\t\t\tif len(value.Tree.Children) == 0 { t.Fatal(\"recursive model was not decoded\") }\n\t\t\tif value.Extension.Data == nil { t.Fatal(\"extension JSON was not retained\") }\n\t\t\tswitch item.name { case \"minimal\": if value.OptionalNullable.Set { t.Fatal(\"absent nullable became present\") }; case \"nullable-null\": if !value.OptionalNullable.Set || !value.OptionalNullable.Null { t.Fatal(\"explicit null was lost\") }; case \"full\": if !value.OptionalNullable.Set || value.OptionalNullable.Null || value.OptionalNullable.Value != \"present-value\" { t.Fatal(\"nullable value was lost\") } }\n", "");
  hardenedProbe = hardenedProbe.replace("\t\"reflect\"\n", "\t\"math/big\"\n\t\"reflect\"\n");
  hardenedProbe = hardenedProbe.replace("if !reflect.DeepEqual(left, right)", "if !jsonEquivalent(left, right)");
  hardenedProbe = hardenedProbe.replace("func decodeComparable", "func jsonEquivalent(left any, right any) bool { switch typed := left.(type) { case json.Number: other, ok := right.(json.Number); if !ok { return false }; leftRat, leftOK := new(big.Rat).SetString(typed.String()); rightRat, rightOK := new(big.Rat).SetString(other.String()); return leftOK && rightOK && leftRat.Cmp(rightRat) == 0; case []any: other, ok := right.([]any); if !ok || len(typed) != len(other) { return false }; for index := range typed { if !jsonEquivalent(typed[index], other[index]) { return false } }; return true; case map[string]any: other, ok := right.(map[string]any); if !ok || len(typed) != len(other) { return false }; for key, item := range typed { candidate, exists := other[key]; if !exists || !jsonEquivalent(item, candidate) { return false } }; return true; default: return reflect.DeepEqual(left, right) } }\n\nfunc decodeComparable");
  return { "go/models.gen.go": hardenedModel, "go/models_gen_test.go": hardenedProbe };
}

function emitPython(types, inputDigest, validFixtures) {
  const definitions = [];
  for (const [name, type] of [...types.entries()].sort(([left], [right]) => byteCompare(left, right))) {
    if (type.kind !== "object") continue;
    const required = type.fields.filter((field) => field.required);
    const optional = type.fields.filter((field) => !field.required);
    const fields = [...required, ...optional].map((field) => {
      const annotation = pythonType(field.type);
      if (field.required) return `    ${field.name}: ${annotation}`;
      return `    ${field.name}: ${annotation} | UnsetType = UNSET`;
    });
    definitions.push(`@dataclass(slots=True)\nclass ${name}:\n${fields.length === 0 ? "    pass" : fields.join("\n")}`);
  }
  const model = `${generatedHeader("python", inputDigest)}from __future__ import annotations\n\nfrom dataclasses import dataclass\nfrom typing import Any, Literal, TypeAlias\n\nJsonValue: TypeAlias = None | bool | int | float | str | list["JsonValue"] | dict[str, "JsonValue"]\n\n@dataclass(frozen=True, slots=True)\nclass UnsetType:\n    pass\n\nUNSET = UnsetType()\n\n${definitions.join("\n\n")}\n`;
  const fixtures = JSON.stringify(validFixtures.map((fixture) => ({ id: fixture.id, document: fixture.document })), null, 2);
  const probe = `${generatedHeader("python", inputDigest)}from __future__ import annotations\n\nimport json\nimport typing\n\nimport models_gen\n\nFIXTURES = ${fixtures}\n\ndef main() -> None:\n    typing.get_type_hints(models_gen.CodegenSpikeEnvelope)\n    results: list[dict[str, object]] = []\n    for item in FIXTURES:\n        encoded = json.dumps(item["document"], ensure_ascii=False, separators=(",", ":"), sort_keys=True)\n        decoded = json.loads(encoded)\n        if decoded != item["document"]:\n            raise AssertionError(item["id"])\n        results.append({"id": item["id"], "passed": True})\n    print(json.dumps({"schema_version": 1, "language": "python", "cases": results}, separators=(",", ":"), sort_keys=True))\n\nif __name__ == "__main__":\n    main()\n`;
  return { "python/models_gen.py": model, "python/probe.py": probe };
}

function pythonDecode(type, expression, forward = "forward") {
  switch (type.kind) {
    case "named": return `_decode_${type.name}(${expression}, ${forward})`;
    case "string": return `_string(${expression})`;
    case "date-time": return `_date_time(${expression})`;
    case "uri-reference": return `_uri_reference(${expression})`;
    case "integer": return `_integer(${expression}, ${type.minimum}, ${type.maximum})`;
    case "number": return `_number(${expression})`;
    case "boolean": return `_boolean(${expression})`;
    case "json": return `_json_value(${expression})`;
    case "array": return `[${pythonDecode(type.item, "item", forward)} for item in _array(${expression})]`;
    case "map": return `{key: ${pythonDecode(type.value, "item", forward)} for key, item in _object(${expression}).items()}`;
    case "enum": return `cast(${pythonType(type)}, _literal(${expression}, ${JSON.stringify(type.values)}))`;
    case "const": return `cast(${pythonType(type)}, _literal(${expression}, [${literal(type.value)}]))`;
    case "nullable": return `None if ${expression} is None else ${pythonDecode(type.value, expression, forward)}`;
    default: fail(`unsupported Python decoder type ${type.kind}`);
  }
}

function emitPythonTyped(types, inputDigest, validFixtures, forwardFixtures, invalidFixtures, rootName) {
  const definitions = [], aliases = [], decoders = [];
  for (const [name, type] of [...types.entries()].sort(([left], [right]) => byteCompare(left, right))) {
    if (type.kind === "object") {
      const required = type.fields.filter((item) => item.required);
      const fields = [...required, ...type.fields.filter((item) => !item.required)].map((item) => item.required ? `    ${item.name}: ${pythonType(item.type)}` : `    ${item.name}: ${pythonType(item.type)} | UnsetType = UNSET`);
      if (type.additional !== false) fields.push(`    _additional_properties: dict[str, ${pythonType(type.additional)}] = field(default_factory=dict, repr=False)`);
      if (name === rootName) fields.push("    _forward_wire: JsonValue | UnsetType = field(default=UNSET, init=False, repr=False)");
      definitions.push(`@dataclass(slots=True)\nclass ${name}:\n${fields.length === 0 ? "    pass" : fields.join("\n")}`);
      const requiredChecks = required.map((item) => `    if ${literal(item.wire)} not in value: raise ValueError("${name}.${item.wire} is required")`).join("\n");
      const knownFields = JSON.stringify(type.fields.map((item) => item.wire));
      const additionalArgument = type.additional === false ? "" : `\n        _additional_properties={key: ${pythonDecode(type.additional, "item")} for key, item in value.items() if key not in known},`;
      const constructorArgs = type.fields.map((item) => item.required ? `        ${item.name}=${pythonDecode(item.type, `value[${literal(item.wire)}]`)},` : `        ${item.name}=((${pythonDecode(item.type, `value[${literal(item.wire)}]`)}) if ${literal(item.wire)} in value else UNSET),`).join("\n");
      const unknownPolicy = type.additional === false ? `    if unknown and not forward: raise ValueError("${name} has unknown fields: " + ",".join(sorted(unknown)))` : "";
      decoders.push(`def _decode_${name}(raw: object, forward: bool) -> ${name}:\n    value = _object(raw)\n    known = set(${knownFields})\n    unknown = set(value) - known\n${unknownPolicy}\n${requiredChecks}\n    return ${name}(\n${constructorArgs}${additionalArgument}\n    )`);
    } else if (type.kind === "union") {
      aliases.push(`${name}: TypeAlias = ${pythonType(type)}`);
      const cases = type.variants.map((variant) => `    if tag == ${literal(variant.tag)}: return ${pythonDecode(variant.type, "value", "forward")}`).join("\n");
      decoders.push(`def _decode_${name}(raw: object, forward: bool) -> ${name}:\n    value = _object(raw); tag = value.get(${literal(type.discriminator)})\n${cases}\n    raise ValueError("unknown ${name} discriminator")`);
    } else aliases.push(`${name}: TypeAlias = ${pythonType(type)}`);
  }
  const helpers = `def _object(value: object) -> dict[str, object]:\n    if not isinstance(value, dict) or not all(isinstance(key, str) for key in value): raise ValueError("expected object")\n    return value\ndef _array(value: object) -> list[object]:\n    if not isinstance(value, list): raise ValueError("expected array")\n    return value\ndef _string(value: object) -> str:\n    if not isinstance(value, str): raise ValueError("expected string")\n    return value\ndef _boolean(value: object) -> bool:\n    if not isinstance(value, bool): raise ValueError("expected boolean")\n    return value\ndef _number(value: object) -> float:\n    if isinstance(value, bool) or not isinstance(value, (int, float)): raise ValueError("expected number")\n    return float(value)\ndef _integer(value: object, minimum: int, maximum: int) -> int:\n    if isinstance(value, bool) or not isinstance(value, int) or value < minimum or value > maximum: raise ValueError("integer outside safe bounds")\n    return value\ndef _literal(value: object, allowed: list[object]):\n    if value not in allowed or isinstance(value, bool) != isinstance(allowed[0], bool): raise ValueError("unexpected literal")\n    return value\ndef _json_value(value: object) -> JsonValue:\n    if value is None or isinstance(value, (bool, int, float, str)): return value\n    if isinstance(value, list): return [_json_value(item) for item in value]\n    if isinstance(value, dict) and all(isinstance(key, str) for key in value): return {key: _json_value(item) for key, item in value.items()}\n    raise ValueError("invalid JSON value")\ndef _date_time(value: object) -> str:\n    wire = _string(value)\n    if re.fullmatch(r"[0-9]{4}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12][0-9]|3[01])T(?:[01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](?:\\.[0-9]+)?(?:Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])", wire) is None: raise ValueError("invalid RFC3339 date-time")\n    datetime.fromisoformat(wire.replace("Z", "+00:00")); return wire\ndef _uri_reference(value: object) -> str:\n    wire = _string(value)\n    if any(character.isspace() for character in wire): raise ValueError("invalid URI-reference")\n    urlsplit(wire); return wire`;
  const model = `${generatedHeader("python", inputDigest)}from __future__ import annotations\n\nfrom dataclasses import dataclass, field, fields, is_dataclass\nfrom datetime import datetime\nimport json\nimport re\nfrom typing import Literal, TypeAlias\nfrom urllib.parse import urlsplit\n\nJsonValue: TypeAlias = None | bool | int | float | str | list["JsonValue"] | dict[str, "JsonValue"]\n\n@dataclass(frozen=True, slots=True)\nclass UnsetType:\n    pass\nUNSET = UnsetType()\n\n${definitions.join("\n\n")}\n\n${aliases.join("\n")}\n\n${helpers}\n\n${decoders.join("\n\n")}\n\ndef _pairs(items: list[tuple[str, object]]) -> dict[str, object]:\n    result: dict[str, object] = {}\n    for key, value in items:\n        if key in result: raise ValueError("duplicate JSON key")\n        result[key] = value\n    return result\ndef _parse(data: str | bytes) -> object:\n    return json.loads(data, object_pairs_hook=_pairs, parse_constant=lambda value: (_ for _ in ()).throw(ValueError(value)))\ndef decode_authoring(data: str | bytes) -> CodegenSpikeEnvelope:\n    return _decode_CodegenSpikeEnvelope(_parse(data), False)\ndef decode_forward(data: str | bytes) -> CodegenSpikeEnvelope:\n    document = _parse(data); value = _decode_CodegenSpikeEnvelope(document, True); value._forward_wire = _json_value(document); return value\ndef _to_wire(value):\n    if isinstance(value, CodegenSpikeEnvelope) and value._forward_wire is not UNSET: return value._forward_wire\n    if value is UNSET: raise ValueError("UNSET has no wire representation")\n    if is_dataclass(value): return {item.name: _to_wire(getattr(value, item.name)) for item in fields(value) if not item.name.startswith("_") and getattr(value, item.name) is not UNSET}\n    if isinstance(value, list): return [_to_wire(item) for item in value]\n    if isinstance(value, dict): return {key: _to_wire(item) for key, item in value.items()}\n    return value\ndef encode_wire(value: CodegenSpikeEnvelope) -> str:\n    return json.dumps(_to_wire(value), ensure_ascii=False, separators=(",", ":"), sort_keys=True)\n`;
  const safePythonHelpers = `def _json_value(value: object) -> JsonValue:\n    if isinstance(value, bool) or value is None or isinstance(value, str): return value\n    if isinstance(value, int):\n        if not -9007199254740991 <= value <= 9007199254740991: raise ValueError("JSON integer outside safe range")\n        return value\n    if isinstance(value, float):\n        if not math.isfinite(value): raise ValueError("non-finite JSON number")\n        return value\n    if isinstance(value, list): return [_json_value(item) for item in value]\n    if isinstance(value, dict) and all(isinstance(key, str) for key in value): return {key: _json_value(item) for key, item in value.items()}\n    raise ValueError("invalid JSON value")\ndef _uri_reference(value: object) -> str:\n    wire = _string(value)\n    if any(character.isspace() or ord(character) < 32 for character in wire) or re.search(r"%(?![0-9A-Fa-f]{2})", wire): raise ValueError("invalid URI-reference")\n    urlsplit(wire); return wire`;
  const strictPythonJSON = `def _validate_json_text(text: str) -> None:\n    index = 0; in_string = False\n    while index < len(text):\n        character = text[index]\n        if character == '"': in_string = not in_string; index += 1; continue\n        if in_string and 0xD800 <= ord(character) <= 0xDFFF: raise ValueError("unpaired Unicode surrogate")\n        if not in_string or character != "\\\\": index += 1; continue\n        index += 1\n        if index >= len(text): raise ValueError("truncated JSON escape")\n        if text[index] != "u": index += 1; continue\n        if index + 4 >= len(text): raise ValueError("truncated Unicode escape")\n        first = int(text[index + 1:index + 5], 16); index += 5\n        if 0xD800 <= first <= 0xDBFF:\n            if index + 5 >= len(text) or text[index] != "\\\\" or text[index + 1] != "u": raise ValueError("unpaired high surrogate")\n            second = int(text[index + 2:index + 6], 16)\n            if not 0xDC00 <= second <= 0xDFFF: raise ValueError("unpaired high surrogate")\n            index += 6\n        elif 0xDC00 <= first <= 0xDFFF: raise ValueError("unpaired low surrogate")`;
  let hardenedPythonModel = model.replaceAll("CodegenSpikeEnvelope", rootName).replace("import json\n", "import json\nimport math\n").replace("from typing import Literal, TypeAlias", "from typing import Literal, TypeAlias, cast");
  hardenedPythonModel = hardenedPythonModel.replace("from datetime import datetime\n", "from datetime import datetime\nfrom decimal import Decimal\n");
  hardenedPythonModel = hardenedPythonModel.replace(
    "    if is_dataclass(value): return {item.name: _to_wire(getattr(value, item.name)) for item in fields(value) if not item.name.startswith(\"_\") and getattr(value, item.name) is not UNSET}\n",
    "    if is_dataclass(value):\n        result = {item.name: _to_wire(getattr(value, item.name)) for item in fields(value) if not item.name.startswith(\"_\") and getattr(value, item.name) is not UNSET}\n        additional = getattr(value, \"_additional_properties\", UNSET)\n        if isinstance(additional, dict): result.update({key: _to_wire(item) for key, item in additional.items()})\n        elif additional is not UNSET: raise ValueError(\"invalid additional properties\")\n        return result\n",
  );
  hardenedPythonModel = hardenedPythonModel.replace("def _json_value(", "def _json_value_unchecked(").replace("def _uri_reference(", "def _uri_reference_unchecked(");
  hardenedPythonModel = hardenedPythonModel.replace(decoders.join("\n\n"), `${safePythonHelpers}\n\n${strictPythonJSON}\n\n${decoders.join("\n\n")}`);
  hardenedPythonModel = hardenedPythonModel.replace(/def _json_value_unchecked[\s\S]*?(?=def _date_time)/u, "");
  hardenedPythonModel = hardenedPythonModel.replace(/def _uri_reference_unchecked[\s\S]*?(?=def _json_value)/u, "");
  hardenedPythonModel = hardenedPythonModel.replace("def _number(value: object) -> float:\n    if isinstance(value, bool) or not isinstance(value, (int, float)): raise ValueError(\"expected number\")\n    return float(value)", "def _number(value: object) -> float:\n    if isinstance(value, bool) or not isinstance(value, (int, float, Decimal)) or not math.isfinite(value): raise ValueError(\"expected finite number\")\n    return float(value)");
  hardenedPythonModel = hardenedPythonModel.replace("def _integer(value: object, minimum: int, maximum: int) -> int:\n    if isinstance(value, bool) or not isinstance(value, int) or value < minimum or value > maximum: raise ValueError(\"integer outside safe bounds\")\n    return value", "def _integer(value: object, minimum: int, maximum: int) -> int:\n    if isinstance(value, bool) or not isinstance(value, (int, float, Decimal)) or not math.isfinite(value) or int(value) != value or value < minimum or value > maximum: raise ValueError(\"integer outside safe bounds\")\n    return int(value)");
  hardenedPythonModel = hardenedPythonModel.replace("    if isinstance(value, float):\n        if not math.isfinite(value): raise ValueError(\"non-finite JSON number\")\n        return value", "    if isinstance(value, (float, Decimal)):\n        if not math.isfinite(value): raise ValueError(\"non-finite JSON number\")\n        integral = value.to_integral_value() if isinstance(value, Decimal) else float(int(value))\n        floating = float(value)\n        if (value == integral and not -9007199254740991 <= value <= 9007199254740991) or (value != integral and floating.is_integer()): raise ValueError(\"unsafe JSON number\")\n        return floating");
  hardenedPythonModel = hardenedPythonModel.replace("    datetime.fromisoformat(wire.replace(\"Z\", \"+00:00\")); return wire", "    match = re.fullmatch(r\"([0-9]{4})-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T.*\", wire)\n    if match is None: raise ValueError(\"invalid RFC3339 date-time\")\n    year, month, day = (int(item) for item in match.groups()); leap = year % 4 == 0 and (year % 100 != 0 or year % 400 == 0)\n    if day > [31, 29 if leap else 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31][month - 1]: raise ValueError(\"invalid RFC3339 date-time\")\n    return wire");
  hardenedPythonModel = hardenedPythonModel.replace("    if day > [31, 29 if leap else 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31][month - 1]", "    if year == 0 or day > [31, 29 if leap else 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31][month - 1]");
  hardenedPythonModel = hardenedPythonModel.replace("    urlsplit(wire); return wire", () => "    if any(ord(character) > 127 for character in wire) or re.search(r\"[^A-Za-z0-9._~!$&'()*+,;=:@/?#%\\[\\]-]\", wire): raise ValueError(\"invalid URI-reference\")\n    if \"[\" in wire or \"]\" in wire:\n        authority = wire.find(\"//\"); opening = wire.find(\"[\"); closing = wire.find(\"]\")\n        tail = len(wire); separators = [position for token in \"/?#\" if (position := wire.find(token, authority + 2)) >= 0]\n        if separators: tail = min(separators)\n        if authority < 0 or opening < authority + 2 or closing <= opening or closing >= tail or re.fullmatch(r\"[0-9A-Fa-f:.]+\", wire[opening + 1:closing]) is None or \"[\" in wire[opening + 1:] or \"]\" in wire[closing + 1:]: raise ValueError(\"invalid URI-reference bracket placement\")\n    urlsplit(wire); return wire");
  hardenedPythonModel = hardenedPythonModel.replace("def _parse(data: str | bytes) -> object:\n    return json.loads(data,", "def _parse(data: str | bytes) -> object:\n    text = data.decode(\"utf-8\", errors=\"strict\") if isinstance(data, bytes) else data\n    _validate_json_text(text)\n    return json.loads(text,");
  hardenedPythonModel = hardenedPythonModel.replace("return json.loads(text, object_pairs_hook=", "return json.loads(text, parse_float=Decimal, object_pairs_hook=");
  const fixtures = [...validFixtures.map((fixture) => ({ id: fixture.id, mode: "strict", document: fixture.text })), ...forwardFixtures.map((fixture) => ({ id: fixture.id, mode: "forward", document: fixture.text }))];
  const invalid = invalidFixtures.map((fixture) => ({ id: fixture.id, document: fixture.text }));
  const probe = `${generatedHeader("python", inputDigest)}from __future__ import annotations\n\nimport json\nimport typing\nimport models_gen\nFIXTURES = json.loads(${literal(JSON.stringify(fixtures))})\nINVALID = json.loads(${literal(JSON.stringify(invalid))})\n\ndef main() -> None:\n    typing.get_type_hints(models_gen.CodegenSpikeEnvelope); results: list[dict[str, object]] = []\n    for item in FIXTURES:\n        if item["mode"] == "forward":\n            try: models_gen.decode_authoring(item["document"]); raise AssertionError("authoring accepted forward fixture")\n            except ValueError: pass\n            value = models_gen.decode_forward(item["document"])\n        else: value = models_gen.decode_authoring(item["document"])\n        if value.payload.type not in ("text", "json") or len(value.tree.children) == 0: raise AssertionError("typed access failed")\n        if item["id"] == "minimal" and value.optional_nullable is not models_gen.UNSET: raise AssertionError("absent nullable")\n        if item["id"] == "nullable-null" and value.optional_nullable is not None: raise AssertionError("explicit null")\n        if item["id"] == "full" and value.optional_nullable != "present-value": raise AssertionError("nullable value")\n        if json.loads(models_gen.encode_wire(value)) != json.loads(item["document"]): raise AssertionError(item["id"])\n        results.append({"id": item["id"], "passed": True})\n    for item in INVALID:\n        try: models_gen.decode_authoring(item["document"]); raise AssertionError("invalid accepted: " + item["id"])\n        except ValueError: pass\n    print(json.dumps({"schema_version": 1, "language": "python", "cases": results}, separators=(",", ":"), sort_keys=True))\nif __name__ == "__main__": main()\n`;
  let hardenedPythonProbe = probe.replace("    for item in INVALID:\n", "    rejections: list[dict[str, object]] = []\n    for item in INVALID:\n");
  hardenedPythonProbe = hardenedPythonProbe.replaceAll("CodegenSpikeEnvelope", rootName);
  hardenedPythonProbe = hardenedPythonProbe.replace("import typing\nimport models_gen", "import typing\nimport pathlib\nimport sys\nsys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))\nimport models_gen");
  hardenedPythonProbe = hardenedPythonProbe.replace("        except ValueError: pass\n    print", "        except ValueError: pass\n        rejections.append({\"id\": item[\"id\"], \"passed\": True})\n    print");
  hardenedPythonProbe = hardenedPythonProbe.replace("\"cases\": results}", "\"cases\": results, \"rejections\": rejections}");
  hardenedPythonProbe = hardenedPythonProbe.replace("        if value.payload.type not in (\"text\", \"json\") or len(value.tree.children) == 0: raise AssertionError(\"typed access failed\")\n        if item[\"id\"] == \"minimal\" and value.optional_nullable is not models_gen.UNSET: raise AssertionError(\"absent nullable\")\n        if item[\"id\"] == \"nullable-null\" and value.optional_nullable is not None: raise AssertionError(\"explicit null\")\n        if item[\"id\"] == \"full\" and value.optional_nullable != \"present-value\": raise AssertionError(\"nullable value\")\n", "");
  return { "python/models_gen.py": hardenedPythonModel, "python/probe.py": hardenedPythonProbe };
}

function emitTypeScript(types, inputDigest, validFixtures) {
  const definitions = [];
  for (const [name, type] of [...types.entries()].sort(([left], [right]) => byteCompare(left, right))) {
    if (type.kind === "object") {
      const seen = new Set();
      const fields = type.fields.map((field) => {
        const nameValue = camel(field.wire);
        if (seen.has(nameValue)) fail(`TypeScript field-name collision in ${name}: ${nameValue}`);
        seen.add(nameValue);
        return `  readonly ${nameValue}${field.required ? "" : "?"}: ${tsType(field.type)};`;
      });
      definitions.push(`export interface ${name} {\n${fields.join("\n")}\n}`);
    } else if (type.kind === "union") definitions.push(`export type ${name} = ${tsType(type)};`);
    else definitions.push(`export type ${name} = ${tsType(type)};`);
  }
  const model = `${generatedHeader("typescript", inputDigest)}export type JsonValue = null | boolean | number | string | ReadonlyArray<JsonValue> | { readonly [key: string]: JsonValue };\nexport type DateTime = string & { readonly __dateTime: unique symbol };\nexport type URIReference = string & { readonly __uriReference: unique symbol };\n\n${definitions.join("\n\n")}\n`;
  const fixtures = JSON.stringify(validFixtures.map((fixture) => ({ id: fixture.id, document: fixture.document })), null, 2);
  const probe = `${generatedHeader("typescript", inputDigest)}import type { CodegenSpikeEnvelope } from "./models.gen.js";\n\nconst fixtures: ReadonlyArray<{ readonly id: string; readonly document: CodegenSpikeEnvelope }> = ${fixtures} as unknown as ReadonlyArray<{ readonly id: string; readonly document: CodegenSpikeEnvelope }>;\nconst cases = fixtures.map((item) => {\n  const encoded = JSON.stringify(item.document);\n  const decoded: unknown = JSON.parse(encoded);\n  if (JSON.stringify(decoded) !== encoded) throw new Error(item.id);\n  return { id: item.id, passed: true };\n});\nconsole.log(JSON.stringify({ schema_version: 1, language: "typescript", cases }));\n`;
  let hardenedTypeScriptProbe = probe.replace("for (const item of invalid) {", "const rejections = [];\nfor (const item of invalid) {");
  hardenedTypeScriptProbe = hardenedTypeScriptProbe.replace("if (!rejected) throw new Error(\"invalid accepted: \" + item.id); }", "if (!rejected) throw new Error(\"invalid accepted: \" + item.id); rejections.push({ id: item.id, passed: true }); }");
  hardenedTypeScriptProbe = hardenedTypeScriptProbe.replace("language: \"typescript\", cases }", "language: \"typescript\", cases, rejections }");
  return { "typescript/models.gen.ts": model, "typescript/probe.ts": hardenedTypeScriptProbe };
}

function tsDecode(type, expression, forward = "forward") {
  switch (type.kind) {
    case "named": return `_decode${type.name}(${expression}, ${forward})`;
    case "string": return `_string(${expression})`;
    case "date-time": return `_safeDateTime(${expression})`;
    case "uri-reference": return `_safeURIReference(${expression})`;
    case "integer": return `_integer(${expression}, ${type.minimum}, ${type.maximum})`;
    case "number": return `_number(${expression})`;
    case "boolean": return `_boolean(${expression})`;
    case "json": return `_safeJSONValue(${expression})`;
    case "array": return `_array(${expression}).map((item) => ${tsDecode(type.item, "item", forward)})`;
    case "map": return `Object.fromEntries(Object.entries(_object(${expression})).map(([key, item]) => [key, ${tsDecode(type.value, "item", forward)}]))`;
    case "enum": return `_literal(${expression}, ${JSON.stringify(type.values)})`;
    case "const": return `_literal(${expression}, [${literal(type.value)}])`;
    case "nullable": return `${expression} === null ? null : ${tsDecode(type.value, expression, forward)}`;
    default: fail(`unsupported TypeScript decoder type ${type.kind}`);
  }
}

function emitTypeScriptTyped(types, inputDigest, validFixtures, forwardFixtures, invalidFixtures, rootName) {
  const definitions = [], decoders = [];
  definitions.push(`function _safeJSONValue(value: unknown): JsonValue { if (value === null || typeof value === "string" || typeof value === "boolean") return value; if (typeof value === "number") { if (!Number.isFinite(value) || (Number.isInteger(value) && !Number.isSafeInteger(value))) throw new Error("JSON integer outside safe range"); return value; } if (Array.isArray(value)) return value.map(_safeJSONValue); const object = _object(value); return Object.fromEntries(Object.entries(object).map(([key, item]) => [key, _safeJSONValue(item)])); }\nfunction _safeDateTime(value: unknown): DateTime { const wire = _string(value); const match = /^([0-9]{4})-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T(?:[01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](?:\\.[0-9]+)?(?:Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])$/.exec(wire); if (match === null) throw new Error("invalid RFC3339 date-time"); const year = Number(match[1]), month = Number(match[2]), day = Number(match[3]); const leap = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0); const days = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31]; const maximum = days[month - 1]; if (maximum === undefined || day > maximum || Number.isNaN(Date.parse(wire))) throw new Error("invalid RFC3339 date-time"); return wire as DateTime; }\nfunction _safeURIReference(value: unknown): URIReference { const wire = _string(value); if (/\\s/.test(wire) || Array.from(wire).some((character) => character.charCodeAt(0) < 32) || /%(?![0-9A-Fa-f]{2})/.test(wire)) throw new Error("invalid URI-reference"); return wire as URIReference; }`);
  definitions[0] = definitions[0]
    .replace("maximum === undefined || day > maximum || Number.isNaN(Date.parse(wire))", "year === 0 || maximum === undefined || day > maximum")
    .replace("if (/\\\\s/.test(wire) || Array.from(wire).some((character) => character.charCodeAt(0) < 32) || /%(?![0-9A-Fa-f]{2})/.test(wire))", () => "if (!_validURIReference(wire))");
  definitions[0] += `\nfunction _validURIReference(wire: string): boolean { if (/\\s/.test(wire) || Array.from(wire).some((character) => character.charCodeAt(0) < 32 || character.charCodeAt(0) > 127) || /%(?![0-9A-Fa-f]{2})/.test(wire) || /[^A-Za-z0-9._~!$&'()*+,;=:@/?#%\\[\\]-]/.test(wire)) return false; const open = wire.indexOf("["), close = wire.indexOf("]"); if (open < 0 && close < 0) return true; if (open < 0 || close <= open || wire.indexOf("[", open + 1) >= 0 || wire.indexOf("]", close + 1) >= 0) return false; const authority = wire.indexOf("//"); if (authority < 0 || open < authority + 2) return false; const separators = [wire.indexOf("/", authority + 2), wire.indexOf("?", authority + 2), wire.indexOf("#", authority + 2)].filter((value) => value >= 0); const end = separators.length === 0 ? wire.length : Math.min(...separators); return close < end && /^[0-9A-Fa-f:.]+$/.test(wire.slice(open + 1, close)); }`;
  definitions[0] = definitions[0].replace(/function _safeURIReference[\s\S]*?return wire as URIReference; \}/u, "function _safeURIReference(value: unknown): URIReference { const wire = _string(value); if (!_validURIReference(wire)) throw new Error(\"invalid URI-reference\"); return wire as URIReference; }");
  for (const [name, type] of [...types.entries()].sort(([left], [right]) => byteCompare(left, right))) {
    if (type.kind === "object") {
      const fields = type.fields.map((item) => `  readonly ${item.wire}${item.required ? "" : "?"}: ${tsType(item.type)};`);
      definitions.push(`export interface ${name} {\n${fields.join("\n")}\n}`);
      const required = type.fields.filter((item) => item.required).map((item) => `  if (!Object.hasOwn(value, ${literal(item.wire)})) throw new Error("${name}.${item.wire} is required");`).join("\n");
      const validate = type.fields.map((item) => item.required ? `  ${tsDecode(item.type, `value[${literal(item.wire)}]`)};` : `  if (Object.hasOwn(value, ${literal(item.wire)})) ${tsDecode(item.type, `value[${literal(item.wire)}]`)};`).join("\n");
      const knownFields = JSON.stringify(type.fields.map((item) => item.wire));
      const additionalPolicy = type.additional === false
        ? `_unknown(value, ${knownFields}, forward);`
        : `for (const [key, item] of Object.entries(value)) if (!${knownFields}.includes(key)) ${tsDecode(type.additional, "item")};`;
      decoders.push(`function _decode${name}(raw: unknown, forward: boolean): ${name} {\n  const value = _object(raw); ${additionalPolicy}\n${required}\n${validate}\n  return value as unknown as ${name};\n}`);
    } else if (type.kind === "union") {
      definitions.push(`export type ${name} = ${tsType(type)};`);
      const cases = type.variants.map((variant) => `    case ${literal(variant.tag)}: return ${tsDecode(variant.type, "value", "forward")};`).join("\n");
      decoders.push(`function _decode${name}(raw: unknown, forward: boolean): ${name} {\n  const value = _object(raw);\n  switch (value[${literal(type.discriminator)}]) {\n${cases}\n    default: throw new Error("unknown ${name} discriminator");\n  }\n}`);
    } else definitions.push(`export type ${name} = ${tsType(type)};`);
  }
  const strictTypeScriptJSON = `function _mathematicalInteger(token: string): boolean { const match = /^(-?)([0-9]+)(?:\\.([0-9]+))?(?:[eE]([+-]?[0-9]+))?$/.exec(token); if (match === null) return false; const fraction = match[3] ?? ""; const exponent = Number(match[4] ?? "0"); if (!Number.isSafeInteger(exponent)) return false; const scale = fraction.length - exponent; if (scale <= 0) return true; const digits = (match[2] ?? "") + fraction; if (scale >= digits.length) return /^0+$/.test(digits); return /^0+$/.test(digits.slice(digits.length - scale)); }\nfunction _validateStrictJSONText(source: string): void { let index = 0; const whitespace = () => { while (index < source.length && /[ \\t\\r\\n]/.test(source[index] ?? "")) index += 1; }; const stringValue = (): string => { const start = index; if (source[index] !== '"') throw new Error("expected JSON string"); index += 1; while (index < source.length) { const code = source.charCodeAt(index); if (code >= 0xD800 && code <= 0xDFFF) throw new Error("unpaired Unicode surrogate"); if (source[index] === '"') { index += 1; return JSON.parse(source.slice(start, index)) as string; } if (source[index] !== "\\\\") { if (code < 32) throw new Error("control character in JSON string"); index += 1; continue; } index += 1; if (index >= source.length) throw new Error("truncated JSON escape"); if (source[index] !== "u") { if (!'"\\\\/bfnrt'.includes(source[index] ?? "")) throw new Error("invalid JSON escape"); index += 1; continue; } const firstText = source.slice(index + 1, index + 5); if (!/^[0-9A-Fa-f]{4}$/.test(firstText)) throw new Error("invalid Unicode escape"); const first = Number.parseInt(firstText, 16); index += 5; if (first >= 0xD800 && first <= 0xDBFF) { if (source[index] !== "\\\\" || source[index + 1] !== "u") throw new Error("unpaired high surrogate"); const secondText = source.slice(index + 2, index + 6); if (!/^[0-9A-Fa-f]{4}$/.test(secondText)) throw new Error("invalid Unicode escape"); const second = Number.parseInt(secondText, 16); if (second < 0xDC00 || second > 0xDFFF) throw new Error("unpaired high surrogate"); index += 6; } else if (first >= 0xDC00 && first <= 0xDFFF) throw new Error("unpaired low surrogate"); } throw new Error("unterminated JSON string"); }; const value = (): void => { whitespace(); const character = source[index]; if (character === '"') { stringValue(); return; } if (character === '{') { index += 1; whitespace(); const seen = new Set<string>(); if (source[index] === '}') { index += 1; return; } while (true) { whitespace(); const key = stringValue(); if (seen.has(key)) throw new Error("duplicate JSON key " + key); seen.add(key); whitespace(); if (source[index] !== ':') throw new Error("expected colon"); index += 1; value(); whitespace(); if (source[index] === '}') { index += 1; return; } if (source[index] !== ',') throw new Error("expected object separator"); index += 1; } } if (character === '[') { index += 1; whitespace(); if (source[index] === ']') { index += 1; return; } while (true) { value(); whitespace(); if (source[index] === ']') { index += 1; return; } if (source[index] !== ',') throw new Error("expected array separator"); index += 1; } } const remainder = source.slice(index); const number = /^-?(?:0|[1-9][0-9]*)(?:\\.[0-9]+)?(?:[eE][+-]?[0-9]+)?/.exec(remainder); if (number !== null) { const token = number[0]; const parsed = Number(token); if (!Number.isFinite(parsed) || (Number.isInteger(parsed) && !_mathematicalInteger(token))) throw new Error("unsafe JSON number lexeme"); index += token.length; return; } for (const literalValue of ["true", "false", "null"]) if (source.startsWith(literalValue, index)) { index += literalValue.length; return; } throw new Error("invalid JSON value"); }; value(); whitespace(); if (index !== source.length) throw new Error("trailing JSON value"); }`;
  const helpers = `function _object(value: unknown): Record<string, unknown> { if (value === null || typeof value !== "object" || Array.isArray(value)) throw new Error("expected object"); return value as Record<string, unknown>; }\nfunction _array(value: unknown): ReadonlyArray<unknown> { if (!Array.isArray(value)) throw new Error("expected array"); return value; }\nfunction _string(value: unknown): string { if (typeof value !== "string") throw new Error("expected string"); return value; }\nfunction _boolean(value: unknown): boolean { if (typeof value !== "boolean") throw new Error("expected boolean"); return value; }\nfunction _number(value: unknown): number { if (typeof value !== "number" || !Number.isFinite(value)) throw new Error("expected finite number"); return value; }\nfunction _integer(value: unknown, minimum: number, maximum: number): number { const number = _number(value); if (!Number.isSafeInteger(number) || number < minimum || number > maximum) throw new Error("integer outside safe bounds"); return number; }\nfunction _literal<T extends string | number | boolean>(value: unknown, allowed: ReadonlyArray<T>): T { if (!allowed.some((item) => item === value)) throw new Error("unexpected literal"); return value as T; }\nfunction _jsonValue(value: unknown): JsonValue { if (value === null || typeof value === "string" || typeof value === "boolean" || (typeof value === "number" && Number.isFinite(value))) return value; if (Array.isArray(value)) return value.map(_jsonValue); const object = _object(value); return Object.fromEntries(Object.entries(object).map(([key, item]) => [key, _jsonValue(item)])); }\nfunction _dateTime(value: unknown): DateTime { const wire = _string(value); if (!/^[0-9]{4}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12][0-9]|3[01])T(?:[01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](?:\\.[0-9]+)?(?:Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])$/.test(wire) || Number.isNaN(Date.parse(wire))) throw new Error("invalid RFC3339 date-time"); return wire as DateTime; }\nfunction _uriReference(value: unknown): URIReference { const wire = _string(value); if (/\\s/.test(wire)) throw new Error("invalid URI-reference"); return wire as URIReference; }\nfunction _unknown(value: Record<string, unknown>, known: ReadonlyArray<string>, forward: boolean): void { if (!forward) for (const key of Object.keys(value)) if (!known.includes(key)) throw new Error("unknown field " + key); }`;
  const model = `${generatedHeader("typescript", inputDigest)}export type JsonValue = null | boolean | number | string | ReadonlyArray<JsonValue> | { readonly [key: string]: JsonValue };\nexport type DateTime = string & { readonly __dateTime: unique symbol };\nexport type URIReference = string & { readonly __uriReference: unique symbol };\n\n${definitions.join("\n\n")}\n\n${helpers}\n\n${strictTypeScriptJSON}\n\n${decoders.join("\n\n")}\n\nfunction _parse(data: string): unknown { _validateStrictJSONText(data); return JSON.parse(data); }\nexport function decodeAuthoring(data: string): ${rootName} { return _decode${rootName}(_parse(data), false); }\nexport function decodeForward(data: string): ${rootName} { return _decode${rootName}(_parse(data), true); }\nexport function encodeWire(value: ${rootName}): string { return JSON.stringify(value); }\n`;
  const fixtures = [...validFixtures.map((fixture) => ({ id: fixture.id, mode: "strict", document: fixture.text })), ...forwardFixtures.map((fixture) => ({ id: fixture.id, mode: "forward", document: fixture.text }))];
  const invalid = invalidFixtures.map((fixture) => ({ id: fixture.id, document: fixture.text }));
  const probe = `${generatedHeader("typescript", inputDigest)}import { decodeAuthoring, decodeForward, encodeWire } from "./models.gen.js";\nconst fixtures = ${JSON.stringify(fixtures)};\nconst invalid = ${JSON.stringify(invalid)};\nconst cases = fixtures.map((item) => {\n  let value;\n  if (item.mode === "forward") { try { decodeAuthoring(item.document); throw new Error("authoring accepted forward fixture"); } catch (error) { if (error instanceof Error && error.message === "authoring accepted forward fixture") throw error; } value = decodeForward(item.document); } else value = decodeAuthoring(item.document);\n  if ((value.payload.type !== "text" && value.payload.type !== "json") || value.tree.children.length === 0) throw new Error("typed access failed");\n  if (item.id === "minimal" && value.optional_nullable !== undefined) throw new Error("absent nullable");\n  if (item.id === "nullable-null" && value.optional_nullable !== null) throw new Error("explicit null");\n  if (item.id === "full" && value.optional_nullable !== "present-value") throw new Error("nullable value");\n  if (JSON.stringify(JSON.parse(encodeWire(value))) !== JSON.stringify(JSON.parse(item.document))) throw new Error(item.id);\n  return { id: item.id, passed: true };\n});\nfor (const item of invalid) { let rejected = false; try { decodeAuthoring(item.document); } catch { rejected = true; } if (!rejected) throw new Error("invalid accepted: " + item.id); }\nconsole.log(JSON.stringify({ schema_version: 1, language: "typescript", cases }));\n`;
  let hardenedTypeScriptProbe = probe.replace("for (const item of invalid) {", "const rejections = [];\nfor (const item of invalid) {");
  hardenedTypeScriptProbe = hardenedTypeScriptProbe.replace("if (!rejected) throw new Error(\"invalid accepted: \" + item.id); }", "if (!rejected) throw new Error(\"invalid accepted: \" + item.id); rejections.push({ id: item.id, passed: true }); }");
  hardenedTypeScriptProbe = hardenedTypeScriptProbe.replace("language: \"typescript\", cases }", "language: \"typescript\", cases, rejections }");
  hardenedTypeScriptProbe = hardenedTypeScriptProbe.replace("  if ((value.payload.type !== \"text\" && value.payload.type !== \"json\") || value.tree.children.length === 0) throw new Error(\"typed access failed\");\n  if (item.id === \"minimal\" && value.optional_nullable !== undefined) throw new Error(\"absent nullable\");\n  if (item.id === \"nullable-null\" && value.optional_nullable !== null) throw new Error(\"explicit null\");\n  if (item.id === \"full\" && value.optional_nullable !== \"present-value\") throw new Error(\"nullable value\");\n", "");
  return { "typescript/models.gen.ts": model, "typescript/probe.ts": hardenedTypeScriptProbe };
}

function aggregateInputs(entries) {
  const ordered = [...entries].sort((left, right) => byteCompare(left.path, right.path));
  const material = ordered.map((entry) => `${entry.kind}\0${entry.path}\0${entry.uri ?? ""}\0${entry.sha256}\0${entry.bytes}\0${entry.mode}\n`).join("");
  return sha256(Buffer.from(material, "utf8"));
}

function aggregateOutputs(entries) {
  const ordered = [...entries].sort((left, right) => byteCompare(left.path, right.path));
  const material = ordered.map((entry) => `${entry.path}\0${entry.language}\0${entry.sha256}\0${entry.bytes}\0${entry.mode}\n`).join("");
  return sha256(Buffer.from(material, "utf8"));
}

async function writeTree(target, files, provenanceRelative, provenance) {
  const temporary = `${target}.tmp`;
  const backup = `${target}.previous`;
  await rm(temporary, { recursive: true, force: true });
  await rm(backup, { recursive: true, force: true });
  await mkdir(temporary, { recursive: true });
  for (const [relative, source] of Object.entries(files).sort(([left], [right]) => byteCompare(left, right))) {
    const safe = safeRelative(relative, "generated output");
    const destination = path.join(temporary, safe);
    within(temporary, destination, "generated output");
    await mkdir(path.dirname(destination), { recursive: true });
    await writeFile(destination, source, { encoding: "utf8", mode: 0o644, flag: "wx" });
    await chmod(destination, 0o644);
  }
  const provenancePath = path.join(temporary, provenanceRelative);
  within(temporary, provenancePath, "provenance output");
  await mkdir(path.dirname(provenancePath), { recursive: true });
  await writeFile(provenancePath, `${JSON.stringify(provenance, null, 2)}\n`, { encoding: "utf8", mode: 0o644, flag: "wx" });
  await chmod(provenancePath, 0o644);
  let hadPrior = false;
  try {
    await rename(target, backup);
    hadPrior = true;
  } catch (error) {
    if (error?.code !== "ENOENT") throw error;
  }
  try {
    await rename(temporary, target);
  } catch (error) {
    if (hadPrior) await rename(backup, target);
    throw error;
  }
  await rm(backup, { recursive: true, force: true });
}

async function main() {
  const args = parseArguments(process.argv);
  if (!args.output.startsWith("build/codegen/")) fail("--output must be below build/codegen/");
  if (!args.result.startsWith(`${args.output}/`)) fail("--result must be below --output");
  const outputAbsolute = path.join(repositoryRoot, args.output);
  const resultRelative = slash(path.relative(outputAbsolute, path.join(repositoryRoot, args.result)));
  if (resultRelative !== "provenance.json") fail("--result must be exactly <output>/provenance.json");
  await rejectSymlinkPath(path.dirname(outputAbsolute), true);

  const configFile = await readRegular(args.config, "configuration");
  const config = parseStrictJSON(configFile.data, args.config);
  rejectDangerousKeys(config);
  rejectUnsafeNumbers(config);
  rejectUnpairedSurrogates(config);
  validateConfiguration(config);

  const resources = new Map();
  const resourceEntries = [];
  for (const resource of config.resources) {
    const file = await readRegular(resource.path, "schema resource");
    const document = parseStrictJSON(file.data, resource.path);
    rejectDangerousKeys(document);
    rejectUnsafeNumbers(document);
    rejectUnpairedSurrogates(document);
    validateSchemaKeywords(document);
    if (document.$schema !== draft202012 || document.$id !== resource.uri) fail(`${resource.path} must declare exact Draft 2020-12 and configured $id`);
    resources.set(resource.uri, document);
    resourceEntries.push({ kind: "resource", path: file.path, uri: resource.uri, sha256: file.sha256, bytes: file.bytes, mode: file.mode });
  }

  const ajv = new Ajv2020({ allErrors: true, strict: false, ownProperties: true, validateFormats: true });
  addFormats(ajv);
  ajv.addFormat("date-time", { type: "string", validate: strictDateTime });
  for (const resource of config.resources) {
    const document = resources.get(resource.uri);
    if (!ajv.validateSchema(document)) fail(`${resource.path} is not valid Draft 2020-12: ${ajv.errorsText(ajv.errors)}`);
    ajv.addSchema(document, resource.uri);
  }
  const rootConfig = config.roots[0];
  const rootSplit = splitReference(rootConfig.ref, rootConfig.ref);
  const rootValidate = ajv.getSchema(rootConfig.ref) ?? ajv.compile({ $ref: rootConfig.ref });
  if (!resources.has(rootSplit.uri)) fail(`root resource is outside the explicit bundle: ${rootSplit.uri}`);
  const names = new Map([[rootConfig.ref, rootConfig.name]]);
  const compiler = new ModelCompiler(resources, names);
  compiler.compileRoot(rootConfig.ref, rootConfig.name);
  if ([...compiler.types.values()].some((type) => type.kind === "pending")) fail("recursive type compilation left an unresolved placeholder");

  const fixtureEntries = [];
  const validFixtures = [];
  const checks = [];
  const forwardFixtures = [];
  const invalidFixtures = [];
  for (const fixtureClass of ["valid", "forward", "invalid"]) {
    for (const fixture of config.fixtures[fixtureClass]) {
      const file = await readRegular(fixture.path, `${fixtureClass} fixture`);
      let document;
      let fixtureInputError;
      try {
        document = parseStrictJSON(file.data, fixture.path);
        rejectDangerousKeys(document);
        rejectUnsafeNumbers(document);
        rejectUnpairedSurrogates(document);
      } catch (error) {
        if (fixtureClass !== "invalid") throw error;
        fixtureInputError = error;
      }
      const accepted = fixtureInputError === undefined && rootValidate(document);
      const passed = fixtureClass === "valid" ? accepted : !accepted;
      checks.push({ id: `schema-${fixtureClass}-${fixture.id}`, passed, detail: accepted ? "accepted" : fixtureInputError?.message ?? ajv.errorsText(rootValidate.errors) });
      if (!passed) fail(`${fixtureClass} fixture ${fixture.id} had the wrong schema result`);
      fixtureEntries.push({ kind: "fixture", id: fixture.id, class: fixtureClass, path: file.path, sha256: file.sha256, bytes: file.bytes, mode: file.mode });
      if (fixtureClass === "valid") validFixtures.push({ id: fixture.id, text: file.data.toString("utf8").trim(), document });
      if (fixtureClass === "forward") forwardFixtures.push({ id: fixture.id, text: file.data.toString("utf8").trim(), document });
      if (fixtureClass === "invalid") invalidFixtures.push({ id: fixture.id, text: file.data.toString("utf8").trim(), document });
    }
  }

  const toolEntries = [];
  for (const toolPath of toolFiles) {
    const file = await readRegular(toolPath, "generator input");
    toolEntries.push({ kind: "generator", path: file.path, uri: "", sha256: file.sha256, bytes: file.bytes, mode: file.mode });
  }
  const configurationEntry = { kind: "configuration", path: configFile.path, uri: "", sha256: configFile.sha256, bytes: configFile.bytes, mode: configFile.mode };
  const inputEntries = [...toolEntries, configurationEntry, ...resourceEntries, ...fixtureEntries];
  const inputsSHA256 = aggregateInputs(inputEntries);

  const generated = {
    ...emitGo(compiler.types, inputsSHA256, validFixtures, forwardFixtures, invalidFixtures, rootConfig.name),
    ...emitPythonTyped(compiler.types, inputsSHA256, validFixtures, forwardFixtures, invalidFixtures, rootConfig.name),
    ...emitTypeScriptTyped(compiler.types, inputsSHA256, validFixtures, forwardFixtures, invalidFixtures, rootConfig.name),
  };
  const expectedOutputPaths = new Set(Object.values(config.outputs).flatMap((value) => [value.model, value.probe]));
  const actualOutputPaths = new Set(Object.keys(generated));
  if (expectedOutputPaths.size !== actualOutputPaths.size || [...expectedOutputPaths].some((value) => !actualOutputPaths.has(value))) fail("configured output paths do not equal the fixed emitter output set");
  const outputs = Object.entries(generated).map(([outputPath, source]) => {
    const language = outputPath.split("/", 1)[0];
    const data = Buffer.from(source, "utf8");
    return { language, path: outputPath, sha256: sha256(data), bytes: data.length, mode: "100644" };
  }).sort((left, right) => byteCompare(left.path, right.path));
  const outputsSHA256 = aggregateOutputs(outputs);
  checks.push({ id: "offline-resource-closure", passed: true, detail: `${resources.size} explicit resources` });
  checks.push({ id: "mapping-profile", passed: true, detail: `${compiler.types.size} named types` });
  checks.push({ id: "deterministic-output-set", passed: true, detail: `${outputs.length} outputs` });

  const provenance = {
    schema_version: 1,
    pipeline_id: pipelineID,
    mapping_profile: mappingProfile,
    command: {
      entry: "scripts/generate.mjs",
      config: args.config,
      output: "<output>",
      result: "<output>/provenance.json",
    },
    generator: {
      path: "scripts/generate.mjs",
      sha256: toolEntries.find((entry) => entry.path === "scripts/generate.mjs").sha256,
      files: toolEntries.map(({ kind, uri, ...entry }) => entry),
    },
    configuration: {
      path: configFile.path,
      sha256: configFile.sha256,
      bytes: configFile.bytes,
      mode: configFile.mode,
    },
    resources: resourceEntries.map(({ kind, ...entry }) => entry),
    fixtures: fixtureEntries.map(({ kind, ...entry }) => entry),
    inputs_sha256: inputsSHA256,
    outputs,
    outputs_sha256: outputsSHA256,
    checks,
  };
  await writeTree(outputAbsolute, generated, resultRelative, provenance);
}

try {
  await main();
} catch (error) {
  console.error(`AROP code generation failed: ${error.message}`);
  process.exit(1);
}
