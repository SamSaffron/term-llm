package cmd

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/providerspec"
)

func TestModelListerRegistryComplete(t *testing.T) {
	for _, spec := range providerspec.Types() {
		constructor := modelListerConstructors[spec.ModelListing]
		if spec.ListModels && (spec.ModelListing == "" || constructor == nil) {
			t.Errorf("%s supports model listing but has no constructor/behavior", spec.Type)
		}
		if spec.ModelListing == "api_key" && apiKeyModelListers[spec.Type] == nil {
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
