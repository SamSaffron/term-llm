# Configuration package

- `config.go` defines persisted settings; `classify.go` and validation govern safety and reload behavior.
- `credentials.go` owns credential lookups; `config_edit.go` handles edits and persistence. Keep secrets out of logs/tests.
- Provider names and supported credentials are defined in code (see `internal/providerspec/README.md`); never maintain a parallel static list.
- Config is read from `$XDG_CONFIG_HOME/term-llm/config.yaml`, falling back to `~/.config/term-llm/config.yaml`.
- Add migration/round-trip and failure tests beside changes; never read user config.
- Focused check: `make test PKG=./internal/config`; after reference-source changes run `make docs`.
