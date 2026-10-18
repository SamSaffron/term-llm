# Adding an inference provider

1. Add its type and one `Spec` to `internal/providerspec/spec.go` (the leaf registry).
2. Set credentials/env, defaults, listing behavior, availability, capabilities and optional wizard metadata there.
3. Add its inference constructor in `internal/llm/factory.go` (`providerConstructors`).
4. If `ListModels` is true, choose a `ModelListing` implementation in `cmd/models.go`; otherwise supply curated models in `internal/llm/models.go`.
5. Add a local credential probe in `internal/llm/provider_availability.go` if needed.
6. Run the registry completeness tests in `internal/llm` and `cmd`, then the usual build and tests.

The generic `openai_compatible` type supports custom named providers without a built-in default block.
