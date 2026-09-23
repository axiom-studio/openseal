# Security Boundaries

OpenSeal is designed for one of two deployments: a single-operator local process reachable only over loopback, or a kernel mounted inside a host that supplies identity, authorization, policy evaluation, and audit. This page describes where the boundary between kernel and deployment falls.

## The Deployment Model

Identity and authorization belong to the deployment rather than to the kernel. OpenSeal implements neither, and binds to loopback by default for that reason.

| Property | Where it lives |
|---|---|
| Authentication | Off unless you set `OPENSEAL_API_TOKEN`. With it set the daemon requires a bearer token on every route; with it unset no credential is parsed and no route returns `401`. See [Enabling API Authentication](#enabling-api-authentication) below |
| Transport security | The embedding host, or a fronting proxy. The API server is plain HTTP — there is no TLS configuration, no certificate handling, and no TLS listener |
| Authorization | The embedding host. There is no role model, no permission check, and no per-scope entitlement check on any route |

> **Plan the deployment around that boundary.** With no token set, any process that can reach the listen address can call every route, including every mutation route. The default loopback bind is what keeps the boundary closed until a deployment deliberately opens it.

Note what authentication does and does not buy you here: a token establishes *that* a caller is authorized to use the API at all. It does not distinguish between callers, and it does not restrict which routes a caller may reach. Authorization remains the embedding host's job.

## Enabling API Authentication

Set `OPENSEAL_API_TOKEN` in the daemon's environment:

```bash
OPENSEAL_API_TOKEN="$(openssl rand -hex 32)" openseal daemon --config daemon.yaml
```

Every route then requires a bearer token, with one exception covered below:

```bash
curl -H "Authorization: Bearer $OPENSEAL_API_TOKEN" http://127.0.0.1:8080/api/v1/capabilities
```

| Request | Response |
|---|---|
| No `Authorization` header | `401`, with `WWW-Authenticate: Bearer realm="openseal"` |
| Wrong token | `401` |
| Correct token | the route's normal response |

This applies on every path — the daemon, the container and the desktop sidecar — not only to the desktop app. The comparison is constant-time, and a request carrying more than one `Authorization` header is rejected.

To confirm the token is actually in force, check that a request **without** it is refused:

```bash
curl -si http://127.0.0.1:8080/api/v1/capabilities | head -1
# HTTP/1.1 401 Unauthorized
```

Use a route other than `/api/v1/health` for that check — see below.

### The Health Route Is Exempt

`GET /api/v1/health` answers without a credential, whether or not a token is set:

```bash
curl -s http://127.0.0.1:8080/api/v1/health
# {"status":"ok"}
```

A container liveness probe cannot carry a credential — Docker's `HEALTHCHECK` and Compose's `healthcheck` run a fixed command with no access to the token — so gating this route would leave every authenticated container permanently `unhealthy`. The exemption is exact: `GET`, that path only. `POST` to the same path, and every other route, still requires the token.

The route returns `{"status":"ok"}` and nothing else — no configuration, no identifiers, no store contents — so it discloses nothing that connecting to the port does not already reveal.

The practical consequence is the one worth remembering: **a `curl` against `/api/v1/health` cannot tell you whether your token is armed**, because it returns `200` either way.

**Leaving it unset leaves every route open.** That is the default, and it is deliberate: it keeps existing deployments working. It is also why the loopback bind matters. If you publish the API beyond loopback, set a token, put an authenticating proxy in front, or both.

The desktop application sets this for you. It generates a token per launch and passes it to the sidecar it spawns, so the bundled daemon is never reachable without it.

## The Two Guardrails

The daemon compensates with two mechanisms, neither of which is a substitute for an authorization layer.

### Loopback by Default, With a Warning

The API binds `127.0.0.1:8080` by default. When the configured listen address is not a loopback address, the daemon emits a startup warning naming the risk:

```text
OpenSeal API is exposed on non-loopback interface
OPENSEAL_API_TOKEN is not set, so every route is reachable without a credential.
Set it, or expose this only through an authorization-aware reverse proxy or local network boundary.
```

With a token set, the same situation reports the narrower risk instead — that a token authenticates but does not authorize:

```text
OpenSeal API is exposed on non-loopback interface
OPENSEAL_API_TOKEN is set, so routes require a bearer token. There is still no
authorization model: any holder of the token can call every route.
```

The daemon starts anyway. The warning is a warning, not a refusal.

#### Running in a Container

A container changes where that boundary sits, and the shipped files use three different addresses on purpose.

| Where | Address | Why |
|---|---|---|
| `docker/daemon.yaml` | `0.0.0.0:8080` | A published port arrives on the container's own interface, never its loopback. Bound to `127.0.0.1` the daemon is unreachable from outside the container and the published port forwards to nothing. |
| `docker-compose.yml` | `127.0.0.1:8080:8080` | The host side. This is the line that decides network exposure. |
| Built-in default | `127.0.0.1:8080` | Unchanged, for the daemon run directly on a host. |

The container binding every interface is safe only because the compose file publishes to loopback. Widening the publish spec to `8080:8080` binds the host side to every interface and puts all API routes, including the approval decision endpoint, in reach of any peer that can route to the host.

> **A host firewall will not contain this.** Docker installs its port forward as a DNAT rule traversed *before* the host's `INPUT` filter chain, so a `ufw default deny incoming` policy does not block a published port. An operator who believes the host is firewalled would be wrong.

Widen the publish spec only together with an authenticating reverse proxy in front of it. Do not instead change `docker/daemon.yaml` back to loopback: that does not reduce exposure, it only stops the container working.

### The Operator Flag

`--standalone-operator` refuses to start unless the listen address is loopback, and it is what turns on the local-operator authorities.

| Property | Description |
|---|---|
| Action policy | Installs a policy naming a single approver, `user:local` |
| Approval authorizer | Enables resolving action approvals |
| ClawHub mutation authority | Enables install, update, pin, unpin, and uninstall |
| Workforce lifecycle authority | Enables evaluate, approve, and apply, acting as `local-operator` |
| Outreach delivery dispatcher | Enables message delivery, for outreach-enabled scopes only |

Running without the flag leaves those mutations unavailable rather than silently self-approving. That is the intended failure mode: absence of an authority produces a `501`, never an assumption.

> **Standalone mode approves as a single local principal.** Every approval recorded in a standalone deployment carries the identity `user:local`, and the workforce authorizer acts as `local-operator`, regardless of who actually made the decision. Approval records are auditable, but they do not attribute. A deployment that needs to know which person approved something needs an embedding host that authenticates people.

## Scopes Partition, Hosts Isolate

Every durable record is scoped, and a scope is the only tenancy primitive in the kernel. It is a partition key.

Scope validation accepts any non-empty `kind` and `id` pair. There is no registry of known scopes, no hierarchy, and no membership check. The kernel uses the scope to partition storage and to route work to the correct worker; deciding whether a caller is entitled to a given scope sits on the deployment side of the boundary.

> **Tenant isolation is the host's half of the contract.** Since the kernel does not authenticate, any caller reaching it can supply any `scopeKind` and `scopeId` and be served the records in that partition. An embedding host provides isolation by authenticating the caller and constraining which scope values that caller may present — partitioning alone is not isolation.

## Secret Handling

Secret values are deliberately confined to the point of use.

| Property | Description |
|---|---|
| Configuration | The daemon configuration file holds no secrets |
| Context file | Holds references to secret sources, never values |
| Resolution | A worker resolves the value at execution time, for the exact action call carrying the reference |
| Durable state | Secret values are never persisted |
| Capability responses | Secret values never appear |
| Prompts | Secret values never enter model prompts |
| Terminal client | Secret values never reach the client |

What crosses into the authoring and proposal layers is the opaque reference and its display label. File-backed credentials are additionally checked for symlinks, non-regular files, permissions looser than `0600`, and empty content — all failing closed. See [Configuration](configuration.md).

## Placing an Authorizing Proxy in Front

The one integration point for external authorization is header pass-through. The terminal client's repeatable `--header name=value` flag adds arbitrary headers to every kernel request, which lets a deployment put an authenticating reverse proxy in front of the daemon and satisfy it from the client.

```bash
openseal --endpoint https://openseal.internal --header 'X-Forwarded-User=alice'
```

Two headers are reserved by the kernel protocol and cannot be overridden: `Content-Type` and `Idempotency-Key`. The client rejects them at flag-parse time with a message naming them.

Header names and values containing whitespace, colons, or line breaks are also rejected, which prevents header injection through the flag.

> **The proxy is doing the authorization, not OpenSeal.** Header pass-through lets a client satisfy an external authority; it does not cause the daemon to evaluate one. A daemon behind a proxy is exactly as unauthenticated as one that is not — the proxy's job is to ensure nothing reaches the daemon that should not.

## Deployment Checklist

| Situation | What to do |
|---|---|
| Local single-operator use | Keep the default loopback bind and use `--standalone-operator` |
| Anything reachable by another host | Put an authorization-aware reverse proxy in front, and terminate TLS there |
| Multi-tenant use | Embed the kernel in a host that authenticates callers and constrains scope values |
| Approvals that must identify a person | Use an embedding host; standalone approvals all record `user:local` |

## Next Steps

- Configure credential references and their checks in [Configuration](configuration.md).
- Read the deployment and persistence characteristics in [Operations](operations.md).
- Understand which routes fail closed, and how, in [API](api.md).
