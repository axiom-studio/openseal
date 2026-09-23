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

  # A missing version degrades the header; a missing directory means no licence
  # text at all, which is the failure this script must not absorb quietly. So
  # the two are guarded differently on purpose.
  #
  # .Version keeps its guard: a local or `replace`d module legitimately has no
  # version, and that is not a reason to fail a release.
  version="$(go list -m -f '{{.Version}}' "$module" 2>/dev/null || true)"
  # .Dir does NOT. If the module cannot be located, the entry silently becomes
  # "No licence file was found" — indistinguishable from a module that really
  # ships none — and a broken lookup across the whole tree produces a complete
  # -looking file with no licence text in it. Let it fail here, loudly.
  dir="$(go list -m -f '{{.Dir}}' "$module")"

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
    # The status is captured rather than left to `set -e`, so this script emits
    # its OWN diagnostic instead of relying on the tool to have emitted one.
    #
    # Every `find` available to test here does speak up on failure — bfs,
    # busybox and GNU findutils all write to stderr for a bad primary or a
    # missing directory — so this is not a reachable silence today. But the
    # previous wording of that claim was universal, and a `find` that exits
    # non-zero printing nothing falsified it in one test. Delegating the
    # diagnostic and asserting that no path is silent are different things;
    # this makes the code match the claim rather than softening the claim.
    if ! file="$(find "$dir" -maxdepth 1 -type f \
      \( -iname 'LICENSE*' -o -iname 'LICENCE*' -o -iname 'COPYING*' \) \
      -print | sed 's#.*/##' | LC_ALL=C sort)"; then
      echo "error: the licence-file lookup failed for ${module}" >&2
      echo "       directory: ${dir}" >&2
      echo "       command:   find <dir> -maxdepth 1 -type f \\( -iname 'LICENSE*' -o -iname 'LICENCE*' -o -iname 'COPYING*' \\) -print | sed | sort" >&2
      echo "       ${OUT} has NOT been written." >&2
      exit 1
    fi
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

# A floor on the control, not merely a note about it.
#
# A handful of modules genuinely ship no licence file at their root; MOST of
# them yielding none means the lookup broke, not that the licences vanished.
# That distinction matters because the degenerate file is structurally perfect
# and every downstream assertion passes on it: `test -s` sees bytes, the image
# check sees the banner line, and CI's module-set gate sees all the module
# headers. Nothing else in the pipeline can catch an all-empty notices file, so
# it is caught here or not at all — and shipping one would defeat the entire
# obligation this script exists to discharge.
#
# Checked BEFORE the temp file is moved into place, so a broken run cannot
# overwrite a good committed copy with a hollow one.
MAX_MISSING="${MAX_MISSING:-5}"
if [ "$missing" -gt "$MAX_MISSING" ]; then
  echo "error: ${missing} of ${count} modules yielded no licence text (max ${MAX_MISSING})" >&2
  echo "the module lookup is probably broken; the notices file would be incomplete" >&2
  echo "${OUT} has NOT been written. Set MAX_MISSING to raise the floor deliberately." >&2
  exit 1
fi

mv "$tmp" "$OUT"
trap - EXIT

echo "wrote ${OUT}: ${count} modules, ${missing} without a licence file at the module root"
if [ "$missing" -gt 0 ]; then
  echo "note: modules without a licence file are listed in the file and need review" >&2
fi
