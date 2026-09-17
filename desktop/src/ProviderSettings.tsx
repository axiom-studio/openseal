import { useEffect, useRef, useState, type FormEvent } from "react";
import {
  Check,
  Eye,
  EyeOff,
  KeyRound,
  LoaderCircle,
  ShieldCheck,
} from "lucide-react";
import {
  loadProvider,
  saveProvider,
  message,
  NativeUpdateRequired,
  type ProviderSettings as Settings,
} from "./api";

export default function ProviderSettings({
  onSaved,
}: {
  onSaved: () => Promise<void>;
}) {
  const [saved, setSaved] = useState<Settings | null>(null);
  const [baseUrl, setBaseUrl] = useState("https://api.openai.com/v1");
  const [model, setModel] = useState("");
  const [apiKey, setApiKey] = useState("");
  const [reveal, setReveal] = useState(false);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [updateRequired, setUpdateRequired] = useState(false);
  const [feedback, setFeedback] = useState("");
  const [warning, setWarning] = useState(false);
  const mounted = useRef(true);
  const busy = useRef(false);
  const result = useRef<HTMLDivElement>(null);

  async function load() {
    setLoading(true);
    setError("");
    setUpdateRequired(false);
    try {
      const settings = await loadProvider();
      if (!mounted.current) return;
      setSaved(settings);
      setBaseUrl(settings.baseUrl);
      setModel(settings.model);
    } catch (e) {
      if (mounted.current) {
        setError(message(e));
        setUpdateRequired(e instanceof NativeUpdateRequired);
      }
    } finally {
      if (mounted.current) setLoading(false);
    }
  }
  useEffect(() => {
    mounted.current = true;
    void load();
    return () => {
      mounted.current = false;
    };
  }, []);
  function changed() {
    setFeedback("");
    setError("");
  }
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (busy.current || !saved) return;
    busy.current = true;
    setSaving(true);
    setError("");
    setFeedback("");
    setReveal(false);
    try {
      const response = await saveProvider({
        baseUrl: baseUrl.trim(),
        model: model.trim(),
        apiKey,
      });
      if (!mounted.current) return;
      setApiKey("");
      setSaved(response.settings);
      setBaseUrl(response.settings.baseUrl);
      setModel(response.settings.model);
      setWarning(!response.reconnected);
      setFeedback(response.message);
      await onSaved();
      requestAnimationFrame(() => result.current?.focus());
    } catch (e) {
      if (mounted.current) {
        setError(message(e));
        requestAnimationFrame(() => result.current?.focus());
      }
    } finally {
      busy.current = false;
      if (mounted.current) setSaving(false);
    }
  }
  const dirty =
    saved &&
    (baseUrl.trim().replace(/\/$/, "") !== saved.baseUrl ||
      model.trim() !== saved.model ||
      !!apiKey.trim());
  const configured = !!saved?.hasApiKey && !!saved.model;

  return (
    <section
      className="settings-section provider-section"
      aria-labelledby="provider-heading"
    >
      <div className="section-heading">
        <h2 id="provider-heading">Model provider</h2>
        {!loading && saved && (
          <span className={`provider-badge ${configured ? "configured" : ""}`}>
            {configured ? <Check size={13} /> : <KeyRound size={13} />}{" "}
            {configured ? "Configured" : "Not configured"}
          </span>
        )}
      </div>
      <p className="provider-description">
        Choose the model OpenSeal uses to create agents and carry out their
        tasks. Connect OpenAI or another provider with an OpenAI-compatible API.
      </p>
      {loading ? (
        <div
          className="skeleton-list"
          aria-busy="true"
          aria-label="Loading provider settings"
        >
          <span />
          <span />
        </div>
      ) : (
        <form onSubmit={submit} className="provider-form">
          <fieldset disabled={saving || !saved}>
            <div className="provider-fields">
              <label className="form-field">
                <span>Provider URL</span>
                <input
                  type="url"
                  aria-label="Provider URL"
                  autoComplete="url"
                  value={baseUrl}
                  onChange={(e) => {
                    setBaseUrl(e.target.value);
                    changed();
                  }}
                  required
                  placeholder="https://api.openai.com/v1"
                  spellCheck={false}
                  aria-describedby="provider-url-hint"
                />
                <small id="provider-url-hint">
                  The API base URL, including its version path.
                </small>
              </label>
              <label className="form-field">
                <span>Model</span>
                <input
                  aria-label="Model"
                  value={model}
                  onChange={(e) => {
                    setModel(e.target.value);
                    changed();
                  }}
                  required
                  maxLength={256}
                  placeholder="Enter a model name"
                  autoComplete="off"
                  spellCheck={false}
                  aria-describedby="provider-model-hint"
                />
                <small id="provider-model-hint">
                  Use the exact model name from your provider.
                </small>
              </label>
            </div>
            <div className="form-field">
              <label htmlFor="provider-api-key">
                API key{" "}
                {saved?.hasApiKey && (
                  <span className="saved-key">
                    <Check size={12} />
                    Saved securely
                  </span>
                )}
              </label>
              <div className="secret-input">
                <input
                  id="provider-api-key"
                  aria-label="API key"
                  type={reveal ? "text" : "password"}
                  value={apiKey}
                  onChange={(e) => {
                    setApiKey(e.target.value);
                    changed();
                  }}
                  placeholder={
                    saved?.hasApiKey
                      ? "Leave blank to keep your saved key"
                      : "Paste your API key"
                  }
                  required={!saved?.hasApiKey}
                  autoComplete="new-password"
                  spellCheck={false}
                  autoCapitalize="none"
                  aria-describedby="provider-key-hint"
                />
                <button
                  type="button"
                  className="icon-button"
                  aria-label={reveal ? "Hide API key" : "Show API key"}
                  onClick={() => setReveal((v) => !v)}
                >
                  {reveal ? <EyeOff size={17} /> : <Eye size={17} />}
                </button>
              </div>
              <small id="provider-key-hint">
                <ShieldCheck size={13} />
                Stored in a private file on this computer. Your saved key is
                never sent back to this screen.
              </small>
            </div>
          </fieldset>
          {(error || feedback) && (
            <div
              ref={result}
              tabIndex={-1}
              className={`provider-feedback ${error || warning ? "is-error" : ""}`}
              role={error || warning ? "alert" : "status"}
            >
              {!error && !warning && <Check size={16} />}
              <span>
                <span>{error || feedback}</span>
                {feedback && !warning && (
                  <small>
                    Provider access is checked when you create an agent.
                  </small>
                )}
              </span>
              {!saved && !updateRequired && (
                <button
                  className="button"
                  type="button"
                  onClick={() => void load()}
                >
                  Reload settings
                </button>
              )}
            </div>
          )}
          <div className="provider-form-footer">
            <p>
              Saving reconnects the local workspace.
              <br />
              Existing agents and saved work are kept.
            </p>
            <button
              className="button primary"
              disabled={saving || !saved || (!dirty && !warning)}
              type="submit"
            >
              {saving && <LoaderCircle size={15} className="spin" />}
              {saving
                ? "Saving and reconnecting…"
                : configured
                  ? "Save changes"
                  : "Save and connect"}
            </button>
          </div>
        </form>
      )}
    </section>
  );
}
