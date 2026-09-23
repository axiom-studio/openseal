#!/usr/bin/env bash
#
# Put a Go toolchain on PATH and pin it to the version go.mod names.
#
# This lives in a script rather than inline in release.yml because both the
# binary job and the desktop job need it and the two inline copies had already
# drifted: one carried an explanation of GOTOOLCHAIN, the other only the
# assertion. A single copy cannot drift.
#
# Two problems are solved here, and they are independent.
#
# 1. Hosted images disagree about whether `go` is on PATH at all.
#
#    The Linux and Windows toolsets declare a default Go version, so one is
#    linked. The macOS toolsets declare Go with no "default" key -- in the same
#    files where node and ruby do have one -- and neither macOS image lists Go
#    under "Language and Runtime". Go is present in the tool cache but nothing
#    puts it on PATH. Relying on the image therefore fails on every macOS leg,
#    before anything is built.
#
# 2. GOTOOLCHAIN=auto's selection is not simply "the go line".
#
#    Measured on this repository, local toolchain go1.26.0:
#
#      go.mod 1.22.0   -> auto selects go1.24.5
#      go.mod 1.24.5   -> auto selects go1.24.5
#      go.mod 1.25.14  -> auto selects go1.25.14
#      go.mod 1.26.0   -> auto selects go1.26.0
#
#    So auto switches DOWN as well as up -- a newer toolchain on the image does
#    NOT break an equality assertion, which is worth stating because the
#    opposite is widely assumed and was assumed here. But the 1.22.0 case shows
#    a floor that the go line alone does not explain, and that the module
#    graph's maximum go requirement (1.25) does not explain either. The exact
#    rule was not established.
#
#    Naming the toolchain removes the question. The asserted version is then the
#    declared one by construction, not by whatever auto resolves to. The go
#    command still fetches it as a module verified against the Go checksum
#    database -- the same mechanism auto uses -- so pinning costs no supply-chain
#    guarantee.
#
# Usage: scripts/ci-setup-go.sh
#
set -euo pipefail

cd "$(dirname "$0")/.."

WANT="$(awk '$1 == "go" { print $2; exit }' go.mod)"
if [ -z "$WANT" ]; then
  echo "::error::could not read the go directive from go.mod" >&2
  exit 1
fi

if ! command -v go >/dev/null 2>&1; then
  CACHE="${RUNNER_TOOL_CACHE:-}"
  if [ -n "$CACHE" ] && [ -d "${CACHE}/go" ]; then
    # Newest cached version wins. This is only a bootstrap: the pin below
    # decides which toolchain actually compiles anything, so the choice here
    # affects nothing but whether a `go` binary exists to re-exec from.
    CANDIDATE="$(find "${CACHE}/go" -maxdepth 3 -type d -name bin -print | LC_ALL=C sort -V | tail -1)"
    if [ -n "$CANDIDATE" ]; then
      PATH="${CANDIDATE}:${PATH}"
      export PATH
      if [ -n "${GITHUB_PATH:-}" ]; then
        echo "$CANDIDATE" >> "$GITHUB_PATH"
      fi
      echo "no go on PATH; bootstrapping from the tool cache at ${CANDIDATE}"
    fi
  fi
fi

if ! command -v go >/dev/null 2>&1; then
  echo "::error::no Go toolchain on PATH, and none under ${RUNNER_TOOL_CACHE:-<RUNNER_TOOL_CACHE unset>}/go" >&2
  exit 1
fi

export GOTOOLCHAIN="go${WANT}"
if [ -n "${GITHUB_ENV:-}" ]; then
  echo "GOTOOLCHAIN=go${WANT}" >> "$GITHUB_ENV"
fi

# Assert the result rather than trusting the pin. A toolchain mismatch is
# otherwise silent: the build succeeds and ships an artifact compiled by a
# version nobody chose.
GOT="$(go version | awk '{print $3}' | sed 's/^go//')"
echo "go.mod wants ${WANT}; toolchain resolved to ${GOT}"
if [ "$GOT" != "$WANT" ]; then
  echo "::error::Go toolchain is ${GOT} but go.mod requires ${WANT}" >&2
  exit 1
fi
