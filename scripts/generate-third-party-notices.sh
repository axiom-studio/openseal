#!/usr/bin/env bash
#
# Generate THIRD_PARTY_NOTICES: the full licence text of every module
# statically linked into the openseal binary.
#
# Why this needs no legal judgement to be correct
# -----------------------------------------------
# The hard question — which of these licences impose an attribution obligation
# on binary distribution — is deliberately not answered here. It is sidestepped
# instead: EVERY linked module's licence text is included, whether or not it
# demands one.
#
# That is safe by construction. Carrying a notice that was not required is not
# a violation of anything; omitting one that was required is. So the
# conservative action and the mechanical action are the same action, and no
# classification step is needed.
#
# A reviewer may still decide a narrower file is preferable, or that some
# licence needs its NOTICE carried separately. That decision has an artifact to
# start from rather than a blank page.
#
# Usage:
#   scripts/generate-third-party-notices.sh [output-path]
#
set -euo pipefail

cd "$(dirname "$0")/.."

OUT="${1:-THIRD_PARTY_NOTICES}"
MODULE="$(go list -m)"

modules="$(go list -deps -f '{{if .Module}}{{.Module.Path}}{{end}}' ./cmd/openseal \
  | sort -u | grep -v '^$' | grep -Fxv "$MODULE")"

count="$(printf '%s\n' "$modules" | grep -c . || true)"

# An explicit template, not a bare `mktemp`. This script runs on every desktop
# release leg, including both macOS runners, and the bare form is a GNU
# convenience rather than a guaranteed one. Passing a template is accepted by
# every implementation, which removes the question instead of answering it.
tmp="$(mktemp "${TMPDIR:-/tmp}/openseal-notices.XXXXXX")"
trap 'rm -f "$tmp"' EXIT

{
  echo "THIRD-PARTY SOFTWARE NOTICES"
  echo
  echo "OpenSeal is distributed as a statically linked binary. The ${count} modules"
  echo "listed below are compiled into it, and their licences and copyright"
  echo "notices are reproduced here in full."
  echo
  echo "This file is generated. Regenerate it with:"
  echo
  echo "    make third-party-notices"
  echo
  echo "Every linked module is included, whether or not its licence requires"
  echo "attribution on binary distribution. Including more than is required is"
  echo "not a violation; omitting what is required would be."
  echo
} > "$tmp"

missing=0
while IFS= read -r module; do
  [ -n "$module" ] || continue

  version="$(go list -m -f '{{.Version}}' "$module" 2>/dev/null || true)"
  dir="$(go list -m -f '{{.Dir}}' "$module" 2>/dev/null || true)"

  {
    echo "================================================================================"
    echo "${module} ${version}"
    echo "================================================================================"
    echo
  } >> "$tmp"

  file=""
  if [ -n "$dir" ] && [ -d "$dir" ]; then
    # `-print` piped through sed, NOT `-printf '%f\n'`. `-printf` is a GNU
    # extension: BSD find — /usr/bin/find on both macOS legs — and busybox find
    # reject it outright. Under `set -e` that killed this assignment at the
    # FIRST module, so the desktop release legs produced no notices file at all,
    # which failed the desktop job and with it the whole release.
    #
    # `-maxdepth` and `-iname` are also outside POSIX but are implemented by
    # both BSD and busybox find, so they stay.
    #
    # sed drains its input, so it cannot close the pipe early and SIGPIPE find
    # under pipefail — the hazard that broke the changelog step in #3735 and
    # came back in #5381. `-exec basename {} \;` would also be portable but
    # spawns a process per file across every linked module.
    #
    # No `2>/dev/null` here. Suppressing the diagnostic never suppressed the
    # exit status; it only made this exact failure silent and cost a QA cycle
    # to diagnose.
    file="$(find "$dir" -maxdepth 1 -type f \
      \( -iname 'LICENSE*' -o -iname 'LICENCE*' -o -iname 'COPYING*' \) \
      -print | sed 's#.*/##' | LC_ALL=C sort)"
    # First line only, taken in the shell rather than by piping into `head`,
    # which closes the pipe early and can SIGPIPE the producer under pipefail.
    file="${file%%$'\n'*}"
  fi

  if [ -n "$file" ]; then
    cat "${dir}/${file}" >> "$tmp"
  else
    echo "No licence file was found at this module's root." >> "$tmp"
    echo "Review this module's licensing before distributing." >> "$tmp"
    missing=$((missing + 1))
  fi
  echo >> "$tmp"

  # A NOTICE file carries its own obligation under Apache-2.0 section 4(d), so
  # it is reproduced in addition to the licence rather than instead of it.
  if [ -n "$dir" ] && [ -f "${dir}/NOTICE" ]; then
    {
      echo "--- NOTICE (${module}) ---"
      echo
      cat "${dir}/NOTICE"
      echo
    } >> "$tmp"
  fi
done <<< "$modules"

mv "$tmp" "$OUT"
trap - EXIT

echo "wrote ${OUT}: ${count} modules, ${missing} without a licence file at the module root"
if [ "$missing" -gt 0 ]; then
  echo "note: modules without a licence file are listed in the file and need review" >&2
fi
