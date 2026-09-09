# The project pins Go 1.24 via mise.toml. The shims directory puts that
# toolchain on PATH even in non-interactive shells, where mise is not active.
export PATH := $(HOME)/.local/share/mise/shims:$(PATH)

GO ?= go

.PHONY: build test vet fmt check

build:
	$(GO) build -o bin/infra ./cmd/infra

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	@test -z "$$(gofmt -l .)" || { echo "unformatted files:"; gofmt -l .; exit 1; }

check: fmt vet test
