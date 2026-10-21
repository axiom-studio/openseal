/**
 * React helpers for apps. Most apps need only these two:
 *
 *   const { start, state, running } = useRun();
 *   const pipeline = useData<Stage[]>("pipeline_by_stage");
 */
import { useCallback, useEffect, useRef, useState } from "react";
import { type DataSnapshot, type Json, type Run, type RunRequest, type RunState, getData, runAgent, watchData } from "./index.ts";

export function useRun() {
  const [state, setState] = useState<RunState | null>(null);
  const [error, setError] = useState<Error | null>(null);
  const run = useRef<Run | null>(null);
  const stop = useRef<(() => void) | null>(null);
  useEffect(() => () => { stop.current?.(); }, []);
  const start = useCallback(async (request: RunRequest) => {
    stop.current?.();
    setError(null);
    try {
      const next = await runAgent(request);
      run.current = next;
      setState(next.state);
      stop.current = next.onUpdate(setState);
      return next;
    } catch (cause) {
      setError(cause instanceof Error ? cause : new Error(String(cause)));
      return null;
    }
  }, []);
  const cancel = useCallback(async () => { if (run.current) setState(await run.current.cancel()); }, []);
  const running = Boolean(state && !["completed", "failed", "canceled"].includes(state.status));
  return { start, cancel, state, running, error };
}

export function useData<T = Json>(name: string) {
  const [snapshot, setSnapshot] = useState<DataSnapshot<T> | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<Error | null>(null);
  useEffect(() => {
    let active = true;
    setLoading(true);
    getData<T>(name).then(value => { if (active) { setSnapshot(value); setLoading(false); } },
      cause => { if (active) { setError(cause instanceof Error ? cause : new Error(String(cause))); setLoading(false); } });
    const stop = watchData<T>(name, value => { if (active) setSnapshot(value); });
    return () => { active = false; stop(); };
  }, [name]);
  return { data: snapshot?.value ?? null, updatedAt: snapshot?.updatedAt ?? null, loading, error };
}
