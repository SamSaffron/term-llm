package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

// writeConfigFixture writes a config file and returns its path.
func writeConfigFixture(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func runConfigCheck(t *testing.T, check *ConfigCheck) []Finding {
	t.Helper()
	if check.Config == nil {
		// An explicit config keeps the check away from the user's real
		// configuration and away from viper's global state.
		check.Config = &config.Config{}
	}
	return check.Run(context.Background())
}

func TestConfigCheckReportsMissingFileAsInfo(t *testing.T) {
	findings := runConfigCheck(t, &ConfigCheck{Path: filepath.Join(t.TempDir(), "config.yaml")})

	if len(findings) != 1 || findings[0].Severity != SeverityInfo {
		t.Fatalf("expected a single info finding, got %+v", findings)
	}
}

func TestConfigCheckReportsUnparseableYAMLAsError(t *testing.T) {
	path := writeConfigFixture(t, "default_provider: [unclosed\n")

	findings := runConfigCheck(t, &ConfigCheck{Path: path})

	if len(findings) != 1 || findings[0].Severity != SeverityError {
		t.Fatalf("expected a single error finding, got %+v", findings)
	}
}

func TestConfigCheckReportsUnknownKeyWithSuggestion(t *testing.T) {
	path := writeConfigFixture(t, "deafult_provider: openai\n")

	findings := runConfigCheck(t, &ConfigCheck{Path: path})

	finding, ok := findingWithTitle(findings, "unknown config key")
	if !ok {
		t.Fatalf("expected an unknown key finding, got %+v", findings)
	}
	if finding.Severity != SeverityWarn {
		t.Fatalf("expected a warning, got %s", finding.Severity)
	}
	if !strings.Contains(finding.Detail, `did you mean "default_provider"`) {
		t.Fatalf("expected a suggestion, got %q", finding.Detail)
	}
	if finding.Fix == nil {
		t.Fatal("expected the finding to be fixable")
	}
}

func TestConfigCheckFixCommentsOutUnknownKeyAndKeepsBackup(t *testing.T) {
	original := `# keep this comment
default_provider: openai
typo_section:
  nested: value
sessions:
  enabled: true
`
	path := writeConfigFixture(t, original)

	findings := runConfigCheck(t, &ConfigCheck{Path: path})
	finding, ok := findingWithTitle(findings, `unknown config key "typo_section"`)
	if !ok {
		t.Fatalf("expected the unknown section to be reported, got %+v", findings)
	}
	if err := finding.Fix(context.Background()); err != nil {
		t.Fatalf("apply fix: %v", err)
	}

	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	text := string(updated)
	if !strings.Contains(text, "# keep this comment") {
		t.Fatalf("fix dropped an unrelated comment:\n%s", text)
	}
	if !strings.Contains(text, "# typo_section:") || !strings.Contains(text, "#   nested: value") {
		t.Fatalf("expected the whole block to be commented out:\n%s", text)
	}
	if !strings.Contains(text, "sessions:\n  enabled: true") {
		t.Fatalf("fix disturbed a later section:\n%s", text)
	}

	matches, err := filepath.Glob(path + ".doctor-bak-*")
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected exactly one backup, got %v (err %v)", matches, err)
	}
	backup, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(backup) != original {
		t.Fatalf("backup does not hold the original file:\n%s", backup)
	}

	if _, stillReported := findingWithTitle(runConfigCheck(t, &ConfigCheck{Path: path}), "unknown config key"); stillReported {
		t.Fatal("expected the commented-out key to stop being reported")
	}
}

func TestConfigCheckReportsDeprecatedProviderAlias(t *testing.T) {
	path := writeConfigFixture(t, "provider: openai\n")

	findings := runConfigCheck(t, &ConfigCheck{Path: path})

	finding, ok := findingWithTitle(findings, "deprecated config key")
	if !ok {
		t.Fatalf("expected the alias to be reported, got %+v", findings)
	}
	if finding.Severity != SeverityInfo {
		t.Fatalf("the alias still works, so it should be informational, got %s", finding.Severity)
	}
}

func TestConfigCheckReportsUnsetEnvironmentReference(t *testing.T) {
	path := writeConfigFixture(t, "providers:\n  openai:\n    api_key: ${TERM_LLM_DOCTOR_TEST_KEY}\n")
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"openai": {APIKey: "${TERM_LLM_DOCTOR_TEST_KEY}"},
	}}

	findings := runConfigCheck(t, &ConfigCheck{Path: path, Config: cfg})

	if _, ok := findingWithTitle(findings, "unset $TERM_LLM_DOCTOR_TEST_KEY"); !ok {
		t.Fatalf("expected the unset variable to be reported, got %+v", findings)
	}

	t.Setenv("TERM_LLM_DOCTOR_TEST_KEY", "sk-test")
	if _, ok := findingWithTitle(runConfigCheck(t, &ConfigCheck{Path: path, Config: cfg}), "unset $"); ok {
		t.Fatal("expected no finding once the variable is set")
	}
}

func TestConfigCheckReportsFailingDeferredValue(t *testing.T) {
	path := writeConfigFixture(t, "providers:\n  openai:\n    api_key: $(exit 7)\n")
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"openai": {APIKey: "$(exit 7)"},
	}}

	findings := runConfigCheck(t, &ConfigCheck{Path: path, Config: cfg, ResolveDeferred: true})

	finding, ok := findingWithTitle(findings, "api_key cannot be resolved")
	if !ok {
		t.Fatalf("expected the failing command to be reported, got %+v", findings)
	}
	if finding.Severity != SeverityError {
		t.Fatalf("a credential that cannot resolve breaks every request, want error, got %s", finding.Severity)
	}

	skipped := runConfigCheck(t, &ConfigCheck{Path: path, Config: cfg})
	if _, ok := findingWithTitle(skipped, "cannot be resolved"); ok {
		t.Fatal("expected resolution to be skipped when disabled")
	}
}

// Loaded config carries every built-in provider because defaults populate the
// map. Only providers the file declares (plus the default) may be reported, or
// doctor would lecture about providers the user never configured.
func TestConfigCheckIgnoresProvidersTheFileDoesNotDeclare(t *testing.T) {
	path := writeConfigFixture(t, "providers:\n  openai: {}\n")
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"openai": {},
		"venice": {},
	}}
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("VENICE_API_KEY", "")

	findings := runConfigCheck(t, &ConfigCheck{Path: path, Config: cfg})

	if _, ok := findingWithTitle(findings, `provider "venice"`); ok {
		t.Fatalf("undeclared providers must be ignored, got %+v", findings)
	}
	if _, ok := findingWithTitle(findings, `provider "openai"`); !ok {
		t.Fatalf("expected the declared provider to be checked, got %+v", findings)
	}
}

func TestConfigCheckReportsUnknownDefaultProvider(t *testing.T) {
	path := writeConfigFixture(t, "default_provider: nope\n")
	cfg := &config.Config{DefaultProvider: "nope"}

	findings := runConfigCheck(t, &ConfigCheck{Path: path, Config: cfg})

	finding, ok := findingWithTitle(findings, "default_provider")
	if !ok {
		t.Fatalf("expected the unknown default provider to be reported, got %+v", findings)
	}
	if finding.Severity != SeverityError {
		t.Fatalf("expected an error, got %s", finding.Severity)
	}
}

func TestConfigCheckSkipsCredentialCheckForExternallyAuthenticatedProviders(t *testing.T) {
	path := writeConfigFixture(t, "default_provider: bedrock\nproviders:\n  bedrock:\n    region: us-east-1\n  ollama: {}\n")
	cfg := &config.Config{
		DefaultProvider: "bedrock",
		Providers: map[string]config.ProviderConfig{
			"bedrock": {Region: "us-east-1"},
			"ollama":  {},
		},
	}

	findings := runConfigCheck(t, &ConfigCheck{Path: path, Config: cfg})

	if _, ok := findingWithTitle(findings, "no usable credential"); ok {
		t.Fatalf("providers that authenticate outside term-llm must not be flagged, got %+v", findings)
	}
}

func TestConfigCheckFlagsMissingCredentialOnlyForTheDefaultProvider(t *testing.T) {
	path := writeConfigFixture(t, "default_provider: openai\nproviders:\n  openai: {}\n  openrouter: {}\n")
	cfg := &config.Config{
		DefaultProvider: "openai",
		Providers: map[string]config.ProviderConfig{
			"openai":     {},
			"openrouter": {},
		},
	}
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")

	findings := runConfigCheck(t, &ConfigCheck{Path: path, Config: cfg})

	openai, ok := findingWithTitle(findings, `provider "openai" has no usable credential`)
	if !ok {
		t.Fatalf("expected the default provider to be reported, got %+v", findings)
	}
	if openai.Severity != SeverityWarn {
		t.Fatalf("expected the default provider to warn, got %s", openai.Severity)
	}
	other, ok := findingWithTitle(findings, `provider "openrouter" has no usable credential`)
	if !ok {
		t.Fatalf("expected the unused provider to be noted, got %+v", findings)
	}
	if other.Severity != SeverityInfo {
		t.Fatalf("expected an unused provider to be informational, got %s", other.Severity)
	}
}
