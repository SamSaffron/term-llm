package llm

import (
	"slices"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/factory"
	"github.com/samsaffron/term-llm/internal/providerspec"
)

// A new provider type must be declared in the leaf registry and wired into
// construction. This checks both directions, including custom endpoints.
func TestProviderConstructorRegistryComplete(t *testing.T) {
	fields := make(map[string]bool)
	for _, field := range config.ProviderKeySpecs() {
		fields[field.Path] = true
	}
	if !fields["type"] || !fields["model"] {
		t.Fatal("provider config schema is missing common fields")
	}
	for _, spec := range providerspec.Types() {
		if spec.Type == "" || spec.Name == "" {
			t.Fatalf("incomplete provider spec: %+v", spec)
		}
		if !factory.Has(spec.Type) {
			t.Errorf("%s has no constructor", spec.Type)
		}
		if spec.Type != providerspec.TypeOpenAICompat {
			if _, ok := config.BuiltinProviderOfType(spec.Type); !ok {
				t.Errorf("%s has no config schema entry", spec.Type)
			}
		}
		for _, def := range spec.Defaults {
			if !fields[def.Path] {
				t.Errorf("%s default %s is absent from config schema", spec.Type, def.Path)
			}
		}
		if !spec.ListModels && (spec.ModelListing != providerspec.ModelListingCurated || len(ProviderModels[spec.Name]) == 0) {
			t.Errorf("%s has no model-listing behavior or curated models", spec.Type)
		}
	}
	for _, kind := range factory.Types() {
		if _, ok := providerspec.ByType(kind); !ok {
			t.Errorf("constructor %s is not registered", kind)
		}
	}
}

func TestEveryAvailabilityHasOneProbe(t *testing.T) {
	for _, kind := range providerspec.Availabilities() {
		if availabilityProbes[kind] == nil {
			t.Errorf("availability %q has no probe", kind)
		}
	}
	for kind := range availabilityProbes {
		if !slices.Contains(providerspec.Availabilities(), kind) {
			t.Errorf("probe for undefined availability %q", kind)
		}
	}
	for _, spec := range providerspec.Types() {
		if spec.Availability != "" && availabilityProbes[spec.Availability] == nil {
			t.Errorf("%s availability %q has no probe", spec.Name, spec.Availability)
		}
	}
}
