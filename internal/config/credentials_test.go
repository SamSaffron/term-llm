package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

func TestCredentialRefConfiguredIncludesEnvFallback(t *testing.T) {
	t.Setenv("TERM_LLM_PRIMARY", "")
	t.Setenv("TERM_LLM_FALLBACK", "fallback-key")
	for _, raw := range []string{"${TERM_LLM_PRIMARY}", "$TERM_LLM_PRIMARY"} {
		ref := Cred(raw, "TERM_LLM_FALLBACK")
		if !ref.Configured() {
			t.Fatalf("%s should include environment fallback", raw)
		}
		got, err := ResolveFirstCredential(ref, Cred("secondary-account"))
		if err != nil || got != "fallback-key" {
			t.Fatalf("cascade = %q, %v", got, err)
		}
	}
}

func TestCredentialRefRetriesFailedAndEmptyResolution(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprint(empty), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "secret")
			if empty {
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ref := Cred("file://" + path)
			got, err := ref.Resolve()
			if got != "" || (!empty && err == nil) || (empty && err != nil) {
				t.Fatalf("first resolve = %q, %v", got, err)
			}
			if err := os.WriteFile(path, []byte("unlocked"), 0o600); err != nil {
				t.Fatal(err)
			}
			if got, err := ref.Resolve(); err != nil || got != "unlocked" {
				t.Fatalf("retry = %q, %v", got, err)
			}
		})
	}
}

func TestCredentialRefConcurrentResolution(t *testing.T) {
	ResetCredentialCache()
	t.Cleanup(ResetCredentialCache)
	original := lookupSRV
	t.Cleanup(func() { lookupSRV = original })
	var calls atomic.Int32
	started := make(chan struct{}, 16)
	release := make(chan struct{})
	lookupSRV = func(_, _, _ string) (string, []*net.SRV, error) {
		calls.Add(1)
		started <- struct{}{}
		<-release
		return "", []*net.SRV{{Target: "example.test.", Port: 443}}, nil
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := Cred("srv://_credential._tcp.example.test/key").Resolve()
			if err != nil || got != "https://example.test:443/key" {
				t.Errorf("Resolve = %q, %v", got, err)
			}
		}()
	}
	<-started
	close(release)
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("lookups = %d, want 1", got)
	}
}

func TestResolveFirstCredentialDoesNotHideErrors(t *testing.T) {
	ref := Cred("file://" + filepath.Join(t.TempDir(), "missing"))
	if got, err := ResolveFirstCredential(ref, Cred("different-account")); err == nil || got != "" {
		t.Fatalf("broken primary should fail, got %q, %v", got, err)
	}
}

func TestResolveDeferredMap(t *testing.T) {
	t.Setenv("TERM_LLM_MAP_SECRET", "resolved")
	path := filepath.Join(t.TempDir(), "secrets.yml")
	if err := os.WriteFile(path, []byte("token: file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := map[string]string{
		"literal": " $literal ", "bare": "$TERM_LLM_MAP_SECRET",
		"embedded": "Bearer ${TERM_LLM_MAP_SECRET}", "env": "${TERM_LLM_MAP_SECRET}",
		"file": "file://" + path + "#token",
	}
	resolved, err := ResolveDeferredMap(original)
	if err != nil {
		t.Fatal(err)
	}
	if resolved["env"] != "resolved" || resolved["file"] != "file-secret" {
		t.Fatalf("resolved = %#v", resolved)
	}
	for _, key := range []string{"literal", "bare", "embedded"} {
		if resolved[key] != original[key] {
			t.Fatalf("literal %s changed", key)
		}
	}
	resolved["env"] = "changed"
	if original["env"] != "${TERM_LLM_MAP_SECRET}" {
		t.Fatal("resolution mutated original map")
	}
	_, err = ResolveDeferredMap(map[string]string{"Authorization": "file://" + path + "#missing"})
	if err == nil || !strings.Contains(err.Error(), "Authorization") {
		t.Fatalf("missing map error context: %v", err)
	}
}

func TestResolveDeferredMapRejectsEmptyReferences(t *testing.T) {
	t.Setenv("TERM_LLM_EMPTY_MAP", "")
	path := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"${TERM_LLM_EMPTY_MAP}", "file://" + path} {
		if _, err := ResolveDeferredMap(map[string]string{"Authorization": value}); err == nil || !strings.Contains(err.Error(), "Authorization") {
			t.Fatalf("empty explicit reference should fail with map key: %v", err)
		}
	}
	if _, err := ResolveDeferredMap(map[string]string{"OPTIONAL": ""}); err != nil {
		t.Fatalf("literal empty environment value must remain valid: %v", err)
	}
}

func TestCredentialRefEmptyDeferredUsesEnvFallback(t *testing.T) {
	t.Setenv("TERM_LLM_EMPTY_FALLBACK", "env-key")
	path := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := Cred("file://"+path, "TERM_LLM_EMPTY_FALLBACK").Resolve(); err != nil || got != "env-key" {
		t.Fatalf("empty deferred fallback = %q, %v", got, err)
	}
}

func TestCredentialRefPanicDoesNotLeavePendingLookup(t *testing.T) {
	original := lookupSRV
	t.Cleanup(func() { lookupSRV = original })
	ref := Cred("srv://_panic._tcp.example.test")
	lookupSRV = func(_, _, _ string) (string, []*net.SRV, error) { panic("resolver panic") }
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected resolver panic")
			}
		}()
		_, _ = ref.Resolve()
	}()
	lookupSRV = func(_, _, _ string) (string, []*net.SRV, error) {
		return "", []*net.SRV{{Target: "recovered.example.test.", Port: 443}}, nil
	}
	if got, err := ref.Resolve(); err != nil || got != "https://recovered.example.test:443" {
		t.Fatalf("retry after panic = %q, %v", got, err)
	}
}

func TestSecretsYAMLSharedByConfigAndMCP(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	configDir := filepath.Join(dir, "term-llm")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(configDir, "secrets.yml")
	ref := "file://" + secretPath
	configYAML := fmt.Sprintf(`default_provider: openai
providers:
  openai:
    api_key: "%s#openai"
image:
  venice:
    api_key: "%s#venice"
search:
  provider: brave
  brave:
    api_key: "%s#brave"
serve:
  telegram:
    token: "%s#telegram"
`, ref, ref, ref, ref)
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	// Loading and inspecting availability work before secrets.yml even exists.
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Image.VeniceKey().Configured() || !cfg.Search.BraveKey().Configured() || !cfg.Serve.Telegram.TokenRef().Configured() {
		t.Fatal("file references should advertise configuration without reading the file")
	}
	secrets := "openai: sk-example\nvenice: venice-key\nbrave: brave-key\ntelegram: '123456:token'\ngithub: github-token\nmcp_authorization: 'Bearer mcp-token'\n"
	if err := os.WriteFile(secretPath, []byte(secrets), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := cfg.GetProviderConfig("openai")
	if provider == nil {
		t.Fatal("missing provider")
	}
	if err := provider.ResolveForInference(); err != nil || provider.ResolvedAPIKey != "sk-example" {
		t.Fatalf("provider key = %q, %v", provider.ResolvedAPIKey, err)
	}
	for want, cred := range map[string]CredentialRef{
		"venice-key": cfg.Image.VeniceKey(), "brave-key": cfg.Search.BraveKey(), "123456:token": cfg.Serve.Telegram.TokenRef(),
	} {
		if got, err := cred.Resolve(); err != nil || got != want {
			t.Fatalf("feature key = %q, %v; want %q", got, err, want)
		}
	}
	mcp, err := ResolveDeferredMap(map[string]string{"Authorization": ref + "#mcp_authorization", "GITHUB_PERSONAL_ACCESS_TOKEN": ref + "#github"})
	if err != nil || mcp["Authorization"] != "Bearer mcp-token" || mcp["GITHUB_PERSONAL_ACCESS_TOKEN"] != "github-token" {
		t.Fatalf("MCP secrets = %#v, %v", mcp, err)
	}
}
