package contain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncImageWritesAgentAsset(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	result, err := SyncImage("agent", false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Name != "agent" {
		t.Fatalf("Name = %q", result.Name)
	}
	dockerfile, err := AgentImageDockerfilePath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dockerfile)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{managedImageMarker, "FROM archlinux:latest", "bash-completion", "term-llm config completion bash > /usr/share/bash-completion/completions/term-llm", "curl -fsSL https://raw.githubusercontent.com/samsaffron/term-llm/main/install.sh", "TERM_LLM_INSTALL_DIR=/usr/local/bin sh", "git clone https://github.com/samsaffron/term-llm.git /root/source/term-llm", "COPY bootstrap/ /opt/term-llm/bootstrap/", "COPY entrypoint.sh /entrypoint.sh", "ENTRYPOINT [\"/entrypoint.sh\"]", "https://claude.ai/install.sh", "/usr/local/bin/claude --version"} {
		if !strings.Contains(text, want) {
			t.Fatalf("Dockerfile missing %q", want)
		}
	}
	for _, rel := range []string{"entrypoint.sh", "bootstrap/bootstrap.yaml", "bootstrap/system.md", "bootstrap/services/webui/run", "bootstrap/services/jobs/run", "bootstrap/skills/memory/SKILL.md", "bootstrap/skills/jobs/SKILL.md", "bootstrap/scripts/update.sh"} {
		data, err := os.ReadFile(filepath.Join(result.Dir, rel))
		if err != nil {
			t.Fatalf("missing synced asset %s: %v", rel, err)
		}
		if strings.Contains(string(data), managedImageMarker) {
			t.Fatalf("synced non-Dockerfile asset %s should not contain managed marker", rel)
		}
		if rel == "entrypoint.sh" && !strings.Contains(string(data), "TERM_LLM_CHATGPT_OAUTH_JSON_B64") {
			t.Fatalf("entrypoint missing ChatGPT OAuth hydration")
		}
		if rel == "entrypoint.sh" && !strings.Contains(string(data), "default_provider") {
			t.Fatalf("entrypoint missing first-boot default_provider config generation")
		}
		if rel == "entrypoint.sh" && (!strings.Contains(string(data), "skills:") || !strings.Contains(string(data), "enabled: true") || !strings.Contains(string(data), "auto_invoke: true")) {
			t.Fatalf("entrypoint missing first-boot skills config generation")
		}
		if rel == "entrypoint.sh" && !strings.Contains(string(data), "chatgpt:gpt-5.4-mini") {
			t.Fatalf("entrypoint missing ChatGPT image provider bootstrap")
		}
		if rel == "entrypoint.sh" {
			for _, want := range []string{"TERM_LLM_CLAUDE_CODE_OAUTH_TOKEN", `provider" = "claude-bin"`, "providers:", "CLAUDE_CODE_OAUTH_TOKEN:"} {
				if !strings.Contains(string(data), want) {
					t.Fatalf("entrypoint missing claude-bin token bootstrap %q", want)
				}
			}
		}
		if rel == "bootstrap/bootstrap.yaml" && (!strings.Contains(string(data), "image_generate") || !strings.Contains(string(data), "show_image") || !strings.Contains(string(data), "view_image")) {
			t.Fatalf("agent bootstrap missing image generation/viewing tools")
		}
		if (rel == "bootstrap/services/webui/run" || rel == "bootstrap/services/jobs/run") && !strings.Contains(string(data), "TERM_LLM_PROVIDER") {
			t.Fatalf("service %s missing TERM_LLM_PROVIDER forwarding", rel)
		}
		if rel == "bootstrap/system.md" {
			if !strings.Contains(string(data), "~/source/term-llm") {
				t.Fatalf("system prompt missing term-llm source path")
			}
			for _, want := range []string{"## Jobs and Services", "term-llm jobs list", "bootstrap-jobs", "runit"} {
				if !strings.Contains(string(data), want) {
					t.Fatalf("system prompt missing jobs/services detail %q", want)
				}
			}
		}
		if rel == "bootstrap/skills/jobs/SKILL.md" {
			for _, want := range []string{"name: jobs", "term-llm jobs create", "term-llm jobs runs", "runner_type"} {
				if !strings.Contains(string(data), want) {
					t.Fatalf("jobs skill missing %q", want)
				}
			}
		}
	}
	if _, err := SyncImage("agent", false); err != nil {
		t.Fatalf("second SyncImage should update marker-free non-Dockerfile assets: %v", err)
	}
	if !strings.HasPrefix(dockerfile, filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm", "images", "agent")) {
		t.Fatalf("Dockerfile path = %q", dockerfile)
	}
}

func TestSyncImageRefusesNonManagedExistingFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dockerfile, err := AgentImageDockerfilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dockerfile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dockerfile, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncImage("agent", false); err == nil || !strings.Contains(err.Error(), "refusing to overwrite non-managed") {
		t.Fatalf("SyncImage error = %v", err)
	}
	if _, err := SyncImage("agent", true); err != nil {
		t.Fatalf("SyncImage force error = %v", err)
	}
}

func TestSyncImageDefaultAndUnknown(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := SyncImage("", false); err != nil {
		t.Fatalf("default SyncImage error = %v", err)
	}
	if _, err := SyncImage("nope", false); err == nil || !strings.Contains(err.Error(), "agent") {
		t.Fatalf("unknown image error = %v", err)
	}
}

func TestAgentImageHashIsStableAndShort(t *testing.T) {
	first, err := AgentImageHash()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 12 {
		t.Fatalf("AgentImageHash length = %d, want 12 (got %q)", len(first), first)
	}
	for _, c := range first {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("AgentImageHash %q contains non-hex char %q", first, c)
		}
	}
	// Determinism: repeated calls must return the same fingerprint as long as
	// the embedded image content has not changed. This is what makes the hash
	// safe to bake into image tags.
	for i := 0; i < 3; i++ {
		again, err := AgentImageHash()
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("AgentImageHash not deterministic: %q vs %q", first, again)
		}
	}
}
