package contain

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestReadEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	contents := "" +
		"# a comment\n" +
		"\n" +
		"WEB_PORT=8222\n" +
		"WEB_TOKEN=\"quoted-secret\"\n" +
		"  WEB_BASE_PATH = /chat \n" +
		"MALFORMED\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := ReadEnvFile(path)
	if err != nil {
		t.Fatalf("ReadEnvFile error = %v", err)
	}
	if values["WEB_PORT"] != "8222" {
		t.Errorf("WEB_PORT = %q want 8222", values["WEB_PORT"])
	}
	if values["WEB_TOKEN"] != "quoted-secret" {
		t.Errorf("WEB_TOKEN = %q want quoted-secret", values["WEB_TOKEN"])
	}
	if values["WEB_BASE_PATH"] != "/chat" {
		t.Errorf("WEB_BASE_PATH = %q want /chat", values["WEB_BASE_PATH"])
	}
	if _, ok := values["MALFORMED"]; ok {
		t.Errorf("MALFORMED line should be skipped, got %q", values["MALFORMED"])
	}
}

func TestReadWebConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeContainerEnv(t, "gw", "WEB_PORT=8222\nWEB_TOKEN=secret-token\nWEB_BASE_PATH=chat\n")

	cfg, err := ReadWebConfig("gw")
	if err != nil {
		t.Fatalf("ReadWebConfig error = %v", err)
	}
	if cfg.Port != "8222" {
		t.Errorf("Port = %q want 8222", cfg.Port)
	}
	if cfg.Token != "secret-token" {
		t.Errorf("Token = %q want secret-token", cfg.Token)
	}
	// BasePath without a leading slash should be normalised.
	if cfg.BasePath != "/chat" {
		t.Errorf("BasePath = %q want /chat", cfg.BasePath)
	}
}

func TestReadWebConfigDefaultsAndTemplates(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// Unrendered template placeholders and missing keys fall back to defaults;
	// a templated token is treated as absent.
	writeContainerEnv(t, "fresh", "WEB_PORT={{web_port}}\nWEB_TOKEN={{web_token}}\n")

	cfg, err := ReadWebConfig("fresh")
	if err != nil {
		t.Fatalf("ReadWebConfig error = %v", err)
	}
	if cfg.Port != strconv.Itoa(webPortBase) {
		t.Errorf("Port = %q want default %q", cfg.Port, strconv.Itoa(webPortBase))
	}
	if cfg.BasePath != defaultWebBasePath {
		t.Errorf("BasePath = %q want default %q", cfg.BasePath, defaultWebBasePath)
	}
	if cfg.Token != "" {
		t.Errorf("Token = %q want empty (templated placeholder)", cfg.Token)
	}
}

func TestReadWebConfigMissingWorkspace(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := ReadWebConfig("does-not-exist"); err == nil {
		t.Fatal("ReadWebConfig for missing workspace succeeded, want error")
	}
}

func TestReadWebConfigRejectsInvalidPort(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cases := []struct {
		name string
		env  string
	}{
		{"zero", "WEB_PORT=0\nWEB_TOKEN=t\n"},
		{"too-big", "WEB_PORT=99999\nWEB_TOKEN=t\n"},
		{"non-numeric", "WEB_PORT=abc\nWEB_TOKEN=t\n"},
		{"trailing-junk", "WEB_PORT=80x\nWEB_TOKEN=t\n"},
		{"negative", "WEB_PORT=-1\nWEB_TOKEN=t\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writeContainerEnv(t, "gw-"+tc.name, tc.env)
			if _, err := ReadWebConfig("gw-" + tc.name); err == nil {
				t.Fatalf("ReadWebConfig with %q succeeded, want error", tc.env)
			}
		})
	}
}

func TestReadWebConfigAcceptsValidAndDefaultPort(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeContainerEnv(t, "ok", "WEB_PORT=8222\nWEB_TOKEN=t\n")
	if _, err := ReadWebConfig("ok"); err != nil {
		t.Fatalf("valid port rejected: %v", err)
	}
	// An unset / templated port must keep falling back to the default, not error.
	writeContainerEnv(t, "tmpl", "WEB_PORT={{web_port}}\nWEB_TOKEN=t\n")
	cfg, err := ReadWebConfig("tmpl")
	if err != nil || cfg.Port != strconv.Itoa(webPortBase) {
		t.Fatalf("templated port: cfg.Port=%q err=%v want %q/nil", cfg.Port, err, strconv.Itoa(webPortBase))
	}
}

func TestReadWebConfigNormalizesTrailingSlash(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// A trailing slash on WEB_BASE_PATH must normalize to the same canonical
	// form the serve bakes into its HTML, so the gateway's rebase needles match.
	writeContainerEnv(t, "ws", "WEB_PORT=8222\nWEB_TOKEN=t\nWEB_BASE_PATH=/chat/\n")
	cfg, err := ReadWebConfig("ws")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BasePath != "/chat" {
		t.Errorf("BasePath = %q want /chat (trailing slash trimmed)", cfg.BasePath)
	}
}

func writeContainerEnv(t *testing.T, name, contents string) {
	t.Helper()
	dir, err := ContainerDir(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
