.PHONY: build frontend frontend-deps

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

frontend: frontend-deps
	npm --prefix frontend run build

frontend-deps: $(FRONTEND_STAMP)

$(FRONTEND_STAMP): frontend/package.json frontend/package-lock.json
	npm --prefix frontend ci
	@touch $@
