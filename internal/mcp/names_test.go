package mcp

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSplitCommandLine(t *testing.T) {
	cases := []struct {
		input string
		want  []string
		fails bool
	}{
		{`npx -y '@scope/mcp-server' "a b" c\ d`, []string{"npx", "-y", "@scope/mcp-server", "a b", "c d"}, false},
		{`bin '' ""`, []string{"bin", "", ""}, false},
		{`a 'unterminated`, nil, true},
		{`a "unterminated`, nil, true},
		{`a \`, nil, true},
		{`  `, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := SplitCommandLine(tc.input)
			if (err != nil) != tc.fails {
				t.Fatalf("err: %v", err)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestDeriveMCPNames(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"npx", "-y", "@sentry/mcp-server@1.0"}, "sentry"},
		{[]string{"pnpm", "dlx", "@playwright/mcp"}, "playwright"},
		{[]string{"uvx", "mcp-server-github"}, "github"},
		{[]string{"bunx", "@scope/server"}, "scope"},
		{[]string{"/usr/bin/My Tool"}, "my-tool"},
	} {
		got := DeriveNameFromCommand(tc.argv)
		if got != tc.want {
			t.Errorf("%v: got %q want %q", tc.argv, got, tc.want)
		}
		if err := ValidateServerName(got); err != nil {
			t.Errorf("%q invalid: %v", got, err)
		}
	}
	for _, tc := range []struct{ raw, want string }{{"https://api.example.com/mcp", "example"}, {"https://mcp.foo.io/docs", "foo-docs"}, {"https://www.EXAMPLE.dev/mcp", "example"}} {
		u, _ := url.Parse(tc.raw)
		if got := DeriveNameFromURL(u); got != tc.want {
			t.Errorf("%q: got %q want %q", tc.raw, got, tc.want)
		}
	}
}

func TestValidateServerName(t *testing.T) {
	for _, name := range []string{"exa", "A_1.test-foo", "a"} {
		if err := ValidateServerName(name); err != nil {
			t.Errorf("valid %q: %v", name, err)
		}
	}
	for _, name := range []string{"", "_first", "bad/name", "a__b", "a b", "a" + strings.Repeat("b", 64)} {
		if err := ValidateServerName(name); err == nil {
			t.Errorf("invalid %q accepted", name)
		}
	}
}

func TestUpdateConfigAtPathConcurrentAtomicPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "term-llm", "mcp.json")
	const count = 32
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- UpdateConfigAtPath(path, func(cfg *Config) error {
				cfg.AddServer(fmt.Sprintf("server-%d", i), ServerConfig{Command: "echo"})
				return nil
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("update: %v", err)
		}
	}
	cfg, err := LoadConfigFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != count {
		t.Fatalf("server count %d want %d", len(cfg.Servers), count)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	if err := UpdateConfigAtPath(path, func(cfg *Config) error { cfg.RemoveServer("server-0"); return fmt.Errorf("abort") }); err == nil {
		t.Fatal("expected abort")
	}
	cfg, err = LoadConfigFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != count {
		t.Fatal("failed mutation persisted")
	}
	files, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if filepath.Ext(file.Name()) == ".tmp" {
			t.Fatalf("temporary file left: %s", file.Name())
		}
	}
}

func TestSaveToPathWritesThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(`{"servers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := UpdateConfigAtPath(link, func(cfg *Config) error {
		cfg.AddServer("linked", ServerConfig{Command: "echo"})
		return nil
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("config symlink was replaced by a regular file")
	}
	cfg, err := LoadConfigFromPath(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Servers["linked"]; !ok {
		t.Fatalf("symlink target not updated: %+v", cfg.Servers)
	}
}
