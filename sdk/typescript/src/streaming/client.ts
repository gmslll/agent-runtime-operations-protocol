import { decodeStreamEvent, type StreamEvent } from "../generated/streaming/streaming.gen.js";

const RUN_ID = /^run_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/u;
const MAX_SAFE = 9_007_199_254_740_991;
const MAX_LINE = 1 << 20;
const MAX_FRAME = 8 << 20;

export interface HeaderReader { get(name: string): string | null }
export interface StreamResponse { readonly status: number; readonly headers: HeaderReader; readonly body: AsyncIterable<Uint8Array> }
export interface StreamTransport { open(request: { readonly url: string; readonly headers: Readonly<Record<string, string>>; readonly signal?: AbortSignal }): Promise<StreamResponse> }
export interface TokenSource { token(signal?: AbortSignal): Promise<string> }
export interface StreamOptions { readonly after?: number; readonly maxReconnects?: number; readonly signal?: AbortSignal }

export class CursorExpiredError extends Error {
  constructor(readonly latestSequence: number, readonly snapshotURL: string) { super("stream cursor expired"); }
}

export class RelayStreamingClient {
  readonly #base: string;
  readonly #tokens: TokenSource;
  readonly #transport: StreamTransport;

  constructor(controlPlaneBase: string, tokens: TokenSource, transport: StreamTransport) {
    const parsed = new URL(controlPlaneBase);
    if (parsed.protocol !== "https:" || parsed.username !== "" || parsed.password !== "" || (parsed.pathname !== "" && parsed.pathname !== "/") || parsed.search !== "" || parsed.hash !== "") throw new Error("invalid control plane URL");
    this.#base = parsed.origin;
    this.#tokens = tokens;
    this.#transport = transport;
  }

  async stream(runID: string, handle: (event: StreamEvent) => void | Promise<void>, options: StreamOptions = {}): Promise<void> {
    if (!RUN_ID.test(runID) || typeof handle !== "function") throw new Error("invalid stream request");
    let after = options.after ?? 0;
    const reconnects = options.maxReconnects ?? 3;
    if (!Number.isSafeInteger(after) || after < 0 || after > MAX_SAFE || !Number.isSafeInteger(reconnects) || reconnects < 0 || reconnects > 100) throw new Error("invalid stream options");
    let last = "";
    for (let reconnect = 0; ; reconnect += 1) {
      const token = await this.#tokens.token(options.signal);
      if (!validBearer(token)) throw new Error("control plane authentication unavailable");
      const headers: Record<string, string> = { Accept: "text/event-stream", Authorization: `Bearer ${token}` };
      if (after !== 0) headers["Last-Event-ID"] = String(after);
      const transportRequest = options.signal === undefined ? { url: `${this.#base}/v1/agent-runs/${runID}/events`, headers } : { url: `${this.#base}/v1/agent-runs/${runID}/events`, headers, signal: options.signal };
      const response = await this.#transport.open(transportRequest);
      if (response.status === 410) throw await cursorExpired(response.body);
      if (response.status !== 200 || exactHeader(response.headers, "content-type") !== "text/event-stream" || exactHeader(response.headers, "cache-control") !== "no-store") throw new Error("stream request rejected");
      let terminal = false;
      for await (const frame of parseSSE(response.body)) {
        if (frame.heartbeat) continue;
        const sequence = strictSequence(frame.id);
        if (sequence <= after) {
          if (sequence === after && frame.data === last) continue;
          throw new Error("conflicting stream replay");
        }
        if (sequence !== after + 1) throw new Error("stream sequence gap");
        const event = decodeStreamEvent(frame.data);
        if (event.runid !== runID || event.runsequence !== sequence || event.type !== frame.event) throw new Error("invalid relay sequence domain");
        await handle(event);
        after = sequence;
        last = frame.data;
        if (terminalEvent(event.type)) { terminal = true; break; }
      }
      if (terminal) return;
      if (reconnect >= reconnects) throw new Error("stream ended before terminal event");
      if (options.signal?.aborted === true) throw new Error("stream aborted");
    }
  }
}

export interface SSEFrame { readonly heartbeat: boolean; readonly id: string; readonly event: string; readonly data: string }

export async function* parseSSE(chunks: AsyncIterable<Uint8Array>): AsyncGenerator<SSEFrame> {
  const decoder = new TextDecoder("utf-8", { fatal: true });
  let buffer = "";
  for await (const chunk of chunks) {
    if (!(chunk instanceof Uint8Array)) throw new Error("invalid stream chunk");
    buffer += decoder.decode(chunk, { stream: true });
    if (new TextEncoder().encode(buffer).byteLength > MAX_FRAME) throw new Error("SSE frame exceeds limit");
    let boundary = buffer.indexOf("\n\n");
    while (boundary >= 0) {
      const wire = buffer.slice(0, boundary);
      buffer = buffer.slice(boundary + 2);
      yield parseFrame(wire);
      boundary = buffer.indexOf("\n\n");
    }
  }
  buffer += decoder.decode();
  if (buffer !== "") throw new Error("unterminated SSE frame");
}

function parseFrame(wire: string): SSEFrame {
  const lines = wire.split("\n").map((line) => line.endsWith("\r") ? line.slice(0, -1) : line);
  if (lines.some((line) => new TextEncoder().encode(line).byteLength > MAX_LINE)) throw new Error("SSE line exceeds limit");
  if (lines.length > 0 && lines.every((line) => line.startsWith(":"))) return { heartbeat: true, id: "", event: "", data: "" };
  const values = new Map<string, string>();
  for (const line of lines) {
    const split = line.indexOf(": ");
    if (split <= 0) throw new Error("invalid SSE field");
    const name = line.slice(0, split);
    if (!(["id", "event", "data"] as const).includes(name as "id") || values.has(name)) throw new Error("invalid SSE field");
    values.set(name, line.slice(split + 2));
  }
  if (values.size !== 3 || values.get("id") === "" || values.get("event") === "" || values.get("data") === "") throw new Error("incomplete SSE frame");
  return { heartbeat: false, id: values.get("id")!, event: values.get("event")!, data: values.get("data")! };
}

async function cursorExpired(body: AsyncIterable<Uint8Array>): Promise<CursorExpiredError> {
  let bytes = 0;
  let text = "";
  const decoder = new TextDecoder("utf-8", { fatal: true });
  for await (const chunk of body) {
    bytes += chunk.byteLength;
    if (bytes > MAX_FRAME) throw new Error("cursor response exceeds limit");
    text += decoder.decode(chunk, { stream: true });
  }
  text += decoder.decode();
  const value: unknown = JSON.parse(text);
  if (value === null || typeof value !== "object" || Array.isArray(value)) throw new Error("invalid cursor response");
  const record = value as Record<string, unknown>;
  const allowed = new Set(["category", "code", "message", "retryable", "latest_sequence", "first_available_sequence", "snapshot_url"]);
  if (Object.keys(record).some((key) => !allowed.has(key)) || record.code !== "STREAM_CURSOR_EXPIRED" || !Number.isSafeInteger(record.latest_sequence) || (record.latest_sequence as number) < 0 || typeof record.snapshot_url !== "string" || !/^\/v1\/[A-Za-z0-9._~!$&'()*+,;=:@/-]+$/u.test(record.snapshot_url)) throw new Error("invalid cursor response");
  return new CursorExpiredError(record.latest_sequence as number, record.snapshot_url);
}

function exactHeader(headers: HeaderReader, name: string): string | null {
  const value = headers.get(name);
  return value === null || value.includes(",") || value.trim() !== value ? null : value.toLowerCase();
}

function strictSequence(value: string): number {
  if (!/^(?:0|[1-9][0-9]{0,15})$/u.test(value)) throw new Error("invalid stream sequence");
  const sequence = Number(value);
  if (!Number.isSafeInteger(sequence) || sequence < 1 || sequence > MAX_SAFE) throw new Error("invalid stream sequence");
  return sequence;
}

function validBearer(value: string): boolean { return value.length >= 16 && value.length <= 8192 && !/[\s]/u.test(value); }
function terminalEvent(type: string): boolean { return new Set(["io.arop.run.succeeded.v1", "io.arop.run.failed.v1", "io.arop.run.cancelled.v1", "io.arop.run.timed_out.v1"]).has(type); }

export function appendUTF8(current: Uint8Array, offset: number, delta: string): Uint8Array {
  if (!Number.isSafeInteger(offset) || offset !== current.byteLength) throw new Error("invalid UTF-8 byte offset");
  const encoded = new TextEncoder().encode(delta);
  const result = new Uint8Array(current.byteLength + encoded.byteLength);
  result.set(current);
  result.set(encoded, current.byteLength);
  return result;
}
