DEFAULT_GOAL := build

BIN := mitt

# `go test ./...` runs packages concurrently. A hang anywhere would otherwise
# hold the whole run open with no output, so the test target carries three
# guards: a per-test binary timeout, a bounded package-level timeout, and a
# package parallelism limit so several proxy/container tests cannot stack up on
# one machine. `-count=1` keeps results honest for a gate run.
GO_TEST_TIMEOUT ?= 10m
GO_TEST_PARALLEL ?= 4
GO_TEST_FLAGS ?= -count=1 -timeout $(GO_TEST_TIMEOUT) -p $(GO_TEST_PARALLEL)

.PHONY: build test clean

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-w -s" -o $(BIN) ./cmd/mitt

test:
	go test ./... $(GO_TEST_FLAGS)

clean:
	rm -f $(BIN)
