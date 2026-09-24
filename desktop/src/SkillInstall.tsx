import { useState } from "react";
import {
  api,
  message,
  supports,
  type Capabilities,
  type Proposal,
} from "./api";

type Skill = NonNullable<NonNullable<Proposal["catalog"]>["skills"]>[string];
type Verification = {
  ok: boolean;
  decision: string;
  reasons?: string[];
  displayName: string;
  publisherHandle?: string;
  version: string;
  pageUrl?: string;
};
type Receipt = {
  sourceIdentity: string;
  version: string;
  outcome: string;
};

const registry = "https://clawhub.ai::";
export function clawHubReference(skill: Skill | undefined): string | null {
  if (!skill?.version || !skill.sourceIdentity?.startsWith(registry))
    return null;
  const reference = skill.sourceIdentity.slice(registry.length);
  if (
    !/^(?:@?[a-zA-Z0-9][a-zA-Z0-9_-]*\/)?[a-zA-Z0-9][a-zA-Z0-9_-]*$/.test(
      reference,
    )
  )
    return null;
  return reference;
}

export default function SkillInstall({
  skill,
  capabilities,
  onInstalled,
}: {
  skill: Skill;
  capabilities: Capabilities | null;
  onInstalled: () => void;
}) {
  const reference = clawHubReference(skill);
  const [verification, setVerification] = useState<Verification | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [installed, setInstalled] = useState(false);
  if (!reference) return null;
  const canVerify = supports(capabilities, "clawhub-lifecycle", "verify");
  const canInstall = supports(capabilities, "clawhub-lifecycle", "install");
  const url = `/clawhub/catalog/${encodeURIComponent(reference)}`;
  async function verify() {
    setBusy(true);
    setError("");
    setVerification(null);
    try {
      const result = await api<Verification>(`${url}/verify`, {
        method: "POST",
        body: { version: skill.version },
      });
      setVerification(result);
    } catch (e) {
      setError(message(e));
    } finally {
      setBusy(false);
    }
  }
  async function install() {
    if (
      !canInstall ||
      busy ||
      !verification?.ok ||
      verification.version !== skill.version
    )
      return;
    setBusy(true);
    setError("");
    try {
      const result = await api<Receipt>(`${url}/install`, {
        method: "POST",
        body: { version: skill.version },
      });
      if (
        result.sourceIdentity !== skill.sourceIdentity ||
        result.version !== skill.version ||
        !["installed", "updated", "unchanged"].includes(result.outcome)
      )
        throw new Error(
          "The installed Skill does not match the reviewed source and version. Check the Marketplace before continuing.",
        );
      setInstalled(true);
      onInstalled();
    } catch (e) {
      setError(message(e));
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="skill-install">
      <p>
        <strong>{skill.name || skill.id}</strong> · {reference} · version{" "}
        {skill.version}
      </p>
      {installed ? (
        <p role="status">
          Skill installed. Create a fresh proposal to check the updated
          workspace.
        </p>
      ) : (
        <>
          <button
            type="button"
            className="button"
            disabled={!canVerify || busy}
            onClick={() => void verify()}
          >
            {busy && !verification
              ? "Verifying Skill…"
              : "Review Skill for installation"}
          </button>
          {verification && (
            <div className="proposal-notes">
              <p>
                <strong>
                  {verification.displayName || skill.name || skill.id}
                </strong>{" "}
                · publisher {verification.publisherHandle || "unknown"} ·
                version {verification.version}
              </p>
              <p>Verification: {verification.decision}</p>
              {!!verification.reasons?.length && (
                <ul>
                  {verification.reasons.map((reason, i) => (
                    <li key={i}>{reason}</li>
                  ))}
                </ul>
              )}
              {verification.ok && verification.version === skill.version && (
                <button
                  type="button"
                  className="button primary"
                  disabled={!canInstall || busy}
                  onClick={() => void install()}
                >
                  {busy
                    ? "Installing Skill…"
                    : `Install ${reference} version ${skill.version}`}
                </button>
              )}
            </div>
          )}
          {capabilities && !canInstall && (
            <p className="muted">
              Skill installation is unavailable for this workspace operator.
            </p>
          )}
        </>
      )}
      {error && (
        <p className="error-text" role="alert">
          {error}
        </p>
      )}
    </div>
  );
}
