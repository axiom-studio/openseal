import { useEffect, useRef, useState } from "react";
import { api, message, scoped, type Run } from "./api";
import {
  previewArtifact,
  saveArtifact,
  textArtifact,
  previewLimit,
  exportLimit,
  type Artifact,
} from "./artifacts";

export default function TaskArtifacts({
  run,
  canDownload,
  reference,
}: {
  run?: Run;
  canDownload: boolean;
  reference?: { id: string; version?: number };
}) {
  const [open, setOpen] = useState(!!reference);
  const [items, setItems] = useState<Artifact[]>([]);
  const [offset, setOffset] = useState(0);
  const [more, setMore] = useState(false);
  const [busy, setBusy] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [working, setWorking] = useState("");
  const [preview, setPreview] = useState<{ key: string; text: string } | null>(
    null,
  );
  const epoch = useRef(0);
  const operation = useRef(0);
  const feedback = useRef<HTMLParagraphElement>(null);
  const previewContent = useRef<HTMLPreElement>(null);
  useEffect(
    () => () => {
      epoch.current++;
      operation.current++;
    },
    [],
  );
  useEffect(() => {
    if (preview) previewContent.current?.focus();
  }, [preview]);
  async function load(page = 0, manual = false) {
    const request = ++epoch.current;
    setBusy(true);
    setError("");
    try {
      const result = reference
        ? [
            await api<Artifact>(
              scoped(
                `/artifacts/${encodeURIComponent(reference.id)}?version=${reference.version || 0}`,
              ),
            ),
          ]
        : await api<Artifact[] | null>(
            scoped(
              `/artifacts?producerRunId=${encodeURIComponent(run!.id)}&latestOnly=false&limit=21&offset=${page}`,
            ),
          );
      if (request !== epoch.current) return;
      if (result !== null && !Array.isArray(result))
        throw new Error("OpenSeal returned an unreadable artifact list.");
      if (
        reference &&
        (result?.[0]?.id !== reference.id ||
          !Number.isSafeInteger(result[0].version) ||
          result[0].version < 1 ||
          typeof result[0].name !== "string" ||
          !Number.isSafeInteger(result[0].sizeBytes) ||
          result[0].sizeBytes < 0 ||
          (result[0].mediaType !== undefined &&
            typeof result[0].mediaType !== "string") ||
          (reference.version && result[0].version !== reference.version))
      )
        throw new Error("The artifact did not match the referenced version.");
      if (
        run &&
        (result || []).some((item) => item.provenance?.runId !== run.id)
      )
        throw new Error("Artifact results do not match this task.");
      setItems((result || []).slice(0, 20));
      setOffset(page);
      setMore((result || []).length > 20);
      setLoaded(true);
      if (manual) setNotice("Artifacts refreshed.");
    } catch (e) {
      if (request === epoch.current)
        setError(`Could not load artifacts. ${message(e)}`);
    } finally {
      if (request === epoch.current) {
        setBusy(false);
        if (manual) requestAnimationFrame(() => feedback.current?.focus());
      }
    }
  }
  useEffect(() => {
    if (open) void load(offset);
    else {
      epoch.current++;
      setBusy(false);
    }
  }, [open, run?.revision]);
  async function useArtifact(artifact: Artifact, action: "preview" | "save") {
    if (working) return;
    const request = ++operation.current;
    const key = `${artifact.id}:${artifact.version}`;
    setWorking(key);
    setError("");
    setNotice("");
    let previewLoaded = false;
    try {
      if (action === "preview") {
        const text = await previewArtifact(artifact);
        if (request !== operation.current) return;
        setPreview({ key, text });
        previewLoaded = true;
        setNotice(
          `Preview loaded for ${artifact.name}, version ${artifact.version}.`,
        );
      } else {
        const result = await saveArtifact(artifact);
        if (request !== operation.current) return;
        setNotice(result);
      }
    } catch (e) {
      if (request === operation.current) setError(message(e));
    } finally {
      if (request === operation.current) {
        setWorking("");
        if (!previewLoaded)
          requestAnimationFrame(() => feedback.current?.focus());
      }
    }
  }
  return (
    <details
      className="task-artifacts"
      open={open}
      onToggle={(e) => setOpen(e.currentTarget.open)}
    >
      <summary>{reference ? "Artifact details" : "Artifacts"}</summary>
      {open && (
        <>
          <p className="muted">
            {reference
              ? reference.version
                ? "The exact referenced version."
                : "This reference does not pin a version. Showing the latest saved version."
              : "Files recorded by this task, including earlier versions."}{" "}
            Text previews support UTF-8 files up to 256 KiB; saving supports
            files up to 100 MiB.
          </p>
          <button
            className="button"
            disabled={busy}
            onClick={() => void load(offset, true)}
          >
            Refresh artifacts
          </button>
          {!loaded && busy && <p role="status">Loading artifacts…</p>}
          {loaded && !items.length && (
            <p className="muted">
              No artifacts have been recorded for this task.
            </p>
          )}
          {!!items.length && (
            <ul className="artifact-list">
              {items.map((artifact) => {
                const key = `${artifact.id}:${artifact.version}`;
                const available =
                  canDownload && artifact.contentAvailability !== "unavailable";
                return (
                  <li key={key}>
                    <h4>{artifact.name}</h4>
                    <p className="muted">
                      Version {artifact.version} · {artifact.classification} ·{" "}
                      {artifact.sizeBytes.toLocaleString()} bytes
                    </p>
                    <p className="muted">
                      {artifact.mediaType || "File"} · Produced by{" "}
                      {artifact.provenance?.producer?.id || "Unknown producer"}
                    </p>
                    {available ? (
                      <div className="inspector-actions">
                        {textArtifact(artifact.mediaType) &&
                          artifact.sizeBytes <= previewLimit && (
                            <button
                              className="button"
                              disabled={!!working}
                              onClick={() =>
                                void useArtifact(artifact, "preview")
                              }
                            >
                              Preview text
                            </button>
                          )}
                        <button
                          className="button"
                          disabled={
                            !!working || artifact.sizeBytes > exportLimit
                          }
                          onClick={() => void useArtifact(artifact, "save")}
                        >
                          {working === key ? "Working…" : "Save as…"}
                        </button>
                      </div>
                    ) : (
                      <p className="inline-help">
                        {!canDownload
                          ? "Artifact downloads are unavailable in this workspace."
                          : "The artifact record is saved, but its content is unavailable."}
                      </p>
                    )}
                    {preview?.key === key && (
                      <div>
                        <h4>Text preview</h4>
                        <pre
                          ref={previewContent}
                          tabIndex={0}
                          aria-label={`Text preview of ${artifact.name}`}
                        >
                          {preview.text}
                        </pre>
                      </div>
                    )}
                  </li>
                );
              })}
            </ul>
          )}
          {!reference && (
            <div className="inspector-actions">
              <button
                className="button"
                disabled={busy || offset === 0}
                onClick={() => void load(Math.max(0, offset - 20), true)}
              >
                Previous artifacts
              </button>
              <button
                className="button"
                disabled={busy || !more}
                onClick={() => void load(offset + 20, true)}
              >
                Next artifacts
              </button>
            </div>
          )}
          <p
            ref={feedback}
            tabIndex={-1}
            role={error ? "alert" : "status"}
            className={error ? "error-text" : "muted"}
          >
            {error || notice}
          </p>
        </>
      )}
    </details>
  );
}
