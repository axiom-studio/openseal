.PHONY: build test vet docker-up docker-down docker-logs

build:
	go build -o openseal ./cmd/openseal

test:
	go test ./...

vet:
	go vet ./...

# Docker Compose targets
docker-up:
	docker compose up --build -d

docker-down:
	docker compose down -v

docker-logs:
	docker compose logs -f openseal
