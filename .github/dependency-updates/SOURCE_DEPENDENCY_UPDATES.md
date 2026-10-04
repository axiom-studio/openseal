# OpenSeal source dependency updates

## Paused

This workflow is disabled in GitHub Actions and its YAML is archived at
`workflow.disabled.yml`, outside `.github/workflows`. Automatic dependency
updates and the direct-push checks bundled with this workflow are paused.
To restore it, move the YAML to `.github/workflows/dependency-updates.yml`,
complete the dedicated App setup described below, and enable the workflow
in GitHub Actions. The following sections describe the retained configuration.

Passing candidates advance `develop` directly. The trusted gate runs existing
Go vet/tests/builds, regenerates third-party notices, installs the exact pnpm
lockfile, builds the desktop frontend, runs Playwright tests, runs daemon-host
tests, checks the locked Tauri Rust dependencies, and builds the actual SDK
Docker image. Linux desktop system dependencies are installed only on the
isolated hosted runner.

A baseline-owned, read-only post-upgrade recipe regenerates
`THIRD_PARTY_NOTICES` inside Renovate's candidate before it is committed. Its
command and output allowlist are fixed by the trusted policy. Validation
regenerates the notices again and rejects any difference. There is no exception
for the private SDK module and no suppression of compatibility tests.

The existing `.github/workflows/release.yml` verifies tagged release assets
before publication. Advancing `develop` here does not publish a release,
promote a latest tag, replace the desktop installation, or deploy a service.

## Shared source maintenance contract

This workflow is configured to run daily at 03:17 UTC and on manual dispatch.
It resolves supported dependency updates with pinned Renovate 44.132.2. A
repository-specific allowlist restricts changed paths to dependency manifests,
lockfiles, vendor files, Docker inputs, and declared generated inputs. Changes
to application code, workflow files, permissions, or Skill manifests are held.
Major updates, replacement/rollback proposals, unstable releases, and minor
updates to 0.x dependencies are held. Docker base-image minor upgrades also
require a separate reviewed source change.

Generation, validation, and promotion are separate jobs on isolated
`ubuntu-24.04` GitHub-hosted runners. They never run on the platform node.
There is one candidate per invocation, jobs have explicit timeouts, and
parallel Go compilation is bounded to two workers. No package lookup,
compilation, image build, or polling from this workflow runs inside a request
or conversation handler.

Renovate is deliberately configured with a supported dependency dashboard and
`prCreation: approval`, with all automerge disabled. The dashboard provides
candidate diagnostics; it is not our promotion authority. Do not approve its
PR controls. The dedicated writer App must have no pull-request write
permission, so approval cannot create a PR. The workflow promotes only a
verified source commit directly to the repository's configured default branch.

Renovate itself uses `--force-with-lease` when preparing a candidate. We limit
that behavior to a checked-unused `dependency-updates/<run>-<attempt>/`
namespace, unique to the workflow invocation. Namespace reuse or collision
holds generation. Renovate is never permitted to automerge the default branch.
The separate promoter uses plain, non-force Git push. It rejects a candidate
unless it is exactly one regular-file-only commit on the tested baseline, the
exact remote candidate still matches, and the default branch has not advanced.
A concurrent default advance fails Git's fast-forward check. The next run must
resolve and test a fresh candidate; it cannot reuse a stale verdict.

Validation copies the check recipe and policy from the trusted baseline before
checking out the candidate. It receives an independently read-only credential.
Promotion runs in a fresh job with no candidate code execution and checks the
GitHub Actions API for the successful exact validation job and matching
workflow run, attempt, and baseline. Before tests, the trusted validator also reconstructs vendor from the exact
candidate module inputs when Go/vendor files changed and canonical vendor is
required. Tampered vendor bytes are rejected even if module metadata matches.
An unrelated npm or Docker candidate skips this reconstruction and preserves
reviewed legacy vendor patches. Tracked or untracked build input changes
after testing hold promotion. The promoted commit and policy digest appear in
the Actions job summary. After successful promotion finishes and Actions persists its exact receipt
summary, a separate trusted cleanup step verifies that the promoted commit
remains reachable from the current default and removes only this run's
unchanged candidate ref using an explicit expected-SHA deletion lease. A changed ref is preserved; cleanup
failure is diagnostic and never reverses a successful default advancement.
The commit stays reachable on the default branch and its receipt stays in
Actions. Failed or changed candidates remain for audit. Generation holds at
32 remaining reserved refs until an administrator reviews exact receipts and
current SHAs and manually cleans owned failed candidates. No branch is deleted
solely because of its prefix, and failed evidence is not silently discarded.

## Required GitHub configuration

Two new GitHub Apps are required. No existing integration, registry, or
publishing token is borrowed. Configure these repository or organization
variables and secrets:

| Configuration | Kind | Required capability |
| --- | --- | --- |
| `DEPENDENCY_UPDATE_APP_ID` | Actions variable | Dedicated writer App ID |
| `DEPENDENCY_UPDATE_APP_PRIVATE_KEY` | Actions secret | Writer App private key |
| `DEPENDENCY_UPDATE_READ_APP_ID` | Actions variable | Independently read-only App ID |
| `DEPENDENCY_UPDATE_READ_APP_PRIVATE_KEY` | Actions secret | Read-only App private key |

Install the writer App only on the participating maintenance repositories.
Its repository permissions are Contents: write, Issues: write, Pull requests:
read, and Metadata: read. It must have no Pull requests: write permission.
Each generated writer token explicitly requests only those permissions and
only the current repository. Issues access is needed for the supported
dependency dashboard; it grants no platform or credential authority.

Install the read App on the participating repositories and any private
internal module repositories they require. Give it Contents: read and
Metadata: read, with no write permissions. The driver verifies the App's own
permission declaration before minting a read token, so using the writer key
for the read configuration is rejected. The validation job never receives the
writer App key or a token with repository write access.

The promotion job uses this repository's GitHub Actions token with Contents:
write and Actions: read. Branch protection and organizational rules must
permit that exact identity to fast-forward the configured default branch.
The workflow does not bypass protections, request administrative authority,
or force-push. If organizational policy prevents this, promotion remains held.

Package registries and official Go/Node toolchain downloads must be reachable
from hosted runners. Private modules must be readable through the read App.
Existing Dockerfiles that resolve private modules without a supported build
credential path can still fail; such failures hold the candidate rather than
passing credentials as Docker build arguments or bypassing the build.

Until both Apps and the Actions configuration are installed, generation and
candidate validation are explicitly held. A regular default-branch push can
still run its checks without App credentials when existing inputs are already
sufficient. These files do not configure secrets or prove a hosted run.

## Source advancement and release

A dependency commit is a source update. This workflow does not create release
tags, publish images/assets, rewrite Skill definition versions, or deploy
services. Existing tag/manual release validation is a separate boundary.
GitHub Actions token pushes do not trigger follow-on workflows, so correctness
does not depend on another push-triggered job: required candidate tests and
builds run before promotion within this workflow. Authorized release automation
must still publish its checked immutable artifacts through the repository's
existing release process before the runtime Skill updater can discover them.

The generated secret configuration stays at mode `0600` and is explicitly
owned by the pinned official image's non-root UID 12021. The hosted runner
needs non-interactive sudo for that ownership change; both validator and
generator retain the image's non-root user and a read-only configuration mount.
The pinned Renovate CLI configuration validator runs with `--strict` before
generation. Local verification covers the official pinned JSON schema and
offline promotion/credential fixtures; it does not claim that registry access,
private modules, real hosted image builds, or release publication succeeded.
Supported behavior is documented by [Renovate configuration](https://docs.renovatebot.com/configuration-options/),
[Renovate self-hosting](https://docs.renovatebot.com/self-hosted-configuration/),
[GitHub App installation tokens](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app),
and [GitHub Actions token behavior](https://docs.github.com/en/actions/concepts/security/github_token).
