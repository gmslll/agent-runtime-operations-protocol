import type { RunCommand, RunRequest, RunStatus, StreamEvent, StreamOptions } from "../../src/consumer/index.js";

export interface ValidatedBrowserSession {
  readonly tenantId: string;
  readonly principalId: string;
  readonly scopes: ReadonlySet<"run:create" | "run:read" | "run:command" | "run:stream">;
}

export interface SessionAuthenticator { authenticate(sessionCookie: string, signal?: AbortSignal): Promise<ValidatedBrowserSession> }
export interface SessionAuthorizer { authorize(session: ValidatedBrowserSession, operation: string): Promise<boolean> | boolean }
export interface ConsumerOperations {
  createRun(request: RunRequest, idempotencyKey: string, signal?: AbortSignal): Promise<RunStatus>;
  getRun(runID: string, signal?: AbortSignal): Promise<RunStatus>;
  submitCommand(runID: string, command: RunCommand, idempotencyKey: string, signal?: AbortSignal): Promise<RunStatus>;
  stream(runID: string, handle: (event: StreamEvent) => void | Promise<void>, options?: StreamOptions): Promise<void>;
}

export interface BrowserRun {
  readonly runId: string;
  readonly state: RunStatus["state"];
  readonly stateVersion: number;
  readonly updatedAt: string;
}

export class BFFApplication {
  readonly #consumer: ConsumerOperations;
  readonly #sessions: SessionAuthenticator;
  readonly #authorizer: SessionAuthorizer;

  constructor(consumer: ConsumerOperations, sessions: SessionAuthenticator, authorizer: SessionAuthorizer) {
    this.#consumer = consumer;
    this.#sessions = sessions;
    this.#authorizer = authorizer;
  }

  async create(sessionCookie: string, request: RunRequest, idempotencyKey: string, signal?: AbortSignal): Promise<BrowserRun> {
    const session = await this.#authorize(sessionCookie, "run:create", signal);
    assertTenantBinding(request.labels, session.tenantId);
    return browserRun(await this.#consumer.createRun(request, idempotencyKey, signal));
  }

  async get(sessionCookie: string, runID: string, signal?: AbortSignal): Promise<BrowserRun> {
    await this.#authorize(sessionCookie, "run:read", signal);
    return browserRun(await this.#consumer.getRun(runID, signal));
  }

  async command(sessionCookie: string, runID: string, command: RunCommand, idempotencyKey: string, signal?: AbortSignal): Promise<BrowserRun> {
    await this.#authorize(sessionCookie, "run:command", signal);
    return browserRun(await this.#consumer.submitCommand(runID, command, idempotencyKey, signal));
  }

  async stream(sessionCookie: string, runID: string, after: number, handle: (event: StreamEvent) => void | Promise<void>, signal?: AbortSignal): Promise<void> {
    await this.#authorize(sessionCookie, "run:stream", signal);
    return this.#consumer.stream(runID, handle, { after, maxReconnects: 5, ...(signal === undefined ? {} : { signal }) });
  }

  async #authorize(sessionCookie: string, operation: "run:create" | "run:read" | "run:command" | "run:stream", signal: AbortSignal | undefined): Promise<ValidatedBrowserSession> {
    if (sessionCookie.length < 16 || sessionCookie.length > 4096 || /[\r\n]/u.test(sessionCookie)) throw new Error("browser session unavailable");
    const session = await this.#sessions.authenticate(sessionCookie, signal);
    if (!validIdentity(session.tenantId) || !validIdentity(session.principalId) || !session.scopes.has(operation) || !await this.#authorizer.authorize(session, operation)) throw new Error("browser operation denied");
    return session;
  }
}

function browserRun(status: RunStatus): BrowserRun {
  return Object.freeze({ runId: status.run_id, state: status.state, stateVersion: status.state_version, updatedAt: status.updated_at });
}

function assertTenantBinding(labels: object | undefined, tenantID: string): void {
  // v1 RunRequest labels are schema-closed and do not carry tenant identity.
  // The authenticated BFF session is therefore the only tenant authority.
  if (labels !== undefined && Object.keys(labels).length !== 0) throw new Error("invalid run labels");
  if (!validIdentity(tenantID)) throw new Error("browser operation denied");
}

function validIdentity(value: string): boolean { return /^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,127}$/u.test(value); }
