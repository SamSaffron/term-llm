package contain

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Run the real entrypoint with all absolute container paths relocated. chown is
// recorded, not executed: these tests need neither root nor an agent OS user.
func TestAgentEntrypointOwnership(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	for _, mode := range []string{"fresh", "populated-first-boot", "sentinel", "custom-init", "dangling-links"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			home := filepath.Join(root, "home")
			config := filepath.Join(home, ".config/term-llm")
			seed := filepath.Join(root, "seed")
			image := filepath.Join(root, "image")
			bin := filepath.Join(root, "bin")
			log := filepath.Join(root, "chown.log")
			write := func(path, text string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(text), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			read := func(path string) string {
				t.Helper()
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				return string(b)
			}
			write(filepath.Join(bin, "chown"), "#!/bin/bash\nprintf '%s\\t' \"$@\" >> \"$CHOWN_LOG\"\nprintf '\\n' >> \"$CHOWN_LOG\"\n")
			write(filepath.Join(bin, "sudo"), "#!/bin/bash\necho unexpected privilege switch >&2\nexit 99\n")
			write(filepath.Join(bin, "runsvdir"), "#!/bin/bash\nprintf '%s' \"$*\" > \"$SUPERVISOR_LOG\"\n")
			write(log, "")
			write(filepath.Join(seed, "bootstrap.yaml"), "name: {{AGENT_NAME}}\n")
			write(filepath.Join(seed, "system.md"), "Hello {{name}}\n")
			write(filepath.Join(seed, "services/demo/run"), "#!/bin/bash\necho {{name}}\n")
			write(filepath.Join(seed, "skills/demo/SKILL.md"), "Skill {{name}}\n")
			write(filepath.Join(seed, "memory/palace.md"), "Memory {{name}}\n")
			write(filepath.Join(seed, "scripts/demo.sh"), "echo {{name}}\n")
			outside := filepath.Join(root, "outside")
			write(filepath.Join(outside, "keep.md"), "outside {{name}}\n")
			if err := os.Symlink(outside, filepath.Join(seed, "memory/external")); err != nil {
				t.Fatal(err)
			}
			// A directory symlink in a copied seed must stay a link, not be traversed.
			var preserved []string
			if mode != "fresh" {
				for _, rel := range []string{"source/project/file", ".cache/file", ".config/term-llm/skills/custom/SKILL.md"} {
					path := filepath.Join(home, rel)
					write(path, "preserved\n")
					preserved = append(preserved, path)
				}
			}
			if mode == "sentinel" {
				write(filepath.Join(config, ".seed-bootstrapped"), "")
				write(filepath.Join(config, "config.yaml"), "existing config\n")
				preserved = append(preserved, filepath.Join(config, "config.yaml"))
			}
			if mode == "custom-init" {
				// Must inherit entrypoint privileges, not run through sudo as agent.
				write(filepath.Join(config, "init.sh"), "#!/bin/bash\nid -u > \"$HOOK_UID\"\necho hook-owned > \"$HOOK_FILE\"\nmkdir -p \"$SYSTEM_SERVICE\"\n")
			}
			if mode == "dangling-links" {
				if err := os.MkdirAll(filepath.Join(config, "agents/test"), 0o755); err != nil {
					t.Fatal(err)
				}
				for _, path := range []string{filepath.Join(home, ".zshrc"), filepath.Join(config, "config.yaml"), filepath.Join(config, "chatgpt_oauth.json"), filepath.Join(config, "agents/test/system.md")} {
					if err := os.Symlink(filepath.Join(outside, "missing"), path); err != nil {
						t.Fatal(err)
					}
				}
			}
			existing := map[string]bool{}
			err := filepath.Walk(home, func(path string, info os.FileInfo, err error) error {
				if os.IsNotExist(err) {
					return nil
				}
				if err != nil {
					return err
				}
				existing[path] = true
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile("images/agent/entrypoint.sh")
			if err != nil {
				t.Fatal(err)
			}
			script := strings.NewReplacer("/home/agent", home, "/opt/term-llm/bootstrap", image, "/seed", seed, "/etc/sv", filepath.Join(root, "etc/sv"), "/etc/runit/runsvdir", filepath.Join(root, "etc/runit/runsvdir")).Replace(string(data))
			entry := filepath.Join(root, "entrypoint.sh")
			write(entry, script)
			cmd := exec.Command("bash", entry)
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "AGENT_NAME=test", "TERM_LLM_PROVIDER=chatgpt", "TERM_LLM_CHATGPT_OAUTH_JSON_B64="+base64.StdEncoding.EncodeToString([]byte(`{"token":"test"}`)), "CHOWN_LOG="+log, "SUPERVISOR_LOG="+filepath.Join(root, "supervisor"), "HOOK_UID="+filepath.Join(root, "hook-uid"), "HOOK_FILE="+filepath.Join(home, "hook-file"), "SYSTEM_SERVICE="+filepath.Join(root, "system-service"))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("entrypoint: %v\n%s", err, out)
			}
			if got := read(filepath.Join(root, "supervisor")); got != filepath.Join(root, "etc/runit/runsvdir") {
				t.Fatalf("supervisor path = %q", got)
			}
			calls := read(log)
			owned := map[string]bool{}
			trees := map[string]bool{}
			for _, line := range strings.Split(strings.TrimSpace(calls), "\n") {
				args := strings.Split(strings.TrimSuffix(line, "\t"), "\t")
				if len(args) < 3 {
					t.Fatalf("invalid chown call %q", line)
				}
				if args[len(args)-2] != "agent:agent" {
					t.Fatalf("wrong bootstrap owner: %q", line)
				}
				target := args[len(args)-1]
				switch args[0] {
				case "-h":
					owned[target] = true
				case "-hR":
					if len(args) != 4 || args[1] != "-P" {
						t.Fatalf("recursive chown must not follow symlinks: %q", line)
					}
					switch target {
					case filepath.Join(config, "services"), filepath.Join(config, "skills"), filepath.Join(config, "agents/test/memory"), filepath.Join(config, "agents/test/scripts"):
						trees[target] = true
					default:
						t.Fatalf("unscoped recursive traversal: %q", line)
					}
				default:
					t.Fatalf("unexpected chown: %q", line)
				}
			}
			for path := range existing {
				if owned[path] {
					t.Fatalf("changed ownership of existing file %s", path)
				}
				for tree := range trees {
					if strings.HasPrefix(path, tree+"/") {
						t.Fatalf("traversed existing tree %s", tree)
					}
				}
			}
			if read(filepath.Join(outside, "keep.md")) != "outside {{name}}\n" {
				t.Fatal("followed seed symlink")
			}
			if mode == "sentinel" {
				if len(trees) != 0 || len(owned) != 1 || !owned[filepath.Join(home, ".zshrc")] {
					t.Fatalf("existing boot touched bootstrap: %s", calls)
				}
			} else {
				if got := read(filepath.Join(config, "agents/test/agent.yaml")); got != "name: test\n" {
					t.Fatalf("rendered config = %q", got)
				}
				if got := read(filepath.Join(config, "agents/test/memory/palace.md")); got != "Memory test\n" {
					t.Fatalf("copied template = %q", got)
				}
				if target, err := os.Readlink(filepath.Join(config, "agents/test/memory/external")); err != nil || target != outside {
					t.Fatalf("copied symlink = %q, %v", target, err)
				}
				// Every new persistent directory/file must receive scoped ownership. Skip
				// pre-existing objects and links, which deliberately preserve ownership.
				err := filepath.Walk(home, func(path string, info os.FileInfo, err error) error {
					if err != nil {
						return err
					}
					if info.Mode()&os.ModeSymlink != 0 {
						return nil
					}
					if existing[path] || (mode == "custom-init" && path == filepath.Join(home, "hook-file")) {
						return nil
					}
					if owned[path] {
						return nil
					}
					for tree := range trees {
						if path == tree || strings.HasPrefix(path, tree+"/") {
							return nil
						}
					}
					return fmt.Errorf("new path lacks ownership: %s (calls: %s)", path, calls)
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "custom-init" {
				if got := strings.TrimSpace(read(filepath.Join(root, "hook-uid"))); got != fmt.Sprint(os.Geteuid()) {
					t.Fatalf("hook dropped entrypoint privileges: %s", got)
				}
				if info, err := os.Stat(filepath.Join(root, "system-service")); err != nil || !info.IsDir() {
					t.Fatalf("hook did not install sandbox system service: %v", err)
				}
				if owned[filepath.Join(home, "hook-file")] {
					t.Fatal("repaired hook-created file")
				}
			}
			if mode == "dangling-links" {
				if _, err := os.Stat(filepath.Join(outside, "missing")); !os.IsNotExist(err) {
					t.Fatalf("wrote through dangling link: %v", err)
				}
			}
			for _, path := range preserved {
				if strings.HasSuffix(path, "config.yaml") {
					continue
				}
				if read(path) != "preserved\n" {
					t.Fatalf("changed existing file %s", path)
				}
			}
			// Boot again: no new bootstrap tree or ownership scan after the sentinel.
			write(log, "")
			again := exec.Command("bash", entry)
			again.Env = cmd.Env
			if out, err := again.CombinedOutput(); err != nil {
				t.Fatalf("second boot: %v\n%s", err, out)
			}
			if got := read(log); got != "" {
				t.Fatalf("second boot changed ownership: %s", got)
			}
		})
	}
}
