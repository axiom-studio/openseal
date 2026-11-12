# Releasing

This guide describes how a release is cut, what the pipeline produces, and the
two failure modes that come from starting a release the wrong way. The workflow
is `.github/workflows/manual-release.yml`.

## The Procedure

Create and push a semver tag from a commit that contains the manual Release
workflow, then start the workflow explicitly.

```bash
git tag v0.2.0
git push origin v0.2.0
gh workflow run manual-release.yml --ref v0.2.0
```

You can also select **Run workflow** under **Actions → Release (manual)** on
`develop` and enter the tag. Leave the tag blank to select the newest release
tag by version. The tag must point to a commit that contains
`.github/workflows/manual-release.yml`.

Once started, the workflow creates the release, builds and attaches every
artifact, generates the checksums file, writes the notes, and publishes. A
release takes roughly 45 minutes.

To run the build and tests without publishing a release, use **Actions → CI
(manual) → Run workflow** and select the branch you want to check. The branch
must contain `.github/workflows/manual-ci.yml`.

Pre-release tags carry a suffix and are handled automatically: `v0.2.0-rc1`
produces a release marked pre-release, and the container `:latest` tag is left
where it is so an unqualified `docker pull` never resolves to a release
candidate.

**The tag decides pre-release status, on every path.** The workflow derives it
from the tag suffix in all three cases — creating a release, adopting one made
from the web UI, and reusing an existing draft — so the web UI's **Set as a
pre-release** checkbox is never authoritative and its setting is overwritten on
each run. That includes a draft edited by hand between a failed run and a
re-run. Getting the checkbox wrong is therefore harmless; getting the tag wrong
is not.

## What The Pipeline Does

The `pick` job selects the release tag. On the run for that tag, the remaining
jobs build and publish the release.

| Job | Runs on | Produces |
| --- | --- | --- |
| `prepare` | `ubuntu-latest` | Validates the tag is semver, then creates the release as a **draft** |
| `binary` | 4 native runners | Archives for `linux/amd64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64` |
| `container` | `ubuntu-latest` | A `linux/amd64` image pushed to GHCR as `:{version}` |
| `desktop` | 4 native runners | Tauri bundles labelled `linux-x86_64`, `macos-x86_64`, `macos-aarch64`, `windows-x86_64` |
| `release` | `ubuntu-latest` | Downloads every asset, writes one `checksums.txt` over all of them, writes the notes, publishes |
| `promote-latest` | `ubuntu-latest` | Moves the container `:latest` tag, for final releases only |

`binary`, `container` and `desktop` each declare `needs: prepare`, and
`release` declares `needs: [binary, container, desktop]`. A `prepare` failure
therefore stops everything — no job downstream of it becomes eligible, so no
artifact is built and nothing is published.

## Why The Release Starts As A Draft

Build jobs upload their assets straight into the release rather than passing
artifacts between jobs, so the release has to exist before any of them run.
`prepare` creates it as a draft, and the single `--draft=false` in the
`release` job is what makes it visible.

The draft is load-bearing, not incidental. It means a partial or failed run
leaves an invisible draft rather than a release advertising downloads that do
not exist, and it means `checksums.txt` is only ever published once it covers
the complete asset set.

## Creating The Release From The Web UI

The GitHub web UI's **Draft a new release** form creates the release *and* its
tag in a single action, and it creates the release already published rather
than as a draft. Start the workflow manually for the tag. It will find the
release created through the form.

This is tolerated, but it is not the normal path:

- The workflow adopts the release, because a release created this way carries
  zero assets and there is nothing to overwrite.
- It immediately returns the release to draft, so the build jobs attach into a
  draft exactly as on the tag-push path.

One consequence cannot be undone. Publishing the release, even for the seconds
before the workflow re-drafts it, may emit a release notification to watchers.
Re-drafting hides the release page; it does not recall a notification already
sent. Watchers can therefore receive mail about a release that is then
unavailable for the duration of the build.

Prefer pushing a tag.

## Replacing A Release

Cut a new tag. The workflow refuses to build a tag whose published release
already carries assets:

```text
release v0.2.0 is published and carries 12 asset(s); refusing to overwrite them.
Cut a new tag, or delete that release deliberately if replacement is genuinely intended.
```

Every asset upload passes `--clobber`, and the `release` job regenerates
`checksums.txt` from whatever is attached at the time. A second run against an
asset-bearing release would therefore replace artifacts that users had already
downloaded *and* emit a checksums file that correctly matched the
replacements — so the verification the release notes instruct users to run
would pass against substituted bytes. That is the one outcome a checksum must
not have, which is why the job stops instead.

Deleting the release to force a replacement is possible and is occasionally the
right call, but it is an explicit act with the above understood, not a routine
step.

## When A Run Fails Partway

Re-run the failed jobs. `prepare` reuses an existing draft rather than
recreating it, so a re-run continues against the release the first attempt
started, and the asset uploads are idempotent.

A run that fails before `release` leaves a draft holding whatever assets did
finish. That draft is not visible to users and can be re-run against or
deleted.

## Related Guides

[Operations](operations.md) covers running a released build.
[Getting started](getting-started.md) covers building from source.
