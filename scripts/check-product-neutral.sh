#!/usr/bin/env bash
set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repository_root"

status=0
for term in 'at''las' 'sen''tinel' 'cor''tex'; do
  if rg -n -i --hidden \
    --glob '!.git/**' \
    --glob '!vendor/**' \
    --glob '!go.sum' \
    --glob '!scripts/check-product-neutral.sh' \
    "$term"; then
    status=1
  fi
done

for identifier in \
  'AXIOM_OPENCLAW_SKILLS_DIR' \
  '/var/lib/axiom' \
  'Axiom-Agent-Runtime' \
  'axiom-agents' \
  'axiom-openseal' \
  'axiom-sdk' \
  '/axiom-sdk' \
  'axiom_sdk'; do
  if rg -n --hidden --fixed-strings \
    --glob '!.git/**' \
    --glob '!vendor/**' \
    --glob '!go.sum' \
    --glob '!scripts/check-product-neutral.sh' \
    "$identifier"; then
    status=1
  fi
done

if (( status != 0 )); then
  echo "OpenSeal source must remain product-neutral." >&2
  exit "$status"
fi
