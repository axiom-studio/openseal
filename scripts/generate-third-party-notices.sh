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

# shellcheck source=scripts/lib/licence-common.sh
. "$(dirname "$0")/lib/licence-common.sh"

OUT="${1:-THIRD_PARTY_NOTICES}"

modules="$(licence_modules)"

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

  files=""
  if [ -n "$dir" ] && [ -d "$dir" ]; then
    # No NOTICE* here: it is reproduced separately below under its own heading
    # for Apache-2.0 section 4(d), and matching it as a licence would emit it
    # twice. See licence_files in scripts/lib/licence-common.sh for why the
    # lookup is shaped the way it is.
    #
    # The status is captured rather than left to `set -e`, so this script emits
    # its OWN diagnostic instead of relying on find to have emitted one. Every
    # find available to test here does speak up on failure, so this is not a
    # reachable silence today — but the previous wording of that claim was
    # universal, and a find that exits non-zero printing nothing falsified it
    # in one test. Delegating the diagnostic and asserting that no path is
    # silent are different things.
    if ! files="$(licence_files "$dir")"; then
      echo "error: the licence-file lookup failed for ${module}" >&2
      echo "       directory: ${dir}" >&2
      echo "       lookup:    licence_files (scripts/lib/licence-common.sh)" >&2
      echo "       ${OUT} has NOT been written." >&2
      exit 1
    fi
  fi

  # EVERY matched file is reproduced, not just the first.
  #
  # A module can carry more than one licence. go.yaml.in/yaml/v2 ships LICENSE
  # (Apache-2.0) alongside LICENSE.libyaml (MIT, covering the files ported from
  # C libyaml), and taking only the alphabetically-first one dropped the MIT
  # notice from every artifact while this file's own header promised that every
  # licence is reproduced in full. Nothing downstream could catch it: the CI
  # gate compares module paths and versions, `test -s` sees bytes, and the
  # image check reads the banner line.
  #
  # Each file is labelled so a reader can tell which text came from which
  # filename, in the same form the NOTICE block below already uses.
  if [ -n "$files" ]; then
    separate=0
    while IFS= read -r name; do
      [ -n "$name" ] || continue
      if [ "$separate" -eq 1 ]; then
        echo >> "$tmp"
      fi
      separate=1
      {
        echo "--- ${name} (${module}) ---"
        echo
        cat "${dir}/${name}"
      } >> "$tmp"
    done <<< "$files"
  else
    echo "No licence file was found at this module's root." >> "$tmp"
    echo "Review this module's licensing before distributing." >> "$tmp"
    # Counted once per module, not once per absent file: the floor below is
    # about how many modules yielded no licence text at all.
    missing=$((missing + 1))
  fi
  echo >> "$tmp"

  # A NOTICE file carries its own obligation under Apache-2.0 section 4(d), so
  # it is reproduced in addition to the licence rather than instead of it.
  #
  # Matched by name rather than tested as the exact path "$dir/NOTICE", and
  # every match reproduced rather than one. The exact test dropped
  # google.golang.org/grpc's NOTICE.txt from every artifact -- the same class
  # of defect as the licence branch's "first file only", surviving one branch
  # away because that fix did not reach this code path.
  if [ -n "$dir" ] && [ -d "$dir" ]; then
    while IFS= read -r name; do
      [ -n "$name" ] || continue
      {
        echo "--- ${name} (${module}) ---"
        echo
        cat "${dir}/${name}"
        echo
      } >> "$tmp"
    done <<< "$(notice_files "$dir")"

    # A NOTICE-like file the allowlist did not recognise is reported rather
    # than dropped. Silence is how NOTICE.txt went missing in the first place.
    while IFS= read -r name; do
      [ -n "$name" ] || continue
      echo "warning: ${module} ships ${name}, which looks like an attribution notice but is not one of NOTICE, NOTICE.txt or NOTICE.md; it was NOT reproduced" >&2
    done <<< "$(notice_files_unrecognised "$dir")"
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
