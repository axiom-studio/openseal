#!/usr/bin/env bash
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
export CI=true GOMAXPROCS=2
export GOFLAGS="${GOFLAGS:+${GOFLAGS} }-p=2"
sudo apt-get update
sudo apt-get install -y --no-install-recommends libwebkit2gtk-4.1-dev libgtk-3-dev libayatana-appindicator3-dev librsvg2-dev patchelf pkg-config
make vet
make test
make build
# Regeneration must be identical to checked-in notices for this exact candidate.
# A changed generated input holds promotion instead of passing a different tree.
make third-party-notices
cd desktop
corepack pnpm install --frozen-lockfile
corepack pnpm run build
corepack pnpm exec playwright install --with-deps chromium
corepack pnpm test
cd ..
make desktop-host-test
cp -f THIRD_PARTY_NOTICES desktop/src-tauri/THIRD_PARTY_NOTICES
corepack pnpm --dir desktop prepare:daemon
cargo check --locked --manifest-path desktop/src-tauri/Cargo.toml
docker build --pull --platform linux/amd64 -f Dockerfile -t dependency-check/openseal:local .
