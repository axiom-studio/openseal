import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { getData, postInteraction, runAgent, useTransport, watchData } from "./index.ts";
import { PROTOCOL_VERSION, type RequestMessage, type RunState, interactionRequestProblem } from "./protocol.ts";
import { OpenSealAppError, type HostWindow, type Transport, frameTransport, httpTransport } from "./transport.ts";

afterEach(() => useTransport(undefined));

function fakeTransport(handler: (method: string, params: any) => unknown) {
  const listeners = new Map<string, Set<(data: any) => void>>();
  const transport: Transport = {
    request: async (method, params) => handler(method, params) as any,
    subscribe(event, listener) {
      const set = listeners.get(event) || new Set();
      set.add(listener as any);
      listeners.set(event, set);
      return () => { set.delete(listener as any); };
    },
  };
  return { transport, emit: (event: string, data: unknown) => listeners.get(event)?.forEach(listener => listener(data)) };
}

test("runAgent starts a run and resolves when an update finishes it", async () => {
  const requests: string[] = [];
  const fake = fakeTransport((method, params) => {
    requests.push(method);
    if (method === "run.start") return { runId: "run-1", status: "running" };
    return { runId: params.runId, status: "running" };
  });
  useTransport(fake.transport, { pollMs: 10_000 });
  const run = await runAgent({ message: "Research NVDA", values: { ticker: "NVDA" } });
  const seen: string[] = [];
  run.onUpdate(state => seen.push(state.status));
  fake.emit("run.updated", { runId: "other", status: "completed" });
  fake.emit("run.updated", { runId: "run-1", status: "completed", summary: "Done" });
  fake.emit("run.updated", { runId: "run-1", status: "failed" });
  const final = await run.done();
  assert.equal(final.summary, "Done");
  assert.deepEqual(seen, ["completed"]);
  assert.deepEqual(requests, ["run.start"]);
});

test("runAgent polls when no update event arrives", async () => {
  let reads = 0;
  const fake = fakeTransport(method => {
    if (method === "run.start") return { runId: "run-2", status: "queued" };
    reads += 1;
    return { runId: "run-2", status: reads >= 2 ? "completed" : "running" } satisfies RunState;
  });
  useTransport(fake.transport, { pollMs: 1 });
  const run = await runAgent({ message: "Refresh" });
  assert.equal((await run.done()).status, "completed");
  assert.equal(reads, 2);
});

test("runAgent rejects an empty message before contacting OpenSeal", async () => {
  useTransport(fakeTransport(() => { throw new Error("should not be called"); }).transport);
  await assert.rejects(runAgent({ message: "  " }), (error: unknown) => error instanceof OpenSealAppError && error.code === "invalid_request");
});

test("calls outside OpenSeal explain how to configure", async () => {
  await assert.rejects(getData("x"), (error: unknown) => error instanceof OpenSealAppError && error.code === "not_available");
});

test("watchData delivers only newer snapshots of the named dataset", async () => {
  const fake = fakeTransport(() => ({ name: "pipeline", value: [1], updatedAt: "2026-10-15T08:00:00Z" }));
  useTransport(fake.transport, { pollMs: 10_000 });
  const values: unknown[] = [];
  const stop = watchData("pipeline", snapshot => values.push(snapshot.value));
  await new Promise(resolve => setTimeout(resolve, 5));
  fake.emit("data.updated", { name: "other", value: [9], updatedAt: "2026-10-15T09:00:00Z" });
  fake.emit("data.updated", { name: "pipeline", value: [0], updatedAt: "2026-10-15T07:00:00Z" });
  fake.emit("data.updated", { name: "pipeline", value: [2], updatedAt: "2026-10-15T09:00:00Z" });
  stop();
  fake.emit("data.updated", { name: "pipeline", value: [3], updatedAt: "2026-10-15T10:00:00Z" });
  assert.deepEqual(values, [[1], [2]]);
});

function fakeWindow() {
  const handlers = new Set<(event: { source: unknown; data: unknown }) => void>();
  const posted: RequestMessage[] = [];
  const parent = { postMessage: (message: unknown) => { posted.push(message as RequestMessage); } };
  const win: HostWindow = { parent, addEventListener: (_type, handler) => handlers.add(handler), removeEventListener: (_type, handler) => handlers.delete(handler) };
  const deliver = (source: unknown, data: unknown) => handlers.forEach(handler => handler({ source, data }));
  return { win, parent, posted, deliver };
}

test("frame transport sends versioned requests and accepts replies only from the parent", async () => {
  const host = fakeWindow();
  const transport = frameTransport(host.win, 1_000);
  const pending = transport.request("data.get", { name: "pipeline" });
  const [message] = host.posted;
  assert.equal(message.kind, "openseal-app-request");
  assert.equal(message.version, PROTOCOL_VERSION);
  assert.deepEqual(message.params, { name: "pipeline" });
  host.deliver({}, { kind: "openseal-app-response", version: PROTOCOL_VERSION, id: message.id, ok: true, result: "forged" });
  host.deliver(host.parent, { kind: "openseal-app-response", version: 99, id: message.id, ok: true, result: "wrong version" });
  host.deliver(host.parent, { kind: "openseal-app-response", version: PROTOCOL_VERSION, id: message.id, ok: true, result: { name: "pipeline", value: 1, updatedAt: "t" } });
  assert.deepEqual(await pending, { name: "pipeline", value: 1, updatedAt: "t" });
});

test("frame transport turns host refusals into typed errors and times out", async () => {
  const host = fakeWindow();
  const transport = frameTransport(host.win, 20);
  const refused = transport.request("run.start", { message: "Go" });
  host.deliver(host.parent, { kind: "openseal-app-response", version: PROTOCOL_VERSION, id: host.posted[0].id, ok: false, error: { code: "not_allowed", message: "Runs are off for this published app." } });
  await assert.rejects(refused, (error: unknown) => error instanceof OpenSealAppError && error.code === "not_allowed" && /published/.test(error.message));
  await assert.rejects(transport.request("run.get", { runId: "r" }), (error: unknown) => error instanceof OpenSealAppError && error.code === "unavailable");
});

test("http transport sends the app key and maps status codes", async () => {
  const calls: Array<{ url: string; init: RequestInit }> = [];
  const responses = [
    new Response(JSON.stringify({ runId: "run-9", status: "queued" }), { status: 200 }),
    new Response("", { status: 404 }),
    new Response(JSON.stringify({ error: { message: "Daily credit limit reached." } }), { status: 429 }),
  ];
  const transport = httpTransport({ baseUrl: "https://seal.example/api/apps/v1/", appId: "app-1", appKey: "key-123",
    fetch: (async (url: string, init: RequestInit) => { calls.push({ url, init }); return responses.shift()!; }) as typeof fetch });
  assert.deepEqual(await transport.request("run.start", { message: "Go" }), { runId: "run-9", status: "queued" });
  assert.equal(calls[0].url, "https://seal.example/api/apps/v1/apps/app-1/runs");
  assert.equal((calls[0].init.headers as Record<string, string>).authorization, "Bearer key-123");
  assert.equal(await transport.request("data.get", { name: "missing" }), null);
  await assert.rejects(transport.request("run.get", { runId: "run-9" }), (error: unknown) => error instanceof OpenSealAppError && error.code === "limit_reached" && error.message === "Daily credit limit reached.");
});

test("postInteraction sends values and a trimmed summary and returns the stored result", async () => {
  const sent: unknown[] = [];
  const fake = fakeTransport((method, params) => {
    sent.push([method, params]);
    return { id: "int-1", values: params.values, summary: params.summary, createdAt: "2026-10-28T09:00:00Z" };
  });
  useTransport(fake.transport);
  const stored = await postInteraction({ score: 8, of: 10, topic: "times tables" }, { summary: "  Scored 8 out of 10 " });
  assert.deepEqual(sent, [["interaction.post", { values: { score: 8, of: 10, topic: "times tables" }, summary: "Scored 8 out of 10" }]]);
  assert.equal(stored.id, "int-1");
  await postInteraction({ done: true }, { summary: "  " });
  assert.deepEqual(sent[1], ["interaction.post", { values: { done: true } }]);
});

test("postInteraction refuses invalid results before contacting OpenSeal", async () => {
  useTransport(fakeTransport(() => { throw new Error("should not be called"); }).transport);
  const invalid = (error: unknown) => error instanceof OpenSealAppError && error.code === "invalid_request";
  await assert.rejects(postInteraction([1, 2] as never), invalid);
  await assert.rejects(postInteraction(null as never), invalid);
  await assert.rejects(postInteraction(new Date() as never), invalid);
  await assert.rejects(postInteraction({ text: "a".repeat(4 * 1024) }), invalid);
  await assert.rejects(postInteraction({ score: 1 }, { summary: "é".repeat(201) }), invalid);
  await assert.rejects(postInteraction({ score: 1 }, { summary: "line one\nline two" }), invalid);
  await assert.rejects(postInteraction({ score: 1 }, { summary: 8 as never }), invalid);
});

test("interactionRequestProblem accepts the limits exactly", () => {
  const room = 4 * 1024 - JSON.stringify({ t: "" }).length;
  assert.equal(interactionRequestProblem({ values: { t: "a".repeat(room) } }), undefined);
  assert.notEqual(interactionRequestProblem({ values: { t: "a".repeat(room + 1) } }), undefined);
  assert.equal(interactionRequestProblem({ values: {}, summary: "😀".repeat(200) }), undefined);
  assert.notEqual(interactionRequestProblem({ values: {} , summary: "😀".repeat(201) }), undefined);
});

test("only an app in its chat can report results", async () => {
  const transport = httpTransport({ baseUrl: "https://seal.example/api/apps/v1", appId: "app-1", appKey: "key",
    fetch: (async () => { throw new Error("should not be called"); }) as typeof fetch });
  await assert.rejects(transport.request("interaction.post", { values: { score: 1 } }), (error: unknown) => error instanceof OpenSealAppError && error.code === "not_allowed");
});
