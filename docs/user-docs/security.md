# Security Boundaries

OpenSeal is designed for one of two deployments: a single-operator local process reachable only over loopback, or a kernel mounted inside a host that supplies identity, authorization, policy evaluation, and audit. This page describes where the boundary between kernel and deployment falls.

## The Deployment Model

Identity and authorization belong to the deployment rather than to the kernel. OpenSeal implements neither, and binds to loopback by default for that reason.

| Property | Where it lives |
|---|---|
| Authentication | The embedding host, or a fronting proxy. No credential is parsed from any request, and no route returns `401 Unauthorized`, because no route distinguishes an authenticated caller from an unauthenticated one |
| Transport security | The embedding host, or a fronting proxy. The API server is plain HTTP — there is no TLS configuration, no certificate handling, and no TLS listener |
| Authorization | The embedding host. There is no role model, no permission check, and no per-scope entitlement check on any route |

> **Plan the deployment around that boundary.** Any process that can reach the listen address can call every route, including every mutation route. The default loopback bind is what keeps the boundary closed until a deployment deliberately opens it.

## The Two Guardrails

The daemon compensates with two mechanisms, neither of which is a substitute for an authorization layer.

### Loopback by Default, With a Warning

The API binds `127.0.0.1:8080` by default. When the configured listen address is not a loopback address, the daemon emits a startup warning naming the risk:

```text
OpenSeal API is exposed on non-loopback interface
OpenSeal has no built-in API authentication.
Expose it only through an authorization-aware reverse proxy or local network boundary.
```

The daemon starts anyway. The warning is a warning, not a refusal.

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
| Container deployment | Read the port-binding note in [Operations](operations.md) before changing the bind address |
| Approvals that must identify a person | Use an embedding host; standalone approvals all record `user:local` |

## Next Steps

- Configure credential references and their checks in [Configuration](configuration.md).
- Read the deployment and persistence characteristics in [Operations](operations.md).
- Understand which routes fail closed, and how, in [API](api.md).
