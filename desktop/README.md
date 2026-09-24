# OpenSeal desktop

The React and Tauri app includes Home, Agents, Work, Reviews, Teams, Channels, and Settings. It owns a
local Go daemon and persists workspace data in the OS application-data directory.
The complete product is still in development: advanced placement and credential
mapping, full team management, broader approval workflows, and advanced channel
workflows remain unfinished. Proposal review supports ClawHub Skill search,
verification, exact-version installation, authorized Skill setting choices,
and private local credential connections.

## Start desktop development

Install Go, Rust, Node.js, pnpm, and the platform-specific
[Tauri prerequisites](https://v2.tauri.app/start/prerequisites/). From the repository root:

```bash
make desktop-install
make desktop-dev
```

The native host starts its own daemon on a loopback port and keeps its API token
in native memory. Add a model provider in Settings before generating an Agent or
Team proposal. The desktop guides review, installation, and later activation.
Use `make desktop-host-test` to verify the native host. The terminal client has
a separate [first-run guide](../docs/user-docs/getting-started.md).

When a proposal requires a Skill, use **Review Skill for installation** for an
exact ClawHub version, or **Find Skill** to choose a registry result. Review its
verification before installing. In **Configure Skill**, select available
settings and credential connections or add a private local connection.
Installation changes workspace inventory;
use **Use installed Skill in a fresh proposal** to check the new inventory.

## Find a topic

| Task | Section |
| --- | --- |
| Generate and install an Agent or Team | [Proposal review](implementation-notes.md#proposal-review), [Create and review a team](implementation-notes.md#create-and-review-a-team) |
| Work with Teams and Channels | [Teams](implementation-notes.md#teams), [Team channels](implementation-notes.md#team-channels), [Automatic Team replies](implementation-notes.md#automatic-team-replies) |
| Submit and inspect work | [Start team work](implementation-notes.md#start-team-work), [Browse work](implementation-notes.md#browse-work), [Task execution and results](implementation-notes.md#task-execution-and-results) |
| Configure the provider or understand process ownership | [Model provider](implementation-notes.md#model-provider), [Native process ownership](implementation-notes.md#native-process-ownership) |
| Verify changes | [Verification](implementation-notes.md#verification), [Installation authority](implementation-notes.md#installation-authority) |

The [implementation notes](implementation-notes.md) retain detailed feature behavior and milestone evidence.


## Verify and build

```bash
make desktop-host-test
make desktop-build
pnpm --dir desktop test
```

The browser tests use synthetic providers. Native smoke testing and release
status are documented in [Verification](implementation-notes.md#verification).
