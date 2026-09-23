.PHONY: build test vet licence-inventory third-party-notices desktop-install desktop-dev desktop-build desktop-host-test docker-up docker-down docker-logs

build:
	go build -o openseal ./cmd/openseal

test:
	go test ./...

vet:
	go vet ./...

# Licences of everything statically linked into the binary. Produces the facts
# for a compliance review; makes no compliance judgement itself.
licence-inventory:
	./scripts/licence-inventory.sh

# Full licence text of every linked module, shipped inside each artifact.
# Generated, not hand-maintained — regenerate after any dependency change.
third-party-notices:
	./scripts/generate-third-party-notices.sh

desktop-install:
	cd desktop && pnpm install

# third-party-notices and the copy are prerequisites of the DEV path too, not
# just the release path. tauri.conf.json declares THIRD_PARTY_NOTICES under
# bundle.resources, and tauri-build copies resources on every cargo build --
# dev included -- erroring on a path that does not exist. The file is
# gitignored and generated, so without this a fresh clone cannot start the app:
# build.rs fails naming a file the contributor has never seen.
desktop-dev: build third-party-notices
	cp THIRD_PARTY_NOTICES desktop/src-tauri/THIRD_PARTY_NOTICES
	cd desktop && pnpm prepare:daemon
	cd desktop && OPENSEAL_DAEMON_PATH=$(CURDIR)/openseal pnpm tauri dev

desktop-build: build third-party-notices
	cp THIRD_PARTY_NOTICES desktop/src-tauri/THIRD_PARTY_NOTICES
	cd desktop && pnpm tauri build

desktop-host-test: build
	OPENSEAL_TEST_DAEMON=$(CURDIR)/openseal cargo test --locked --manifest-path desktop/crates/daemon-host/Cargo.toml -- --include-ignored

# Docker Compose targets
docker-up:
	docker compose up --build -d

docker-down:
	docker compose down -v

docker-logs:
	docker compose logs -f openseal
