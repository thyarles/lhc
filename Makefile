# Developer tasks for lhc. Run `make` for the list.

.DEFAULT_GOAL := help
.PHONY: help build test lint vuln deps check e2e report snapshot clean

BIN     := lhc
LDFLAGS := -s -w -X github.com/thyarles/lhc-go/internal/version.Version=$(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo dev)

help:  ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

build:  ## Build the static binary ./lhc
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/lhc

test:  ## Unit tests with the race detector
	go test -race ./...

lint:  ## golangci-lint (gofumpt, goimports, depguard, forbidigo, ...)
	golangci-lint run ./...

vuln:  ## govulncheck against the Go vulnerability database
	govulncheck ./...

deps:  ## Dependency allowlist and go.mod tidiness
	./scripts/check-deps.sh
	go mod tidy -diff

check: test lint vuln deps  ## Everything CI runs
	@echo "  All checks passed."

e2e:  ## End-to-end test: real binary, fake commands, fake SMTP relay
	go test -race -tags e2e ./test/e2e/...

report: build  ## Preview the report on this machine (no mail, no state change)
	./$(BIN) report

snapshot:  ## Local GoReleaser build into dist/ (no publishing)
	goreleaser release --snapshot --clean

clean:  ## Remove build output
	rm -rf $(BIN) dist/
