package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
)

func TestMemoryPolicyPromotionExactOutputAndWatermark(t *testing.T) {
	requireMemoryPolicyShell(t)
	for _, tc := range []struct {
		name, output      string
		reject, wantError bool
		credentialsOnly   bool
		cap               int
	}{
		{name: "reject", output: "# Proposed\n\nA durable observation.", reject: true, wantError: true, cap: 1000},
		{name: "over cap", output: strings.Repeat("word ", 100), wantError: true, cap: 100},
		{name: "credentials only", output: "unexpected response", cap: 1000, credentialsOnly: true},
		{name: "accept exact", output: "# Proposed\n\nA durable observation.", cap: 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			tmp := t.TempDir()
			agentDir := filepath.Join(tmp, "agent")
			if err := os.MkdirAll(filepath.Join(agentDir, "memory"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte("name: policy-test\ndescription: test\n"), 0600); err != nil {
				t.Fatal(err)
			}
			recent := filepath.Join(agentDir, "memory", "recent.md")
			if err := os.WriteFile(recent, []byte("known good recent"), 0600); err != nil {
				t.Fatal(err)
			}
			guard := filepath.Join(tmp, "guard")
			capture := filepath.Join(tmp, "capture.json")
			exit := "exit 0"
			if tc.reject {
				exit = "exit 1"
			}
			script := "#!/bin/sh\nread -r payload\nprintf '%s' \"$payload\" > '" + capture + "'\ncase \"$payload\" in *'\"mode\":\"input\"'*) printf '{\"content\":\"sanitized prompt\"}';; *) " + exit + ";; esac\n"
			if err := os.WriteFile(guard, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv(memoryGuardEnv, guard)
			store, err := memorydb.NewStore(memorydb.Config{Path: filepath.Join(tmp, "memory.db")})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			oldWatermark := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
			if err := store.SetMeta(ctx, memoryPromoteMetaKey(agentDir), oldWatermark); err != nil {
				t.Fatal(err)
			}
			fragmentPath := "fragments/notes/test.md"
			if tc.credentialsOnly {
				fragmentPath = "fragments/credentials/test.md"
			}
			if err := store.CreateFragment(ctx, &memorydb.Fragment{Agent: agentDir, Path: fragmentPath, Content: "fact"}); err != nil {
				t.Fatal(err)
			}
			provider := mockprovider.NewMockProvider("policy").AddTextResponse(tc.output)
			_, err = runMemoryPromoteFlow(ctx, testEnv("", ""), &config.Config{}, engine.NewEngine(provider, nil), store, memoryPromoteOptions{Agent: agentDir, Since: time.Hour, RecentMaxBytes: tc.cap})
			if (err != nil) != tc.wantError {
				t.Fatalf("unexpected promotion result: %v", err)
			}
			wantRequests := 1
			if tc.credentialsOnly {
				wantRequests = 0
			}
			if len(provider.RecordedRequests()) != wantRequests {
				t.Fatal("promotion did not reach output validation")
			}
			data, err := os.ReadFile(recent)
			if err != nil {
				t.Fatal(err)
			}
			meta, err := store.GetMeta(ctx, memoryPromoteMetaKey(agentDir))
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantError || tc.credentialsOnly {
				if string(data) != "known good recent" || meta != oldWatermark {
					t.Fatal("rejected promotion changed summary or watermark")
				}
			} else {
				if string(data) != tc.output || meta == oldWatermark {
					t.Fatal("accepted promotion did not persist exact output and watermark")
				}
			}
			if tc.name != "over cap" && !tc.credentialsOnly {
				raw, err := os.ReadFile(capture)
				if err != nil {
					t.Fatal(err)
				}
				var payload map[string]string
				if err := json.Unmarshal(raw, &payload); err != nil {
					t.Fatal(err)
				}
				if payload["mode"] != "promotion" || payload["path"] != "recent.md" || payload["content"] != tc.output {
					t.Fatal("policy did not receive exact promotion payload")
				}
			}
		})
	}
}

func TestMemoryPolicyPromotionIncludesOldCanonicalRecentlyEdited(t *testing.T) {
	ctx := context.Background()
	store, err := memorydb.NewStore(memorydb.Config{Path: filepath.Join(t.TempDir(), "memory.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.CreateFragment(ctx, &memorydb.Fragment{Agent: "jarvis", Path: "fragments/projects/old-canonical.md", Content: "fresh observation", CreatedAt: now.Add(-90 * 24 * time.Hour), UpdatedAt: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateFragment(ctx, &memorydb.Fragment{Agent: "jarvis", Path: "fragments/projects/stale.md", Content: "old observation", CreatedAt: now.Add(-90 * 24 * time.Hour), UpdatedAt: now.Add(-60 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	frags, _, err := collectPromoteChangedFragments(ctx, store, "jarvis", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(frags) != 1 || frags[0].Path != "fragments/projects/old-canonical.md" {
		t.Fatalf("promotion omitted edited canonical or included stale record: %v", frags)
	}
}

func TestMemoryPolicyPromotionContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context.md")
	t.Setenv("TERM_LLM_MEMORY_PROMOTION_CONTEXT", path)
	if _, err := appendPromotionContext("prompt"); err == nil {
		t.Fatal("missing context ignored")
	}
	if err := os.WriteFile(path, []byte("dated canonical observation"), 0600); err != nil {
		t.Fatal(err)
	}
	prompt, err := appendPromotionContext("original")
	if err != nil || !strings.Contains(prompt, "dated canonical observation") || !strings.HasPrefix(prompt, "original") {
		t.Fatal("context missing")
	}
	if err := os.WriteFile(path, make([]byte, (1<<20)+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := appendPromotionContext("prompt"); err == nil {
		t.Fatal("oversize context accepted")
	}
}

func TestMemoryPolicyPromotionOmitsCredentialRecords(t *testing.T) {
	prompt := buildPromotePrompt([]memorydb.Fragment{
		{Path: "fragments/credentials/test.md", Content: "synthetic-private-marker"},
		{Path: "fragments/notes/accounts-and-credentials.md", Content: "synthetic-private-marker"},
		{Path: "fragments/projects/test.md", Content: "safe fact"},
	}, "old cache")
	if strings.Contains(prompt, "synthetic-private-marker") || !strings.Contains(prompt, "safe fact") {
		t.Fatal("credential records included or safe record lost")
	}
}

func TestMemoryPolicyPromotionSanitizesCanonicalContext(t *testing.T) {
	requireMemoryPolicyShell(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "context.md")
	if err := os.WriteFile(path, []byte("BEGIN_PRIVATE/SECRET_END"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERM_LLM_MEMORY_PROMOTION_CONTEXT", path)
	guard := filepath.Join(dir, "guard")
	script := `#!/bin/sh
read -r payload
case "$payload" in *'BEGIN_PRIVATE/SECRET_END'*) printf '{"content":"context redacted"}';; *) exit 1;; esac
`
	if err := os.WriteFile(guard, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(memoryGuardEnv, guard)
	prompt, err := appendPromotionContext("original prompt")
	if err != nil {
		t.Fatal(err)
	}
	provider := mockprovider.NewMockProvider("policy").AddTextResponse("# Safe\n\nObservation.")
	eng := engine.NewEngine(provider, nil)
	if _, err := runMemoryPromoteRequest(context.Background(), eng, "", prompt, 100, true); err != nil {
		t.Fatal(err)
	}
	requests := provider.RecordedRequests()
	if len(requests) != 1 || requests[0].Messages[1].Parts[0].Text != "context redacted" || requests[0].DebugRaw {
		t.Fatal("canonical context bypassed sanitization or raw-debug suppression")
	}
	if err := os.WriteFile(guard, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := runMemoryPromoteRequest(context.Background(), eng, "", prompt, 100, true); err == nil {
		t.Fatal("input rejection ignored")
	}
	if len(provider.RecordedRequests()) != 1 {
		t.Fatal("input rejection still called model")
	}
}
