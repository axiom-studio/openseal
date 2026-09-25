# Security Boundaries

OpenSeal is designed for one of two deployments: a single-operator local process reachable only over loopback, or a kernel mounted inside a host that supplies identity, authorization, policy evaluation, and audit. This page describes where the boundary between kernel and deployment falls.

## The Deployment Model

Identity and authorization belong to the deployment rather than to the portable kernel. The plain daemon binds to loopback by default. An optional API bearer token protects all routes, and the desktop host supplies a token and local owner authority; this is not a multi-user role system.

| Property | Where it lives |
|---|---|
| Authentication | The optional `OPENSEAL_API_TOKEN` bearer token protects every daemon route when set. A host or fronting proxy supplies identity-aware authentication for shared deployments |
| Transport security | The embedding host, or a fronting proxy. The API server is plain HTTP — there is no TLS configuration, no certificate handling, and no TLS listener |
| Authorization | The desktop's explicit local-owner mode or an embedding host. The bearer token alone supplies no roles or per-scope entitlements |

> **Plan the deployment around that boundary.** Without `OPENSEAL_API_TOKEN`, any process that can reach the listen address can call available routes. With a token, callers must present it, but all token holders share the same daemon authority. Keep the API on loopback unless an authorizing host controls access.

## Local Guardrails

The daemon uses a loopback default and explicit operator modes. The optional bearer token authenticates possession of one secret, not a person or tenant.

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

The daemon starts anyway. The warning is a warning, not a refusal. Its wording
describes the absence of built-in identity-aware authorization even when a
shared bearer token is configured.

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
| Workforce authoring recovery | Enables generation retry and refinement as `local-operator`; evaluate, approve, and apply remain unavailable |
| Outreach delivery dispatcher | Enables message delivery, for outreach-enabled scopes only |

Running without the flag leaves those mutations unavailable rather than silently self-approving. That is the intended failure mode: absence of an authority produces a `501`, never an assumption.

> **Standalone mode approves actions as a single local principal.** Its action approvals carry `user:local`, and authoring retry/refinement acts as `local-operator`. These records do not identify a person. A deployment needing individual attribution requires a host that authenticates people.

The desktop's separate `--desktop-operator` mode requires a loopback listener, a nonempty `OPENSEAL_API_TOKEN`, and a local workspace scope. It provides local policy evaluation, owner review, and installation for that workspace. The native desktop host creates and retains the per-launch token.

## Scopes Partition, Hosts Isolate

Every durable record is scoped, and a scope is the only tenancy primitive in the kernel. It is a partition key.

Scope validation accepts any non-empty `kind` and `id` pair. There is no registry of known scopes, no hierarchy, and no membership check. The kernel uses the scope to partition storage and to route work to the correct worker; deciding whether a caller is entitled to a given scope sits on the deployment side of the boundary.

> **Tenant isolation is the host's half of the contract.** A caller with API access can supply scope values; the bearer token does not constrain them. An embedding host provides isolation by authenticating the caller and constraining which scope values that caller may present — partitioning alone is not isolation.

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

> **The proxy is doing the authorization.** Header pass-through lets a client satisfy an external authority; it does not make the daemon evaluate roles or tenant entitlements. The proxy must ensure unauthorized requests cannot reach the daemon.

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
