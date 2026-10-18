.PHONY: build frontend frontend-deps complexity complexity-baseline complexity-report plans-lint check-changed docs

FRONTEND_STAMP := frontend/node_modules/.term-llm-install-stamp
VERSION ?= $(shell git describe --tags --abbrev=0 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
TERM_LLM_LDFLAGS := -s -w \
	-X github.com/samsaffron/term-llm/cmd.Version=$(VERSION) \
	-X github.com/samsaffron/term-llm/cmd.Commit=$(COMMIT) \
	-X github.com/samsaffron/term-llm/cmd.Date=$(BUILD_DATE)

build: frontend
	go build -ldflags "$(TERM_LLM_LDFLAGS)" -o term-llm .
	@if [ "$$(go env GOHOSTOS)" = "darwin" ] && [ "$$(go env GOOS)" = "darwin" ]; then \
		codesign --force --sign - --identifier com.term-llm.cli ./term-llm; \
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

docs:
	go run ./cmd/gendocs

check-changed:
	scripts/check-changed.sh $(ARGS)

$(FRONTEND_STAMP): frontend/package.json frontend/package-lock.json
	npm --prefix frontend ci
	@touch $@
