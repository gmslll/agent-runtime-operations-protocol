import { readFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

import Ajv2020 from "ajv/dist/2020.js";
import addFormats from "ajv-formats";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const ajv = new Ajv2020({ allErrors: true, strict: true, validateFormats: true });
addFormats(ajv);
for (const relative of [
  "schemas/common/identifiers.schema.json",
  "schemas/resources/asset-ref-v1.schema.json",
  "schemas/resources/asset-exchange-v1.schema.json",
]) {
  ajv.addSchema(JSON.parse(await readFile(path.join(root, relative), "utf8")));
}
const validate = ajv.getSchema("https://arop.invalid/schemas/v1/resources/asset-exchange-v1.schema.json");
if (validate === undefined) throw new Error("asset exchange schema was not loaded");
const groups = {
  valid: ["upload-request.json", "download-request.json", "upload-grant.json", "download-grant.json"],
  invalid: ["absolute-broker-url.json", "secret-and-endpoint.json", "partial-asset.json"],
  forward: ["grant-future-field.json"],
};
for (const name of groups.valid) {
  const document = JSON.parse(await readFile(path.join(root, "examples/assets/valid", name), "utf8"));
  if (!validate(document)) throw new Error(`${name} rejected: ${ajv.errorsText(validate.errors)}`);
}
for (const name of [...groups.invalid, ...groups.forward]) {
  const directory = groups.invalid.includes(name) ? "invalid" : "forward";
  const document = JSON.parse(await readFile(path.join(root, "examples/assets", directory, name), "utf8"));
  if (validate(document)) throw new Error(`${name} was accepted`);
}
const openapi = await readFile(path.join(root, "openapi/fragments/control-plane/assets-v1.yaml"), "utf8");
for (const route of ["/v1/runs/{run_id}/assets:exchange:", "/v1/asset-content/{grant_id}:"]) {
  if (!openapi.includes(route)) throw new Error(`missing route ${route}`);
}
for (const field of ["grant_id", "method", "broker_path", "bearer_token", "expires_at", "max_uses"]) {
  if (!openapi.includes(field)) throw new Error(`missing public field ${field}`);
}
console.log("asset exchange contract accepted");
