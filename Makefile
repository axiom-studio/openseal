.PHONY: build build-web test vet docker-up docker-down docker-logs

build-web:
	cd web && npm install && npm run build
	cp -r web/dist pkg/webui/dist

build: build-web
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
