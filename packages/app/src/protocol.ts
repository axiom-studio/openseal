/**
 * The contract between an app and the OpenSeal host. Inside the chat's
 * canvas the app runs in a sandboxed frame and these messages travel over
 * postMessage to the parent page; an exported app sends the same requests to
 * the OpenSeal API over HTTPS. The version lets either side refuse a message
 * it does not understand rather than guess.
 */
export const PROTOCOL_VERSION = 1;

export type Json = null | boolean | number | string | Json[] | { [key: string]: Json };

/** What the app asks the Agent to do. Values are the app's typed inputs. */
export interface RunRequest {
  /** A short, plain sentence describing the request, shown in the chat. */
  message: string;
  /** Typed inputs, such as form values. */
  values?: Record<string, Json>;
}

export type RunStatus = "queued" | "running" | "waiting" | "completed" | "failed" | "canceled";

export interface RunState {
  runId: string;
  status: RunStatus;
  /** The Agent's latest plain-language progress note, when there is one. */
  progress?: string;
  /** The final answer, once completed. */
  summary?: string;
  /** Structured output the Agent chose to return, once completed. */
  output?: Json;
  /** Why the run failed or was stopped, in plain words. */
  failure?: string;
}

/** A named dataset the app reads, kept fresh by a routine or by runs. */
export interface DataSnapshot<T = Json> {
  name: string;
  value: T;
  /** When the data was last refreshed (ISO 8601). */
  updatedAt: string;
}

export interface Methods {
  "run.start": { params: RunRequest; result: RunState };
  "run.get": { params: { runId: string }; result: RunState };
  "run.cancel": { params: { runId: string }; result: RunState };
  "data.get": { params: { name: string }; result: DataSnapshot | null };
}
export type Method = keyof Methods;

export interface Events {
  "run.updated": RunState;
  "data.updated": DataSnapshot;
}
export type EventName = keyof Events;

export interface RequestMessage<M extends Method = Method> {
  kind: "openseal-app-request";
  version: typeof PROTOCOL_VERSION;
  id: string;
  method: M;
  params: Methods[M]["params"];
}

export interface ResponseMessage<M extends Method = Method> {
  kind: "openseal-app-response";
  version: typeof PROTOCOL_VERSION;
  id: string;
  ok: boolean;
  result?: Methods[M]["result"];
  error?: AppErrorShape;
}

export interface EventMessage<E extends EventName = EventName> {
  kind: "openseal-app-event";
  version: typeof PROTOCOL_VERSION;
  event: E;
  data: Events[E];
}

/** Reasons a request can fail, so apps can react without parsing prose. */
export type AppErrorCode =
  | "not_available" // no OpenSeal host or API reachable
  | "not_allowed" // this app may not do that (e.g. runs are off when published)
  | "limit_reached" // a rate or credit limit stopped the request
  | "not_found"
  | "invalid_request"
  | "unavailable"; // a temporary failure; retrying later may succeed

export interface AppErrorShape {
  code: AppErrorCode;
  message: string;
}

const object = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);

export function isResponseMessage(value: unknown): value is ResponseMessage {
  return object(value) && value.kind === "openseal-app-response" && value.version === PROTOCOL_VERSION &&
    typeof value.id === "string" && typeof value.ok === "boolean";
}

export function isEventMessage(value: unknown): value is EventMessage {
  return object(value) && value.kind === "openseal-app-event" && value.version === PROTOCOL_VERSION &&
    (value.event === "run.updated" || value.event === "data.updated") && object(value.data);
}

export function isRequestMessage(value: unknown): value is RequestMessage {
  return object(value) && value.kind === "openseal-app-request" && value.version === PROTOCOL_VERSION &&
    typeof value.id === "string" && typeof value.method === "string" && object(value.params);
}

export const TERMINAL_STATUSES: ReadonlySet<RunStatus> = new Set(["completed", "failed", "canceled"]);
