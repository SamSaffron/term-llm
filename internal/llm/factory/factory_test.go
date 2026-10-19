package factory

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/providerspec"
)

// Every declared provider type needs a constructor and vice versa.
func TestConstructorsMatchProviderSpec(t *testing.T) {
	for _, spec := range providerspec.Types() {
		if !Has(spec.Type) {
			t.Errorf("%s has no constructor", spec.Type)
		}
	}
	for _, kind := range Types() {
		if _, ok := providerspec.ByType(kind); !ok {
			t.Errorf("constructor %s is not registered in providerspec", kind)
		}
	}
}

// Every dispatch layer below the outer constructor table must only name registered
// types, and the shared layers must cover exactly the types they serve.
func TestProviderDispatchLayersComplete(t *testing.T) {
	for providerType := range apiKeyProviderConstructors {
		spec, ok := providerspec.ByType(providerType)
		if !ok {
			t.Errorf("API-key constructor %s is not registered", providerType)
			continue
		}
		if !spec.RequiresAPIKey() || spec.APIKeyEnv == "" {
			t.Errorf("API-key constructor %s has a spec without an API-key environment variable", providerType)
		}
	}
	for providerType := range binProviderConstructors {
		if _, ok := providerspec.ByType(providerType); !ok {
			t.Errorf("bin constructor %s is not registered", providerType)
		}
	}
	for _, spec := range providerspec.Types() {
		_, apiKey := apiKeyProviderConstructors[spec.Type]
		_, bin := binProviderConstructors[spec.Type]
		if apiKey && bin {
			t.Errorf("%s is in both the API-key and bin dispatch layers", spec.Type)
		}
		if spec.FixedToolBridge && !bin {
			t.Errorf("%s declares a fixed tool bridge but is not a CLI-bin provider", spec.Type)
		}
	}
}
