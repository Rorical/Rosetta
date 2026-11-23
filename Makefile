GO ?= go
# Keep private working material and generated Solidity dependencies out of Go checks.
GO_FILES = $$(find . \( -path './.plans' -o -path './.git' -o -path './contracts/lib' -o -path './internal/testutil/opengine/target' -o -path './web/node_modules' \) -prune -o -name '*.go' -print)

VERSION ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null)
LDFLAGS = -X github.com/Rorical/Rosetta/internal/control.version=$(VERSION)

.PHONY: build build-console web-deps fmt fmt-check vet test test-race check check-console check-op forge-test

build:
	CGO_ENABLED=0 $(GO) build -ldflags "$(LDFLAGS)" -o bin/rosetta ./cmd/rosetta
	CGO_ENABLED=0 $(GO) build -o bin/routeaudit ./cmd/routeaudit
	CGO_ENABLED=0 $(GO) build -o bin/paperaudit ./cmd/paperaudit
	CGO_ENABLED=0 $(GO) build -o bin/bidaudit ./cmd/bidaudit
	CGO_ENABLED=0 $(GO) build -o bin/bidmodel ./cmd/bidmodel

# The console UI is embedded, so `go build` alone produces a binary that only
# reports missing assets.
web-deps:
	npm --prefix web ci

build-console:
	npm --prefix web run build
	CGO_ENABLED=0 $(GO) build -o bin/rosetta-console ./cmd/rosetta-console

check-console:
	npm --prefix web test
	npm --prefix web run typecheck
	$(GO) vet ./cmd/rosetta-console ./internal/console ./internal/control ./web
	$(GO) test -race -timeout=240s ./cmd/rosetta-console ./internal/console ./internal/control ./web

fmt:
	gofmt -w $(GO_FILES)

fmt-check:
	@unformatted="$$(gofmt -l $(GO_FILES))"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt required for:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

vet:
	$(GO) vet ./...

test:
	$(GO) test -timeout=240s ./...

test-race:
	$(GO) test -race -timeout=240s ./...

check: fmt-check build vet test-race check-console forge-test

forge-test:
	cd contracts && forge fmt --check && forge test --offline

# Offline native OP reference execution; Rust is a test-only dependency.
check-op: fmt-check
	cargo fmt --manifest-path internal/testutil/opengine/Cargo.toml -- --check
	cargo build --locked --manifest-path internal/testutil/opengine/Cargo.toml
	cargo test --locked --manifest-path internal/testutil/opengine/Cargo.toml
	CGO_ENABLED=1 $(GO) test -tags contracts,opnative -race -timeout=240s ./internal/remote ./internal/exec ./internal/planning

# Offline GBDT training/export parity. Install scripts/bid-learning/requirements.txt
# in a virtual environment and set BID_PYTHON to its Python executable.
BID_PYTHON ?= python3
.PHONY: check-bid-learning
check-bid-learning:
	$(GO) build -o bin/bidmodel ./cmd/bidmodel
	$(BID_PYTHON) -m unittest discover -s scripts/bid-learning -p 'test_*.py'
	$(GO) test -race ./internal/bidmodel ./internal/planning ./internal/remote ./cmd/bidmodel
