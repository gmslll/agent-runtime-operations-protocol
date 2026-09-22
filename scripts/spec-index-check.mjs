// IR-03 boundary: only strict structured-file and JSON Schema validation live
// here. Generic governance, Git, evidence and report semantics are Go-owned.
import { mkdir, writeFile } from "node:fs/promises";
import path from "node:path";
import Ajv2020 from "ajv/dist/2020.js";
import { loadStructuredFile, parseJSONWithUniqueKeys, repositoryRoot, walkFiles } from "./lib/repository.mjs";

const checks = [];
const errors = [];
function record(name, passed, detail) {
  checks.push({ name, passed, detail });
  if (!passed) errors.push(`${name}: ${detail}`);
}
function relative(filePath) { return path.relative(repositoryRoot, filePath).split(path.sep).join("/"); }
function formatAjvErrors(values = []) { return values.map((value) => `${value.instancePath || "/"} ${value.message}`).join("; "); }
function resolveJsonPointer(document, fragment) {
  if (!fragment || fragment === "#") return document;
  if (!fragment.startsWith("#/")) throw new Error(`unsupported JSON Schema fragment: ${fragment}`);
  return fragment.slice(2).split("/").map((token) => token.replaceAll("~1", "/").replaceAll("~0", "~"))
    .reduce((current, token) => {
      if (current === null || typeof current !== "object" || !Object.hasOwn(current, token)) throw new Error(`JSON Pointer does not resolve: ${fragment}`);
      return current[token];
    }, document);
}
function collectRefs(value, output = []) {
  if (Array.isArray(value)) for (const item of value) collectRefs(item, output);
  else if (value && typeof value === "object") {
    if (typeof value.$ref === "string") output.push(value.$ref);
    for (const item of Object.values(value)) collectRefs(item, output);
  }
  return output;
}

const yamlFiles = (await walkFiles(repositoryRoot, (filePath) => [".yaml", ".yml"].includes(path.extname(filePath))))
  .filter((filePath) => !relative(filePath).startsWith("build/"));
const jsonFiles = (await walkFiles(repositoryRoot, (filePath) => filePath.endsWith(".json")))
  .filter((filePath) => !relative(filePath).startsWith("build/"));
const yamlProblems = [];
for (const filePath of yamlFiles) {
  try { await loadStructuredFile(filePath); } catch (error) { yamlProblems.push(`${relative(filePath)}: ${error.message}`); }
}
record("yaml-syntax", yamlProblems.length === 0,
  yamlProblems.length === 0 ? `${yamlFiles.length} YAML files parse as one document with unique keys` : yamlProblems.join("; "));
const jsonProblems = [];
for (const filePath of jsonFiles) {
  try { await loadStructuredFile(filePath); } catch (error) { jsonProblems.push(`${relative(filePath)}: ${error.message}`); }
}
record("json-syntax-and-unique-keys", jsonProblems.length === 0,
  jsonProblems.length === 0 ? `${jsonFiles.length} JSON files parse without duplicates or trailing values` : jsonProblems.join("; "));
let duplicateProbe = false;
try { parseJSONWithUniqueKeys('{"outer":{"same":1,"same":2}}', "duplicate-key-probe"); }
catch (error) { duplicateProbe = /duplicate object key/u.test(error.message); }
record("json-duplicate-key-negative-probe", duplicateProbe, "nested duplicate JSON keys are rejected before schema validation");

const planningBindings = [
  ["spec/schemas/artifact-manifest.schema.json", "spec/artifact-manifest.yaml"],
  ["spec/schemas/requirements.schema.json", "spec/requirements.yaml"],
  ["spec/schemas/conflicts.schema.json", "spec/conflicts.yaml"],
];
const ajv = new Ajv2020({ allErrors: true, strict: true, validateFormats: true });
const planningProblems = [];
for (const [schemaPath, documentPath] of planningBindings) {
  try {
    const schema = await loadStructuredFile(path.join(repositoryRoot, schemaPath));
    const document = await loadStructuredFile(path.join(repositoryRoot, documentPath));
    if (!ajv.validateSchema(schema)) planningProblems.push(`${schemaPath}: ${formatAjvErrors(ajv.errors)}`);
    else {
      const validate = ajv.compile(schema);
      if (!validate(document)) planningProblems.push(`${documentPath}: ${formatAjvErrors(validate.errors)}`);
    }
  } catch (error) { planningProblems.push(`${schemaPath}: ${error.message}`); }
}
record("planning-metadata-json-schema", planningProblems.length === 0,
  planningProblems.length === 0 ? "artifact, requirement and conflict metadata pass executable schemas" : planningProblems.join("; "));

const metaSchemaFiles = await walkFiles(path.join(repositoryRoot, "spec/schemas"), (filePath) => filePath.endsWith(".schema.json"));
const metaProblems = [];
for (const schemaFile of metaSchemaFiles) {
  try {
    const schema = await loadStructuredFile(schemaFile);
    if (!ajv.validateSchema(schema)) metaProblems.push(`${relative(schemaFile)}: ${formatAjvErrors(ajv.errors)}`);
  } catch (error) { metaProblems.push(`${relative(schemaFile)}: ${error.message}`); }
}
record("planning-meta-schema-validity", metaProblems.length === 0,
  metaProblems.length === 0 ? `${metaSchemaFiles.length} Draft 2020-12 planning schemas valid` : metaProblems.join("; "));

const schemaFiles = await walkFiles(path.join(repositoryRoot, "schemas"), (filePath) => filePath.endsWith(".schema.json"));
const schemaDocuments = new Map();
const schemaByID = new Map();
const refProblems = [];
for (const schemaFile of schemaFiles) {
  try {
    const schema = await loadStructuredFile(schemaFile);
    schemaDocuments.set(schemaFile, schema);
    if (typeof schema.$id !== "string") refProblems.push(`${relative(schemaFile)} has no $id`);
    else if (schemaByID.has(schema.$id)) refProblems.push(`${relative(schemaFile)} duplicates $id ${schema.$id}`);
    else schemaByID.set(schema.$id, schema);
  } catch (error) { refProblems.push(`${relative(schemaFile)}: ${error.message}`); }
}
for (const [schemaFile, schema] of schemaDocuments) {
  for (const reference of collectRefs(schema)) {
    try {
      const hashIndex = reference.indexOf("#");
      const base = hashIndex >= 0 ? reference.slice(0, hashIndex) : reference;
      const fragment = hashIndex >= 0 ? reference.slice(hashIndex) : "";
      let target = schema;
      if (/^https?:/iu.test(base)) {
        target = schemaByID.get(base);
        if (!target) throw new Error(`remote or unbundled $ref forbidden: ${reference}`);
      } else if (base) {
        const targetPath = path.resolve(path.dirname(schemaFile), base);
        if (!targetPath.startsWith(`${repositoryRoot}${path.sep}`)) throw new Error(`$ref escapes repository: ${reference}`);
        target = schemaDocuments.get(targetPath) ?? await loadStructuredFile(targetPath);
      }
      resolveJsonPointer(target, fragment);
    } catch (error) { refProblems.push(`${relative(schemaFile)} $ref ${reference}: ${error.message}`); }
  }
}
record("json-schema-local-ref-closure", refProblems.length === 0,
  refProblems.length === 0 ? `${schemaFiles.length} protocol schemas have offline ref closure` : refProblems.join("; "));

const result = {
  checks,
  errors,
  input_paths: [...yamlFiles.map(relative), ...jsonFiles.map(relative), ...schemaFiles.map(relative), "scripts/spec-index-check.mjs", "scripts/lib/repository.mjs"],
  summary: { yaml_files: yamlFiles.length, json_files: jsonFiles.length, protocol_schemas: schemaFiles.length, planning_meta_schemas: metaSchemaFiles.length },
};
const resultFile = process.env.AROP_RESULT_FILE;
if (!resultFile) {
  console.error("AROP_RESULT_FILE is required; Go owns governance and report generation.");
  process.exit(2);
}
await mkdir(path.dirname(path.resolve(resultFile)), { recursive: true });
await writeFile(path.resolve(resultFile), `${JSON.stringify(result, null, 2)}\n`, "utf8");
if (errors.length) process.exit(1);
console.log(`AROP Schema validation passed: ${checks.length} checks.`);
