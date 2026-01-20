package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryList(t *testing.T) {
	// Create temp directory structure
	tmpDir := t.TempDir()

	// Create a skill
	skillDir := filepath.Join(tmpDir, "skills", "test-skill")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatalf("failed to create skill dir: %v", err)
	}

	content := `---
name: test-skill
description: "A test skill"
---

# Test Skill
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatalf("failed to write SKILL.md: %v", err)
	}

	// Create registry with custom search path
	registry, err := NewRegistry(RegistryConfig{
		SearchPaths:           []string{filepath.Join(tmpDir, "skills")},
		IncludeProjectSkills:  false,
		IncludeCodexPaths:     false,
		IncludeClaudePaths:    false,
		IncludeGeminiPaths:    false,
		IncludeUniversalPaths: false,
		UseBuiltin:            false,
	})
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	// List skills
	skills, err := registry.List()
	if err != nil {
		t.Fatalf("failed to list skills: %v", err)
	}

	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}

	if skills[0].Name != "test-skill" {
		t.Errorf("expected skill name 'test-skill', got %q", skills[0].Name)
	}
}

func TestRegistryGet(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a skill
	skillDir := filepath.Join(tmpDir, "skills", "get-test")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatalf("failed to create skill dir: %v", err)
	}

	content := `---
name: get-test
description: "Testing Get method"
---

# Body content for Get test
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatalf("failed to write SKILL.md: %v", err)
	}

	registry, err := NewRegistry(RegistryConfig{
		SearchPaths:           []string{filepath.Join(tmpDir, "skills")},
		IncludeProjectSkills:  false,
		IncludeCodexPaths:     false,
		IncludeClaudePaths:    false,
		IncludeGeminiPaths:    false,
		IncludeUniversalPaths: false,
		UseBuiltin:            false,
	})
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	// Get the skill
	skill, err := registry.Get("get-test")
	if err != nil {
		t.Fatalf("failed to get skill: %v", err)
	}

	if skill.Name != "get-test" {
		t.Errorf("expected name 'get-test', got %q", skill.Name)
	}

	if !skill.IsLoaded() {
		t.Error("expected skill to be fully loaded via Get")
	}

	if skill.Body == "" {
		t.Error("expected body to be loaded")
	}
}

func TestRegistryGetNotFound(t *testing.T) {
	registry, err := NewRegistry(RegistryConfig{
		IncludeProjectSkills:  false,
		IncludeCodexPaths:     false,
		IncludeClaudePaths:    false,
		IncludeGeminiPaths:    false,
		IncludeUniversalPaths: false,
		UseBuiltin:            false,
	})
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	_, err = registry.Get("nonexistent-skill")
	if err == nil {
		t.Error("expected error for nonexistent skill")
	}
}

func TestRegistryPrecedence(t *testing.T) {
	tmpDir := t.TempDir()

	// Create two directories with the same skill name
	highPrioDir := filepath.Join(tmpDir, "high-priority")
	lowPrioDir := filepath.Join(tmpDir, "low-priority")

	for _, dir := range []string{highPrioDir, lowPrioDir} {
		skillDir := filepath.Join(dir, "same-skill")
		if err := os.MkdirAll(skillDir, 0755); err != nil {
			t.Fatalf("failed to create skill dir: %v", err)
		}
	}

	// High priority skill
	highContent := `---
name: same-skill
description: "High priority version"
---
`
	if err := os.WriteFile(filepath.Join(highPrioDir, "same-skill", "SKILL.md"), []byte(highContent), 0644); err != nil {
		t.Fatalf("failed to write high priority SKILL.md: %v", err)
	}

	// Low priority skill
	lowContent := `---
name: same-skill
description: "Low priority version"
---
`
	if err := os.WriteFile(filepath.Join(lowPrioDir, "same-skill", "SKILL.md"), []byte(lowContent), 0644); err != nil {
		t.Fatalf("failed to write low priority SKILL.md: %v", err)
	}

	// Create registry with high priority first
	registry, err := NewRegistry(RegistryConfig{
		SearchPaths:           []string{highPrioDir, lowPrioDir},
		IncludeProjectSkills:  false,
		IncludeCodexPaths:     false,
		IncludeClaudePaths:    false,
		IncludeGeminiPaths:    false,
		IncludeUniversalPaths: false,
		UseBuiltin:            false,
	})
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	// List skills
	skills, err := registry.List()
	if err != nil {
		t.Fatalf("failed to list skills: %v", err)
	}

	if len(skills) != 1 {
		t.Fatalf("expected 1 skill (first wins), got %d", len(skills))
	}

	if skills[0].Description != "High priority version" {
		t.Errorf("expected high priority description, got %q", skills[0].Description)
	}

	// Check shadow count
	if registry.ShadowCount("same-skill") != 1 {
		t.Errorf("expected shadow count of 1, got %d", registry.ShadowCount("same-skill"))
	}
}

func TestRegistryDisabled(t *testing.T) {
	tmpDir := t.TempDir()

	// Create skills
	for _, name := range []string{"enabled-skill", "disabled-skill"} {
		skillDir := filepath.Join(tmpDir, "skills", name)
		if err := os.MkdirAll(skillDir, 0755); err != nil {
			t.Fatalf("failed to create skill dir: %v", err)
		}

		content := "---\nname: " + name + "\ndescription: \"Test\"\n---\n"
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
			t.Fatalf("failed to write SKILL.md: %v", err)
		}
	}

	registry, err := NewRegistry(RegistryConfig{
		SearchPaths:           []string{filepath.Join(tmpDir, "skills")},
		IncludeProjectSkills:  false,
		IncludeCodexPaths:     false,
		IncludeClaudePaths:    false,
		IncludeGeminiPaths:    false,
		IncludeUniversalPaths: false,
		UseBuiltin:            false,
		Disabled:              []string{"disabled-skill"},
	})
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	// List should not include disabled skill
	skills, err := registry.List()
	if err != nil {
		t.Fatalf("failed to list skills: %v", err)
	}

	if len(skills) != 1 {
		t.Fatalf("expected 1 skill (disabled filtered out), got %d", len(skills))
	}

	if skills[0].Name != "enabled-skill" {
		t.Errorf("expected enabled-skill, got %q", skills[0].Name)
	}

	// Get disabled skill should error
	_, err = registry.Get("disabled-skill")
	if err == nil {
		t.Error("expected error getting disabled skill")
	}
}

func TestRegistryListBySource(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a skill in user scope
	userDir := filepath.Join(tmpDir, "user-skills", "user-skill")
	if err := os.MkdirAll(userDir, 0755); err != nil {
		t.Fatalf("failed to create user skill dir: %v", err)
	}

	content := `---
name: user-skill
description: "A user skill"
---
`
	if err := os.WriteFile(filepath.Join(userDir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatalf("failed to write SKILL.md: %v", err)
	}

	registry, err := NewRegistry(RegistryConfig{
		SearchPaths:           []string{filepath.Join(tmpDir, "user-skills")},
		IncludeProjectSkills:  false,
		IncludeCodexPaths:     false,
		IncludeClaudePaths:    false,
		IncludeGeminiPaths:    false,
		IncludeUniversalPaths: false,
		UseBuiltin:            false,
	})
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	// List by user source
	userSkills, err := registry.ListBySource(SourceUser)
	if err != nil {
		t.Fatalf("failed to list user skills: %v", err)
	}

	if len(userSkills) != 1 {
		t.Errorf("expected 1 user skill, got %d", len(userSkills))
	}

	// List by local source (should be empty)
	localSkills, err := registry.ListBySource(SourceLocal)
	if err != nil {
		t.Fatalf("failed to list local skills: %v", err)
	}

	if len(localSkills) != 0 {
		t.Errorf("expected 0 local skills, got %d", len(localSkills))
	}
}

func TestRegistryReload(t *testing.T) {
	tmpDir := t.TempDir()
	skillsDir := filepath.Join(tmpDir, "skills")

	// Create initial skill
	skillDir := filepath.Join(skillsDir, "initial-skill")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatalf("failed to create skill dir: %v", err)
	}

	content := `---
name: initial-skill
description: "Initial skill"
---
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatalf("failed to write SKILL.md: %v", err)
	}

	registry, err := NewRegistry(RegistryConfig{
		SearchPaths:           []string{skillsDir},
		IncludeProjectSkills:  false,
		IncludeCodexPaths:     false,
		IncludeClaudePaths:    false,
		IncludeGeminiPaths:    false,
		IncludeUniversalPaths: false,
		UseBuiltin:            false,
	})
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	// Initial list
	skills, _ := registry.List()
	if len(skills) != 1 {
		t.Fatalf("expected 1 initial skill, got %d", len(skills))
	}

	// Get the skill to cache it
	_, err = registry.Get("initial-skill")
	if err != nil {
		t.Fatalf("failed to get initial-skill: %v", err)
	}

	// Add new skill
	newSkillDir := filepath.Join(skillsDir, "new-skill")
	if err := os.MkdirAll(newSkillDir, 0755); err != nil {
		t.Fatalf("failed to create new skill dir: %v", err)
	}

	newContent := `---
name: new-skill
description: "New skill"
---
`
	if err := os.WriteFile(filepath.Join(newSkillDir, "SKILL.md"), []byte(newContent), 0644); err != nil {
		t.Fatalf("failed to write new SKILL.md: %v", err)
	}

	// List will scan the directory again (List doesn't cache)
	// So we expect 2 skills now
	skills, _ = registry.List()
	if len(skills) != 2 {
		t.Fatalf("expected 2 skills after adding new skill, got %d", len(skills))
	}

	// But Get cache should still have the old skill
	skill, err := registry.Get("initial-skill")
	if err != nil {
		t.Fatalf("failed to get initial-skill from cache: %v", err)
	}
	if skill.Name != "initial-skill" {
		t.Errorf("expected cached skill name 'initial-skill', got %q", skill.Name)
	}

	// Reload clears the cache
	if err := registry.Reload(); err != nil {
		t.Fatalf("failed to reload: %v", err)
	}

	// After reload, cache is empty but List still works
	skills, _ = registry.List()
	if len(skills) != 2 {
		t.Errorf("expected 2 skills after reload, got %d", len(skills))
	}
}

func TestCreateSkillDir(t *testing.T) {
	tmpDir := t.TempDir()

	err := CreateSkillDir(tmpDir, "my-new-skill")
	if err != nil {
		t.Fatalf("failed to create skill dir: %v", err)
	}

	// Check SKILL.md was created
	skillPath := filepath.Join(tmpDir, "my-new-skill", "SKILL.md")
	if _, err := os.Stat(skillPath); os.IsNotExist(err) {
		t.Error("SKILL.md was not created")
	}

	// Parse the created skill
	skill, err := LoadFromDir(filepath.Join(tmpDir, "my-new-skill"), SourceLocal, true)
	if err != nil {
		t.Fatalf("failed to load created skill: %v", err)
	}

	if skill.Name != "my-new-skill" {
		t.Errorf("expected name 'my-new-skill', got %q", skill.Name)
	}
}

func TestCopySkill(t *testing.T) {
	tmpDir := t.TempDir()

	// Create source skill
	srcDir := filepath.Join(tmpDir, "src", "original-skill")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatalf("failed to create src skill dir: %v", err)
	}

	srcContent := `---
name: original-skill
description: "Original skill"
---

# Original instructions
`
	if err := os.WriteFile(filepath.Join(srcDir, "SKILL.md"), []byte(srcContent), 0644); err != nil {
		t.Fatalf("failed to write source SKILL.md: %v", err)
	}

	// Create references directory
	refsDir := filepath.Join(srcDir, "references")
	if err := os.MkdirAll(refsDir, 0755); err != nil {
		t.Fatalf("failed to create references dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(refsDir, "guide.md"), []byte("# Guide"), 0644); err != nil {
		t.Fatalf("failed to write reference file: %v", err)
	}

	// Load source skill
	srcSkill, err := LoadFromDir(srcDir, SourceUser, true)
	if err != nil {
		t.Fatalf("failed to load source skill: %v", err)
	}

	// Copy to destination
	destDir := filepath.Join(tmpDir, "dest")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		t.Fatalf("failed to create dest dir: %v", err)
	}

	if err := CopySkill(srcSkill, destDir, "copied-skill"); err != nil {
		t.Fatalf("failed to copy skill: %v", err)
	}

	// Verify copied skill
	copiedSkill, err := LoadFromDir(filepath.Join(destDir, "copied-skill"), SourceUser, true)
	if err != nil {
		t.Fatalf("failed to load copied skill: %v", err)
	}

	if copiedSkill.Name != "copied-skill" {
		t.Errorf("expected name 'copied-skill', got %q", copiedSkill.Name)
	}

	// Check references were copied
	if len(copiedSkill.References) != 1 || copiedSkill.References[0] != "guide.md" {
		t.Errorf("expected references [guide.md], got %v", copiedSkill.References)
	}
}

func TestDefaultRegistryConfig(t *testing.T) {
	cfg := DefaultRegistryConfig()

	if cfg.Mode != "auto" {
		t.Errorf("expected mode 'auto', got %q", cfg.Mode)
	}

	if !cfg.AutoInvoke {
		t.Error("expected AutoInvoke to be true")
	}

	if cfg.MetadataBudgetTokens != 8000 {
		t.Errorf("expected MetadataBudgetTokens 8000, got %d", cfg.MetadataBudgetTokens)
	}

	if cfg.MaxActive != 8 {
		t.Errorf("expected MaxActive 8, got %d", cfg.MaxActive)
	}

	if cfg.IncludeProjectSkills {
		t.Error("expected IncludeProjectSkills to be false by default")
	}

	if !cfg.IncludeCodexPaths {
		t.Error("expected IncludeCodexPaths to be true")
	}

	if !cfg.UseBuiltin {
		t.Error("expected UseBuiltin to be true")
	}
}
