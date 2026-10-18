package cmd

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestModelsRefreshFlag(t *testing.T) {
	t.Parallel()
	if flag := modelsCmd.Flags().Lookup("refresh"); flag == nil {
		t.Fatal("models command missing --refresh")
	}
}

func TestModelsFlagDefaults(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, value string }{
		{"provider", ""},
		{"json", "false"},
		{"refresh", "false"},
	} {
		flag := modelsCmd.Flags().Lookup(tc.name)
		if flag == nil || flag.DefValue != tc.value {
			t.Errorf("%s default = %v, want %q", tc.name, flag, tc.value)
		}
	}
}

// TestModelsRunSeesParsedFlags guards against binding RunE to a copy of the
// options taken before Cobra parses flags (a value-receiver method value).
func TestModelsRunSeesParsedFlags(t *testing.T) {
	saved := modelsOptions
	t.Cleanup(func() {
		modelsOptions = saved
		_ = modelsCmd.Flags().Set("provider", "")
	})
	modelsOptions.LoadConfig = func() (*config.Config, error) {
		return &config.Config{DefaultProvider: "default-sentinel-provider"}, nil
	}
	if err := modelsCmd.Flags().Set("provider", "flag-sentinel-provider"); err != nil {
		t.Fatal(err)
	}
	err := modelsCmd.RunE(modelsCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "flag-sentinel-provider") {
		t.Fatalf("models ignored --provider: err = %v", err)
	}
}
