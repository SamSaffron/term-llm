# Adding an inference provider

1. Add its type and one `Spec` to `internal/providerspec/spec.go` (the leaf registry).
2. Set credentials/env, defaults, listing behavior, availability, capabilities and optional wizard metadata there.
3. Add its inference constructor in `internal/llm/factory.go`: a `providerConstructors` literal entry, or an entry in `apiKeyProviderConstructors`/`binProviderConstructors` (the outer table derives from those).
4. If `ListModels` is true, set a `ModelListing` constant (anything but `ModelListingCurated`; `ListModels` is derived from it) and add its implementation to `modelListerConstructors` in `cmd/models.go`; otherwise supply curated models in `internal/llm/models.go`.
5. For login-based providers, add an `Availability` constant and its probe in `availabilityProbes` (`internal/llm/provider_availability.go`); the setup wizard has its own table in `internal/ui/wizard.go`.
6. Run the registry completeness tests in `internal/llm` and `cmd`, then the usual build and tests.

The generic `openai_compatible` type supports custom named providers without a built-in default block.
