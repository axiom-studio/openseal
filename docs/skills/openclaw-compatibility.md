# OpenClaw skill compatibility

OpenSeal treats an OpenClaw/AgentSkills directory as a portable source format,
not as a separate execution runtime. The importer preserves the source artifact
and compiles behavior into native prompt, requirement, installer, resource,
transport, credential, policy, and provenance contracts.

The upstream references for this boundary are the current
[OpenClaw skills specification](https://docs.openclaw.ai/tools/skills) and
[OpenClaw skills CLI](https://docs.openclaw.ai/cli/skills). Registry operations
follow the versioned ClawHub API and its `clawhub.skill.verify.v1` verification
envelope.

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
| ClawHub search, explore, detail, versions, files, verify, archive | typed registry client | Native |
| ClawHub install, update, update-all, pin, verify, uninstall | verified atomic installer and lockfile | Native with local-modification and rollback protection |
| registry slug differing from declared skill name | source reference plus declared definition identity | Supported; registry identity remains provenance |
| local or Git acquisition | compiler bundle input | Acquisition adapter boundary; compilation semantics are identical |
| plugins that contribute skills | ordinary discovered skill directories | The skill is compatible; non-skill plugin capabilities remain plugin concerns |

## Security invariants

Third-party artifacts are untrusted. Registry installs require a passing
verification result unless an embedding application makes an explicit unsafe
choice. Archive extraction rejects traversal, symlinks, case-colliding paths,
oversized files, and oversized archives. Activation and action execution remain
separate: a verified install does not grant a model permission to invoke a
skill, resolve a credential, or perform a side effect.

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
