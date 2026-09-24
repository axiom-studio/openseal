import { useState } from "react";
import { api, message, supports, type Capabilities } from "./api";
import SkillInstall from "./SkillInstall";

type Summary = {
  slug: string;
  name: string;
  description?: string;
  version?: string;
};
type Detail = Summary & { owner?: string; version: string };

export default function SkillDiscovery({
  query,
  capabilities,
  onInstalled,
}: {
  query: string;
  capabilities: Capabilities | null;
  onInstalled: () => void;
}) {
  const [items, setItems] = useState<Summary[] | null>(null);
  const [selected, setSelected] = useState<Detail | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const canInspect = supports(
    capabilities,
    "clawhub-lifecycle",
    "inspect_catalog",
  );
  async function search() {
    setBusy(true);
    setError("");
    setSelected(null);
    try {
      const result = await api<{ items: Summary[] }>(
        `/clawhub/search?query=${encodeURIComponent(query)}`,
      );
      setItems(result.items || []);
    } catch (e) {
      setError(message(e));
    } finally {
      setBusy(false);
    }
  }
  async function inspect(item: Summary) {
    setBusy(true);
    setError("");
    try {
      const result = await api<Detail>(
        `/clawhub/catalog/${encodeURIComponent(item.slug)}`,
      );
      if (!result.version)
        throw new Error("This Skill has no exact version to review.");
      setSelected(result);
    } catch (e) {
      setError(message(e));
    } finally {
      setBusy(false);
    }
  }
  const reference =
    selected &&
    (selected.slug.includes("/")
      ? selected.slug
      : selected.owner
        ? `@${selected.owner}/${selected.slug}`
        : selected.slug);
  return (
    <div className="skill-discovery">
      <button
        className="button"
        type="button"
        disabled={!canInspect || busy}
        onClick={() => void search()}
      >
        {busy ? "Searching Skills…" : `Find Skill ${query}`}
      </button>
      {items && (
        <ul>
          {items.map((item) => (
            <li key={item.slug}>
              <strong>{item.name || item.slug}</strong>
              {item.description && (
                <span className="muted"> · {item.description}</span>
              )}
              <button
                className="button"
                type="button"
                disabled={busy}
                onClick={() => void inspect(item)}
              >
                Review this Skill
              </button>
            </li>
          ))}
        </ul>
      )}
      {items?.length === 0 && <p>No matching Skills were found.</p>}
      {selected && reference && (
        <SkillInstall
          key={reference + selected.version}
          skill={{
            id: query,
            name: selected.name,
            version: selected.version,
            sourceIdentity: `https://clawhub.ai::${reference}`,
          }}
          capabilities={capabilities}
          onInstalled={onInstalled}
        />
      )}
      {error && (
        <p role="alert" className="error-text">
          {error}
        </p>
      )}
    </div>
  );
}
