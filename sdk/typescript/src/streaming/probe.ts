import { RelayStreamingClient, CursorExpiredError, appendUTF8, parseSSE, type HeaderReader, type StreamResponse, type StreamTransport } from "./client.js";

const runID = "run_01932f13-0cd2-7a82-8fa3-1cb5ce13ef10";
const attemptID = "att_01932f13-0cd2-7a82-8fa3-1cb5ce13ef12";
const event = (sequence: number, terminal = false): string => JSON.stringify({ specversion: "1.0", id: "evt_01932f13-0cd2-7a82-8fa3-1cb5ce13ef11", source: "https://runtime.example.invalid/instances/runtime-a", type: terminal ? "io.kinglucky.arop.run.succeeded.v1" : "io.kinglucky.arop.output.delta.v1", subject: `runs/${runID}`, time: "2026-09-27T08:00:01Z", datacontenttype: "application/json", dataschema: "https://arop.invalid/schemas/v1/events/lifecycle-events-v1.schema.json", runid: runID, attemptid: attemptID, producersequence: sequence, runsequence: sequence, traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", data: terminal ? { state: "succeeded" } : { output_id: "answer", offset: 0, delta: "商品" } });
const body = (wire: string): AsyncIterable<Uint8Array> => ({ async *[Symbol.asyncIterator]() { const bytes = new TextEncoder().encode(wire); yield bytes.slice(0, 7); yield bytes.slice(7); } });
const headers = (values: Record<string, string>): HeaderReader => ({ get: (name) => values[name.toLowerCase()] ?? null });
let calls = 0;
const transport: StreamTransport = { async open(request): Promise<StreamResponse> { calls += 1; if (calls === 2 && request.headers["Last-Event-ID"] !== "1") throw new Error("missing reconnect cursor"); const sequence = calls; const value = event(sequence, sequence === 2); return { status: 200, headers: headers({ "content-type": "text/event-stream", "cache-control": "no-store" }), body: body(`id: ${sequence}\nevent: ${JSON.parse(value).type as string}\ndata: ${value}\n\n`) }; } };
const received: number[] = [];
await new RelayStreamingClient("https://control.example.invalid", { async token() { return "control-token-000000"; } }, transport).stream(runID, (value) => { received.push(value.runsequence!); }, { maxReconnects: 1 });
if (received.join(",") !== "1,2") throw new Error("reconnect failed");
let text = appendUTF8(new Uint8Array(), 0, "商品");
text = appendUTF8(text, 6, "🙂e\u0301");
if (text.byteLength !== 13) throw new Error("UTF-8 offset failed");
let rejected = 0;
for (const wire of ["id: 1\nid: 1\nevent: x\ndata: {}\n\n", "id: 2\nevent: x\ndata: {}"] ) { try { for await (const _ of parseSSE(body(wire))) void _; } catch { rejected += 1; } }
if (rejected !== 2 || !(new CursorExpiredError(2, `/v1/agent-runs/${runID}`) instanceof Error)) throw new Error("negative probe failed");
console.log(JSON.stringify({ schema_version: 1, cases: 5, passed: true }));
