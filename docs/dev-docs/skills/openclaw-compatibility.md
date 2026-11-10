# OpenClaw skill compatibility

OpenSeal treats an OpenClaw/AgentSkills directory as a portable source format,
not as a separate execution runtime. The importer preserves the source artifact
and compiles behavior into native prompt, requirement, installer, resource,
transport, credential, policy, and provenance contracts.

```mermaid
flowchart LR
    Source[SKILL.md and source files] --> Hash[Load and hash]
    Hash --> Compile[Compile portable semantics]
    Compile --> Definition[Source-qualified Skill definition]
    Compile --> Diagnostics[Actionable diagnostics]
    Definition --> Binding[Agent or Team Skill binding]
    Binding --> Activate[Host eligibility snapshot]
    Activate --> Execute[Governed native action]
```

The upstream references for this boundary are the current
[OpenClaw skills specification](https://docs.openclaw.ai/tools/skills) and
[OpenClaw skills CLI](https://docs.openclaw.ai/cli/skills).

## Use from an embedding host

OpenSeal does not fetch Skills from a registry. A host supplies source
directories through the secure source catalog or passes a bundle to the public
Go facade, which exposes compilation (`CompileOpenClawSkill`), byte-exact export
(`ExportOpenClawSkill`), and source artifact import/export. A compiled Skill
does not authorize an Agent: create or edit an exact binding for the selected
deployment before its prompt or actions can activate.

## Compatibility guarantees

| Upstream semantic | OpenSeal representation | Guarantee |
| --- | --- | --- |
| `SKILL.md` identity and instructions | `Definition`, `PromptModule` | Native; invalid identity fails closed |
| Complete source directory | retained compiler artifact plus digested `Resource` index | Byte-exact export with mutation and digest checks |
| `license`, `homepage`, source version | `SourceProvenance` | Preserved |
| `user-invocable`, `disable-model-invocation` | `PromptModule` invocation policy | Native |
| `allowed-tools` | prompt tool allowlist and compiled action permissions | Native policy input |
| `command-dispatch: tool` | typed action plus `tool` transport | Native deterministic dispatch |
| `command-arg-mode: raw` | transport argument projection | Produces the upstream `command`, `commandName`, and `skillName` envelope |
| Unknown dispatch or argument modes | compiler error | Rejected explicitly; never silently degraded |
| `metadata.openclaw.always` | always-active prompt flag | Native |
| `skillKey` | definition configuration key | Preserved for binding/config adapters |
| `emoji` | definition icon | Preserved for presentation adapters |
| `primaryEnv` | credential requirement on deterministic actions | Secret value stays outside definitions, prompts, arguments, and activity |
| `requires.bins`, `anyBins`, `env`, `config`, `os` | typed requirements | Native eligibility input |
| legacy `metadata.clawdbot` | same typed metadata parser | Supported compatibility read |
| brew, node, go, uv, and download installer fields | typed installer candidates | Preserved; execution is an explicit host policy boundary |
| `references/`, `scripts/`, `assets/`, and other files | typed, media-aware, digested resources | Native progressive-disclosure inventory |
| `{baseDir}` | trusted per-skill activation resource root | Materialized only in the immutable deployment snapshot; missing/relative roots make the skill unavailable |
| per-agent allowlists and enablement | scoped `SkillBinding` | Native and revisioned |
| `skills.entries.*.config` | binding configuration | Native; configuration is scoped to the deployment |
| `skills.entries.*.env` and `apiKey` | opaque credential references and worker-time resolution | Native security boundary; raw values are never model-visible |
| session skill snapshots | deterministic activation snapshot ID | Stable across restart for equivalent bindings/host capabilities; host revision changes force refresh identity |
| workspace/project/personal/managed/bundled/plugin/extra roots | secure source catalog | Declared-name conflicts use documented precedence; one grouping level is supported |
| directory refresh | debounced effective-source watcher | Emits only when the winning skill surface changes; shadowed edits stay quiet |
| local or Git acquisition | compiler bundle input | Acquisition adapter boundary; compilation semantics are identical |
| plugins that contribute skills | ordinary discovered skill directories | The skill is compatible; non-skill plugin capabilities remain plugin concerns |

## Security invariants

Third-party artifacts are untrusted. Source loading rejects traversal, unsafe
symlinks, and oversized files. Activation and action execution remain separate:
a compiled Skill does not grant a model permission to invoke it, resolve a
credential, or perform a side effect.

The canonical tool dispatcher materializes only declared input mappings and
compiler literals. Credential values travel through a separate worker-only
channel. Persisted definitions, bindings, action proposals, approvals, and
activity contain requirement names or opaque credential references, never
resolved values.

## Host extension boundaries

OpenSeal owns secure multi-root precedence and effective-source refresh, while
the host chooses which concrete roots exist and which symlink targets it
trusts. Sandbox provisioning, installer execution, remote-node probing, Git
acquisition, and plugin discovery still depend on the host environment.
OpenSeal exposes typed contracts for these concerns without pretending that a
particular host implementation exists. An embedding application must advertise
and test each adapter it enables.

## Compiler result

A successful compilation contains a canonical definition, source provenance,
an immutable resource index, preserved installer candidates, and a round-trip
source artifact. Version identity incorporates source, origin, verification
trust, and process semantics so different sources cannot
silently collapse into one executable definition.

The compiler lowers deterministic constructs where their semantics are exact:

- Skill instructions become a native prompt module with invocation policy.
- Tool dispatch becomes a typed tool action and argument projection.
- Safe OpenAPI-described read operations can become typed HTTP actions.
- Declared command processes become argument-safe process contracts.
- Requirements, configuration, credentials, resources, and installers remain
  typed inputs to activation and host policy.

If safe native execution requires a host adapter that is not present, the
compiled action remains unavailable with an explicit diagnostic. Unknown
dispatch modes, unsafe paths, ambiguous source mutations, digest mismatches,
and semantics that cannot be preserved fail closed.
