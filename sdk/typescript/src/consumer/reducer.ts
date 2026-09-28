import type { JsonValue, StreamEvent } from "../generated/streaming/streaming.gen.js";
import { appendUTF8 } from "../streaming/client.js";

const TERMINAL = new Map<string, RunTerminalState>([
  ["io.kinglucky.arop.run.succeeded.v1", "succeeded"],
  ["io.kinglucky.arop.run.failed.v1", "failed"],
  ["io.kinglucky.arop.run.cancelled.v1", "cancelled"],
  ["io.kinglucky.arop.run.timed_out.v1", "timed_out"],
]);

export type RunTerminalState = "succeeded" | "failed" | "cancelled" | "timed_out";

export interface OutputView {
  readonly text: string;
  readonly bytes: number;
  readonly state?: "started" | "reset" | "completed";
  readonly revision?: number;
  readonly snapshot?: ReadonlyArray<JsonValue>;
  readonly digest?: string;
}

export interface RunView {
  readonly runId: string;
  readonly sequence: number;
  readonly outputs: Readonly<Record<string, OutputView>>;
  readonly terminal?: RunTerminalState;
  readonly terminalData?: Readonly<Record<string, JsonValue>>;
  readonly lastEventFingerprint?: string;
}

export function initialRunView(runId: string): RunView {
  if (!/^run_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/u.test(runId)) throw new Error("invalid run id");
  return { runId, sequence: 0, outputs: Object.freeze({}) };
}

export function reduceRunEvent(view: RunView, event: StreamEvent): RunView {
  if (event.runid !== view.runId || event.runsequence === undefined) throw new Error("event does not belong to relay run");
  const fingerprint = stableJSON(event);
  if (event.runsequence === view.sequence) {
    if (view.lastEventFingerprint === fingerprint) return view;
    throw new Error("conflicting event replay");
  }
  if (event.runsequence !== view.sequence + 1) throw new Error("event sequence gap");
  if (view.terminal !== undefined) throw new Error("event arrived after terminal state");

  let outputs = view.outputs;
  if (event.type === "io.kinglucky.arop.output.delta.v1") outputs = reduceDelta(outputs, event.data);
  else if (event.type === "io.kinglucky.arop.output.snapshot.v1") outputs = reduceSnapshot(outputs, event.data);
  else if (event.type === "io.kinglucky.arop.output.state.v1") outputs = reduceOutputState(outputs, event.data);

  const terminal = TERMINAL.get(event.type);
  const next: RunView = {
    runId: view.runId,
    sequence: event.runsequence,
    outputs,
    lastEventFingerprint: fingerprint,
    ...(terminal === undefined ? {} : { terminal, terminalData: event.data }),
  };
  return Object.freeze(next);
}

function reduceDelta(outputs: RunView["outputs"], data: Readonly<Record<string, JsonValue>>): RunView["outputs"] {
  const outputID = requiredOutputID(data.output_id);
  const offset = requiredSafeInteger(data.offset, 0);
  if (typeof data.delta !== "string") throw new Error("invalid output delta");
  const current = outputs[outputID] ?? { text: "", bytes: 0 };
  if (current.snapshot !== undefined) throw new Error("delta cannot append to structured snapshot");
  const encoded = appendUTF8(new TextEncoder().encode(current.text), offset, data.delta);
  const updated: OutputView = Object.freeze({ ...current, text: new TextDecoder("utf-8", { fatal: true }).decode(encoded), bytes: encoded.byteLength });
  return Object.freeze({ ...outputs, [outputID]: updated });
}

function reduceSnapshot(outputs: RunView["outputs"], data: Readonly<Record<string, JsonValue>>): RunView["outputs"] {
  const outputID = requiredOutputID(data.output_id);
  const revision = requiredSafeInteger(data.revision, 1);
  if (!Array.isArray(data.content) || data.content.length === 0 || typeof data.digest !== "string" || !/^sha256:[0-9a-f]{64}$/u.test(data.digest)) throw new Error("invalid output snapshot");
  const current = outputs[outputID];
  if (current?.revision !== undefined && revision <= current.revision) throw new Error("non-monotonic snapshot revision");
  const updated: OutputView = Object.freeze({ text: "", bytes: 0, revision, snapshot: Object.freeze([...data.content]), digest: data.digest });
  return Object.freeze({ ...outputs, [outputID]: updated });
}

function reduceOutputState(outputs: RunView["outputs"], data: Readonly<Record<string, JsonValue>>): RunView["outputs"] {
  const outputID = requiredOutputID(data.output_id);
  if (data.state !== "started" && data.state !== "reset" && data.state !== "completed") throw new Error("invalid output state");
  const current = outputs[outputID] ?? { text: "", bytes: 0 };
  const updated: OutputView = data.state === "reset" ? Object.freeze({ text: "", bytes: 0, state: data.state }) : Object.freeze({ ...current, state: data.state });
  return Object.freeze({ ...outputs, [outputID]: updated });
}

function requiredOutputID(value: JsonValue | undefined): string {
  if (typeof value !== "string" || !/^[A-Za-z][A-Za-z0-9._-]{0,127}$/u.test(value)) throw new Error("invalid output id");
  return value;
}

function requiredSafeInteger(value: JsonValue | undefined, minimum: number): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < minimum) throw new Error("invalid safe integer");
  return value;
}

function stableJSON(value: unknown): string {
  if (value === null || typeof value !== "object") return JSON.stringify(value);
  if (Array.isArray(value)) return `[${value.map(stableJSON).join(",")}]`;
  const record = value as Record<string, unknown>;
  return `{${Object.keys(record).sort().map((key) => `${JSON.stringify(key)}:${stableJSON(record[key])}`).join(",")}}`;
}
