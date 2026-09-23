# Shared helpers for the two licence tools.
#
# scripts/generate-third-party-notices.sh produces what ships inside every
# artifact. scripts/licence-inventory.sh produces the table a reviewer reads
# when deciding what the project owes attribution for. They must agree about
# which modules are linked and where each module's licence text lives, and they
# did not: both implemented the enumeration and the lookup separately, and the
# copies had already drifted on which filenames count as a licence.
#
# Sourced, not executed. Callers set `set -euo pipefail` and cd to the
# repository root themselves.

# licence_modules prints one module path per line: everything contributing
# packages to ./cmd/openseal, excluding the standard library (no .Module) and
# this module itself.
#
# Go links statically, so this set -- not `go list -m all` -- is what actually
# travels inside the binaries, the container image and the desktop bundles.
licence_modules() {
  local self
  self="$(go list -m)"
  go list -deps -f '{{if .Module}}{{.Module.Path}}{{end}}' ./cmd/openseal \
    | sort -u | grep -v '^$' | grep -Fxv "$self"
}

# licence_files prints the licence filenames at a module root, one per line,
# sorted. $1 is the module directory.
#
# NOTICE* is deliberately NOT matched, and both callers agree on that.
#
# The inventory used to include it, which is how the two tools came to disagree:
# a module shipping only a NOTICE was reported as licensed by the inventory and
# as "No licence file was found" by the generator. The inventory's reading was
# the wrong one -- a NOTICE is not a licence grant, and a reviewer seeing
# "0 MISSING" would have drawn a conclusion the shipped artifact does not
# support. Such a module is now MISSING in both, and the inventory says
# separately that a NOTICE is present so the signal is not lost.
#
# The generator reproduces NOTICE under its own heading for Apache-2.0 section
# 4(d); matching it here would emit it twice.
#
# `-print` piped through sed, NOT `-printf '%f\n'`: -printf is a GNU extension
# that BSD find (both macOS legs) and busybox find reject outright. sed drains
# its input, so it cannot SIGPIPE find under pipefail.
#
# No 2>/dev/null: suppressing the diagnostic never suppressed the exit status,
# it only made a failure silent. The caller decides what to do with a failure.
# `! -iname '*.go'` for the same reason notice_files_unrecognised carries it:
# a module can ship source whose name matches these globs. github.com/lib/pq
# ships notice.go, and license.go or copying.go at a module root is equally
# possible. Reproducing Go source as the licence grant is worse than omitting
# one, because it looks like diligence and a reviewer has no reason to doubt it.
#
# No linked module matches both today -- checked across all 86 -- so this is a
# guard, not a fix. It is here because the opposite decision is written thirty
# lines below for the identical hazard, and two helpers in one file disagreeing
# about whether Go source can be attribution is the shape that produced #5403
# and #5404.
licence_files() {
  local dir="$1"
  find "$dir" -maxdepth 1 -type f ! -iname '*.go' \
    \( -iname 'LICENSE*' -o -iname 'LICENCE*' -o -iname 'COPYING*' \) \
    -print | sed 's#.*/##' | LC_ALL=C sort
}

# notice_files prints the attribution-notice filenames at a module root, one
# per line, sorted. $1 is the module directory.
#
# An explicit allowlist -- NOTICE, NOTICE.txt, NOTICE.md -- and deliberately
# NOT a NOTICE* glob.
#
# github.com/lib/pq ships notice.go, notice_example_test.go and notice_test.go.
# A NOTICE* glob would reproduce Go SOURCE into THIRD_PARTY_NOTICES as though
# it were an attribution notice. That is worse than omitting one: it looks like
# diligence and is not, and a reviewer would have no reason to doubt it.
#
# The exact-name test this replaces (`[ -f "$dir/NOTICE" ]`) dropped
# google.golang.org/grpc's NOTICE.txt from every artifact -- an Apache-2.0
# section 4(d) notice the project is obliged to carry.
notice_files() {
  local dir="$1"
  find "$dir" -maxdepth 1 -type f \
    \( -iname 'NOTICE' -o -iname 'NOTICE.txt' -o -iname 'NOTICE.md' \) \
    -print | sed 's#.*/##' | LC_ALL=C sort
}

# notice_files_unrecognised prints NOTICE-like filenames the allowlist did not
# accept, excluding Go source. $1 is the module directory.
#
# An allowlist trades completeness for safety: a future dependency shipping
# NOTICE.rst would be dropped exactly as grpc's NOTICE.txt was. This turns that
# residual from silent into loud. It is deliberately quiet about *.go, because
# lib/pq's notice*.go files are the reason the allowlist exists and warning
# about them every run would train the reader to ignore the warning.
notice_files_unrecognised() {
  local dir="$1"
  find "$dir" -maxdepth 1 -type f -iname 'NOTICE*' ! -iname '*.go' -print \
    | sed 's#.*/##' | LC_ALL=C sort \
    | { grep -vixE 'NOTICE|NOTICE\.txt|NOTICE\.md' || true; }
}
