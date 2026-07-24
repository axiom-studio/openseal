# OpenSeal documentation

Use these guides in order for a first deployment:

1. [Getting started](getting-started.md) — build, configure, start, and verify a
   standalone daemon.
2. [Core concepts](concepts.md) — understand Agents, Teams, objectives, Runs,
   Skills, approvals, conversations, and evidence.
3. [Callable runbooks](callable-runbooks.md) — compose exact deterministic
   operations into otherwise cognitive Agents.
4. [Terminal UI](tui.md) — create and operate resources interactively.
5. [CLI reference](cli.md) — inspect every implemented command and option.
6. [REST API](api.md) — build capability-aware clients and integrations.
7. [Operations](operations.md) — persistence, recovery, secrets, PostgreSQL, and
   test gates.

Architecture and compatibility references:

- [Autonomous runtime architecture](architecture/autonomous-agent-runtime.md)
- [OpenClaw skill compatibility](skills/openclaw-compatibility.md)

These documents describe code implemented in this repository. Optional behavior
is identified as an embedding boundary, and clients are expected to use the
server capability document as the source of truth for a running deployment.
