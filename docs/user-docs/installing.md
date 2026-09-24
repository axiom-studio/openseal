# Installing OpenSeal

OpenSeal publishes three families of artifact on every release: a command-line binary, a container image, and a desktop application. Each is a different way to run the same kernel, and the right one depends on where it will run rather than on what it can do. This page covers choosing between them, installing each, and verifying what was downloaded. Building from source remains available and is covered in [Getting Started](getting-started.md).

## Choosing an Artifact

| You want | Use | Notes |
|---|---|---|
| A daemon and terminal client on your own machine | Command-line binary | Smallest install. Requires glibc 2.34 or newer on Linux |
| A server deployment, or any Linux older than the floor below | Container image | Self-contained; carries no glibc requirement |
| A graphical workspace with nothing else to install | Desktop application | Bundles its own daemon |

The Linux binary and the container image are built from the same source and differ in one respect that matters: the binary links the system C library, and the image does not.

## Command-Line Binary

Download the archive for your platform from the releases page. Archives are named for the release version and platform:

```text
openseal_<version>_<os>_<arch>.tar.gz     linux, darwin
openseal_<version>_<os>_<arch>.zip        windows
```

Published platforms:

| Operating system | Architecture |
|---|---|
| linux | amd64 |
| darwin | amd64 |
| darwin | arm64 |
| windows | amd64 |

Each archive contains the `openseal` binary, `LICENSE`, `README.md`, and `THIRD_PARTY_NOTICES`. Extract it and place the binary on your path:

```bash
tar -xzf openseal_<version>_linux_amd64.tar.gz
sudo install openseal_<version>_linux_amd64/openseal /usr/local/bin/openseal
openseal version
```

Continue from [Starting the Daemon](getting-started.md) once the binary is in place.

### Third-Party Licence Notices

OpenSeal is a statically linked binary, so the code of every library it links travels inside the artifact you downloaded. `THIRD_PARTY_NOTICES` reproduces the licence text of all of them, plus any `NOTICE` file a dependency carries under Apache-2.0 section 4(d). If you redistribute OpenSeal — repackaging it, bundling it, or shipping it inside your own product — that file is the attribution you are inheriting, and it travels with the binary rather than replacing your own obligations.

Every artifact family carries it, so you never have to go looking for the right download:

| Artifact | Where the notices are |
|---|---|
| Binary archive | `THIRD_PARTY_NOTICES`, beside the binary |
| Container image | `/app/THIRD_PARTY_NOTICES` |
| Desktop bundle | Bundled as an application resource |

The file is regenerated from the dependency graph on every release, so it describes the build you have rather than a snapshot taken earlier.

### The Linux glibc Requirement

OpenSeal's durable store is SQLite through cgo, so the Linux binary links the system C library dynamically and requires **glibc 2.34 or newer**. Below that it does not start, and the failure is a loader error before the program runs:

```text
/lib/x86_64-linux-gnu/libc.so.6: version `GLIBC_2.34' not found
```

| Distribution | Runs the binary |
|---|---|
| Ubuntu 22.04 and newer | Yes |
| Debian 12 and newer | Yes |
| RHEL 9, Rocky 9 | Yes |
| Ubuntu 20.04, Debian 11 | No |
| RHEL 8, Rocky 8, Amazon Linux 2 | No |

On a distribution below the floor, use the container image. It is built on Alpine against musl and carries no equivalent requirement.

The release pipeline enforces this floor rather than inheriting it: a build whose binary requires a higher version fails the release instead of publishing an artifact that silently drops distributions.

## Container Image

Images are published to the GitHub Container Registry for `linux/amd64`:

```bash
docker pull ghcr.io/axiom-studio/openseal:<version>
```

A final release also updates `latest`. A pre-release does not, so `latest` never resolves to a release candidate.

The image runs as an unprivileged user and serves the API on port 8080. The published port must be reachable, so the container binds all of its own interfaces — the host side is what decides exposure. See [Security Boundaries](security.md) for that distinction and for enabling API authentication before exposing the port beyond loopback.

## Desktop Application

Installers are published for macOS, Windows, and Linux. The daemon is bundled inside the application; nothing else needs installing.

| Platform | Installers |
|---|---|
| macOS | `.dmg` |
| Windows | `.msi`, `.exe` |
| Linux | `.deb`, `.rpm`, `.AppImage` |

Windows and Linux each publish more than one installer. They contain the same application and differ only in how they install it:

| Installer | Choose it when |
|---|---|
| `.msi` | Windows, centrally managed — deployable through Group Policy or Intune |
| `.exe` | Windows, installing for yourself — a per-user setup needing no administrator |
| `.deb` | Debian or Ubuntu, so the package manager tracks and upgrades it |
| `.rpm` | Fedora, RHEL or openSUSE, for the same reason |
| `.AppImage` | Any Linux, no package manager involved — one executable file you run |

The release page for a version is the authoritative list of what it actually published.

### Unsigned Builds

The macOS and Windows installers are not yet code-signed, so both systems warn on first launch. The downloads are the ones the release published; the warning reflects the absence of a signing certificate, not a problem with the file.

| System | What you see | How to proceed |
|---|---|---|
| macOS | Gatekeeper refuses to open the application | Right-click the application and choose **Open** |
| Windows | SmartScreen warns about an unrecognised publisher | Choose **More info**, then **Run anyway** |

Verify the download against `checksums.txt` before doing either.

## Verifying a Download

Every release publishes a `checksums.txt` covering all of its artifacts — binary archives and desktop installers alike. Download it alongside whatever you are installing:

```bash
# Linux
sha256sum -c checksums.txt --ignore-missing

# macOS, which ships shasum rather than sha256sum
shasum -a 256 -c checksums.txt --ignore-missing
```

`--ignore-missing` checks only the files present in the current directory, so there is no need to download every artifact to verify one.

A checksum confirms the file arrived intact and matches what the release published. It is not a signature and does not establish who produced the release.

## Upgrading a Container Deployment

Releases that change how the container runs carry an upgrade note in their release notes. The one such change so far moved the container off root: a data volume created by an earlier release is still owned by root, and the daemon cannot open its database until its ownership is corrected once. The release notes give the exact command.

Read the release notes before upgrading a deployment with a persisted volume.

## Next Steps

| Page | Description |
|---|---|
| [Getting Started](getting-started.md) | Starting the daemon, confirming it serves, and connecting the terminal client |
| [Configuration](configuration.md) | The daemon configuration file and the standalone context |
| [Security Boundaries](security.md) | Where the kernel's responsibility ends, and enabling API authentication |
| [Operations](operations.md) | Running a deployment over time |
