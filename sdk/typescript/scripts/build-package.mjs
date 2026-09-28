import { createHash } from "node:crypto";
import { mkdir, readFile, readdir, writeFile } from "node:fs/promises";
import path from "node:path";
import ts from "typescript";

const cliArguments = process.argv;
const args = parseArgs(cliArguments.slice(2));
const root = path.resolve(args.root ?? ".");
const outDir = path.resolve(args.outDir);
const packageRoot = path.join(root, "sdk/typescript");
const sourceRoot = path.join(packageRoot, "src");
const metadata = strictPackageMetadata(JSON.parse(await readFile(path.join(packageRoot, "package.json"), "utf8")));
const sources = (await sourceInventory(sourceRoot)).filter((file) => !file.endsWith(".test.ts") && !file.endsWith("/probe.ts"));
const emitted = compile(sources, sourceRoot);
const packedMetadata = { ...metadata };
delete packedMetadata.devDependencies;
const files = new Map([["package/package.json", Buffer.from(`${JSON.stringify(packedMetadata, null, 2)}\n`, "utf8")]]);
for (const [name, data] of emitted) files.set(`package/dist/${name}`, data);
const archive = gzip(tar(files));
const archiveName = `arop-sdk-${metadata.version}.tgz`;
await mkdir(outDir, { recursive: true, mode: 0o700 });
await writeFile(path.join(outDir, archiveName), archive, { mode: 0o600 });
const manifest = {
  schema_version: 1,
  name: metadata.name,
  version: metadata.version,
  artifacts: [{ name: archiveName, sha256: createHash("sha256").update(archive).digest("hex"), bytes: archive.byteLength }],
  files: [...files].sort(([left], [right]) => left < right ? -1 : left > right ? 1 : 0).map(([name, data]) => ({ name, sha256: createHash("sha256").update(data).digest("hex"), bytes: data.byteLength })),
};
await writeFile(path.join(outDir, "manifest.json"), `${JSON.stringify(manifest, null, 2)}\n`, { mode: 0o600 });

function parseArgs(values) {
  const result = {};
  for (let index = 0; index < values.length; index += 2) {
    const key = values[index], value = values[index + 1];
    if ((key !== "--out-dir" && key !== "--root") || value === undefined) throw new Error("usage: build-package.mjs --out-dir DIR [--root ROOT]");
    result[key === "--out-dir" ? "outDir" : "root"] = value;
  }
  if (typeof result.outDir !== "string") throw new Error("missing --out-dir");
  return result;
}

function strictPackageMetadata(value) {
  const keys = Object.keys(value).sort();
  const allowed = ["description", "devDependencies", "engines", "exports", "files", "license", "main", "module", "name", "sideEffects", "type", "types", "version"].sort();
  if (JSON.stringify(keys) !== JSON.stringify(allowed) || value.name !== "@arop/sdk" || value.version !== "0.1.0-dev.0" || value.type !== "module" || value.sideEffects !== false || value.license !== "Apache-2.0") throw new Error("invalid package metadata");
  if (JSON.stringify(value.files) !== JSON.stringify(["dist"]) || value.devDependencies?.typescript !== "5.9.3") throw new Error("invalid package allowlist");
  if (Object.values(value.exports).some((entry) => entry === null || typeof entry !== "object" || typeof entry.import !== "string" || typeof entry.types !== "string" || Object.keys(entry).sort().join(",") !== "import,types")) throw new Error("invalid exports map");
  return value;
}

async function sourceInventory(directory) {
  const result = [];
  for (const entry of (await readdir(directory, { withFileTypes: true })).sort((left, right) => left.name < right.name ? -1 : left.name > right.name ? 1 : 0)) {
    const current = path.join(directory, entry.name);
    if (entry.isSymbolicLink()) throw new Error("source inventory contains symlink");
    if (entry.isDirectory()) result.push(...await sourceInventory(current));
    else if (entry.isFile() && entry.name.endsWith(".ts")) result.push(current);
    else if (!entry.isFile()) throw new Error("source inventory contains special file");
  }
  return result;
}

function compile(sources, sourceRoot) {
  const options = { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.NodeNext, moduleResolution: ts.ModuleResolutionKind.NodeNext, strict: true, declaration: true, sourceMap: false, declarationMap: false, noEmitOnError: true, rootDir: sourceRoot, outDir: path.join(sourceRoot, "__arop_dist__"), skipLibCheck: false, lib: ["lib.es2022.d.ts", "lib.dom.d.ts"] };
  const outputs = new Map();
  const host = ts.createCompilerHost(options, true);
  host.writeFile = (fileName, content) => {
    const marker = `${path.sep}__arop_dist__${path.sep}`;
    const at = fileName.indexOf(marker);
    if (at < 0) throw new Error("compiler emitted outside output root");
    const relative = fileName.slice(at + marker.length).split(path.sep).join("/");
    if (!/^[A-Za-z0-9._/-]+$/u.test(relative) || relative.includes("..") || outputs.has(relative)) throw new Error("invalid compiler output");
    outputs.set(relative, Buffer.from(content, "utf8"));
  };
  const program = ts.createProgram(sources, options, host);
  const result = program.emit();
  const diagnostics = ts.getPreEmitDiagnostics(program).concat(result.diagnostics);
  if (diagnostics.length !== 0 || result.emitSkipped) throw new Error(ts.formatDiagnosticsWithColorAndContext(diagnostics, { getCanonicalFileName: (file) => file, getCurrentDirectory: () => sourceRoot, getNewLine: () => "\n" }));
  return new Map([...outputs].sort(([left], [right]) => left < right ? -1 : left > right ? 1 : 0));
}

function tar(files) {
  const chunks = [];
  for (const [name, data] of [...files].sort(([left], [right]) => left < right ? -1 : left > right ? 1 : 0)) {
    if (Buffer.byteLength(name) > 100 || data.byteLength > 0o77777777777) throw new Error("package file cannot be represented by ustar");
    const header = Buffer.alloc(512);
    header.write(name, 0, 100, "utf8");
    octal(header, 100, 8, 0o644); octal(header, 108, 8, 0); octal(header, 116, 8, 0);
    octal(header, 124, 12, data.byteLength); octal(header, 136, 12, 0);
    header.fill(0x20, 148, 156); header[156] = 0x30;
    header.write("ustar\0", 257, 6, "ascii"); header.write("00", 263, 2, "ascii");
    header.write("root", 265, 4, "ascii"); header.write("root", 297, 4, "ascii");
    octal(header, 148, 8, [...header].reduce((sum, byte) => sum + byte, 0));
    chunks.push(header, data, Buffer.alloc((512 - (data.byteLength % 512)) % 512));
  }
  chunks.push(Buffer.alloc(1024));
  return Buffer.concat(chunks);
}

function octal(buffer, offset, length, value) {
  const wire = value.toString(8).padStart(length - 1, "0");
  if (wire.length !== length - 1) throw new Error("tar field overflow");
  buffer.write(wire, offset, length - 1, "ascii"); buffer[offset + length - 1] = 0;
}

function gzip(data) {
  const blocks = [];
  for (let offset = 0; offset < data.byteLength || offset === 0; offset += 65535) {
    const length = Math.min(65535, data.byteLength - offset);
    const block = Buffer.alloc(5 + length);
    block[0] = offset + length >= data.byteLength ? 1 : 0;
    block.writeUInt16LE(length, 1); block.writeUInt16LE((~length) & 0xffff, 3);
    data.copy(block, 5, offset, offset + length); blocks.push(block);
    if (offset + length >= data.byteLength) break;
  }
  const trailer = Buffer.alloc(8); trailer.writeUInt32LE(crc32(data), 0); trailer.writeUInt32LE(data.byteLength >>> 0, 4);
  return Buffer.concat([Buffer.from([0x1f, 0x8b, 0x08, 0x00, 0, 0, 0, 0, 0, 0xff]), ...blocks, trailer]);
}

function crc32(data) {
  let value = 0xffffffff;
  for (const byte of data) { value ^= byte; for (let bit = 0; bit < 8; bit += 1) value = (value >>> 1) ^ ((value & 1) === 1 ? 0xedb88320 : 0); }
  return (value ^ 0xffffffff) >>> 0;
}
