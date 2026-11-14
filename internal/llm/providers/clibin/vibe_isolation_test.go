package clibin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/acp"
	"github.com/term-llm/term-llm/internal/llm/core"
)

func TestVibeBinProviderIsolatesUserConfiguration(t *testing.T) {
	for _, source := range []string{"default home", "inherited VIBE_HOME", "provider VIBE_HOME"} {
		t.Run(source, func(t *testing.T) {
			logPath := installFakeVibeACP(t)
			userHome := t.TempDir()
			t.Setenv("HOME", userHome)
			t.Setenv("USERPROFILE", userHome)
			t.Setenv("VIBE_HOME", "")
			vibeHome := filepath.Join(userHome, ".vibe")
			var extraEnv map[string]string
			switch source {
			case "inherited VIBE_HOME":
				vibeHome = t.TempDir()
				t.Setenv("VIBE_HOME", vibeHome)
			case "provider VIBE_HOME":
				vibeHome = t.TempDir()
				extraEnv = map[string]string{"VIBE_HOME": vibeHome}
			}
			if err := os.MkdirAll(vibeHome, 0o700); err != nil {
				t.Fatal(err)
			}
			hooksPath := filepath.Join(vibeHome, "hooks.toml")
			hooks := []byte("[[hooks]]\nname = \"sentinel\"\ntype = \"post_agent\"\ncommand = \"touch sentinel\"\n")
			if err := os.WriteFile(hooksPath, hooks, 0o600); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"config.toml", ".env", "plugins/sentinel/plugin.json", "tools/sentinel.py"} {
				path := filepath.Join(vibeHome, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("must not be imported"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			marker := filepath.Join(t.TempDir(), "hook-ran")
			t.Setenv("VIBE_TEST_HOOK_MARKER", marker)
			// Model Vibe's home-based hook discovery in the subprocess, without
			// requiring a live model, keyring, or Python installation. Fail if
			// a tool-free prompt makes the user's hooks visible to that process.
			script := strings.Replace(fakeVibeACP, "turn=0", `
if [ -f "${VIBE_HOME:-$HOME/.vibe}/hooks.toml" ]; then
  touch "$VIBE_TEST_HOOK_MARKER"
fi
echo "ENV VIBE_HOME=$VIBE_HOME" >> "$VIBE_FAKE_LOG"
turn=0`, 1)
			binPath, err := exec.LookPath(vibeACPBinary)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}

			p := NewVibeBinProvider("glm-5-3", extraEnv)
			defer p.CleanupMCP()
			messages := []core.Message{core.UserText("no tools needed")}
			collectVibeTestEvents(t, p, core.Request{Messages: messages})
			state, ok := p.ExportProviderState()
			if !ok {
				t.Fatal("session state missing")
			}
			p.CleanupMCP()
			// The isolated home must survive cleanup and be reused on resume.
			restored := NewVibeBinProvider("glm-5-3", extraEnv)
			defer restored.CleanupMCP()
			if err := restored.ImportProviderState(state); err != nil {
				t.Fatal(err)
			}
			messages = append(messages, core.AssistantText("reply 1"), core.UserText("follow up"))
			collectVibeTestEvents(t, restored, core.Request{Messages: messages})
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("user hook was visible to vibe-acp: %v", err)
			}
			wantHome := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "term-llm", "vibe-bin", "home")
			log, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(log), "ENV VIBE_HOME="+wantHome+"\n") != 2 || !strings.Contains(string(log), `"method":"session/load"`) {
				t.Fatalf("vibe-acp did not reuse its isolated home on resume:\n%s", log)
			}
			info, err := os.Stat(wantHome)
			if err != nil || !info.IsDir() {
				t.Fatalf("isolated home missing: %v", err)
			}
			gotHooks, err := os.ReadFile(hooksPath)
			if err != nil || string(gotHooks) != string(hooks) {
				t.Fatalf("user hooks changed: %q, %v", gotHooks, err)
			}
			for _, name := range []string{"hooks.toml", "config.toml", ".env", "plugins", "tools"} {
				if _, err := os.Stat(filepath.Join(wantHome, name)); !os.IsNotExist(err) {
					t.Fatalf("isolated home must not import %s: %v", name, err)
				}
			}
		})
	}
}

func TestVibeRuntimeHomeRejectsSymlink(t *testing.T) {
	base := t.TempDir()
	target := t.TempDir()
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(base, "home")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := vibeRuntimeHome(filepath.Join(base, "cwd")); err == nil {
		t.Fatal("accepted a runtime home symlink")
	}
	after, err := os.Stat(target)
	if err != nil || before.Mode().Perm() != after.Mode().Perm() {
		t.Fatalf("symlink target permissions changed: %v", err)
	}
}

func TestVibeRuntimeHomeRejectsFile(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "home"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := vibeRuntimeHome(filepath.Join(base, "cwd")); err == nil {
		t.Fatal("accepted a regular file as runtime home")
	}
}

func TestVibeAuthenticationErrorExplainsEnvFileSignIn(t *testing.T) {
	p := NewVibeBinProvider("", nil)
	err := p.classifyVibeACPError(&acp.RPCError{Code: -32000, Message: "Unauthenticated"}, nil)
	if !isVibeNotSignedIn(err) {
		t.Fatalf("authentication classification lost: %v", err)
	}
	for _, text := range []string{"OS-keyring", ".env file", "providers.<name>.env.MISTRAL_API_KEY", "billing and quota"} {
		if !strings.Contains(err.Error(), text) {
			t.Errorf("authentication error missing %q: %v", text, err)
		}
	}
}
