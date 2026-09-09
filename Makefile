GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w -X main.Version=$(VERSION) -X main.Commit=$(COMMIT)

.PHONY: all build test vet lint bench cross clean

all: lint test build

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/nemuz ./cmd/nemuz
	@echo "binary: $$(du -h bin/nemuz | cut -f1)"

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

# lint enforces the size budget as well as vet. A file over 800 lines is a
# design failure, not a style preference: Hermes' gateway/run.py reached 33,682
# lines and became impossible to review, test, or merge without conflict.
lint: vet
	@./bench/filesize.sh

# cross builds every target the release does. Without it a Linux-only change
# compiles fine here and fails when a tag is pushed.
cross:
	@for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do \
		GOOS=$${target%/*} GOARCH=$${target#*/} $(GO) build -o /dev/null ./... \
			&& printf 'ok    %s\n' "$$target" \
			|| { printf 'FAIL  %s\n' "$$target"; exit 1; }; \
	done

bench: build
	@./bench/size.sh
	@./bench/startup.sh

clean:
	rm -rf bin dist
