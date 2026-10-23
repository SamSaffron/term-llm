package responsesproviders

import (
	"encoding/json"
	"fmt"

	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseGrokCatalogRequiresResponsesBackend(t *testing.T) {
	models, err := parseGrokModels([]byte(`{"data":[
		{"model":"missing"},
		{"model":"chat","api_backend":"chat_completions"},
		{"model":"mixed-case","api_backend":"ReSpOnSeS"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "mixed-case" {
		t.Fatalf("Responses-compatible models = %+v, want only mixed-case", models)
	}
}

func TestParseGrokCatalogFiltersUnknownSafeEfforts(t *testing.T) {
	models, err := parseGrokModels([]byte(`{"data":[
		{"model":"future","api_backend":"responses","reasoning_efforts":[{"value":"high"},{"value":"turbo"},{"value":"LOW"}]},
		{"model":"future-only","api_backend":"responses","reasoning_efforts":[{"value":"adaptive"}]}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v, want both future models retained", models)
	}
	if got := strings.Join(models[0].ReasoningEfforts, ","); got != "high,low" {
		t.Fatalf("known efforts = %q, want high,low", got)
	}
	if len(models[1].ReasoningEfforts) != 0 {
		t.Fatalf("unknown-only efforts = %v, want none", models[1].ReasoningEfforts)
	}
}

func TestParseGrokCatalogRejectsLimitsAndUnsafeIDs(t *testing.T) {
	isolateGrokLLMTestEnv(t)
	var records strings.Builder
	for i := 0; i < maxGrokModels+1; i++ {
		if i > 0 {
			records.WriteByte(',')
		}
		fmt.Fprintf(&records, `{"model":"m%d","api_backend":"responses"}`, i)
	}
	if _, err := parseGrokModels([]byte(`{"data":[` + records.String() + `]}`)); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("model count error = %v", err)
	}
	for _, id := range []string{"bad id", "bad\nheader", strings.Repeat("x", 257)} {
		body := `{"data":[{"model":` + fmt.Sprintf("%q", id) + `,"api_backend":"responses"}]}`
		if _, err := parseGrokModels([]byte(body)); err == nil {
			t.Fatalf("unsafe ID accepted: %q", id)
		}
	}
	if _, err := parseGrokModels(make([]byte, maxGrokModelsBodyBytes+1)); err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Fatalf("body bound error = %v", err)
	}
	for _, body := range []string{
		`{"data":[{"model":"safe","name":"bad\u001b[31m","api_backend":"responses"}]}`,
		`{"data":[{"model":"safe","name":"bad\u202ename","api_backend":"responses"}]}`,
		`{"data":[{"model":"safe","api_backend":"responses","reasoning_efforts":[{"value":"high\n"}]}]}`,
	} {
		if _, err := parseGrokModels([]byte(body)); err == nil {
			t.Fatalf("unsafe display metadata accepted: %s", body)
		}
	}
}

func TestParseGrokCatalogFallsBackFromEmptyDataToModels(t *testing.T) {
	isolateGrokLLMTestEnv(t)
	models, err := parseGrokModels([]byte(`{"data":[],"models":[{"model":"grok-alt","api_backend":"responses","context_window":12345}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "grok-alt" || models[0].InputLimit != 12345 {
		t.Fatalf("alternate models = %+v", models)
	}
}

func TestGrokModelCacheSecurityAndAccountIsolation(t *testing.T) {
	isolateGrokLLMTestEnv(t)
	cache := grokModelsCache{AccountID: "acct_1", FetchedAt: time.Now(), Models: []ModelInfo{{ID: "grok-4.6", DisplayName: "Grok 4.6", InputLimit: 500_000, OutputLimit: 8_000}}}
	if err := saveGrokModelsCache(cache); err != nil {
		t.Fatal(err)
	}
	path, err := grokModelsCachePath()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("cache mode = %o, want 600", info.Mode().Perm())
	}
	if _, err := loadGrokModelsCache("acct_2"); err == nil {
		t.Fatal("another account loaded cached catalog")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted map[string]any
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted["origin"] != grokModelsURL || persisted["client_version"] != "1.0.13" {
		t.Fatalf("cache protocol metadata = origin %v version %v", persisted["origin"], persisted["client_version"])
	}
	for field, mismatch := range map[string]string{
		"origin":         "https://other.grok.example/v1/models",
		"client_version": "0.0.0",
	} {
		var mutated map[string]any
		if err := json.Unmarshal(data, &mutated); err != nil {
			t.Fatal(err)
		}
		mutated[field] = mismatch
		encoded, err := json.Marshal(mutated)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadGrokModelsCache("acct_1"); err == nil {
			t.Fatalf("cache with mismatched %s was accepted", field)
		}
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadGrokModelsCache("acct_1"); err == nil || !strings.Contains(err.Error(), "insecure permissions") {
		t.Fatalf("insecure cache error = %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "target"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := loadGrokModelsCache("acct_1"); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlink cache error = %v", err)
	}
}
