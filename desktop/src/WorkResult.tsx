import { useState } from "react";
import { Copy } from "lucide-react";
import type { Run } from "./api";

export default function WorkResult({ run }: { run: Run }) {
  const [feedback, setFeedback] = useState("");
  const output = run.output;
  const text = [output?.reply, output?.answer, output?.summary].find(
    (value): value is string => typeof value === "string" && !!value.trim(),
  );
  const hasOutput = !!output && Object.keys(output).length > 0;
  return (
    <section aria-label="Work result">
      <h3>Result</h3>
      {text ? (
        <>
          <p className="preserve-lines work-result-text">{text}</p>
          <button
            className="button"
            onClick={async () => {
              try {
                await navigator.clipboard.writeText(text);
                setFeedback("Result copied.");
              } catch {
                setFeedback(
                  "Could not copy. Select the result text and copy it manually.",
                );
              }
            }}
          >
            <Copy size={14} /> Copy result
          </button>
          <p className="muted" role="status">
            {feedback}
          </p>
        </>
      ) : !hasOutput ? (
        <p className="muted">
          {run.status === "completed"
            ? "This run completed without a text result."
            : ["failed", "canceled"].includes(run.status)
              ? "This run ended without a result."
              : "Results will appear when the agent produces them."}
        </p>
      ) : null}
      {hasOutput && (
        <details>
          <summary>Structured output</summary>
          <pre>{JSON.stringify(output, null, 2)}</pre>
        </details>
      )}
    </section>
  );
}
