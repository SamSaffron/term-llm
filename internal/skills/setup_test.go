package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestNewSetupLazilyBuildsPromptMetadata(t *testing.T) {
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, ".config"))
	t.Setenv("CODEX_HOME", filepath.Join(tmp, ".codex"))

	skillDir := filepath.Join(tmp, ".skills", "test-skill")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: test-skill
description: "A test skill"
---

# Test Skill
`), 0644); err != nil {
		t.Fatal(err)
	}

	setup, err := NewSetup(&config.SkillsConfig{
		Enabled:               true,
		AutoInvoke:            true,
		MetadataBudgetTokens:  8000,
		MaxVisibleSkills:      8,
		IncludeProjectSkills:  true,
		IncludeEcosystemPaths: false,
	})
	if err != nil {
		t.Fatalf("NewSetup() error = %v", err)
	}
	if setup == nil {
		t.Fatal("NewSetup() = nil, want non-nil")
	}

	if setup.XML != "" {
		t.Fatalf("setup.XML = %q before metadata generation, want empty", setup.XML)
	}
	if len(setup.Skills) != 0 {
		t.Fatalf("len(setup.Skills) = %d before metadata generation, want 0", len(setup.Skills))
	}
	if setup.TotalSkills != 0 {
		t.Fatalf("setup.TotalSkills = %d before metadata generation, want 0", setup.TotalSkills)
	}
	if setup.HasOverflow {
		t.Fatal("setup.HasOverflow = true before metadata generation, want false")
	}
	if setup.HasSkillsXML() {
		t.Fatal("HasSkillsXML() = true before metadata generation, want false")
	}

	secondSkillDir := filepath.Join(tmp, ".skills", "late-skill")
	if err := os.MkdirAll(secondSkillDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secondSkillDir, "SKILL.md"), []byte(`---
name: late-skill
description: "A skill added after setup creation"
---

# Late Skill
`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := setup.EnsurePromptMetadata(); err != nil {
		t.Fatalf("EnsurePromptMetadata() error = %v", err)
	}
	if !setup.HasSkillsXML() {
		t.Fatal("HasSkillsXML() = false, want true after metadata generation")
	}
	if !strings.Contains(setup.XML, "<available_skills>") {
		t.Fatalf("setup.XML missing <available_skills>: %q", setup.XML)
	}
	if !strings.Contains(setup.XML, "<name>test-skill</name>") {
		t.Fatalf("setup.XML missing test skill entry: %q", setup.XML)
	}
	if !strings.Contains(setup.XML, "<name>late-skill</name>") {
		t.Fatalf("setup.XML missing late skill entry: %q", setup.XML)
	}
	if setup.TotalSkills != 2 {
		t.Fatalf("setup.TotalSkills = %d, want 2", setup.TotalSkills)
	}
	if setup.HasOverflow {
		t.Fatal("setup.HasOverflow = true, want false")
	}
}

func TestNewSetupReturnsNilWhenNoValidSkillsExist(t *testing.T) {
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, ".config"))
	t.Setenv("CODEX_HOME", filepath.Join(tmp, ".codex"))
	if err := os.Mkdir(filepath.Join(tmp, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, ".skills", "broken"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".skills", "broken", "SKILL.md"), []byte(`---
name: Broken
---
`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	setup, err := NewSetup(&config.SkillsConfig{
		Enabled:               true,
		AutoInvoke:            true,
		MetadataBudgetTokens:  8000,
		MaxVisibleSkills:      8,
		IncludeProjectSkills:  true,
		IncludeEcosystemPaths: false,
	})
	if err != nil {
		t.Fatalf("NewSetup() error = %v", err)
	}
	if setup != nil {
		t.Fatalf("NewSetup() = %#v, want nil when no valid skills are available", setup)
	}
}

func BenchmarkNewSetupWithoutPromptMetadata(b *testing.B) {
	const skillCount = 200

	origWD, err := os.Getwd()
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	root := b.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		b.Fatalf("mkdir .git: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		b.Fatalf("chdir: %v", err)
	}

	b.Setenv("HOME", root)
	b.Setenv("XDG_CONFIG_HOME", filepath.Join(root, ".config"))
	b.Setenv("CODEX_HOME", filepath.Join(root, ".codex"))

	for i := 0; i < skillCount; i++ {
		name := fmt.Sprintf("bench-skill-%03d", i)
		skillDir := filepath.Join(root, ".skills", name)
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			b.Fatalf("mkdir skill dir: %v", err)
		}
		content := fmt.Sprintf(`---
name: %s
description: "Benchmark skill %03d for lazy setup"
---

# %s
`, name, i, name)
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
			b.Fatalf("write SKILL.md: %v", err)
		}
	}

	cfg := &config.SkillsConfig{
		Enabled:               true,
		AutoInvoke:            true,
		MetadataBudgetTokens:  8000,
		MaxVisibleSkills:      50,
		IncludeProjectSkills:  true,
		IncludeEcosystemPaths: false,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		setup, err := NewSetup(cfg)
		if err != nil {
			b.Fatalf("NewSetup: %v", err)
		}
		if setup == nil {
			b.Fatal("NewSetup returned nil, want setup for discovered skills")
		}
		if setup.XML != "" || len(setup.Skills) != 0 || setup.TotalSkills != 0 {
			b.Fatalf("NewSetup built prompt metadata eagerly: XML=%q skills=%d total=%d", setup.XML, len(setup.Skills), setup.TotalSkills)
		}
	}
}
