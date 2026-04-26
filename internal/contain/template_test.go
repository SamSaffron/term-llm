package contain

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestLoadTemplateDefaultIsAgent(t *testing.T) {
	tmpl, err := LoadTemplate("")
	if err != nil {
		t.Fatal(err)
	}
	if tmpl.Name != "agent" || !tmpl.Builtin {
		t.Fatalf("default template = %+v, want built-in agent", tmpl)
	}
}

func TestCreateWorkspaceBuiltinBasic(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cwd := t.TempDir()
	dir, err := CreateWorkspace("mybox", CreateOptions{Template: "basic", CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	compose, err := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(compose)
	for _, want := range []string{`org.term-llm.contain.name: "mybox"`, `org.term-llm.contain.config_dir: "` + dir + `"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("compose.yaml missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "{{") {
		t.Fatalf("compose.yaml still contains placeholders:\n%s", text)
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil {
		t.Fatal(err)
	}
}

func TestCreateWorkspaceBuiltinAgent(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	configDir, err := config.GetConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oauthJSON := []byte(`{"access_token":"secret"}`)
	if err := os.WriteFile(filepath.Join(configDir, "chatgpt_oauth.json"), oauthJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	imageDockerfile, err := AgentImageDockerfilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(imageDockerfile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imageDockerfile, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, err := CreateWorkspace("bot", CreateOptions{Template: "agent", CWD: t.TempDir(), NoInput: true, Values: map[string]string{
		"provider": "chatgpt",
		"web_port": "9090",
	}})
	if err != nil {
		t.Fatal(err)
	}
	compose, err := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(compose)
	hash, err := AgentImageHash()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"bot agent workspace", "AGENT_NAME: \"bot\"", "restart: unless-stopped", "term-llm-agent:bot-" + hash} {
		if !strings.Contains(text, want) {
			t.Fatalf("agent compose missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "{{") {
		t.Fatalf("agent compose.yaml still contains placeholders:\n%s", text)
	}
	if _, err := os.Stat(filepath.Join(dir, ".env")); err != nil {
		t.Fatal(err)
	}
	envData, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	wantOAuth := "TERM_LLM_CHATGPT_OAUTH_JSON_B64=" + base64.StdEncoding.EncodeToString(oauthJSON)
	if !strings.Contains(string(envData), "WEB_PORT=9090") || !strings.Contains(string(envData), "TERM_LLM_PROVIDER=chatgpt") || !strings.Contains(string(envData), wantOAuth) || strings.Contains(string(envData), "{{") {
		t.Fatalf("env not rendered correctly:\n%s", envData)
	}
	envInfo, err := os.Stat(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if envInfo.Mode().Perm() != 0o600 {
		t.Fatalf(".env mode = %v, want 0600", envInfo.Mode().Perm())
	}
	imageData, err := os.ReadFile(imageDockerfile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(imageData), managedImageMarker) {
		t.Fatalf("agent template did not sync managed image; Dockerfile = %s", imageData)
	}
}

func TestCreateWorkspaceExternalFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tmpl := filepath.Join(t.TempDir(), "template.yaml")
	if err := os.WriteFile(tmpl, []byte("services:\n  app:\n    image: alpine\n    labels:\n      name: '{{name}}'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, err := CreateWorkspace("box", CreateOptions{Template: tmpl, CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "compose.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "template.yaml")); !os.IsNotExist(err) {
		t.Fatalf("single-file template copied sibling/original unexpectedly: %v", err)
	}
}

func TestCreateWorkspaceExternalDirectory(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "docker-compose.yml"), []byte("services:\n  app:\n    image: alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(src, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(src, "scripts", "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho {{name}}\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".DS_Store"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir, err := CreateWorkspace("box", CreateOptions{Template: src, CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "compose.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "docker-compose.yml")); !os.IsNotExist(err) {
		t.Fatalf("docker-compose.yml was not renamed: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "scripts", "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("executable bit not preserved: %v", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(dir, ".DS_Store")); !os.IsNotExist(err) {
		t.Fatalf("junk file copied unexpectedly: %v", err)
	}
}

func TestCreateWorkspaceDoesNotOverwriteAndUnknownPlaceholderErrors(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := CreateWorkspace("box", CreateOptions{Template: "basic", CWD: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateWorkspace("box", CreateOptions{Template: "basic", CWD: t.TempDir()}); err == nil {
		t.Fatal("CreateWorkspace overwrote existing target")
	}

	tmpl := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(tmpl, []byte("services: {{missing}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateWorkspace("bad", CreateOptions{Template: tmpl, CWD: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "unknown template placeholder") {
		t.Fatalf("unknown placeholder error = %v", err)
	}
}

func TestExtractClaudeOAuthToken(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "sk-ant-oat in line",
			in:   "Generated long-lived OAuth token:\nsk-ant-oat01-abcdefghijklmnopqrstuvwxyz0123456789\n",
			want: "sk-ant-oat01-abcdefghijklmnopqrstuvwxyz0123456789",
		},
		{
			name: "fallback to last token-like line",
			in:   "header line\n\nABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789\n",
			want: "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
		},
		{
			name: "no token",
			in:   "browser opened\nwaiting...\n",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractClaudeOAuthToken(tc.in)
			if got != tc.want {
				t.Fatalf("extractClaudeOAuthToken = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAddClaudeCodeOAuthTemplateValueNoInputSkips(t *testing.T) {
	values := map[string]string{"provider": "claude-bin"}
	var stdout bytes.Buffer
	if err := addClaudeCodeOAuthTemplateValue(values, CreateOptions{NoInput: true, Stdout: &stdout}); err != nil {
		t.Fatal(err)
	}
	if got := values["claude_code_oauth_token"]; got != "" {
		t.Fatalf("token = %q, want empty when no-input", got)
	}
	if !strings.Contains(stdout.String(), "skipping `claude setup-token`") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestAddClaudeCodeOAuthTemplateValuePreservesProvided(t *testing.T) {
	values := map[string]string{"provider": "claude-bin", "claude_code_oauth_token": "sk-ant-oat01-prefilled"}
	if err := addClaudeCodeOAuthTemplateValue(values, CreateOptions{NoInput: true}); err != nil {
		t.Fatal(err)
	}
	if got := values["claude_code_oauth_token"]; got != "sk-ant-oat01-prefilled" {
		t.Fatalf("preset token clobbered: %q", got)
	}
}

func TestAddClaudeCodeOAuthTemplateValueNonClaudeProviderEmpty(t *testing.T) {
	values := map[string]string{"provider": "chatgpt"}
	if err := addClaudeCodeOAuthTemplateValue(values, CreateOptions{NoInput: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := values["claude_code_oauth_token"]; !ok {
		t.Fatal("expected empty token placeholder so .env render does not fail")
	}
	if got := values["claude_code_oauth_token"]; got != "" {
		t.Fatalf("token = %q, want empty for non-claude-bin provider", got)
	}
}

func TestCreateWorkspaceClaudeBinNoInputRendersEmptyToken(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	imageDockerfile, err := AgentImageDockerfilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(imageDockerfile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imageDockerfile, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, err := CreateWorkspace("claudebox", CreateOptions{Template: "agent", CWD: t.TempDir(), NoInput: true, Values: map[string]string{
		"provider": "claude-bin",
		"web_port": "9091",
	}})
	if err != nil {
		t.Fatal(err)
	}
	envData, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(envData), "TERM_LLM_PROVIDER=claude-bin") {
		t.Fatalf(".env did not select claude-bin provider:\n%s", envData)
	}
	for _, want := range []string{"TERM_LLM_CLAUDE_CODE_OAUTH_TOKEN=", "CLAUDE_CODE_OAUTH_TOKEN="} {
		if !strings.Contains(string(envData), want) {
			t.Fatalf(".env missing %s line:\n%s", want, envData)
		}
	}
	if strings.Contains(string(envData), "{{") {
		t.Fatalf(".env still contains placeholders:\n%s", envData)
	}
}

func TestCreateWorkspaceClaudeBinUsesProvidedToken(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	imageDockerfile, err := AgentImageDockerfilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(imageDockerfile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imageDockerfile, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, err := CreateWorkspace("claudeprovided", CreateOptions{Template: "agent", CWD: t.TempDir(), NoInput: true, Values: map[string]string{
		"provider":                "claude-bin",
		"web_port":                "9092",
		"claude_code_oauth_token": "sk-ant-oat01-test-token-value",
	}})
	if err != nil {
		t.Fatal(err)
	}
	envData, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"TERM_LLM_CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-test-token-value", "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-test-token-value"} {
		if !strings.Contains(string(envData), want) {
			t.Fatalf(".env did not include provided token as %s:\n%s", want, envData)
		}
	}
}

func TestDefaultClaudeSetupTokenRunnerCapturesNonTerminalOutput(t *testing.T) {
	tmp := t.TempDir()
	script := filepath.Join(tmp, "claude")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho setup starting\necho sk-ant-oat01-scripted-token-value-1234567890\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stdout bytes.Buffer
	out, err := defaultClaudeSetupTokenRunner(CreateOptions{Stdin: strings.NewReader(""), Stdout: &stdout})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "sk-ant-oat01-scripted-token-value-1234567890") {
		t.Fatalf("captured output missing token: %q", out)
	}
	if !strings.Contains(stdout.String(), "setup starting") {
		t.Fatalf("stdout was not teed: %q", stdout.String())
	}
}

func TestUnknownTemplateErrorListsBuiltins(t *testing.T) {
	_, err := LoadTemplate("definitely-not-a-template")
	if err == nil || !strings.Contains(err.Error(), "basic") || !strings.Contains(err.Error(), "agent") || strings.Contains(err.Error(), "term-llm") {
		t.Fatalf("LoadTemplate unknown error = %v", err)
	}
}
