# Configuration

OpenSeal reads two files: a daemon configuration naming ports, storage, and source policies, and a standalone context holding credential references. Neither ever holds a secret value.

## The Daemon Configuration File

The daemon reads a YAML file named by `--config`, defaulting to `daemon.yaml` in the working directory. If the file does not exist it is created with defaults and startup continues.

| Field | Type | Default |
|---|---|---|
| `api.listenAddr` | `string` | `127.0.0.1:8080` |
| `logLevel` | `string` | `info` |
| `storage.driver` | `string` | `sqlite` |
| `storage.path` | `string` | `data/openseal.db` |
| `storage.artifactsPath` | `string` | `data/artifacts` |
| `sourcePolicies` | list | empty |

A minimal configuration:

```yaml
logLevel: info
api:
  listenAddr: 127.0.0.1:8080
storage:
  driver: sqlite
  path: data/openseal.db
  artifactsPath: data/artifacts
```

> **Unknown fields are rejected, not ignored.** A misspelled key fails at load rather than silently taking a default, so a configuration that starts is a configuration that was fully understood.

Validation enforces three rules. The storage driver must be exactly `sqlite`. Both storage paths must be non-empty. Every source policy must carry a valid scope and a valid policy, and no scope may list the same policy identifier and version twice.

Relative storage paths resolve against the directory holding the configuration file, not against the shell's working directory.

> **`logLevel` is accepted but has no effect.** The value is read, defaulted, validated, and written into the startup log line, but it is never applied to the logger. The daemon always emits at info level, so setting `logLevel: debug` changes nothing about what appears in the log.

## Source Policies

Source policies are credential-free, scope-bound network authorities. A policy names the sources an Agent may reach and, optionally, enables outreach for its scope.

```yaml
sourcePolicies:
  - scope:
      kind: local
      id: default
    policy:
      id: research-sources
      version: "1.0.0"
      enabled: true
```

No source access and no outreach delivery is available for a scope that has no listed policy, which makes the empty default a closed one rather than an open one.

The static `sourcePolicies` list in this file is distinct from the durable source-policy lifecycle exposed under `/api/v1/source-policies`. The lifecycle routes require a service the standalone daemon does not install — see [API](api.md).

## The Standalone Context

The standalone context file is the local counterpart to an embedding host's secret manager. It holds references to secret sources, never secret values. It is read from the path named by `--context`, defaulting to `context.yaml`, and a missing file is not an error — it means no local credential source is configured.

```yaml
apiVersion: openseal.dev/v1alpha1
kind: StandaloneContext

authoring:
  baseURL: https://api.openai.com/v1
  model: gpt-5.4
  credential:
    kind: api-key
    id: authoring-model

credentials:
  - scope:
      kind: local
      id: default
    kind: api-key
    id: authoring-model
    displayName: Authoring model
    bindingKeys: [MODEL_PROVIDER]
    env: OPENAI_API_KEY
```

| Field | Type | Description |
|---|---|---|
| `apiVersion` | `string` | Must be `openseal.dev/v1alpha1` |
| `kind` | `string` | Must be `StandaloneContext` |
| `authoring.baseURL` | `string` | OpenAI-compatible model endpoint |
| `authoring.model` | `string` | Model name |
| `authoring.credential` | reference | Opaque `kind` and `id` pair |
| `credentials[].scope` | `kind:id` | Scope the credential belongs to |
| `credentials[].kind` | `string` | Credential kind, part of its reference |
| `credentials[].id` | `string` | Credential identifier, part of its reference |
| `credentials[].displayName` | `string` | Human-readable label, required |
| `credentials[].bindingKeys` | list | Binding keys the credential can satisfy |
| `credentials[].env` | `string` | Environment variable holding the value |
| `credentials[].file` | `string` | File holding the value |

Each credential entry must configure exactly one of `env` or `file`; setting both or neither is a load error. Relative file paths resolve against the directory containing the context file. Unknown fields are rejected, and duplicate `kind`/`id` pairs within a scope are rejected.

When an `authoring` block is present it takes precedence over the `OPENSEAL_LLM_BASE_URL`, `OPENSEAL_LLM_MODEL`, and `OPENAI_API_KEY` environment variables, and its credential is resolved through the opaque reference mechanism rather than read directly from the environment.

### File-Backed Credential Checks

File-backed credentials are checked at resolution time, and every check fails closed.

| Property | Description |
|---|---|
| No symbolic links | The path must not be a symlink |
| Regular files only | The path must be a regular file |
| Owner-only permissions | The mode must be `0600` or stricter; any group or other permission bit is refused |
| Non-empty content | A file that reads as empty after trimming is refused |

Environment-backed credentials fail if the named variable is empty.

Only the opaque reference and the display label cross into the authoring layer, so a proposal can select a credential without the value being exposed. The value is resolved later, by the worker, for the exact action call that carries the reference.

## Credentials Decide Which Workers Exist

The context file has a second effect that is easy to miss. Every scope appearing on a credential entry contributes a worker scope, and those are merged with the scopes contributed by outreach-enabled source policies.

```text
credential scopes + outreach-enabled policy scopes → worker scopes → Run and action workers
```

> **A daemon with no credentials and no outreach-enabled policies derives no worker scopes, and therefore starts no Run or action workers.** Run creation is not advertised in that state. If Runs cannot be created on an otherwise healthy daemon, this is the first thing to check. See [Objectives and Runs](objectives-and-runs.md).

## Environment Variables

| Field | Value |
|---|---|
| `OPENSEAL_SKILLS_DIR` | Directory holding installed Skills; defaults to `skills/` beside the configuration file |
| `OPENSEAL_LLM_BASE_URL` | Workforce authoring model endpoint |
| `OPENSEAL_LLM_MODEL` | Workforce authoring model name |
| `OPENAI_API_KEY` | Workforce authoring credential |
| `CLAWHUB_REGISTRY` | ClawHub registry base URL; defaults to `https://clawhub.ai` |
| `OPENSEAL_API_URL` | Default endpoint for the terminal client |

## Daemon Flags

| Field | Value | Default |
|---|---|---|
| `--config` | Path to the daemon configuration file | `daemon.yaml` |
| `--context` | Path to the standalone context file | `context.yaml` |
| `--scope` | Durable standalone authoring scope as `kind:id` | `local:default` |
| `--standalone-operator` | Grant local-operator authorities; requires a loopback listen address | off |
| `--help` | Print help | — |

## Next Steps

- Understand what `--standalone-operator` grants, and what it does not, in [Security](security.md).
- Deploy the configured daemon using [Operations](operations.md).
- Configure the model endpoint authoring needs in [Workforces](workforces.md).
