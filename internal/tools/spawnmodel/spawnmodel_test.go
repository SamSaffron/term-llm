package spawnmodel

import (
	"errors"
	"strings"
	"testing"
)

type fakeCatalog struct {
	err       error
	validated []string
}

func (c *fakeCatalog) ValidateSpawnModel(model string) error {
	c.validated = append(c.validated, model)
	return c.err
}

func TestValidateRequiresProviderModel(t *testing.T) {
	for _, model := range []string{
		"requested-model",
		":requested-model",
		"provider:",
		" provider:requested-model",
		"provider:requested-model ",
		"pro vider:requested-model",
		"provider:requested model",
	} {
		catalog := &fakeCatalog{}
		err := Validate(catalog, model)
		if err == nil || !strings.Contains(err.Error(), "provider:model") {
			t.Fatalf("Validate(%q) = %v, want provider:model guidance", model, err)
		}
		if len(catalog.validated) != 0 {
			t.Fatalf("Validate(%q) consulted the catalog for a malformed model", model)
		}
	}
}

func TestValidateDelegatesToCatalog(t *testing.T) {
	if err := Validate(nil, "codex:gpt-5.4"); err != nil {
		t.Fatalf("Validate without catalog = %v, want nil", err)
	}
	if err := Validate(&fakeCatalog{err: errors.New("never")}, ""); err != nil {
		t.Fatalf("Validate(empty model) = %v, want nil", err)
	}

	reject := errors.New(`unknown provider "codex"`)
	catalog := &fakeCatalog{err: reject}
	if err := Validate(catalog, "codex:gpt-5.4"); !errors.Is(err, reject) {
		t.Fatalf("Validate = %v, want catalog error", err)
	}
	if len(catalog.validated) != 1 || catalog.validated[0] != "codex:gpt-5.4" {
		t.Fatalf("catalog validated %v", catalog.validated)
	}
}
