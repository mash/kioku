.PHONY: build test check
build:
	go build -trimpath -o bin/kioku ./cmd/kioku
	go build -trimpath -o bin/kiokud ./cmd/kiokud
test:
	go test -race ./...
check: test
	go vet ./...
