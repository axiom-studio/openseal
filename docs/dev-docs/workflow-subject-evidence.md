# Conversation subjects for durable workflows

A conversation workflow matches the provider's exact conversation identity.
A recipient or participant identity does not establish that conversation.
Connectors must return the canonical conversation ID from a successful provider
receipt, including when the send argument names a person.

A conversation adapter can declare `subjectEvidence` mappings from governed
actions to a bounded, dot-separated output object path:

```yaml
subjectEvidence:
  - action: read_conversation
    subjectPath: receipt.conversationId
  - action: send_message
    subjectPath: receipt.conversationId
```

The referenced action must exist, declare a non-null string at that output path,
and use credentials declared identically by the adapter. Mappings do not grant
actions or credentials. Existing adapters without mappings retain their contract.

For a mapped adapter, `create_workflow` in `openseal.runbooks` requires
`subjectEvidenceActionCallId`. Its durable ActionCall must have succeeded in the
originating conversation Run, using the exact current Agent, connector binding,
binding revision, Skill version, and currently allowed action. The declared
output conversation ID must equal the requested subject exactly. Failed,
foreign, stale, or model-authored receipts cannot establish proof.

`workflow_sources` returns mapping declarations and bounded `knownSubjects`
containing only an external conversation ID and its ActionCall ID. It can inspect
one explicit receipt and the latest relevant checkpoint receipt. Checkpoint
metadata chooses a lookup; the durable ActionCall establishes every fact.
Sources without mappings and unrelated latest actions require no receipt reads.
Point lookups still load the existing durable ActionCall record; the query-count
bound is not a bound on the record's payload size.

Subjects are bounded canonical provider references. IDs containing `/` or `@`
retain their identity; no normalization, prefix inference, wildcard matching,
or participant-to-conversation rewrite occurs. Authenticated buffered events
remain eligible from the original human request boundary, so a fast reply can
arrive before registration without being lost.

Memory and SQLite regression tests cover rejected provenance, exact subject
matching, early replies, bounded discovery, and opaque provider IDs.
