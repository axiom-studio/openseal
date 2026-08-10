# Standalone context and local Vault

Embedding hosts can resolve model and Skill credentials through managed grants
and a Vault. Standalone OpenSeal provides the same separation with an optional
`context.yaml`: durable resources keep only opaque `kind/id` references, while
the context maps each reference to a local environment variable or private
file. The authoring provider reference is resolved inside the local daemon;
Skill-action references are resolved only inside the worker handling an
authorized ActionCall. Values are never added to prompts, definitions,
ChangeSets, SQLite, activity, logs, capability documents, or the TUI.

Start from the checked-in safe template:

```bash
cp context.example.yaml context.yaml
export OPENAI_API_KEY='...'
./openseal daemon \
  --config ./local.yaml \
  --context ./context.yaml \
  --standalone-operator
```

`context.yaml` is ignored by Git. It contains source names, not values:

```yaml
apiVersion: openseal.dev/v1alpha1
kind: StandaloneContext

authoring:
  baseURL: https://api.openai.com/v1
  model: gpt-5.4
  credential: {kind: api-key, id: authoring-model}

credentials:
  - scope: {kind: local, id: default}
    kind: api-key
    id: authoring-model
    displayName: Authoring model
    bindingKeys: [MODEL_PROVIDER]
    env: OPENAI_API_KEY
```

Every credential requires an explicit scope, kind, ID, operator-facing display
name, and exactly one source:

- `env` reads one environment variable when the credential is needed.
- `file` reads one file relative to `context.yaml`. It must be a regular,
  non-symlink file with mode `0600` or stricter.

Inline values and unknown YAML fields are rejected. References are
scope-isolated, duplicate identities are rejected, empty sources fail at use,
and file permissions are checked again at resolution time. The capability API
and Composer receive only display names, binding keys, and opaque references.

The local context is deliberately not a multi-user secret service: it has no
remote API, secret replication, rotation workflow, or identity-aware ACLs. Use
an embedding host with Vault/KMS-backed credential leases for networked or
multi-user deployments. The portable `runtime.CredentialResolver` boundary is
the same in both modes, so moving from local context to an embedding host does
not change Agent, Team, Skill, or Run data.
