/**
 * @openseal/app: what an OpenSeal app imports to work with its Agent.
 *
 *   import { runAgent, getData } from "@openseal/app";
 *   const run = await runAgent({ message: "Research NVDA", values: { ticker: "NVDA" } });
 *   const result = await run.done();
 *
 * Inside the chat's canvas it talks to the host page. A downloaded app calls
 * configure() once with the OpenSeal address and its app key.
 */
import { type DataSnapshot, type Json, type RunRequest, type RunState, TERMINAL_STATUSES } from "./protocol.ts";
import { type HostWindow, type HttpOptions, OpenSealAppError, type Transport, frameTransport, httpTransport } from "./transport.ts";

export type { AppErrorCode, DataSnapshot, Json, RunRequest, RunState, RunStatus } from "./protocol.ts";
export { OpenSealAppError } from "./transport.ts";
export type { HttpOptions } from "./transport.ts";

/**
 * The size a Desk shows an app's widget at. An app may add src/Widget.tsx,
 * whose default export receives { size } and shows the app at a glance.
 */
export type WidgetSize = "small" | "medium" | "large";
export interface WidgetProps { size: WidgetSize }

let transport: Transport | undefined;
let pollMs = 2_000;

/** Use the OpenSeal API directly. Needed only outside OpenSeal. */
export function configure(options: HttpOptions): void {
  transport = httpTransport(options);
  pollMs = Math.max(500, options.pollMs ?? 2_000);
}

/** For tests and custom hosts. */
export function useTransport(next: Transport | undefined, options: { pollMs?: number } = {}): void {
  transport = next;
  pollMs = Math.max(0, options.pollMs ?? pollMs);
}

const embeddedWindow = (): HostWindow | undefined => {
  const win = (globalThis as { window?: Window }).window;
  return win && win.parent && win.parent !== win ? win as unknown as HostWindow : undefined;
};

function current(): Transport {
  if (!transport) {
    const win = embeddedWindow();
    if (win) transport = frameTransport(win);
  }
  if (!transport) throw new OpenSealAppError("not_available", "Not running inside OpenSeal. Call configure() with your OpenSeal address and app key.");
  return transport;
}

const isTerminal = (state: RunState) => TERMINAL_STATUSES.has(state.status);
/** A delay that ends early when `until` settles, so finished work leaves no timer behind. */
const sleep = (ms: number, until?: Promise<unknown>) => {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const delay = new Promise(resolve => { timer = setTimeout(resolve, ms); });
  return (until ? Promise.race([delay, until]) : delay).finally(() => clearTimeout(timer));
};

/** A run the app started. */
export interface Run {
  readonly runId: string;
  /** The latest known state. */
  readonly state: RunState;
  /** Called on every change until the run finishes. Returns an unsubscribe function. */
  onUpdate(listener: (state: RunState) => void): () => void;
  /** Resolves when the run completes, fails or is canceled. */
  done(): Promise<RunState>;
  cancel(): Promise<RunState>;
}

/**
 * Ask the Agent to do something. The request appears in the app's chat as
 * the user's turn and runs with the app's permissions.
 */
export async function runAgent(request: RunRequest): Promise<Run> {
  if (!request || typeof request.message !== "string" || !request.message.trim()) {
    throw new OpenSealAppError("invalid_request", "runAgent needs a message describing the request.");
  }
  const channel = current();
  let state = await channel.request("run.start", { message: request.message.trim(), ...(request.values ? { values: request.values } : {}) });
  const listeners = new Set<(state: RunState) => void>();
  let settle: ((state: RunState) => void) | undefined;
  const finished = new Promise<RunState>(resolve => { settle = resolve; });
  const accept = (next: RunState) => {
    if (next.runId !== state.runId || isTerminal(state)) return;
    state = next;
    for (const listener of listeners) listener(state);
    if (isTerminal(state)) { settle?.(state); stopEvents(); }
  };
  const stopEvents = channel.subscribe("run.updated", accept);
  if (isTerminal(state)) { settle?.(state); stopEvents(); } else void (async () => {
    // Events arrive from the canvas host; polling covers exported apps and any missed event.
    while (!isTerminal(state)) {
      await sleep(pollMs, finished);
      if (isTerminal(state)) break;
      try { accept(await channel.request("run.get", { runId: state.runId })); } catch { /* keep the last known state and retry */ }
    }
  })();
  return {
    get runId() { return state.runId; },
    get state() { return state; },
    onUpdate(listener) { listeners.add(listener); return () => { listeners.delete(listener); }; },
    done: () => finished,
    async cancel() { const next = await channel.request("run.cancel", { runId: state.runId }); accept(next); return state; },
  };
}

/** Read a named dataset, or null when it has not been produced yet. */
export async function getData<T = Json>(name: string): Promise<DataSnapshot<T> | null> {
  return await current().request("data.get", { name }) as DataSnapshot<T> | null;
}

/** Watch a dataset; the listener gets each newer snapshot. Returns an unsubscribe function. */
export function watchData<T = Json>(name: string, listener: (snapshot: DataSnapshot<T>) => void): () => void {
  const channel = current();
  let latest = "";
  let active = true;
  let stopped: () => void = () => undefined;
  const stopping = new Promise<void>(resolve => { stopped = resolve; });
  const deliver = (snapshot: DataSnapshot | null) => {
    if (!active || !snapshot || snapshot.name !== name || snapshot.updatedAt <= latest) return;
    latest = snapshot.updatedAt;
    listener(snapshot as DataSnapshot<T>);
  };
  const stop = channel.subscribe("data.updated", deliver);
  void (async () => {
    while (active) {
      try { deliver(await channel.request("data.get", { name })); } catch { /* try again on the next tick */ }
      await sleep(Math.max(pollMs, 5_000), stopping);
    }
  })();
  return () => { active = false; stopped(); stop(); };
}
