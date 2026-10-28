import {
  type AppErrorCode, type AppErrorShape, type EventName, type Events, type Method, type Methods, PROTOCOL_VERSION,
  type RequestMessage, isEventMessage, isResponseMessage,
} from "./protocol.ts";

export class OpenSealAppError extends Error implements AppErrorShape {
  readonly code: AppErrorCode;
  constructor(code: AppErrorCode, message: string) {
    super(message);
    this.name = "OpenSealAppError";
    this.code = code;
  }
}

type Listener<E extends EventName> = (data: Events[E]) => void;

/** How requests reach OpenSeal. Apps never use this directly. */
export interface Transport {
  request<M extends Method>(method: M, params: Methods[M]["params"]): Promise<Methods[M]["result"]>;
  subscribe<E extends EventName>(event: E, listener: Listener<E>): () => void;
}

/** The window an app talks to inside the canvas. Narrowed for tests. */
export interface HostWindow {
  parent: { postMessage(message: unknown, targetOrigin: string): void };
  addEventListener(type: "message", listener: (event: { source: unknown; data: unknown }) => void): void;
  removeEventListener(type: "message", listener: (event: { source: unknown; data: unknown }) => void): void;
}

let sequence = 0;
const nextId = () => `${Date.now().toString(36)}-${(sequence++).toString(36)}-${Math.random().toString(36).slice(2, 8)}`;

/**
 * Inside the canvas. The frame is sandboxed with an opaque origin, so the
 * host cannot be addressed by origin; replies are accepted only from the
 * parent window, and the host validates every request against the app's
 * permissions before acting.
 */
export function frameTransport(win: HostWindow, timeoutMs = 30_000): Transport {
  const pending = new Map<string, { resolve: (value: unknown) => void; reject: (error: Error) => void; timer: ReturnType<typeof setTimeout> }>();
  const listeners = new Map<EventName, Set<Listener<EventName>>>();
  win.addEventListener("message", event => {
    if (event.source !== win.parent) return;
    if (isResponseMessage(event.data)) {
      const waiting = pending.get(event.data.id);
      if (!waiting) return;
      pending.delete(event.data.id);
      clearTimeout(waiting.timer);
      if (event.data.ok) waiting.resolve(event.data.result ?? null);
      else waiting.reject(new OpenSealAppError(event.data.error?.code || "unavailable", event.data.error?.message || "The request failed."));
    } else if (isEventMessage(event.data)) {
      for (const listener of listeners.get(event.data.event) || []) listener(event.data.data);
    }
  });
  return {
    request(method, params) {
      const id = nextId();
      const message: RequestMessage = { kind: "openseal-app-request", version: PROTOCOL_VERSION, id, method, params };
      return new Promise((resolve, reject) => {
        const timer = setTimeout(() => {
          pending.delete(id);
          reject(new OpenSealAppError("unavailable", "OpenSeal did not answer in time."));
        }, timeoutMs);
        pending.set(id, { resolve: resolve as (value: unknown) => void, reject, timer });
        win.parent.postMessage(message, "*");
      });
    },
    subscribe(event, listener) {
      const set = listeners.get(event) || new Set();
      set.add(listener as Listener<EventName>);
      listeners.set(event, set);
      return () => { set.delete(listener as Listener<EventName>); };
    },
  };
}

export interface HttpOptions {
  /** OpenSeal API base, e.g. https://helloseal.ai/api/apps/v1 */
  baseUrl: string;
  /** The app's id, as shown in its download. */
  appId: string;
  /** A revocable key that can run only this app's Agent and read its data. */
  appKey: string;
  fetch?: typeof fetch;
  /** How often to check a run or dataset for changes while subscribed. */
  pollMs?: number;
}

const httpErrors: Record<number, AppErrorCode> = { 400: "invalid_request", 401: "not_allowed", 403: "not_allowed", 404: "not_found", 429: "limit_reached" };

/**
 * Outside OpenSeal (a downloaded app). Requests go to the OpenSeal API with
 * the app key; updates are read by polling, so an exported app needs nothing
 * beyond HTTPS.
 */
export function httpTransport(options: HttpOptions): Transport {
  const call = options.fetch ?? globalThis.fetch?.bind(globalThis);
  const base = `${options.baseUrl.replace(/\/+$/, "")}/apps/${encodeURIComponent(options.appId)}`;
  const send = async (method: string, path: string, body?: unknown) => {
    if (!call) throw new OpenSealAppError("not_available", "This environment cannot reach OpenSeal.");
    let response: Response;
    try {
      response = await call(`${base}${path}`, {
        method, headers: { authorization: `Bearer ${options.appKey}`, ...(body ? { "content-type": "application/json" } : {}) },
        body: body ? JSON.stringify(body) : undefined,
      });
    } catch {
      throw new OpenSealAppError("not_available", "OpenSeal could not be reached.");
    }
    if (response.status === 404 && path.startsWith("/data/")) return null;
    if (!response.ok) {
      let message = "The request failed.";
      try { message = (await response.json())?.error?.message || message; } catch { /* keep the generic message */ }
      throw new OpenSealAppError(httpErrors[response.status] || "unavailable", message);
    }
    return response.json();
  };
  const transport: Transport = {
    request(method, params) {
      const p = params as Record<string, string>;
      switch (method) {
        case "run.start": return send("POST", "/runs", params);
        case "run.get": return send("GET", `/runs/${encodeURIComponent(p.runId)}`);
        case "run.cancel": return send("POST", `/runs/${encodeURIComponent(p.runId)}/cancel`);
        case "data.get": return send("GET", `/data/${encodeURIComponent(p.name)}`);
        case "interaction.post": return Promise.reject(new OpenSealAppError("not_allowed", "Only an app in its chat can report results to its Agent."));
        default: return Promise.reject(new OpenSealAppError("invalid_request", `Unknown request ${String(method)}.`));
      }
    },
    subscribe() {
      // Exported apps observe changes by polling through the public API helpers.
      return () => undefined;
    },
  };
  return transport;
}
