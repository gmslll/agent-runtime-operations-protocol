import { ConsumerClient, ControlPlaneError } from "./client.js";
import { initialRunView, reduceRunEvent } from "./reducer.js";
import type { HTTPRequest, HTTPResponse, HTTPTransport } from "./client.js";
import type { RunRequest, RunStatus } from "../generated/run/run.gen.js";
import type { StreamEvent } from "../generated/streaming/streaming.gen.js";
import type { HeaderReader, StreamResponse, StreamTransport, TokenSource } from "../streaming/client.js";

const RUN = "run_01956e7b-9abc-7def-8abc-0123456789ab";
const ATTEMPT = "att_01956e7b-9abc-7def-8abc-0123456789ab";
const EVENT = "evt_01956e7b-9abc-7def-8abc-0123456789ab";
const TRACE = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01";

class Headers implements HeaderReader {
  readonly #values: Readonly<Record<string, string>>;
  constructor(values: Readonly<Record<string, string>>) { this.#values = values; }
  get(name: string): string | null { return this.#values[name.toLowerCase()] ?? null; }
}

class Tokens implements TokenSource {
  calls = 0;
  async token(): Promise<string> { this.calls += 1; return `server-token-${String(this.calls).padStart(4, "0")}`; }
}

class HTTP implements HTTPTransport {
  readonly requests: HTTPRequest[] = [];
  responses: HTTPResponse[] = [];
  async request(request: HTTPRequest): Promise<HTTPResponse> {
    this.requests.push(request);
    const response = this.responses.shift();
    if (response === undefined) throw new Error("missing response");
    return response;
  }
}

class Streams implements StreamTransport {
  readonly requests: Readonly<Record<string, string>>[] = [];
  responses: StreamResponse[] = [];
  async open(request: { readonly headers: Readonly<Record<string, string>> }): Promise<StreamResponse> {
    this.requests.push(request.headers);
    const response = this.responses.shift();
    if (response === undefined) throw new Error("missing stream response");
    return response;
  }
}

const tests: Array<readonly [string, () => void | Promise<void>]> = [];
function test(name: string, run: () => void | Promise<void>): void { tests.push([name, run]); }
function assert(value: unknown, message = "assertion failed"): asserts value { if (!value) throw new Error(message); }
function throws(run: () => unknown, pattern: RegExp): void { try { run(); } catch (error) { assert(error instanceof Error && pattern.test(error.message)); return; } throw new Error("expected failure"); }

test("reducer_utf8_replay_and_unknown_forward_event", () => {
  let view = initialRunView(RUN);
  const first = event(1, "io.kinglucky.arop.output.delta.v1", { output_id: "answer", offset: 0, delta: "商品" });
  view = reduceRunEvent(view, first);
  assert(view.outputs.answer?.bytes === 6);
  assert(reduceRunEvent(view, first) === view);
  view = reduceRunEvent(view, event(2, "io.kinglucky.arop.progress.future.v1", { future: true }));
  view = reduceRunEvent(view, event(3, "io.kinglucky.arop.output.delta.v1", { output_id: "answer", offset: 6, delta: "😀" }));
  assert(view.outputs.answer?.text === "商品😀" && view.outputs.answer.bytes === 10);
});

test("reducer_rejects_gap_conflict_and_post_terminal", () => {
  const initial = initialRunView(RUN);
  throws(() => reduceRunEvent(initial, event(2, "io.kinglucky.arop.progress.update.v1", {})), /gap/u);
  const one = reduceRunEvent(initial, event(1, "io.kinglucky.arop.progress.update.v1", {}));
  throws(() => reduceRunEvent(one, event(1, "io.kinglucky.arop.progress.update.v1", { changed: true })), /conflicting/u);
  const terminal = reduceRunEvent(one, event(2, "io.kinglucky.arop.run.succeeded.v1", { state: "succeeded" }));
  throws(() => reduceRunEvent(terminal, event(3, "io.kinglucky.arop.progress.update.v1", {})), /terminal/u);
});

test("consumer_create_get_command_shields_server_token", async () => {
  const tokens = new Tokens(); const http = new HTTP(); const streams = new Streams();
  http.responses.push(jsonResponse(201, statusJSON(), { location: `/v1/agent-runs/${RUN}` }), jsonResponse(200, statusJSON()), jsonResponse(200, statusJSON()));
  const client = new ConsumerClient("https://control.example.invalid", tokens, http, streams);
  const request = JSON.parse(runRequestJSON()) as RunRequest;
  const created = await client.createRun(request, "create-key-0001");
  const found = await client.getRun(RUN);
  const commanded = await client.submitCommand(RUN, { schema_version: 1, command_id: "command-key-0001", type: "run.cancel", expected_state_version: 1, data: {} }, "command-key-0001");
  assert(tokens.calls === 3 && http.requests.length === 3);
  assert(http.requests.every((item) => item.headers.Authorization?.startsWith("Bearer server-token-") === true));
  assert(!JSON.stringify([created, found, commanded]).includes("server-token"));
});

test("consumer_typed_retry_error_is_redacted", async () => {
  const tokens = new Tokens(); const http = new HTTP(); const streams = new Streams();
  http.responses.push(jsonResponse(503, JSON.stringify({ category: "dependency", code: "DEPENDENCY_UNAVAILABLE", message: "unavailable", retryable: true, retry_after_seconds: 7 }), { "retry-after": "7" }));
  const client = new ConsumerClient("https://control.example.invalid", tokens, http, streams);
  try { await client.getRun(RUN); } catch (error) {
    assert(error instanceof ControlPlaneError && error.status === 503 && error.retryAfterSeconds === 7);
    assert(!error.message.includes("unavailable")); return;
  }
  throw new Error("expected remote error");
});

test("relay_reconnect_reauthenticates_and_resumes", async () => {
  const tokens = new Tokens(); const http = new HTTP(); const streams = new Streams();
  const one = event(1, "io.kinglucky.arop.progress.update.v1", { percent: 10 });
  const two = event(2, "io.kinglucky.arop.run.succeeded.v1", { state: "succeeded" });
  streams.responses.push(streamResponse(sse(one)), streamResponse(sse(one) + sse(two)));
  const client = new ConsumerClient("https://control.example.invalid", tokens, http, streams);
  const seen: number[] = [];
  await client.stream(RUN, (item) => { seen.push(item.runsequence ?? 0); }, { maxReconnects: 1 });
  assert(JSON.stringify(seen) === "[1,2]" && tokens.calls === 2);
  assert(streams.requests[1]?.["Last-Event-ID"] === "1");
  assert(streams.requests[0]?.Authorization !== streams.requests[1]?.Authorization);
});

for (const [name, run] of tests) {
  await run();
  console.log(`ok ${name}`);
}

function event(sequence: number, type: string, data: Readonly<Record<string, import("../generated/streaming/streaming.gen.js").JsonValue>>): StreamEvent {
  return { specversion: "1.0", id: EVENT, source: "https://runtime.example.invalid/instances/a" as StreamEvent["source"], type, subject: `runs/${RUN}`, time: "2026-09-21T08:00:01Z" as StreamEvent["time"], datacontenttype: "application/json", dataschema: "https://arop.invalid/schemas/v1/events/output-events-v1.schema.json" as StreamEvent["dataschema"], runid: RUN, attemptid: ATTEMPT, producersequence: sequence, runsequence: sequence, traceparent: TRACE, data };
}

function statusJSON(): string { return JSON.stringify({ schema_version: 1, run_id: RUN, agent: { id: "demo", version: "1.0.0", skill_id: "chat", manifest_digest: `sha256:${"0".repeat(64)}` }, state: "queued", state_version: 1, created_at: "2026-09-21T08:00:00Z", updated_at: "2026-09-21T08:00:00Z", deadline_at: "2026-09-21T08:05:00Z", authorization_snapshot_digest: `sha256:${"1".repeat(64)}`, trace: { traceparent: TRACE } }); }
function runRequestJSON(): string { return JSON.stringify({ schema_version: 1, agent: { id: "demo", version: "1.0.0", skill_id: "chat", manifest_digest: `sha256:${"0".repeat(64)}` }, input: [{ type: "text", text: "hello" }], deadline_at: "2026-09-21T08:05:00Z", effects: { level: "none" }, trace: { traceparent: TRACE } }); }
function jsonResponse(status: number, body: string, extra: Readonly<Record<string, string>> = {}): HTTPResponse { return { status, headers: new Headers({ "content-type": "application/json", ...extra }), body: new TextEncoder().encode(body) }; }
function streamResponse(wire: string): StreamResponse { return { status: 200, headers: new Headers({ "content-type": "text/event-stream", "cache-control": "no-store" }), body: chunks(wire) }; }
function sse(item: StreamEvent): string { return `id: ${item.runsequence}\nevent: ${item.type}\ndata: ${JSON.stringify(item)}\n\n`; }
async function* chunks(value: string): AsyncGenerator<Uint8Array> { yield new TextEncoder().encode(value); }
