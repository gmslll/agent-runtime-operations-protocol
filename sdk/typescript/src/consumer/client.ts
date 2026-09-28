import { decodeAROPError, type AROPError } from "../generated/control-plane/publication.gen.js";
import { decodeRunStatus, encodeRunCommand, encodeRunRequest, type RunCommand, type RunRequest, type RunStatus } from "../generated/run/run.gen.js";
import { RelayStreamingClient, type HeaderReader, type StreamOptions, type StreamTransport, type TokenSource } from "../streaming/client.js";
import type { StreamEvent } from "../generated/streaming/streaming.gen.js";

const RUN_ID = /^run_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/u;
const KEY = /^[!-~]{8,200}$/u;
const MAX_BODY = 8 << 20;

export interface HTTPRequest {
  readonly method: "GET" | "POST";
  readonly url: string;
  readonly headers: Readonly<Record<string, string>>;
  readonly body?: Uint8Array;
  readonly signal?: AbortSignal;
}

export interface HTTPResponse {
  readonly status: number;
  readonly headers: HeaderReader;
  readonly body: Uint8Array;
}

export interface HTTPTransport { request(request: HTTPRequest): Promise<HTTPResponse> }

export class ControlPlaneError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    readonly category: string,
    readonly retryable: boolean,
    readonly retryAfterSeconds?: number,
  ) { super("control plane request failed"); }
}

export class ConsumerClient {
  readonly #base: string;
  readonly #tokens: TokenSource;
  readonly #http: HTTPTransport;
  readonly #relay: RelayStreamingClient;

  constructor(controlPlaneBase: string, tokens: TokenSource, http: HTTPTransport, streams: StreamTransport) {
    const parsed = new URL(controlPlaneBase);
    if (parsed.protocol !== "https:" || parsed.username !== "" || parsed.password !== "" || (parsed.pathname !== "" && parsed.pathname !== "/") || parsed.search !== "" || parsed.hash !== "") throw new Error("invalid control plane URL");
    if (tokens === null || http === null || streams === null) throw new Error("invalid consumer configuration");
    this.#base = parsed.origin;
    this.#tokens = tokens;
    this.#http = http;
    this.#relay = new RelayStreamingClient(parsed.origin, tokens, streams);
  }

  async createRun(request: RunRequest, idempotencyKey: string, signal?: AbortSignal): Promise<RunStatus> {
    const response = await this.#request("POST", "/v1/agent-runs", encodeRunRequest(request), idempotencyKey, signal);
    if (response.status !== 201) throw remoteError(response);
    const status = decodeStatus(response);
    if (exactHeader(response.headers, "location") !== `/v1/agent-runs/${status.run_id}`) throw new Error("invalid create response");
    return status;
  }

  async getRun(runID: string, signal?: AbortSignal): Promise<RunStatus> {
    validateRunID(runID);
    const response = await this.#request("GET", `/v1/agent-runs/${runID}`, undefined, undefined, signal);
    if (response.status !== 200) throw remoteError(response);
    const status = decodeStatus(response);
    if (status.run_id !== runID) throw new Error("invalid run response");
    return status;
  }

  async submitCommand(runID: string, command: RunCommand, idempotencyKey: string, signal?: AbortSignal): Promise<RunStatus> {
    validateRunID(runID);
    const response = await this.#request("POST", `/v1/agent-runs/${runID}/commands`, encodeRunCommand(command), idempotencyKey, signal);
    if (response.status !== 200) throw remoteError(response);
    const status = decodeStatus(response);
    if (status.run_id !== runID) throw new Error("invalid command response");
    return status;
  }

  stream(runID: string, handle: (event: StreamEvent) => void | Promise<void>, options: StreamOptions = {}): Promise<void> {
    return this.#relay.stream(runID, handle, options);
  }

  async #request(method: "GET" | "POST", path: string, body: string | undefined, idempotencyKey: string | undefined, signal: AbortSignal | undefined): Promise<HTTPResponse> {
    if (idempotencyKey !== undefined && !KEY.test(idempotencyKey)) throw new Error("invalid idempotency key");
    const token = await this.#tokens.token(signal);
    if (!validBearer(token)) throw new Error("control plane authentication unavailable");
    const headers: Record<string, string> = { Accept: "application/json", Authorization: `Bearer ${token}` };
    let bytes: Uint8Array | undefined;
    if (body !== undefined) {
      bytes = new TextEncoder().encode(body);
      if (bytes.byteLength > 1 << 20) throw new Error("request body exceeds limit");
      headers["Content-Type"] = "application/json";
    }
    if (idempotencyKey !== undefined) headers["Idempotency-Key"] = idempotencyKey;
    const base = { method, url: `${this.#base}${path}`, headers } as const;
    const transportRequest: HTTPRequest = {
      ...base,
      ...(bytes === undefined ? {} : { body: bytes }),
      ...(signal === undefined ? {} : { signal }),
    };
    const response = await this.#http.request(transportRequest);
    if (!Number.isInteger(response.status) || response.body.byteLength > MAX_BODY) throw new Error("invalid control plane response");
    return response;
  }
}

function decodeStatus(response: HTTPResponse): RunStatus {
  if (exactHeader(response.headers, "content-type") !== "application/json") throw new Error("invalid control plane response");
  return decodeRunStatus(new TextDecoder("utf-8", { fatal: true }).decode(response.body));
}

function remoteError(response: HTTPResponse): ControlPlaneError {
  if (exactHeader(response.headers, "content-type") !== "application/json") return new ControlPlaneError(response.status, "REMOTE_REJECTED", "protocol", false);
  let wire: AROPError;
  try { wire = decodeAROPError(new TextDecoder("utf-8", { fatal: true }).decode(response.body)); }
  catch { return new ControlPlaneError(response.status, "REMOTE_REJECTED", "protocol", false); }
  const retryHeader = exactHeader(response.headers, "retry-after");
  const retryAfter = retryHeader === null ? undefined : strictRetryAfter(retryHeader);
  if ((response.status === 429 || response.status === 503) && (retryAfter === undefined || wire.retry_after_seconds !== retryAfter)) return new ControlPlaneError(response.status, "REMOTE_REJECTED", "protocol", false);
  if (response.status === 401 && exactHeader(response.headers, "www-authenticate") === null) return new ControlPlaneError(response.status, "REMOTE_REJECTED", "protocol", false);
  return new ControlPlaneError(response.status, wire.code, wire.category, wire.retryable, retryAfter);
}

function exactHeader(headers: HeaderReader, name: string): string | null {
  const value = headers.get(name);
  return value === null || value === "" || value.includes(",") || value.trim() !== value ? null : value;
}

function strictRetryAfter(value: string): number | undefined {
  if (!/^[1-9][0-9]{0,4}$/u.test(value)) return undefined;
  const result = Number(value);
  return result <= 86400 ? result : undefined;
}

function validateRunID(value: string): void { if (!RUN_ID.test(value)) throw new Error("invalid run id"); }
function validBearer(value: string): boolean { return value.length >= 16 && value.length <= 8192 && !/[\s]/u.test(value); }
