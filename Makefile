# The project pins Go 1.24 via mise.toml. The shims directory puts that
# toolchain on PATH even in non-interactive shells, where mise is not active.
export PATH := $(HOME)/.local/share/mise/shims:$(PATH)

GO ?= go

.PHONY: build test vet fmt check

build:
	$(GO) build -o bin/infrata ./cmd/infrata

# -count=1 is not a preference, it disables the test cache — and without it
# `make check` can pass while tests/integration has never run against the
# code being checked. That suite shells out to `go build` rather than
# importing any infra package (`go list -deps ./tests/integration` prints
# only itself), so Go sees no changed input and reuses its last result.
# Demonstrated: sabotage internal/executor, run `go test ./...`, and the
# output is
#     FAIL infra/internal/cli
#     FAIL infra/internal/executor
#     ok   infra/tests/integration  (cached)
# The suite that exists to catch what unit tests cannot is the one that can
# silently report a pass it did not earn. Measured cost of the fix: none —
# the full suite is FASTER with -count=1 (2.9s vs 3.7s), because cache
# lookup costs more than running tests this small.
test:
	$(GO) test -count=1 ./...

vet:
	$(GO) vet ./...

fmt:
	@test -z "$$(gofmt -l .)" || { echo "unformatted files:"; gofmt -l .; exit 1; }

check: fmt vet test
