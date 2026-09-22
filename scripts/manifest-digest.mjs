import path from "node:path";

import {
  loadStructuredFile,
  manifestDigest,
  repositoryRoot,
} from "./lib/repository.mjs";

const requestedPath = process.argv[2];
if (!requestedPath) {
  console.error("usage: npm run manifest:digest -- <manifest.yaml|manifest.json>");
  process.exit(2);
}

const manifestPath = path.resolve(repositoryRoot, requestedPath);
const manifest = await loadStructuredFile(manifestPath);
console.log(manifestDigest(manifest));
