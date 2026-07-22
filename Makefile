.PHONY: build test vet neutrality docker-up docker-down docker-logs

build:
	go build -o openseal ./cmd/openseal

test: neutrality
	go test ./...

vet: neutrality
	go vet ./...

neutrality:
	./scripts/check-product-neutral.sh

# Docker Compose targets
docker-up:
	docker compose up --build -d

docker-down:
	docker compose down -v

docker-logs:
	docker compose logs -f openseal
