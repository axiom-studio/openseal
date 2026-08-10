# Operations

This guide covers configuration, persistence, recovery, secrets, and test
commands for standalone and embedded OpenSeal deployments.

## Standalone daemon configuration

The daemon reads YAML. Missing files are created from defaults.

```yaml
logLevel: info
storage:
  driver: sqlite
  path: data/openseal.db
  artifactsPath: data/artifacts
api:
  listenAddr: :8080
```

Only `sqlite` is accepted by the standalone configuration. Relative database,
artifact, and Skill paths are resolved from the configuration file's directory.
`OPENSEAL_SKILLS_DIR` overrides the daemon's default `skills` directory.

The daemon starts one HTTP listener. `api.listenAddr` serves the versioned
kernel API; use `/api/v1/health` for health checks. Durable schedules and
external events are configured through Objective-owned Runbook triggers and
event-source subscriptions. Removed `workflowsDir`, `triggers`, and `webhook` daemon keys are
rejected instead of being silently ignored.

## Standalone context, local Vault, and model-backed authoring

`openseal daemon --context ./context.yaml` loads the optional standalone
context. It is the local counterpart to a host-managed Vault and model grants:
the file contains provider routing plus opaque `kind/id` references, while each
reference resolves from an environment variable or an owner-only file. Inline
secret values and unknown fields are rejected. File sources must be regular and
mode `0600` or stricter.

The context may configure one OpenAI-compatible authoring provider. Its API key
is resolved through the same opaque reference boundary used by governed Skill
actions. The daemon advertises only secret-free credential choices to Composer.
Values never enter Skills, prompts, ChangeSets, SQLite, activity, logs, or API
responses. See [Standalone context and local Vault](standalone-context.md) and
`context.example.yaml`.

The legacy `OPENSEAL_LLM_BASE_URL`, `OPENSEAL_LLM_MODEL`, and `OPENAI_API_KEY`
environment trio remains supported when no `authoring` context is present, but
the context file is the documented standalone configuration path. Embedded and
multi-user hosts should use Vault/KMS-backed credential resolvers or signed
credential leases instead of the local context.

## Persistence and recovery

Standalone mode uses one SQLite store for definitions, deployments, objectives,
Runs, turns, leases, budgets, actions, approvals, collaboration, conversations,
activity, artifacts, Skills, ChangeSets, Projects, source checkpoints, and
outreach. Artifact bytes are stored separately beneath `artifactsPath` and tied
to immutable catalog records by size and digest.

```mermaid
flowchart TB
    Client[TUI or API client] --> API[Versioned kernel API]
    API --> Engine[Engine services]
    Engine --> Queue[Durable Runs and actions]
    Worker[Leased workers] --> Queue
    Worker --> Checkpoint[Turn and action checkpoints]
    Queue --> DB[(SQLite or PostgreSQL)]
    Checkpoint --> DB
    Engine --> Content[(Artifact content store)]
    Worker --> Adapter[Model / Skill transport adapters]
```

For a graceful stop, send `SIGINT` or `SIGTERM`. OpenSeal stops workers and the
kernel API server. On restart it restores persistent registries and installed
ClawHub Skills. An expired Run or action lease is reclaimable from its last
durable checkpoint; idempotency keys protect repeated commands and dispatch
proposals.

Deterministic HCL runbooks remain available as explicit, process-bounded
operations:

```bash
openseal validate ./runbook.hcl
openseal run ./runbook.hcl
```

They are not loaded or scheduled by the daemon and do not create durable Agent
Runs.

Back up the SQLite database and artifact content together. Do not copy a live
database file without using a SQLite-safe snapshot procedure.

### Temporary execution transport

`pkg/store` is a separate, deliberately non-durable transport for bounded
execution inputs such as a context file handed to a sandbox. It uses opaque
cryptographic identifiers, private filesystem permissions, maximum-size and
expiry enforcement, run association, and synchronous cleanup. It never treats
an identifier as a filesystem path and does not restore its index after a
process restart. Hosts choose the root, size, and TTL and call `Cleanup` from
their own lifecycle. Anything a user must inspect, retain, cite, or download
after recovery belongs in the Artifact content store instead.

## Embedded PostgreSQL

The public facade includes a PostgreSQL store for shared deployments:

```go
store, err := openseal.NewPostgresStore(
    ctx,
    dsn,
    openseal.WithPostgresSchema("openseal"),
    openseal.WithPostgresPool(openseal.DefaultPostgresPoolConfig()),
)
```

Opening the store applies versioned migrations under an advisory migration
lock. Pool and migration-lock options are available through the facade. The
embedding host owns database credentials, backup, high availability, and
network policy.

## Capability and security boundaries

OpenSeal owns portable models, validation, durable orchestration, action and
approval state, exact Skill bindings, source provenance, and local stores. An
embedding host can supply:

- identity, scope, and authorization
- policy and eligible approval principals
- opaque credential selection and worker-time secret resolution
- model and Skill transports
- sandbox or remote execution
- artifact content stores and authorized resolvers
- source-policy persistence and network access
- event publication and external event sources

The host must advertise only operations it has actually wired. Secrets must be
resolved after authorization and schema validation. Do not log raw provider
responses when they can contain sensitive content.

## Source access and outreach

Source policies are credential-free, scope-bound network authority used by
configured source and outreach workers. Daemon YAML can seed standalone
configuration; the source-policy lifecycle API and persistent store are the
canonical runtime control surface. A policy version must specify
an ID, version, enabled state, one or more HTTPS hosts, a bounded maximum item
count, and explicit normalized path/method restrictions before lifecycle
registration. Outreach additionally requires an
explicit approval policy and maximum body size.

Policy validation does not perform network I/O. Dispatch adapters must enforce
the exact decision on every request and redirect. Source policy configuration
does not itself provide API credentials or an HTTP provider implementation.
Registration is immutable and grants no authority. Activation and revocation
use compare-and-swap lifecycle revisions and append durable actor/reason audit
events. Runtime resolution always checks the currently active exact version;
revocation and version drift fail closed without a process restart.

## Deterministic quality gates

```bash
make test
make vet
```

The default suite includes a three-Agent Snakes and Ladders acceptance test. It
exercises independent objective portfolios, a Team, child Runs, deterministic
actions, restart recovery, collaboration, a winner message, and a verified game
log artifact without external network calls.

The default suite also includes a three-Agent market-research Project. It
joins recurring source monitoring, deduplicated provenance-linked evidence,
restart recovery, cited PDF generation, approval-gated email delivery, and a
durable provider receipt into one portable end-to-end journey. Source and
delivery transports are deterministic governed fixtures in this test; host
credentials are resolved ephemerally and the resolved value is verified absent
from persisted kernel state.

Run either acceptance scenario directly while developing:

```bash
go test ./pkg/openseal \
  -run 'Test(ThreeAgentSnakesAndLadders|MarketResearchProject)' \
  -count=1
```

Run race-sensitive kernel packages explicitly when changing concurrency:

```bash
go test -race ./pkg/runtime ./pkg/openseal
```

## Opt-in live tests

Live tests make real network or model calls and are excluded from the default
suite by the `integration` build tag. Configure credentials in the environment,
never in source files:

```bash
export AUTHORING_TEST_BASE_URL=https://provider.example/v1/chat/completions
export AUTHORING_TEST_API_KEY='replace-with-a-real-secret'
export AUTHORING_TEST_MODEL='provider-model-name'

go test -tags=integration ./pkg/authoring -run TestLiveGenerator
```

The real ClawHub/model acceptance additionally requires an explicit opt-in:

```bash
export OPENSEAL_RUN_LIVE_E2E=1
# Optional; defaults to @nubzparmesan/minnow-writing
export OPENSEAL_E2E_CLAWHUB_SKILL='@owner/skill'

go test -tags=integration ./pkg/openseal \
  -run TestLiveClawHubSkillModelE2E
```

These tests verify that transport credentials are not persisted or exposed.
They should run against a controlled provider account with bounded spend.
