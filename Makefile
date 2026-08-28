.PHONY: build frontend frontend-deps

FRONTEND_STAMP := frontend/node_modules/.term-llm-install-stamp

build: frontend
	go build -o term-llm .

frontend: frontend-deps
	npm --prefix frontend run build

frontend-deps: $(FRONTEND_STAMP)

$(FRONTEND_STAMP): frontend/package.json frontend/package-lock.json
	npm --prefix frontend ci
	@touch $@
