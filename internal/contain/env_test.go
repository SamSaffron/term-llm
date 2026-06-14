package contain

import (
	"os"
	"path/filepath"
	"testing"
)

func writeEnvForTest(t *testing.T, name, contents string) {
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

func TestReadWebConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeEnvForTest(t, "alpha", "WEB_PORT=8222\nWEB_TOKEN=secret-token\nWEB_BASE_PATH=/chat/\n")

	web, err := ReadWebConfig("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if web.Port != "8222" || web.Token != "secret-token" {
		t.Errorf("web = %+v", web)
	}
	// Trailing slash is normalized away to match the serve's canonical shape.
	if web.BasePath != "/chat" {
		t.Errorf("BasePath = %q, want /chat", web.BasePath)
	}
}

func TestReadWebConfigDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeEnvForTest(t, "bare", "OTHER=1\n")

	web, err := ReadWebConfig("bare")
	if err != nil {
		t.Fatal(err)
	}
	if web.Port != "8081" || web.BasePath != "/chat" || web.Token != "" {
		t.Errorf("web = %+v, want defaults with empty token", web)
	}
}

func TestReadWebConfigUnrenderedPlaceholders(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeEnvForTest(t, "tmpl", "WEB_PORT={{web_port}}\nWEB_TOKEN={{web_token}}\nWEB_BASE_PATH={{base}}\n")

	web, err := ReadWebConfig("tmpl")
	if err != nil {
		t.Fatal(err)
	}
	if web.Port != "8081" || web.BasePath != "/chat" || web.Token != "" {
		t.Errorf("web = %+v, want placeholder values replaced by defaults", web)
	}
}

func TestReadWebConfigInvalidPort(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeEnvForTest(t, "badport", "WEB_PORT=80x\nWEB_TOKEN=t\n")

	if _, err := ReadWebConfig("badport"); err == nil {
		t.Fatal("invalid WEB_PORT accepted, want error")
	}
}

func TestReadWebConfigMissingWorkspace(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := ReadWebConfig("nope"); err == nil {
		t.Fatal("missing workspace read, want error")
	}
}

func TestReadEnvFileParsing(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	contents := "# comment\nA=1\nB = \"quoted\" \n\nNOEQUALS\nC='single'\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := ReadEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if values["A"] != "1" || values["B"] != "quoted" || values["C"] != "single" {
		t.Errorf("values = %+v", values)
	}
	if _, ok := values["NOEQUALS"]; ok {
		t.Error("line without '=' should be skipped")
	}
}
