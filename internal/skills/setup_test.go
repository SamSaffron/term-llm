package skills

import (
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
	if setup.TotalSkills != 1 {
		t.Fatalf("setup.TotalSkills = %d, want 1", setup.TotalSkills)
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
