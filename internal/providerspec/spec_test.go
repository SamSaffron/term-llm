package providerspec

import "testing"

func TestRegistryUniqueAndComplete(t *testing.T) {
	names := make(map[string]bool)
	types := make(map[Type]bool)
	for _, spec := range Types() {
		if spec.Name == "" || spec.Type == "" || spec.DisplayName == "" {
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
			got, ok = Lookup(alias)
			if !ok || got.Type != spec.Type {
				t.Errorf("alias lookup %s: %+v, %v", alias, got, ok)
			}
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
