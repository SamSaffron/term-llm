package runtime

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

// TestCloneConfigForServeJobDeepCopiesProviderPointers asserts the historical clone boundary
// does not alias provider slices/pointers used by per-run overrides.
func TestCloneConfigForServeJobDeepCopiesProviderPointers(t *testing.T) {
	yes := true
	base := &config.Config{
		DefaultProvider: "p",
		Providers: map[string]config.ProviderConfig{
			"p": {Model: "m", Models: []string{"m"}, UseNativeSearch: &yes},
		},
	}
	clone := CloneConfig(base)
	cp := clone.Providers["p"]
	if len(cp.Models) == 0 || cp.UseNativeSearch == nil {
		t.Fatalf("clone lost provider fields: %+v", cp)
	}
	cp.Models[0] = "MUTATED"
	*cp.UseNativeSearch = false
	clone.Providers["p"] = cp
	if base.Providers["p"].Models[0] != "m" {
		t.Errorf("base Models aliased the clone: %v", base.Providers["p"].Models)
	}
	if base.Providers["p"].UseNativeSearch == nil || *base.Providers["p"].UseNativeSearch != true {
		t.Errorf("base UseNativeSearch aliased the clone: %v", base.Providers["p"].UseNativeSearch)
	}
}
