// Generated from schemas/resources/asset-exchange-v1.schema.json using the
// arop-wire-model-v1 object, const discriminator, and closed-field rules.

export interface AssetRef {
  readonly asset_id: string;
  readonly name: string;
  readonly media_type: string;
  readonly size_bytes: number;
  readonly digest?: string;
  readonly access: { readonly mode: "brokered"; readonly expires_at?: string };
}

export interface UploadRequest {
  readonly kind: "upload_request";
  readonly run_id: string;
  readonly name: string;
  readonly media_type: string;
  readonly size_bytes: number;
  readonly digest?: string;
}

export interface DownloadRequest {
  readonly kind: "download_request";
  readonly run_id: string;
  readonly asset_id: string;
}

export interface GrantResponse {
  readonly kind: "grant";
  readonly asset: AssetRef;
  readonly grant_id: string;
  readonly method: "upload" | "download";
  readonly broker_path: string;
  readonly bearer_token: string;
  readonly expires_at: string;
  readonly max_uses: number;
}

export type AssetExchange = UploadRequest | DownloadRequest | GrantResponse;

const runID = /^run_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/u;
const assetID = /^asset_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/u;
const grantID = /^grant_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/u;
const digest = /^sha256:[0-9a-f]{64}$/u;
const mediaType = /^[a-z0-9!#$&^_.+-]+\/[a-z0-9!#$&^_.+-]+$/u;
const forbiddenName = /(?:^|[\\/])\.\.(?:[\\/]|$)/u;
const token = /^agt_[A-Za-z0-9_-]{8,128}$/u;
const brokerPath = /^\/v1\/asset-content\/grant_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/u;
const dateTime = /^[0-9]{4}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12][0-9]|3[01])T(?:[01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](?:\.[0-9]+)?(?:Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])$/u;

function objectValue(value: unknown): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) throw new Error("expected object");
  return value;
}

function closed(value: Record<string, unknown>, allowed: readonly string[], required: readonly string[]): void {
  for (const key of Object.keys(value)) if (!allowed.includes(key)) throw new Error("unknown field " + key);
  for (const key of required) if (!Object.hasOwn(value, key)) throw new Error("missing field " + key);
}

function nameValue(value: unknown): string {
  if (typeof value !== "string" || value.length < 1 || value.length > 512 || forbiddenName.test(value)) throw new Error("invalid name");
  return value;
}

function assetRef(value: unknown): AssetRef {
  const record = objectValue(value);
  closed(record, ["asset_id", "name", "media_type", "size_bytes", "digest", "access"], ["asset_id", "name", "media_type", "size_bytes", "access"]);
  if (typeof record.asset_id !== "string" || !assetID.test(record.asset_id)) throw new Error("invalid asset id");
  if (typeof record.media_type !== "string" || !mediaType.test(record.media_type)) throw new Error("invalid media type");
  if (typeof record.size_bytes !== "number" || !Number.isSafeInteger(record.size_bytes) || record.size_bytes < 0) throw new Error("invalid size");
  if (record.digest !== undefined && (typeof record.digest !== "string" || !digest.test(record.digest))) throw new Error("invalid digest");
  const access = objectValue(record.access);
  closed(access, ["mode", "expires_at"], ["mode"]);
  if (access.mode !== "brokered") throw new Error("invalid access");
  if (access.expires_at !== undefined && (typeof access.expires_at !== "string" || !dateTime.test(access.expires_at))) throw new Error("invalid expiry");
  return record as unknown as AssetRef;
}

export function decodeAssetExchange(data: string): AssetExchange {
  const value = objectValue(JSON.parse(data));
  closed(value, ["kind", "run_id", "name", "media_type", "size_bytes", "digest", "asset_id", "asset", "grant_id", "method", "broker_path", "bearer_token", "expires_at", "max_uses"], ["kind"]);
  if (value.kind === "upload_request") {
    closed(value, ["kind", "run_id", "name", "media_type", "size_bytes", "digest"], ["kind", "run_id", "name", "media_type", "size_bytes"]);
    if (typeof value.run_id !== "string" || !runID.test(value.run_id)) throw new Error("invalid run id");
    nameValue(value.name);
    if (typeof value.media_type !== "string" || !mediaType.test(value.media_type)) throw new Error("invalid media type");
    if (typeof value.size_bytes !== "number" || !Number.isSafeInteger(value.size_bytes) || value.size_bytes < 0) throw new Error("invalid size");
    if (value.digest !== undefined && (typeof value.digest !== "string" || !digest.test(value.digest))) throw new Error("invalid digest");
    return value as unknown as UploadRequest;
  }
  if (value.kind === "download_request") {
    closed(value, ["kind", "run_id", "asset_id"], ["kind", "run_id", "asset_id"]);
    if (typeof value.run_id !== "string" || !runID.test(value.run_id) || typeof value.asset_id !== "string" || !assetID.test(value.asset_id)) throw new Error("invalid download request");
    return value as unknown as DownloadRequest;
  }
  if (value.kind === "grant") {
    closed(value, ["kind", "asset", "grant_id", "method", "broker_path", "bearer_token", "expires_at", "max_uses"], ["kind", "asset", "grant_id", "method", "broker_path", "bearer_token", "expires_at", "max_uses"]);
    if (value.method !== "upload" && value.method !== "download") throw new Error("invalid method");
    if (typeof value.grant_id !== "string" || !grantID.test(value.grant_id) || typeof value.broker_path !== "string" || !brokerPath.test(value.broker_path) || typeof value.bearer_token !== "string" || !token.test(value.bearer_token) || typeof value.expires_at !== "string" || !dateTime.test(value.expires_at)) throw new Error("invalid grant");
    if (typeof value.max_uses !== "number" || !Number.isSafeInteger(value.max_uses) || value.max_uses < 1) throw new Error("invalid max uses");
    assetRef(value.asset);
    nameValue((value.asset as AssetRef).name);
    return value as unknown as GrantResponse;
  }
  throw new Error("unknown asset exchange discriminator");
}
