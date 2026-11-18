# Team coordination

OpenSeal coordinates Team work through the same durable primitives used by an
individual Agent. There is no second Team executor. A Team request, its child
Run, channel projection, activity, budget, and artifacts all refer
to one collaboration fact.

```mermaid
flowchart LR
    S[Source Run] --> O[Durable work offer]
    O --> C[Eligible roster candidates]
    C --> B[Accept or decline bids]
    B --> D[Deterministic assignment]
    D --> R[Child Run]
    R --> J[Join source Run]
```

## Work offers and assignment

When the recipient is a Team, request creation snapshots every eligible roster
candidate. Eligibility comes from the requested member (a semantic role names
one roster member by id; without one every member is a candidate), the member's
Skill requirements, active deployment state, and current Run capacity. The snapshot contains no credentials.

Eligible Agents submit an explicit `accept` or `decline` bid with an idempotency
key. Assignment is deterministic: accepted candidates with available capacity
are ordered by available slots, then active Run count, then stable Agent
deployment identity. The selected Agent and the reason are persisted before the
child Run is created. A retry or another replica therefore cannot silently pick
a different worker.

The public Go facade exposes `SubmitAgentRequestBid`; request inspection returns
the candidate evidence, bids, and final assignment decision.

## Completion

A member's finished work completes the request directly and wakes the source
Run. Nobody reviews an Agent's answer before it is delivered.

## Channel behavior

Conversation projection reports offers, decisions, child work, and
completion without making chat the executor. Existing Team coordination policy
limits speakers, requires role relevance, suppresses duplicate content, and can
keep observers quiet by default. The durable request and Runs remain the source
of truth even when no participant needs to speak.

## Safety and recovery invariants

- Team skill grants and Agent authority are both narrowing boundaries.
- Shared request context and artifacts cannot contain credentials.
- Budgets are allocated to the child Run and cannot exceed the source Run.
- Request, source Run, child Run, dependency resolution, and activity changes
  use transactional compare-and-swap storage operations.
- Idempotency suppresses duplicate requests, bids, decisions, and child Runs.
- Every decision retains the exact Team policy snapshot that governed it.

