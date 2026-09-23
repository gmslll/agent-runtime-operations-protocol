import {
  manifestFileDigest,
  resolveRepositoryFile,
} from "./lib/repository.mjs";

const requestedPath = process.argv[2];
if (!requestedPath) {
  console.error("usage: npm run manifest:digest -- <manifest.yaml|manifest.json>");
  process.exit(2);
}

const manifestPath = await resolveRepositoryFile(requestedPath);
console.log(await manifestFileDigest(manifestPath));
