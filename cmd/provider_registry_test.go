package cmd

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/providerspec"
)

func TestModelListerRegistryComplete(t *testing.T) {
	for _, spec := range providerspec.Types() {
		constructor := modelListerConstructors[spec.ModelListing]
		if spec.ListModels && constructor == nil {
			t.Errorf("%s supports model listing but has no constructor/behavior", spec.Type)
		}
		if spec.ModelListing == providerspec.ModelListingAPIKey && apiKeyModelListers[spec.Type] == nil {
			t.Errorf("%s has no API-key model lister", spec.Type)
		}
		if !spec.ListModels && constructor != nil {
			t.Errorf("%s has a lister but listing is disabled", spec.Type)
		}
	}
	for kind := range modelListerConstructors {
		found := false
		for _, spec := range providerspec.Types() {
			found = found || spec.ModelListing == kind
		}
		if !found {
			t.Errorf("unused model-listing behavior %s", kind)
		}
	}
}

func TestAPIKeyModelListersMatchRegistry(t *testing.T) {
	for providerType := range apiKeyModelListers {
		spec, ok := providerspec.ByType(providerType)
		if !ok || spec.ModelListing != providerspec.ModelListingAPIKey {
			t.Errorf("API-key model lister %s is not registered with api_key listing", providerType)
		}
	}
}
