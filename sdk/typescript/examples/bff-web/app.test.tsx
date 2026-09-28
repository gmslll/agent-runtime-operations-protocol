import type { RunRequest, RunStatus, StreamEvent } from "../../src/consumer/index.js";
import { BFFApplication, type ConsumerOperations, type ValidatedBrowserSession } from "./app.js";
import { WebRunController } from "./web.js";

const RUN = "run_01956e7b-9abc-7def-8abc-0123456789ab";
const secret = "server-only-bearer-never-browser";
const session: ValidatedBrowserSession = { tenantId: "tenant-a", principalId: "user-a", scopes: new Set(["run:create", "run:read", "run:command", "run:stream"]) };
const status = { run_id: RUN, state: "queued", state_version: 1, updated_at: "2026-09-21T08:00:00Z" } as RunStatus;
const fake: ConsumerOperations = {
  async createRun() { return status; }, async getRun() { return status; }, async submitCommand() { return status; },
  async stream(_run, handle) { await handle({ runid: RUN, runsequence: 1, type: "io.kinglucky.arop.run.succeeded.v1", data: { state: "succeeded" } } as unknown as StreamEvent); },
};
const bff = new BFFApplication(fake, { async authenticate(value) { if (value.includes(secret)) throw new Error("leaked token"); return session; } }, { authorize(value, operation) { return value.tenantId === "tenant-a" && value.scopes.has(operation as never); } });
const request = { labels: {} } as unknown as RunRequest;
const created = await bff.create("opaque-browser-session-0001", request, "create-key-0001");
if (created.runId !== RUN || JSON.stringify(created).includes(secret) || Object.keys(created).some((key) => /token|endpoint/iu.test(key))) throw new Error("BFF leaked privileged material");
const web = new WebRunController(bff, "opaque-browser-session-0001", RUN);
const view = await web.resume();
if (view.sequence !== 1 || view.terminal !== "succeeded" || JSON.stringify(view).includes(secret)) throw new Error("web recovery failed");
console.log("ok bff_authorizes_and_shields_tokens");
console.log("ok web_resumes_with_opaque_cursor");
