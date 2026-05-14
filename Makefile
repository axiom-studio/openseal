.PHONY: build test vet

build:
	go build -o openseal ./cmd/openseal

test:
	go test ./...

vet:
	go vet ./...
