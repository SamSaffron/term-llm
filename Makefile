.PHONY: build test frontend frontend-deps complexity complexity-baseline complexity-report plans-lint check-changed docs pkgmap api-types

FRONTEND_STAMP := frontend/node_modules/.term-llm-install-stamp
VERSION ?= $(shell git describe --tags --abbrev=0 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
TERM_LLM_LDFLAGS := -s -w \
	-X github.com/samsaffron/term-llm/cmd.Version=$(VERSION) \
	-X github.com/samsaffron/term-llm/cmd.Commit=$(COMMIT) \
	-X github.com/samsaffron/term-llm/cmd.Date=$(BUILD_DATE)

# Override to select a certificate explicitly; otherwise retain an existing
# signing authority instead of silently replacing it with an ad-hoc signature.
export CODESIGN_IDENTITY ?=

build: frontend
	@set -e; \
	identity="$$CODESIGN_IDENTITY"; \
	macos=false; \
	if [ "$$(go env GOHOSTOS)" = "darwin" ] && [ "$$(go env GOOS)" = "darwin" ]; then \
		macos=true; \
		if [ -z "$$identity" ]; then \
			identity=$$(codesign -dvv ./term-llm 2>&1 | sed -n 's/^Authority=//p' | head -n 1); \
		fi; \
	fi; \
	go build -ldflags "$(TERM_LLM_LDFLAGS)" -o term-llm .; \
	if [ "$$macos" = true ]; then \
		codesign --force --sign "$${identity:--}" --identifier com.term-llm.cli ./term-llm; \
	fi

frontend: frontend-deps
	npm --prefix frontend run build
	go run ./cmd/builduisource

frontend-deps: $(FRONTEND_STAMP)

complexity:
	go run ./cmd/complexity --check ledger/go-complexity-baseline.json --threshold 30 --check-size ledger/go-size-baseline.json

complexity-baseline:
	go run ./cmd/complexity --write-baseline ledger/go-complexity-baseline.json --write-size-baseline ledger/go-size-baseline.json --threshold 30

complexity-report:
	go run ./cmd/complexity --threshold 20

plans-lint:
	scripts/lint-plans.sh

docs: frontend pkgmap
	go run ./cmd/gendocs

pkgmap:
	go run ./cmd/genpkgmap

api-types:
	go run ./cmd/gentsapi

check-changed:
	scripts/check-changed.sh $(ARGS)

# Focused Go tests with an isolated HOME/XDG/TMPDIR:
#   make test PKG=./internal/llm RUN='^TestName$' ARGS=-count=1
# RUN reaches the script through the environment, unexpanded, so regex `$`
# and quotes survive both make and the shell.
# The wrapper expands recursive selectors from repository-owned packages and
# rejects explicit tmp/ scratch targets (even if they are force-added to git).
PKG ?= ./...
test: export GOTEST_RUN = $(value RUN)
test:
	scripts/gotest.sh $(ARGS) $(PKG)

$(FRONTEND_STAMP): frontend/package.json frontend/package-lock.json
	npm --prefix frontend ci
	@touch $@
