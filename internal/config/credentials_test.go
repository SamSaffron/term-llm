package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialRefResolvesLiteralEnvAndDeferredValues(t *testing.T) {
	ResetCredentialCache()
	t.Cleanup(ResetCredentialCache)

	t.Setenv("TERM_LLM_TEST_KEY", "env-key")

	if got, err := Cred("literal").Resolve(); err != nil || got != "literal" {
		t.Fatalf("literal = %q (err %v)", got, err)
	}
	if got, err := Cred("", "TERM_LLM_TEST_KEY").Resolve(); err != nil || got != "env-key" {
		t.Fatalf("env fallback = %q (err %v)", got, err)
	}
	if got, err := Cred("${TERM_LLM_TEST_KEY}").Resolve(); err != nil || got != "env-key" {
		t.Fatalf("env reference = %q (err %v)", got, err)
	}
	if got, err := Cred("$(printf deferred-key)").Resolve(); err != nil || got != "deferred-key" {
		t.Fatalf("command value = %q (err %v)", got, err)
	}

	secretFile := filepath.Join(t.TempDir(), "key.txt")
	if err := os.WriteFile(secretFile, []byte("file-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := Cred("file://" + secretFile).Resolve(); err != nil || got != "file-key" {
		t.Fatalf("file value = %q (err %v)", got, err)
	}
}

func TestCredentialRefConfiguredDoesNotResolve(t *testing.T) {
	ResetCredentialCache()
	t.Cleanup(ResetCredentialCache)

	marker := filepath.Join(t.TempDir(), "resolved")
	ref := Cred("$(touch " + marker + "; printf secret)")

	if !ref.Configured() {
		t.Fatal("deferred credential should report as configured")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("Configured() must not execute the deferred command")
	}

	got, err := ref.Resolve()
	if err != nil || got != "secret" {
		t.Fatalf("Resolve() = %q (err %v)", got, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("Resolve() should have executed the command: %v", err)
	}
}

func TestCredentialRefResolveIsMemoized(t *testing.T) {
	ResetCredentialCache()
	t.Cleanup(ResetCredentialCache)

	counter := filepath.Join(t.TempDir(), "count")
	ref := Cred("$(printf x >> " + counter + "; printf secret)")

	for i := 0; i < 3; i++ {
		if got, err := ref.Resolve(); err != nil || got != "secret" {
			t.Fatalf("Resolve() = %q (err %v)", got, err)
		}
	}

	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 1 {
		t.Fatalf("deferred value resolved %d times, want 1", len(data))
	}
}

func TestCredentialRefConfiguredEnvSemantics(t *testing.T) {
	ResetCredentialCache()
	t.Cleanup(ResetCredentialCache)

	if Cred("").Configured() {
		t.Fatal("empty credential should not be configured")
	}
	if Cred("", "TERM_LLM_TEST_MISSING").Configured() {
		t.Fatal("credential with unset env fallback should not be configured")
	}
	if Cred("${TERM_LLM_TEST_MISSING}").Configured() {
		t.Fatal("reference to unset env var should not be configured")
	}

	t.Setenv("TERM_LLM_TEST_MISSING", "value")
	if !Cred("${TERM_LLM_TEST_MISSING}").Configured() {
		t.Fatal("reference to set env var should be configured")
	}
}

func TestResolveFirstCredentialPreservesPrecedence(t *testing.T) {
	ResetCredentialCache()
	t.Cleanup(ResetCredentialCache)

	primary := Cred("", "TERM_LLM_TEST_PRIMARY")
	secondary := Cred("secondary-key")

	got, err := ResolveFirstCredential(primary, secondary)
	if err != nil || got != "secondary-key" {
		t.Fatalf("cascade = %q (err %v), want secondary-key", got, err)
	}

	t.Setenv("TERM_LLM_TEST_PRIMARY", "primary-key")
	got, err = ResolveFirstCredential(Cred("", "TERM_LLM_TEST_PRIMARY"), secondary)
	if err != nil || got != "primary-key" {
		t.Fatalf("cascade = %q (err %v), want primary-key", got, err)
	}
}

func TestLoadConfigDoesNotResolveFeatureCredentials(t *testing.T) {
	ResetCredentialCache()
	t.Cleanup(ResetCredentialCache)

	dir := t.TempDir()
	marker := filepath.Join(dir, "resolved")
	configYAML := "default_provider: openai\n" +
		"image:\n  venice:\n    api_key: \"$(touch " + marker + "; printf image-key)\"\n" +
		"search:\n  brave:\n    api_key: \"$(touch " + marker + "; printf search-key)\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", dir)

	// The config lives at <XDG_CONFIG_HOME>/term-llm/config.yaml.
	if err := os.MkdirAll(filepath.Join(dir, "term-llm"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "config.yaml"), filepath.Join(dir, "term-llm", "config.yaml")); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.HasPrefix(cfg.Image.Venice.APIKey, "$(") {
		t.Fatalf("test config was not loaded: image.venice.api_key = %q", cfg.Image.Venice.APIKey)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("loading config must not resolve image/search credentials")
	}

	if got, err := cfg.Image.VeniceKey().Resolve(); err != nil || got != "image-key" {
		t.Fatalf("image key = %q (err %v)", got, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("image key resolution should have run the command: %v", err)
	}
}
