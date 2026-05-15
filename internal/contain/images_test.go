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
	for _, want := range []string{managedImageMarker, "ARG AGENT_BASE_IMAGE=archlinux:latest", "FROM ${AGENT_BASE_IMAGE}", "bash-completion", "zsh", "kitty-terminfo", "alias tl=term-llm", "compdef tl=term-llm", "term-llm config completion bash > /usr/share/bash-completion/completions/term-llm", "term-llm config completion zsh > /usr/share/zsh/site-functions/_term-llm", "curl -fsSL https://raw.githubusercontent.com/samsaffron/term-llm/main/install.sh", "TERM_LLM_INSTALL_DIR=/usr/local/bin sh", "useradd -m -s /bin/zsh -G wheel agent", "agent ALL=(ALL) NOPASSWD: ALL", "sudo -Hu agent playwright install chromium", "git clone https://github.com/samsaffron/term-llm.git /home/agent/source/term-llm", "COPY bootstrap/ /opt/term-llm/bootstrap/", "COPY entrypoint.sh /entrypoint.sh", "ENTRYPOINT [\"/entrypoint.sh\"]", "https://claude.ai/install.sh", "/home/agent/.local/bin/claude", "/usr/local/bin/claude --version"} {
		if !strings.Contains(text, want) {
			t.Fatalf("Dockerfile missing %q", want)
		}
	}
	archData, err := os.ReadFile(filepath.Join(result.Dir, "Dockerfile.arch"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(archData), managedImageMarker) || !strings.Contains(string(archData), "ARG AGENT_BASE_IMAGE=archlinux:latest") {
		t.Fatalf("Dockerfile.arch was not synced as managed Arch Dockerfile")
	}
	fedoraData, err := os.ReadFile(filepath.Join(result.Dir, "Dockerfile.fedora"))
	if err != nil {
		t.Fatal(err)
	}
	fedoraText := string(fedoraData)
	for _, want := range []string{managedImageMarker, "ARG AGENT_BASE_IMAGE=fedora:43", "FROM ${AGENT_BASE_IMAGE}", "dnf -y upgrade", "git", "gcc-c++", "diffutils", "uv pip install playwright --system", "https://github.com/void-linux/runit.git", "gcc -std=gnu17 -O2 -Wall -Wno-error=incompatible-pointer-types -Wno-error=implicit-function-declaration", "sed -i '1i#include <grp.h>' chkshsgr.c", "make runsvdir runsv sv utmpset", "install -m 0755 runsvdir runsv sv utmpset", "/etc/bashrc", "/etc/zshrc"} {
		if !strings.Contains(fedoraText, want) {
			t.Fatalf("Dockerfile.fedora missing %q", want)
		}
	}
	for _, rel := range []string{"entrypoint.sh", "bootstrap/bootstrap.yaml", "bootstrap/system.md", "bootstrap/soul.md", "bootstrap/services/webui/run", "bootstrap/services/jobs/run", "bootstrap/services/bootstrap-jobs/run", "bootstrap/skills/memory/SKILL.md", "bootstrap/skills/jobs/SKILL.md", "bootstrap/skills/self/SKILL.md", "bootstrap/skills/widgets/SKILL.md", "bootstrap/memory/recent.md", "bootstrap/scripts/update.sh"} {
		data, err := os.ReadFile(filepath.Join(result.Dir, rel))
		if err != nil {
			t.Fatalf("missing synced asset %s: %v", rel, err)
		}
		if strings.Contains(string(data), managedImageMarker) {
			t.Fatalf("synced non-Dockerfile asset %s should not contain managed marker", rel)
		}
		if rel == "entrypoint.sh" && !strings.Contains(string(data), "CONFIG_DIR=\"/home/agent/.config/term-llm\"") {
			t.Fatalf("entrypoint should use agent home for persistent config")
		}
		if rel == "entrypoint.sh" && !strings.Contains(string(data), "alias tl=term-llm") {
			t.Fatalf("entrypoint should seed tl alias for persisted agent home")
		}
		if rel == "entrypoint.sh" && (!strings.Contains(string(data), "PROMPT='%F{cyan}${AGENT_NAME:-agent}%f:%F{yellow}%~%f %# '") || !strings.Contains(string(data), "bindkey '^?' backward-delete-char") || !strings.Contains(string(data), "infocmp \"$TERM\"") || !strings.Contains(string(data), "compdef tl=term-llm")) {
			t.Fatalf("entrypoint should seed a friendly zsh prompt and backspace bindings")
		}
		if rel == "entrypoint.sh" && !strings.Contains(string(data), "chown -R \"$AGENT_USER:$AGENT_USER\" \"$AGENT_HOME\"") {
			t.Fatalf("entrypoint should ensure agent owns persistent home")
		}
		if rel == "entrypoint.sh" && !strings.Contains(string(data), "TERM_LLM_CHATGPT_OAUTH_JSON_B64") {
			t.Fatalf("entrypoint missing ChatGPT OAuth hydration")
		}
		if rel == "entrypoint.sh" && !strings.Contains(string(data), "copy_tree_once \"$BOOTSTRAP_DIR/memory\" \"$AGENT_DIR/memory\"") {
			t.Fatalf("entrypoint missing memory seed copy")
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
		if rel == "entrypoint.sh" && !strings.Contains(string(data), "model: gpt-5.5-medium") {
			t.Fatalf("entrypoint missing ChatGPT LLM model bootstrap")
		}
		if rel == "entrypoint.sh" {
			for _, want := range []string{"default_provider", "model: opus-xhigh", "skills:"} {
				if !strings.Contains(string(data), want) {
					t.Fatalf("entrypoint missing first-boot config content %q", want)
				}
			}
		}
		if rel == "entrypoint.sh" {
			if strings.Contains(string(data), "CLAUDE_CODE_OAUTH_TOKEN:") || strings.Contains(string(data), "claude_code_oauth_token:") {
				t.Fatalf("entrypoint should not persist Claude OAuth token under providers.env in config.yaml")
			}
		}
		if rel == "bootstrap/bootstrap.yaml" && (!strings.Contains(string(data), "image_generate") || !strings.Contains(string(data), "show_image") || !strings.Contains(string(data), "view_image")) {
			t.Fatalf("agent bootstrap missing image generation/viewing tools")
		}
		if (rel == "bootstrap/services/webui/run" || rel == "bootstrap/services/jobs/run") && !strings.Contains(string(data), "TERM_LLM_PROVIDER") {
			t.Fatalf("service %s missing TERM_LLM_PROVIDER forwarding", rel)
		}
		if (rel == "bootstrap/services/webui/run" || rel == "bootstrap/services/jobs/run") && !strings.Contains(string(data), "exec sudo -E -Hu agent") {
			t.Fatalf("service %s should re-exec as agent user while preserving provider credentials", rel)
		}
		if rel == "bootstrap/services/webui/run" && !strings.Contains(string(data), "--files-dir /home/agent/Files") {
			t.Fatalf("webui should serve files from agent home")
		}
		if rel == "bootstrap/services/webui/run" && !strings.Contains(string(data), "--enable-widgets") {
			t.Fatalf("webui should enable widgets")
		}
		if rel == "bootstrap/services/webui/run" && !strings.Contains(string(data), "--widgets-dir /home/agent/.config/term-llm/widgets") {
			t.Fatalf("webui should use the persistent agent widgets dir")
		}
		if rel == "bootstrap/services/bootstrap-jobs/run" && (!strings.Contains(string(data), "exec sudo -Hu agent") || !strings.Contains(string(data), `\"command\": \"sudo\"`) || !strings.Contains(string(data), `\"system-upgrade\"`) || !strings.Contains(string(data), `"pacman"`) || !strings.Contains(string(data), `"dnf"`)) {
			t.Fatalf("bootstrap jobs should run as agent and use sudo for package upgrades")
		}
		if rel == "bootstrap/system.md" {
			if !strings.Contains(string(data), "~/source/term-llm") {
				t.Fatalf("system prompt missing term-llm source path")
			}
			for _, want := range []string{"## REMOVE AFTER ONBOARDING", "Learn what the user prefers to be called", "persistent memory", "## /REMOVE AFTER ONBOARDING"} {
				if !strings.Contains(string(data), want) {
					t.Fatalf("system prompt missing onboarding detail %q", want)
				}
			}
			for _, want := range []string{"## Action Discipline", "inspect, act, verify, summarize", "vague future-tense promises"} {
				if !strings.Contains(string(data), want) {
					t.Fatalf("system prompt missing action discipline detail %q", want)
				}
			}
			for _, want := range []string{"## Jobs and Services", "term-llm jobs list", "bootstrap-jobs", "runit", "memory/recent.md"} {
				if !strings.Contains(string(data), want) {
					t.Fatalf("system prompt missing jobs/services detail %q", want)
				}
			}
		}
		if rel == "bootstrap/soul.md" {
			for _, want := range []string{"Never announce work and then stop", "promise without", "worse than silence"} {
				if !strings.Contains(string(data), want) {
					t.Fatalf("soul prompt missing action discipline detail %q", want)
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
		if rel == "bootstrap/skills/widgets/SKILL.md" {
			for _, want := range []string{"name: widgets", "--enable-widgets", "--widgets-dir /home/agent/.config/term-llm/widgets", "/chat/widgets/<widget-name>/", "sudo sv restart /etc/runit/runsvdir/webui"} {
				if !strings.Contains(string(data), want) {
					t.Fatalf("widgets skill missing %q", want)
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
