#!/usr/bin/env bash
#
# Emit the licence inventory for everything statically linked into the
# openseal binary.
#
# This produces the FACTS. It deliberately makes no compliance judgement:
# which licences impose an attribution obligation on binary distribution,
# whether a combined THIRD_PARTY_NOTICES satisfies them, and which artifacts
# must carry it are decisions for whoever owns licence compliance. Running this
# is not an audit; it is the input to one.
#
# Scope note: this lists modules actually linked into ./cmd/openseal, not the
# whole module graph. `go list -m all` reports 162 modules, most of which are
# test-only or transitive-but-unused and never reach a shipped artifact. Go
# links statically, so the linked set is what travels inside the binaries, the
# container image and the desktop bundles.
#
# Usage:
#   scripts/licence-inventory.sh              # table to stdout
#   scripts/licence-inventory.sh --tsv        # tab-separated, for a spreadsheet
#
set -euo pipefail

cd "$(dirname "$0")/.."

# shellcheck source=scripts/lib/licence-common.sh
. "$(dirname "$0")/lib/licence-common.sh"

FORMAT="table"
if [ "${1:-}" = "--tsv" ]; then
  FORMAT="tsv"
fi

modules="$(licence_modules)"

if [ "$FORMAT" = "table" ]; then
  printf '%-52s %-10s %-14s %s\n' "MODULE" "VERSION" "LICENCE FILE" "FIRST LINE"
  printf '%-52s %-10s %-14s %s\n' "------" "-------" "------------" "----------"
fi

missing=0
count=0
while IFS= read -r module; do
  [ -n "$module" ] || continue
  count=$((count + 1))

  version="$(go list -m -f '{{.Version}}' "$module" 2>/dev/null || true)"
  dir="$(go list -m -f '{{.Dir}}' "$module" 2>/dev/null || true)"

  file=""
  first=""
  if [ -n "$dir" ] && [ -d "$dir" ]; then
    # The same lookup the generator uses, so the two tools cannot disagree
    # about whether a module is licensed. See scripts/lib/licence-common.sh.
    file="$(licence_files "$dir")"
    # First line only, taken in the shell rather than by piping into `head`,
    # which closes the pipe early and can SIGPIPE the producer under pipefail.
    file="${file%%$'\n'*}"
    if [ -n "$file" ]; then
      # First non-empty line is usually enough to identify the licence family;
      # classification is the reviewer's job, not this script's.
      first="$(grep -m1 -v '^[[:space:]]*$' "${dir}/${file}" 2>/dev/null | cut -c1-60 || true)"
    fi
  fi

  if [ -z "$file" ]; then
    file="MISSING"
    first="no licence file found at the module root"
    # A NOTICE is not a licence grant, so it does not clear MISSING -- but it
    # is worth naming, because it changes what a reviewer has to read.
    #
    # notice_files, not a test for the exact name NOTICE: the same shared
    # helper the generator uses, so the two cannot disagree about what counts
    # as a notice. An exact test here would miss NOTICE.txt exactly as the
    # generator's did.
    if [ -n "$dir" ] && [ -d "$dir" ] && [ -n "$(notice_files "$dir")" ]; then
      first="no licence file; a NOTICE is present, review it"
    fi
    missing=$((missing + 1))
  fi

  if [ "$FORMAT" = "tsv" ]; then
    printf '%s\t%s\t%s\t%s\n' "$module" "$version" "$file" "$first"
  else
    printf '%-52s %-10s %-14s %s\n' "$module" "$version" "$file" "$first"
  fi
done <<< "$modules"

if [ "$FORMAT" = "table" ]; then
  echo
  echo "${count} modules linked into the binary; ${missing} with no licence file at the module root."
  echo
  echo "This is an inventory, not a compliance decision. See VibeFlow issue #5362."
fi
