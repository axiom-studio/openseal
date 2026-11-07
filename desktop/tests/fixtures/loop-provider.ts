import {
  createServer,
  type IncomingMessage,
  type ServerResponse,
} from "node:http";

// A local fake OpenAI-compatible Chat Completions provider for desktop runtime
// specs. The daemon's step host sends one request per model step: the
// system prompt and the Run's session path as `messages`, the kernel's tool
// list as `tools`, and expects one assistant message back (text and/or
// `tool_calls`). Authoring requests (pkg/authoring) use the same endpoint and
// offer `submit_authoring_intent` as their first tool.

export type ChatToolCall = {
  id: string;
  type: "function";
  function: { name: string; arguments: string };
};

export type ChatMessage = {
  role: "system" | "user" | "assistant" | "tool";
  content: string | null;
  tool_calls?: ChatToolCall[];
  tool_call_id?: string;
};

export type ChatTool = {
  type: "function";
  function: { name: string; description?: string; parameters: any };
};

export type ChatRequest = {
  model: string;
  messages: ChatMessage[];
  tools?: ChatTool[];
  tool_choice?: unknown;
  max_completion_tokens?: number;
  stream?: boolean;
};

export type Reply = {
  status?: number;
  text?: string;
  toolCalls?: { name: string; args: unknown }[];
  usage?: { prompt_tokens: number; completion_tokens: number };
};

// Kernel tool names (pkg/runtime/control_tools.go). One place to change.
export const FINAL_TOOL = "turn_final_answer";
export const DELEGATE_TOOL = "agent_delegate";
export const REQUEST_DECISION_TOOL = "agent_request_decision";
export const COMPLETION_REVIEW_TOOL = "agent_request_review";
export const AUTHORING_TOOL = "submit_authoring_intent";
// The desktop Team router's forced tool (internal/daemon/conversation_provider.go).
export const ROUTE_TOOL = "route_message";

// Kernel notes reach the provider as user messages with this prefix
// (internal/daemon/openai_render.go).
export const NOTE_PREFIX = "Note from OpenSeal: ";

export const DEFAULT_USAGE = { prompt_tokens: 100, completion_tokens: 80 };

export function isAuthoring(r: ChatRequest): boolean {
  return r.tools?.[0]?.function.name === AUTHORING_TOOL;
}

export function hasTool(r: ChatRequest, name: string): boolean {
  return (r.tools ?? []).some((tool) => tool.function.name === name);
}

export function tool(r: ChatRequest, name: string): ChatTool | undefined {
  return (r.tools ?? []).find((t) => t.function.name === name);
}

function text(message: ChatMessage): string {
  return typeof message.content === "string" ? message.content : "";
}

export function isNote(content: string): boolean {
  return content.startsWith(NOTE_PREFIX);
}

// All role:user contents in order, kernel notes included.
export function userTexts(r: ChatRequest): string[] {
  return r.messages.filter((m) => m.role === "user").map(text);
}

// The first role:user content that is not a kernel note: the Run goal for a
// task Run, whose session is seeded with it.
export function firstUserText(r: ChatRequest): string {
  return userTexts(r).find((t) => !isNote(t)) ?? "";
}

// The last role:user content that is not a kernel note: the newest message
// of a chat step (the run_context note follows every chat message).
export function lastUserText(r: ChatRequest): string {
  return (
    userTexts(r)
      .filter((t) => !isNote(t))
      .at(-1) ?? ""
  );
}

// Contents of the role:tool messages that answer calls of `name`.
export function toolResults(r: ChatRequest, name: string): string[] {
  const ids = new Set<string>();
  for (const message of r.messages)
    for (const call of message.tool_calls ?? [])
      if (call.function.name === name) ids.add(call.id);
  return r.messages
    .filter(
      (m) => m.role === "tool" && m.tool_call_id && ids.has(m.tool_call_id),
    )
    .map(text);
}

let replies = 0;

// reply renders one Chat Completions response body: one choice whose
// finish_reason is tool_calls when there are calls and stop otherwise.
// Call ids are "fx-<n>", unique across the process.
export function reply(r: Reply): string {
  if (r.status !== undefined && r.status !== 200)
    return JSON.stringify({
      error: { message: "Synthetic provider rate limit" },
    });
  const calls = (r.toolCalls ?? []).map((call) => ({
    id: `fx-${++replies}`,
    type: "function" as const,
    function: { name: call.name, arguments: JSON.stringify(call.args ?? {}) },
  }));
  return JSON.stringify({
    choices: [
      {
        finish_reason: calls.length ? "tool_calls" : "stop",
        message: {
          role: "assistant",
          content: r.text ?? null,
          ...(calls.length ? { tool_calls: calls } : {}),
        },
      },
    ],
    usage: r.usage ?? DEFAULT_USAGE,
  });
}

async function readBody(req: IncomingMessage): Promise<string> {
  let body = "";
  for await (const chunk of req) body += chunk;
  return body;
}

// startLoopProvider serves `handle` on 127.0.0.1 at a free port. `calls`
// records every decoded request in arrival order, `errors` every handler
// failure. baseUrl ends in /v1; the
// daemon appends /chat/completions.
export async function startLoopProvider(
  handle: (r: ChatRequest) => Promise<Reply> | Reply,
): Promise<{
  baseUrl: string;
  calls: ChatRequest[];
  errors: string[];
  close(): Promise<void>;
}> {
  const calls: ChatRequest[] = [];
  // A handler that throws (a failed expect) answers HTTP 500 and is recorded
  // here, so the spec can assert none happened.
  const errors: string[] = [];
  const server = createServer(
    async (req: IncomingMessage, res: ServerResponse) => {
      let status = 500;
      let body = JSON.stringify({ error: { message: "fixture failure" } });
      try {
        const request = JSON.parse(await readBody(req)) as ChatRequest;
        calls.push(request);
        const answer = await handle(request);
        status = answer.status ?? 200;
        body = reply(answer);
      } catch (error) {
        errors.push(String(error));
        body = JSON.stringify({ error: { message: String(error) } });
      }
      if (res.destroyed) return;
      res.writeHead(status, { "Content-Type": "application/json" });
      res.end(body);
    },
  );
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address() as { port: number };
  return {
    baseUrl: `http://127.0.0.1:${port}/v1`,
    calls,
    errors,
    close: () => {
      server.closeAllConnections();
      return new Promise<void>((resolve) => server.close(() => resolve()));
    },
  };
}
