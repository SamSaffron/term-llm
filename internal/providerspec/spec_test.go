package providerspec

import (
	"slices"
	"testing"
)

func TestRegistryUniqueAndComplete(t *testing.T) {
	names := make(map[string]bool)
	types := make(map[Type]bool)
	for _, spec := range Types() {
		if spec.Name == "" || spec.Type == "" {
			t.Errorf("incomplete spec: %+v", spec)
		}
		if names[spec.Name] || types[spec.Type] {
			t.Errorf("duplicate provider: %s (%s)", spec.Name, spec.Type)
		}
		names[spec.Name], types[spec.Type] = true, true
		got, ok := ByType(spec.Type)
		if !ok || got.Name != spec.Name {
			t.Errorf("type lookup %s: %+v, %v", spec.Type, got, ok)
		}
		if spec.Type == TypeOpenAICompat {
			continue
		}
		got, ok = Lookup(spec.Name)
		if !ok || got.Type != spec.Type {
			t.Errorf("name lookup %s: %+v, %v", spec.Name, got, ok)
		}
		for _, alias := range spec.Aliases {
			if _, ok = Lookup(alias); ok {
				t.Errorf("Lookup must be name-only, but resolved alias %q", alias)
			}
			for _, label := range []string{alias, alias + " (model)"} {
				got, ok = LookupSessionAlias(label)
				if !ok || got.Type != spec.Type {
					t.Errorf("session alias lookup %q: %+v, %v", label, got, ok)
				}
			}
		}
		if want := spec.ModelListing != "" && spec.ModelListing != ModelListingCurated; spec.ListModels != want {
			t.Errorf("%s ListModels = %v, want %v from ModelListing %q", spec.Name, spec.ListModels, want, spec.ModelListing)
		}
		if spec.Availability != "" && !slices.Contains(Availabilities(), spec.Availability) {
			t.Errorf("%s has undefined availability %q", spec.Name, spec.Availability)
		}
	}
	if len(Fields()) == 0 {
		t.Fatal("provider config schema is empty")
	}
}

func TestAllReturnsIndependentDefaults(t *testing.T) {
	first := All()
	first[0].Defaults[0].Value = "changed"
	if All()[0].Defaults[0].Value == "changed" {
		t.Fatal("registry defaults were mutated")
	}
}
