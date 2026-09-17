import { useEffect, useId, useRef, useState } from "react";
import {
  api,
  ApiError,
  message,
  scoped,
  supports,
  type Capabilities,
  type Run,
} from "./api";

type Pending = { id: string; instruction: string };
function restore(key: string): { draft: string; pending: Pending | null } {
  try {
    const value = JSON.parse(localStorage.getItem(key) || "null");
    if (typeof value?.draft === "string" && value.draft.length <= 4000) {
      const p = value.pending;
      return {
        draft: value.draft,
        pending:
          typeof p?.id === "string" &&
          p.id.length === 36 &&
          typeof p.instruction === "string" &&
          p.instruction.length <= 4000
            ? p
            : null,
      };
    }
  } catch {
    /* A damaged saved draft must not block the task. */
  }
  return { draft: "", pending: null };
}
export default function WorkGuidance({
  run,
  capabilities,
  onChange,
  launch,
}: {
  run: Run;
  capabilities: Capabilities | null;
  onChange: (run: Run) => void;
  launch?: { id: string; context: string };
}) {
  const key = `openseal.guidance.${run.id}`;
  const [initial] = useState(() => restore(key));
  const [draft, setDraft] = useState(initial.draft);
  const [pending, setPending] = useState<Pending | null>(initial.pending);
  const [open, setOpen] = useState(!!initial.draft || !!initial.pending);
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState(
    initial.pending
      ? "A previous submission needs checking. Check saved guidance before retrying."
      : "",
  );
  const [failed, setFailed] = useState(false);
  const alive = useRef(true);
  const lock = useRef(false);
  const input = useRef<HTMLTextAreaElement>(null);
  const feedback = useRef<HTMLParagraphElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const label = useId();
  const help = useId();
  const terminal = ["completed", "failed", "canceled"].includes(run.status);
  const allowed =
    !terminal && supports(capabilities, "agent-runs", "intervene");
  const saved = run.pendingInterventions || [];
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);
  useEffect(() => {
    try {
      if (draft || pending)
        localStorage.setItem(key, JSON.stringify({ draft, pending }));
      else localStorage.removeItem(key);
    } catch {
      /* In-memory editing remains available. */
    }
  }, [key, draft, pending]);
  useEffect(() => {
    if (!launch) return;
    setOpen(true);
    if (!draft && !pending) setDraft(launch.context);
    else {
      setNotice(
        "Your existing guidance draft is kept. Include the clarification you want the lead to consider before sending.",
      );
      setFailed(false);
    }
  }, [launch]);
  useEffect(() => {
    if (open && launch) input.current?.focus();
  }, [open, launch]);
  function recorded(next: Run, submission: Pending) {
    return (
      next.id === run.id &&
      next.pendingInterventions?.some(
        (item) =>
          item.id === submission.id &&
          item.instruction === submission.instruction &&
          item.actor.type === "user" &&
          item.actor.id === "local-operator",
      )
    );
  }
  function success() {
    setPending(null);
    setDraft("");
    setOpen(false);
    setFailed(false);
    setNotice(
      "Guidance saved in this task’s context. Actions already started may finish.",
    );
  }
  async function refresh(submission = pending) {
    const next = await api<Run>(
      scoped(`/agent-runs/${encodeURIComponent(run.id)}`),
    );
    if (!alive.current) return false;
    if (next.id !== run.id)
      throw new Error("The saved task did not match this task.");
    onChange(next);
    if (submission && recorded(next, submission)) {
      success();
      return true;
    }
    return false;
  }
  async function check() {
    if (lock.current) return;
    lock.current = true;
    setBusy(true);
    setFailed(false);
    try {
      if (!(await refresh()) && alive.current)
        setNotice(
          "This instruction is not recorded yet. Retry guidance uses the same request to avoid duplicates.",
        );
    } catch (e) {
      if (alive.current) {
        setFailed(true);
        setNotice(`Could not check saved guidance. ${message(e)}`);
      }
    } finally {
      lock.current = false;
      if (alive.current) {
        setBusy(false);
        requestAnimationFrame(() => feedback.current?.focus());
      }
    }
  }
  async function submit() {
    if (lock.current || !allowed || !(pending?.instruction || draft.trim()))
      return;
    const submission = pending || {
      id: crypto.randomUUID(),
      instruction: draft.trim(),
    };
    // Persist the request identity before sending, including across app restarts.
    try {
      localStorage.setItem(key, JSON.stringify({ draft, pending: submission }));
    } catch {
      setFailed(true);
      setNotice(
        "Could not preserve this request for recovery. Check available local storage before sending.",
      );
      requestAnimationFrame(() => feedback.current?.focus());
      return;
    }
    lock.current = true;
    setPending(submission);
    setBusy(true);
    setFailed(false);
    setNotice("");
    try {
      const result = await api<{ run: Run }>(
        scoped(`/agent-runs/${encodeURIComponent(run.id)}/commands`),
        {
          method: "POST",
          body: {
            kind: "intervene",
            interventionId: submission.id,
            instruction: submission.instruction,
            expectedRevision: run.revision,
            actor: { type: "user", id: "local-operator" },
            summary: "Guidance added from desktop",
            visibility: "scope",
          },
        },
      );
      if (!alive.current) return;
      if (!recorded(result.run, submission))
        throw new Error("The response did not confirm this instruction.");
      onChange(result.run);
      success();
    } catch (e) {
      if (!alive.current) return;
      setFailed(true);
      if (e instanceof ApiError && e.status === 409) {
        try {
          if (!(await refresh(submission)) && alive.current) {
            setPending(null);
            setNotice(
              "This task changed. Your draft is preserved. Review the latest state before sending again.",
            );
          }
        } catch {
          if (alive.current)
            setNotice(
              "This task changed, but its latest state could not be loaded. Check saved guidance before retrying.",
            );
        }
      } else {
        if (e instanceof ApiError && e.status >= 400 && e.status < 500) {
          setPending(null);
          setNotice(`Guidance was not saved. ${message(e)}`);
        } else {
          setNotice(`${message(e)} Check saved guidance before retrying.`);
        }
      }
    } finally {
      lock.current = false;
      if (alive.current) {
        setBusy(false);
        requestAnimationFrame(() => feedback.current?.focus());
      }
    }
  }
  if (!allowed && !saved.length && !draft && !pending) return null;
  return (
    <section className="work-guidance" aria-label="Task guidance">
      {!open && allowed && (
        <button
          ref={trigger}
          className="button"
          onClick={() => {
            setOpen(true);
            requestAnimationFrame(() => input.current?.focus());
          }}
        >
          Add guidance
        </button>
      )}
      {open && (
        <form
          onSubmit={(e) => {
            e.preventDefault();
            void submit();
          }}
        >
          <label htmlFor={label}>Guidance for this task</label>
          <p id={help} className="muted">
            {terminal
              ? "This task has ended. Unsent guidance is kept here for you to copy."
              : !allowed
                ? "Adding guidance is unavailable in this workspace. Your draft is kept here for you to copy."
                : run.status === "paused"
                  ? "Guidance is saved while work stays paused. Resume when you are ready."
                  : run.status === "sleeping"
                    ? "Saving guidance wakes this task so it can consider your instruction."
                    : "Guidance is included in subsequent turns. Existing approval and dependency waits remain in effect; actions already started may finish."}
          </p>
          <textarea
            ref={input}
            id={label}
            aria-describedby={help}
            rows={4}
            maxLength={4000}
            value={draft}
            disabled={busy}
            readOnly={!allowed || !!pending}
            onChange={(e) => setDraft(e.target.value)}
          />
          <p className="muted">
            {draft.length.toLocaleString()} / 4,000 characters
          </p>
          <div className="inspector-actions">
            {allowed && (
              <button
                className="button primary"
                type="submit"
                disabled={busy || !draft.trim()}
              >
                {busy
                  ? "Saving…"
                  : pending
                    ? "Retry guidance"
                    : "Save guidance"}
              </button>
            )}
            {pending && (
              <button
                className="button"
                type="button"
                disabled={busy}
                onClick={() => void check()}
              >
                Check saved guidance
              </button>
            )}
            {!draft && !pending && allowed && (
              <button
                className="button"
                type="button"
                onClick={() => {
                  setOpen(false);
                  requestAnimationFrame(() => trigger.current?.focus());
                }}
              >
                Close guidance
              </button>
            )}
          </div>
        </form>
      )}
      <p
        ref={feedback}
        tabIndex={-1}
        role={failed ? "alert" : "status"}
        className={failed ? "error-text" : "muted"}
      >
        {notice}
      </p>
      {!!saved.length && (
        <details>
          <summary>Saved guidance ({saved.length})</summary>
          <p className="muted">
            Saved instructions remain in the task context. This history does not
            confirm that an instruction has been followed.
          </p>
          <ol className="guidance-history">
            {[...saved].reverse().map((item) => (
              <li key={item.id}>
                <p className="preserve-lines">{item.instruction}</p>
                <p className="muted">
                  {item.actor.id} ·{" "}
                  <time dateTime={item.createdAt}>
                    {new Date(item.createdAt).toLocaleString()}
                  </time>
                </p>
              </li>
            ))}
          </ol>
        </details>
      )}
    </section>
  );
}
