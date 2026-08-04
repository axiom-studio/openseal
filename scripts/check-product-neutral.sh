#!/usr/bin/env bash
set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repository_root"

status=0
# Check for product-specific terms (case-insensitive for these)
for term in 'at''las' 'sen''tinel' 'cor''tex'; do
  if rg -n -i --hidden \
    --glob '!.git/**' \
    --glob '!vendor/**' \
    --glob '!go.sum' \
    --glob '!*_test.go' \
    --glob '!scripts/check-product-neutral.sh' \
    "$term"; then
    status=1
  fi
done

# Check for "Vault" product name (case-sensitive to avoid matching generic "vault" references)
if rg -n --hidden \
  --glob '!.git/**' \
  --glob '!vendor/**' \
  --glob '!go.sum' \
  --glob '!*_test.go' \
  --glob '!scripts/check-product-neutral.sh' \
  '\bVault\b'; then
  status=1
fi

for identifier in \
  'AgentLibrary' \
  'agentLibraryId' \
  'AXIOM_OPENCLAW_SKILLS_DIR' \
  '/var/lib/axiom' \
  'Axiom-Agent-Runtime' \
  'Axiom Bot' \
  'axiom-agents' \
  'axiom-openseal' \
  'axiom-sdk' \
  '/axiom-sdk' \
  'axiom_sdk'; do
  if rg -n --hidden --fixed-strings \
    --glob '!.git/**' \
    --glob '!vendor/**' \
    --glob '!go.sum' \
    --glob '!*_test.go' \
    --glob '!scripts/check-product-neutral.sh' \
    "$identifier"; then
    status=1
  fi
done

if compgen -G 'pkg/llm/*.go' >/dev/null; then
  echo "pkg/llm is the retired visual-workflow assistant; use the canonical provider, authoring, and hosted-turn contracts." >&2
  status=1
fi

if (( status != 0 )); then
  echo "OpenSeal source must remain product-neutral." >&2
  exit "$status"
fi
