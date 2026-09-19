# Workforce identities

New authoring-created Agent and Team definitions and their default deployments use independently generated, cryptographically random 21-character NanoIDs. Display names do not determine identity. The model submits semantic form keys; the trusted Change Set service allocates resource IDs after compilation and rewrites the complete candidate reference graph before calculating its reviewed digest.

`authoringKey` is a stable form reference on a definition, separate from `id` and `displayName`. Projection and refinement use that key to preserve the same resource when its behavior or display name changes. New roots always allocate new IDs even when the model selects the same role key. Existing-resource amendments retain their identities. Placement edits inherit omitted deployment identities, so replaying the same edit does not allocate another deployment.

The candidate, semantic keys, and deployment placement are persisted together in the existing Change Set transaction. Idempotency replay returns that snapshot; a failed generation that never committed has no published identity. References in assignments, Team role requirements, conversation owners and handlers, Runbook delegation, and project objective/source references are rewritten as one graph.

This is a compatible transition for new resources, not a destructive rewrite of historical identities. Previously published definitions, deployment IDs, audit records, and links remain valid. Legacy semantic projection remains supported. A bulk historical rekey requires a separately governed migration of all immutable digests and cross-resource references; changing stored primary keys alone is not supported.

IDs do not grant authority. Tenant ownership remains on scoped deployments and repository filters; hosts must resolve opaque definition access through a tenant-owned deployment rather than infer ownership from an ID prefix. Imports and explicitly supplied existing deployment references retain their reviewed identities.
