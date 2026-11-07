.PHONY: build test check release
BUILDINFO := github.com/mash/kioku/internal/buildinfo
build:
	@set -eu; \
	ldflags="-X $(BUILDINFO).Version=$(or $(VERSION),dev) -X $(BUILDINFO).Commit=$$(git rev-parse HEAD)"; \
	go build -trimpath -ldflags "$$ldflags" -o bin/kioku ./cmd/kioku; \
	go build -trimpath -ldflags "$$ldflags" -o bin/kiokud ./cmd/kiokud
test:
	go test -race ./...
check: test
	go vet ./...

# make release VERSION=v0.1.0 [COMMIT=<40-character SHA>]
# Builds the CLI archives, SHA256SUMS.txt and RELEASE_NOTES.md into dist/.
VERSION ?=
COMMIT ?= $(shell git rev-parse HEAD)
TARGETS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64
release:
	@echo '$(VERSION)' | grep -Eqx 'v[0-9]+\.[0-9]+\.[0-9]+' || { echo 'VERSION must be vX.Y.Z' >&2; exit 1; }
	@echo '$(COMMIT)' | grep -Eqx '[0-9a-f]{40}' || { echo 'COMMIT must be a full SHA' >&2; exit 1; }
	@# Never merge releases or delete an existing output directory.
	mkdir dist
	@set -eu; \
	ldflags="-s -w -X $(BUILDINFO).Version=$(VERSION) -X $(BUILDINFO).Commit=$(COMMIT) -X $(BUILDINFO).BuiltAt=$$(date -u +%Y-%m-%dT%H:%M:%SZ)"; \
	host=$$(go env GOOS)/$$(go env GOARCH); \
	for target in $(TARGETS); do \
		os=$${target%/*}; arch=$${target#*/}; tmp=bin/release/$${os}_$${arch}; mkdir -p $$tmp; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "$$ldflags" -o $$tmp/kioku ./cmd/kioku; \
		chmod 0755 $$tmp/kioku; \
		if [ $$target = $$host ]; then \
			$$tmp/kioku version | grep -q '"commit":"$(COMMIT)".*"version":"$(VERSION)"' || { echo 'built client version check failed' >&2; exit 1; }; \
		fi; \
		tar -C $$tmp -czf dist/kioku_$(VERSION)_$${os}_$${arch}.tar.gz kioku; \
	done
	@cd dist && { sha256sum kioku_*.tar.gz 2>/dev/null || shasum -a 256 kioku_*.tar.gz; } > SHA256SUMS.txt
	@printf '%s\n' \
		'# Kioku CLI $(VERSION)' '' \
		'Client binaries for macOS and Linux (amd64/arm64).' '' \
		'Verify the archive against SHA256SUMS.txt, extract `kioku`, and run `kioku version`. macOS users can verify with `shasum -a 256 -c SHA256SUMS.txt`; Linux users can use `sha256sum -c SHA256SUMS.txt` after downloading the archives being verified.' '' \
		'The CLI requires a Kioku server and an authorized account. The archives contain only the client binary; they contain no server binary, credentials or production configuration.' '' \
		'Source revision: `$(COMMIT)`.' > dist/RELEASE_NOTES.md
