package mcp

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	internalmcp "github.com/samsaffron/term-llm/internal/mcp"
)

func TestMCPPolicyBrowseTestsUseInstalledLocalName(t *testing.T) {
	for _, tc := range []struct{ name, pkg, local string }{
		{"friendly", "@vendor/remote-mcp", "friendly"},
		{"io.vendor/remote", "@vendor/mcp", "vendor"},
		{"io.vendor/remote", "@vendor/mcp-tools", "tools"},
	} {
		t.Run(tc.name+"/"+tc.pkg, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			server := internalmcp.RegistryServer{Name: tc.name, Packages: []internalmcp.PackageInfo{{RegistryType: "npm", Identifier: tc.pkg}}}
			if got := registryLocalName(&server); got != tc.local {
				t.Fatalf("local name=%q want=%q", got, tc.local)
			}
			model := &Model{servers: []internalmcp.RegistryServerWrapper{{Server: server}}, installed: map[string]bool{tc.local: true}}
			if !model.isServerInstalled(&server) {
				t.Fatal("installed-state derivation differed")
			}
			msg := model.installServer(0)().(installResultMsg)
			if msg.err != nil || msg.name != tc.local {
				t.Fatalf("install=%+v", msg)
			}
			cfg, err := internalmcp.LoadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := cfg.Servers[tc.local]; !ok {
				t.Fatal("installer used different local key")
			}
			path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm", "config.yaml")
			if err := os.WriteFile(path, []byte("mcp: {servers: {"+tc.local+": {agents: []}}}"), 0600); err != nil {
				t.Fatal(err)
			}
			result := model.testServer(0)().(testResultMsg)
			var denied *internalmcp.PermissionError
			if !errors.As(result.err, &denied) || denied.Server != tc.local {
				t.Fatalf("connection test bypassed local policy using display %q: %v", server.DisplayName(), result.err)
			}
		})
	}
}
