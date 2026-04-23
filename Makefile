.PHONY: build hermes test vet

build:
	go build -o openseal ./cmd/openseal

hermes:
	go build -o hermes ./cmd/hermes

test:
	go test ./...

vet:
	go vet ./...
