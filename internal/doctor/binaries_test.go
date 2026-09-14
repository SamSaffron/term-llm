package doctor

import (
	"context"
	"os/exec"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestBinaryCheckExplicitList(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not available on PATH")
	}
	missing := "term-llm-doctor-binary-that-cannot-exist-3c82d1"
	if path, err := exec.LookPath(missing); err == nil {
		t.Fatalf("test command unexpectedly exists at %s", path)
	}

	installHint := "install the imaginary doctor test command"
	check := BinaryCheck{Binaries: []BinarySpec{
		{Name: "go", Missing: SeverityError, Purpose: "running Go tests"},
		{Name: missing, Missing: SeverityWarn, Purpose: "testing absent commands", Install: installHint},
	}}
	findings := check.Run(context.Background())

	if len(findings) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(findings), findings)
	}
	if findings[0].Severity != SeverityOK {
		t.Errorf("found binary severity = %s, want %s", findings[0].Severity, SeverityOK)
	}
	if findings[1].Severity != SeverityWarn {
		t.Errorf("missing binary severity = %s, want %s", findings[1].Severity, SeverityWarn)
	}
	if findings[1].Remedy != installHint {
		t.Errorf("missing binary remedy = %q, want %q", findings[1].Remedy, installHint)
	}
}

func TestBinaryConfiguredProviderSpecsSortedAndDeduped(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"a-claude":       {Type: config.ProviderTypeClaudeBin},
		"b-claude-again": {Type: config.ProviderTypeClaudeBin},
		"c-cursor":       {Type: config.ProviderTypeCursorBin},
	}}

	specs := configuredProviderBinaries(cfg)
	if len(specs) != 2 {
		t.Fatalf("got %d specs, want 2: %+v", len(specs), specs)
	}
	want := []string{"claude", "cursor-agent"}
	for i, name := range want {
		if specs[i].Name != name {
			t.Errorf("spec %d name = %q, want %q", i, specs[i].Name, name)
		}
		if specs[i].Missing != SeverityError {
			t.Errorf("spec %q missing severity = %s, want %s", specs[i].Name, specs[i].Missing, SeverityError)
		}
	}
}
