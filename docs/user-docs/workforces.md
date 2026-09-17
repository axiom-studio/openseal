# Workforces

Workforce authoring turns a prompt describing a desired outcome into a reviewed proposal for Agents, Teams, roles, Skills, and boundaries. Workforce bundles move an existing workforce between deployments as a portable, signed document. Neither creates anything until it is explicitly applied.

## Authoring a Workforce From a Prompt

Authoring is a chain of durable records, each with its own endpoint. Nothing is created until the final step.

```text
compile → change set → refine → evaluate → approve → prepare activation → apply
```

| Step | Description |
|---|---|
| Compile | A prompt is compiled into a non-activating proposal; no resources are created |
| Create a change set | The proposal becomes a durable change set that survives restarts |
| Refine | Open questions are answered against the change set, and generation can be retried |
| Evaluate | The change set is evaluated against policy |
| Approve | A decision is recorded |
| Prepare activation | The change set is staged for application |
| Apply | Resources are created in one transaction |

The separation between compile and apply is the point of the design. A compiled proposal is inspectable and refinable without having created a single Agent, and applying it is one transaction rather than a partial rollout that has to be cleaned up by hand.

A change set also carries a placement, which is patched separately, and open questions raised during generation, which are answered through refinements.

## Configuring Authoring

Workforce authoring appears in the capability document only when a complete model configuration is present. The daemon resolves an endpoint, a credential, and a model name from either the standalone context file or three environment variables.

| Field | Value |
|---|---|
| `OPENSEAL_LLM_BASE_URL` | Model endpoint, OpenAI-compatible |
| `OPENAI_API_KEY` | API key |
| `OPENSEAL_LLM_MODEL` | Model name |

> **All three or none.** The daemon fails startup when some but not all three values are present, rather than starting with authoring half-configured. Setting one of the three and expecting a default for the rest does not work.

When the standalone context file defines an `authoring` block, it takes precedence over all three environment variables, and its credential is resolved through the context's opaque reference mechanism rather than read directly from the environment. See [Configuration](configuration.md).

With a complete configuration the daemon also starts a durable authoring worker bound to a single scope. Change-set operations are advertised only when both the durable store and that worker are present.

> **The governed half of authoring requires a lifecycle authorizer.** Evaluate, approve, and apply need one, and standalone mode supplies it only under `--standalone-operator`. Without the flag the daemon logs that generation retry is disabled and installs no authorizer, so compile and inspection work while the governed steps do not. The flag itself refuses to start unless the API listen address is a loopback address.

## Workforce Bundles

A workforce bundle is a portable YAML document describing a complete set of Agents, Teams, and bindings. Bundles move a workforce between deployments without re-authoring it.

Five operations inspect a bundle without changing anything, and one installs it.

| Operation | Description |
|---|---|
| Validate | Verify structure and signatures, returning the digest and valid signature keys |
| Inspect | Report the bundle's contents |
| Compare | Diff two bundles |
| Installation preview | Show what installing into a given placement would do |
| Upgrade plan | Compute the change from a current bundle to a target bundle |
| Install | Apply the bundle in one transaction |

### Working With Bundles Offline

The five read operations are available without a running daemon. `openseal bundle` reads the files directly and prints indented JSON.

```bash
openseal bundle validate ./workforce.yaml
openseal bundle inspect ./workforce.yaml
openseal bundle diff ./current.yaml ./target.yaml
openseal bundle plan-upgrade ./current.yaml ./target.yaml
```

`validate` verifies against an empty trust policy and reports the digest along with any valid signature keys. Verifying against a real trust policy is a function of the installing host, not of the offline command.

### Installation Requires a Host

Installation is advertised only when the host has supplied both an installation store and an actor identifier. The actor is host-authenticated configuration and is never taken from client input, so an installing client cannot choose whose authority the installation records.

> **The bundled daemon does not wire bundle installation.** The workforce bundle capability always reports installation as unavailable, and `POST /api/v1/workforce-bundles/install` always returns `501 Not Implemented`. The five read operations work in every deployment; applying a bundle requires an embedding host that supplies the installation store and actor.

Bundles appear in the terminal client's **Imports** destination, which offers the inspection operations against a local YAML path.

## Choosing Between Authoring and Bundles

| Situation | What to do |
|---|---|
| A workforce does not exist yet | Author it from a prompt, then apply the change set |
| A workforce exists in another deployment | Export it as a bundle and install it through a host |
| A bundle needs checking before it is trusted | Use `openseal bundle validate` and `inspect` offline |
| Two workforces need comparing | Use `openseal bundle diff`, or the compare operation over the API |
| An existing workforce needs updating from a newer bundle | Compute an upgrade plan first, then apply it through a host |

## Next Steps

- Declare the model endpoint and credential in [Configuration](configuration.md).
- Understand what the operator flag grants in [Security](security.md).
- Grant the resulting Agents and Teams their capabilities in [Skills and Approvals](skills-and-approvals.md).
