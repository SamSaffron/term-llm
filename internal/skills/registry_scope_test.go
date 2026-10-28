package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const scopeTestManifest = `---
name: helper
description: Scope fixture
user_installed: true
tools:
  - name: check_build
    description: Check build
    script: check.sh
    user_installed: true
---
Build instructions remain available.
`

func TestSkillToolDiscoveryScope(t *testing.T) {
	paths := []string{".skills", ".claude/skills", ".agents/skills", ".codex/skills", ".gemini/skills", ".cursor/skills", "config/term-llm/skills"}
	for _, scope := range []string{"project", "user", "home"} {
		userInstalled := scope != "project"
		for _, path := range paths {
			if !userInstalled && path == "config/term-llm/skills" {
				continue
			}
			for _, lookup := range []string{"direct", "listed", "canonical-name"} {
				t.Run(scope+"/"+path+"/"+lookup, func(t *testing.T) {
					root := t.TempDir()
					home, project := filepath.Join(root, "home"), filepath.Join(root, "project")
					if scope == "home" {
						project = home
					}
					t.Setenv("HOME", home)
					t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
					t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
					base := project
					if userInstalled {
						base = home
					}
					dirName := "helper"
					if lookup == "canonical-name" {
						dirName = "different-directory"
					}
					writeScopeManifest(t, filepath.Join(base, path, dirName))
					registry, err := NewRegistry(RegistryConfig{
						AutoInvoke: true, IncludeProjectSkills: true, IncludeEcosystemPaths: true, ProjectDir: project,
					})
					if err != nil {
						t.Fatal(err)
					}
					if lookup == "listed" {
						// Exercise both the list cache and metadata-to-full reload.
						for range 2 {
							listed, err := registry.List()
							if err != nil || len(listed) != 1 {
								t.Fatalf("List() = %v, %v", listed, err)
							}
							if listed[0].UserInstalled != userInstalled {
								t.Fatalf("listed scope = %v, want %v", listed[0].UserInstalled, userInstalled)
							}
						}
					}
					for _, origin := range []SkillActivationOrigin{SkillActivationModel, SkillActivationUser} {
						activation, err := NewActivator(registry).Activate(ActivationRequest{Name: "helper", Origin: origin})
						if err != nil {
							t.Fatal(err)
						}
						if activation.Skill.UserInstalled != userInstalled || len(activation.ToolDefs) != 1 || activation.ToolDefs[0].UserInstalled != userInstalled {
							t.Fatalf("activation lost scope: skill=%+v tools=%+v", activation.Skill, activation.ToolDefs)
						}
						if activation.Prompt != "Build instructions remain available." {
							t.Fatalf("instructions changed: %q", activation.Prompt)
						}
					}
				})
			}
		}
	}
}

func TestProjectSkillToolsDoNotInheritShadowedUserTrust(t *testing.T) {
	root := t.TempDir()
	home, project := filepath.Join(root, "home"), filepath.Join(root, "project")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	writeScopeManifest(t, filepath.Join(home, ".claude", "skills", "helper"))
	projectSkill := filepath.Join(project, ".claude", "skills", "helper")
	writeScopeManifest(t, projectSkill)
	registry, err := NewRegistry(RegistryConfig{
		AutoInvoke: true, IncludeProjectSkills: true, IncludeEcosystemPaths: true, ProjectDir: project,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.List(); err != nil {
		t.Fatal(err)
	}
	activation, err := NewActivator(registry).Activate(ActivationRequest{Name: "helper", Origin: SkillActivationModel})
	if err != nil {
		t.Fatal(err)
	}
	if activation.BaseDir != projectSkill || activation.ToolDefs[0].UserInstalled {
		t.Fatalf("project skill inherited user trust: %+v", activation)
	}
}

func writeScopeManifest(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(scopeTestManifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSkillLookupCannotEscapeSearchRoots(t *testing.T) {
	root := t.TempDir()
	home, project := filepath.Join(root, "home"), filepath.Join(root, "project")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	writeScopeManifest(t, filepath.Join(project, ".skills", "helper"))
	registry, err := NewRegistry(RegistryConfig{
		AutoInvoke: true, IncludeProjectSkills: true, IncludeEcosystemPaths: true, ProjectDir: project,
	})
	if err != nil {
		t.Fatal(err)
	}
	// This resolves to the project fixture only from the deeper user config
	// search root. Joining it before validation would incorrectly grant trust.
	const escapeName = "../../../../project/.skills/helper"
	if _, err := os.Stat(filepath.Join(home, "config", "term-llm", "skills", escapeName, "SKILL.md")); err != nil {
		t.Fatalf("escape lookup must resolve to the project fixture: %v", err)
	}
	for _, name := range []string{escapeName, "../helper", "helper/../helper", "helper/", `..\helper`, ".", ".."} {
		t.Run(name, func(t *testing.T) {
			skill, err := registry.Get(name)
			if err == nil {
				t.Fatalf("invalid lookup %q loaded %s with user-installed=%v", name, skill.SourcePath, skill.UserInstalled)
			}
			if !strings.Contains(err.Error(), "invalid skill lookup name") {
				t.Fatalf("lookup must reject the name before filesystem access, got %v", err)
			}
		})
	}
}
