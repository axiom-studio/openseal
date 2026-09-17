.PHONY: build test vet desktop-install desktop-dev desktop-build desktop-host-test docker-up docker-down docker-logs

build:
	go build -o openseal ./cmd/openseal

test:
	go test ./...

vet:
	go vet ./...

desktop-install:
	cd desktop && pnpm install

desktop-dev: build
	cd desktop && pnpm prepare:daemon
	cd desktop && OPENSEAL_DAEMON_PATH=$(CURDIR)/openseal pnpm tauri dev

desktop-build: build
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
