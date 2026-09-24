import { useState, type FormEvent } from "react";
import {
  api,
  message,
  saveSkillCredential,
  scope,
  type Capability,
  type Proposal,
} from "./api";

type Field = NonNullable<
  NonNullable<Capability["context"]>["bindingConfigurationFields"]
>[number];
function scalar(field: Field, index: number): unknown {
  const value = field.options[index]?.value;
  if (!value) return undefined;
  return value[field.type as keyof typeof value];
}

function NewSkillConnection({
  kind,
  bindingKey,
}: {
  kind: string;
  bindingKey: string;
}) {
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [secret, setSecret] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  async function submit(event: FormEvent) {
    event.preventDefault();
    if (busy || !kind || !name.trim() || !secret.trim()) return;
    setBusy(true);
    setError("");
    setNotice("");
    try {
      const result = await saveSkillCredential({
        kind,
        bindingKey,
        displayName: name.trim(),
        secret,
      });
      setSecret("");
      if (result.reconnected) window.location.reload();
      else setNotice(result.message);
    } catch (e) {
      setError(message(e));
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="skill-connection">
      <button type="button" className="button" onClick={() => setOpen(!open)}>
        {open ? "Cancel new connection" : `Add ${bindingKey} connection`}
      </button>
      {open && (
        <form onSubmit={(event) => void submit(event)}>
          <label className="form-field">
            <span>Connection name</span>
            <input
              value={name}
              onChange={(event) => setName(event.target.value)}
              maxLength={128}
              required
            />
          </label>
          <label className="form-field">
            <span>{bindingKey} secret</span>
            <input
              type="password"
              value={secret}
              onChange={(event) => setSecret(event.target.value)}
              autoComplete="off"
              required
            />
          </label>
          <p className="muted">
            The secret is saved in a private local file. The workspace restarts
            so it can use the new connection.
          </p>
          <button
            type="submit"
            className="button primary"
            disabled={busy || !name.trim() || !secret.trim()}
          >
            {busy ? "Saving connection…" : "Save connection"}
          </button>
        </form>
      )}
      {notice && <p role="status">{notice}</p>}
      {error && (
        <p className="error-text" role="alert">
          {error}
        </p>
      )}
    </div>
  );
}

export default function SkillConfiguration({
  proposal,
  capability,
  skillId,
  ownerId,
  onChange,
}: {
  proposal: Proposal;
  capability: Capability | undefined;
  skillId: string;
  ownerId: string;
  onChange: (proposal: Proposal) => void;
}) {
  const fields =
    capability?.context?.bindingConfigurationFields?.filter(
      (field) => field.catalogSkillId === skillId,
    ) || [];
  const requirements = proposal.requiredCredentialBindings?.[ownerId] || [];
  const choices = capability?.context?.credentialBindings || [];
  const [selected, setSelected] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  if (!fields.length && !requirements.length) return null;
  const optionFor = (key: string, kind: string) =>
    choices.filter(
      (choice) =>
        (!kind || choice.reference.kind === kind) &&
        (!choice.bindingKeys?.length || choice.bindingKeys.includes(key)),
    );
  const complete =
    fields.every(
      (field) =>
        !field.required ||
        selected[`field:${field.key}`] !== undefined ||
        proposal.placement?.bindingConfigs?.[ownerId]?.[skillId]?.[
          field.key
        ] !== undefined,
    ) &&
    requirements.every(
      (requirement) =>
        selected[`credential:${requirement.key}`] !== undefined ||
        !!proposal.placement?.credentialReferences?.[ownerId]?.[
          requirement.key
        ],
    );
  async function save() {
    if (!capability?.operations.includes("patch") || busy || !complete) return;
    setBusy(true);
    setError("");
    try {
      const placement = structuredClone(proposal.placement || {});
      placement.credentialReferences ||= {};
      placement.credentialReferences[ownerId] ||= {};
      placement.bindingConfigs ||= {};
      placement.bindingConfigs[ownerId] ||= {};
      placement.bindingConfigs[ownerId][skillId] ||= {};
      for (const field of fields) {
        const index = selected[`field:${field.key}`];
        if (index !== undefined)
          placement.bindingConfigs[ownerId][skillId][field.key] = scalar(
            field,
            Number(index),
          );
      }
      for (const requirement of requirements) {
        const index = selected[`credential:${requirement.key}`];
        if (index !== undefined)
          placement.credentialReferences[ownerId][requirement.key] = optionFor(
            requirement.key,
            requirement.kind,
          )[Number(index)].reference;
      }
      const updated = await api<Proposal>(
        `/authoring/workforce/change-sets/${encodeURIComponent(proposal.id)}/placement`,
        {
          method: "PATCH",
          key: crypto.randomUUID(),
          body: {
            scope,
            changeSetId: proposal.id,
            expectedRevision: proposal.revision,
            placement,
            reason: `Configure Skill ${skillId} in desktop proposal review`,
          },
        },
      );
      onChange(updated);
      setSaved(true);
    } catch (e) {
      setError(message(e));
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="skill-configuration">
      <h4>Configure {skillId}</h4>
      {fields.map((field) => (
        <label className="form-field" key={field.key}>
          <span>
            {field.prompt}
            {field.required ? " (required)" : ""}
          </span>
          <select
            value={selected[`field:${field.key}`] ?? ""}
            onChange={(e) =>
              setSelected((current) => ({
                ...current,
                [`field:${field.key}`]: e.target.value,
              }))
            }
          >
            <option value="">
              {proposal.placement?.bindingConfigs?.[ownerId]?.[skillId]?.[
                field.key
              ] !== undefined
                ? "Keep current value"
                : "Choose a value"}
            </option>
            {field.options.map((option, i) => (
              <option key={i} value={i}>
                {option.label}
              </option>
            ))}
          </select>
        </label>
      ))}
      {requirements.map((requirement) => (
        <div key={requirement.key}>
          <label className="form-field">
            <span>{requirement.key} credential</span>
            <select
              value={selected[`credential:${requirement.key}`] ?? ""}
              onChange={(e) =>
                setSelected((current) => ({
                  ...current,
                  [`credential:${requirement.key}`]: e.target.value,
                }))
              }
            >
              <option value="">
                {proposal.placement?.credentialReferences?.[ownerId]?.[
                  requirement.key
                ]
                  ? "Keep current connection"
                  : "Choose a connection"}
              </option>
              {optionFor(requirement.key, requirement.kind).map((choice, i) => (
                <option key={choice.reference.id} value={i}>
                  {choice.displayName}
                </option>
              ))}
            </select>
            {!optionFor(requirement.key, requirement.kind).length && (
              <small>
                No connection is available for this credential. Add one below.
              </small>
            )}
          </label>
          {requirement.kind && (
            <NewSkillConnection
              kind={requirement.kind}
              bindingKey={requirement.key}
            />
          )}
        </div>
      ))}
      <button
        type="button"
        className="button primary"
        disabled={
          !complete || !capability?.operations.includes("patch") || busy
        }
        onClick={() => void save()}
      >
        {busy ? "Saving configuration…" : "Save Skill configuration"}
      </button>
      {saved && (
        <p role="status">
          Configuration saved. Review the updated proposal requirements.
        </p>
      )}
      {error && (
        <p className="error-text" role="alert">
          {error}
        </p>
      )}
    </div>
  );
}
