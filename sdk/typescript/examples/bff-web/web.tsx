import { initialRunView, reduceRunEvent, type RunView } from "../../src/consumer/index.js";
import type { BFFApplication } from "./app.js";

export class WebRunController {
  #view: RunView;
  readonly #bff: BFFApplication;
  readonly #sessionCookie: string;

  constructor(bff: BFFApplication, sessionCookie: string, runID: string) {
    this.#bff = bff;
    this.#sessionCookie = sessionCookie;
    this.#view = initialRunView(runID);
  }

  get view(): RunView { return this.#view; }

  async resume(signal?: AbortSignal): Promise<RunView> {
    await this.#bff.stream(this.#sessionCookie, this.#view.runId, this.#view.sequence, (event) => {
      this.#view = reduceRunEvent(this.#view, event);
    }, signal);
    return this.#view;
  }
}
