.PHONY: build build-web test vet

build-web:
	cd web && npm install && npm run build
	cp -r web/dist pkg/webui/dist

build: build-web
	go build -o openseal ./cmd/openseal

test:
	go test ./...

vet:
	go vet ./...
