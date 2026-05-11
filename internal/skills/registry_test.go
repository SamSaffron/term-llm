package skills

import (
	"fmt"
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
		IncludeProjectSkills:  false,
		IncludeEcosystemPaths: false,
	})
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}
	// Replace search paths with only the test directory
	registry.searchPaths = []searchPath{{
		path:   filepath.Join(tmpDir, "skills"),
		source: SourceUser,
	}}

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
		IncludeProjectSkills:  false,
		IncludeEcosystemPaths: false,
	})
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}
	// Replace search paths with only the test directory
	registry.searchPaths = []searchPath{{
		path:   filepath.Join(tmpDir, "skills"),
		source: SourceUser,
	}}

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

func TestRegistryGetByMismatchedFrontmatterName(t *testing.T) {
	tmpDir := t.TempDir()
	skillsDir := filepath.Join(tmpDir, "skills")
	skillDir := filepath.Join(skillsDir, "directory-name")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatalf("failed to create skill dir: %v", err)
	}

	content := `---
name: frontmatter-name
description: "Directory and frontmatter names differ"
---

# Loaded by canonical frontmatter name
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatalf("failed to write SKILL.md: %v", err)
	}

	registry, err := NewRegistry(RegistryConfig{
		IncludeProjectSkills:  false,
		IncludeEcosystemPaths: false,
	})
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}
	registry.searchPaths = []searchPath{{
		path:   skillsDir,
		source: SourceCodex,
	}}

	listed, err := registry.List()
	if err != nil {
		t.Fatalf("failed to list skills: %v", err)
	}
	if len(listed) != 1 || listed[0].Name != "frontmatter-name" {
		t.Fatalf("expected listed frontmatter-name skill, got %#v", listed)
	}

	skill, err := registry.Get("frontmatter-name")
	if err != nil {
		t.Fatalf("failed to get skill by frontmatter name: %v", err)
	}
	if skill.SourcePath != skillDir {
		t.Errorf("expected source path %q, got %q", skillDir, skill.SourcePath)
	}
	if !skill.IsLoaded() || skill.Body == "" {
		t.Error("expected full skill body to be loaded")
	}
}

func TestRegistryGetFallsBackWhenCachedListMetadataPathStale(t *testing.T) {
	tmpDir := t.TempDir()
	highDir := filepath.Join(tmpDir, "high")
	lowDir := filepath.Join(tmpDir, "low")
	for _, base := range []string{highDir, lowDir} {
		skillDir := filepath.Join(base, "same-skill")
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			t.Fatalf("mkdir skill dir: %v", err)
		}
	}

	highContent := `---
name: same-skill
description: "High priority version"
---

# High
`
	lowContent := `---
name: same-skill
description: "Low priority version"
---

# Low
`
	if err := os.WriteFile(filepath.Join(highDir, "same-skill", "SKILL.md"), []byte(highContent), 0o644); err != nil {
		t.Fatalf("write high skill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lowDir, "same-skill", "SKILL.md"), []byte(lowContent), 0o644); err != nil {
		t.Fatalf("write low skill: %v", err)
	}

	registry, err := NewRegistry(RegistryConfig{
		IncludeProjectSkills:  false,
		IncludeEcosystemPaths: false,
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	registry.searchPaths = []searchPath{
		{path: highDir, source: SourceUser},
		{path: lowDir, source: SourceCodex},
	}

	listed, err := registry.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 || listed[0].Description != "High priority version" {
		t.Fatalf("List = %#v, want high-priority skill", listed)
	}

	if err := os.RemoveAll(filepath.Join(highDir, "same-skill")); err != nil {
		t.Fatalf("remove high skill: %v", err)
	}

	skill, err := registry.Get("same-skill")
	if err != nil {
		t.Fatalf("Get after cached path removal: %v", err)
	}
	if skill.Description != "Low priority version" {
		t.Fatalf("Get description = %q, want low priority fallback", skill.Description)
	}
	if skill.SourcePath != filepath.Join(lowDir, "same-skill") {
		t.Fatalf("SourcePath = %q, want low-priority path", skill.SourcePath)
	}
}

func TestRegistryGetFallsBackWhenCachedLoadedPathStale(t *testing.T) {
	tmpDir := t.TempDir()
	highDir := filepath.Join(tmpDir, "high")
	lowDir := filepath.Join(tmpDir, "low")
	for _, base := range []string{highDir, lowDir} {
		skillDir := filepath.Join(base, "same-skill")
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			t.Fatalf("mkdir skill dir: %v", err)
		}
	}

	if err := os.WriteFile(filepath.Join(highDir, "same-skill", "SKILL.md"), []byte(`---
name: same-skill
description: "High priority loaded version"
---

# High
`), 0o644); err != nil {
		t.Fatalf("write high skill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lowDir, "same-skill", "SKILL.md"), []byte(`---
name: same-skill
description: "Low priority fallback version"
---

# Low
`), 0o644); err != nil {
		t.Fatalf("write low skill: %v", err)
	}

	registry, err := NewRegistry(RegistryConfig{
		IncludeProjectSkills:  false,
		IncludeEcosystemPaths: false,
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	registry.searchPaths = []searchPath{
		{path: highDir, source: SourceUser},
		{path: lowDir, source: SourceCodex},
	}

	loaded, err := registry.Get("same-skill")
	if err != nil {
		t.Fatalf("initial Get: %v", err)
	}
	if !loaded.IsLoaded() || loaded.Description != "High priority loaded version" {
		t.Fatalf("initial Get = %#v, want loaded high-priority skill", loaded)
	}

	if err := os.RemoveAll(filepath.Join(highDir, "same-skill")); err != nil {
		t.Fatalf("remove high skill: %v", err)
	}

	skill, err := registry.Get("same-skill")
	if err != nil {
		t.Fatalf("Get after loaded path removal: %v", err)
	}
	if skill.Description != "Low priority fallback version" {
		t.Fatalf("Get description = %q, want low priority fallback", skill.Description)
	}
	if skill.SourcePath != filepath.Join(lowDir, "same-skill") {
		t.Fatalf("SourcePath = %q, want low-priority path", skill.SourcePath)
	}
}

func TestRegistryListReturnsDefensiveCopies(t *testing.T) {
	tmpDir := t.TempDir()
	skillsDir := filepath.Join(tmpDir, "skills")
	skillDir := filepath.Join(skillsDir, "copy-test")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	content := `---
name: copy-test
description: "Original description"
metadata:
  owner: alice
tools:
  - name: copy_tool
    description: Copy tool
    script: scripts/copy.sh
    input:
      type: object
      properties:
        message:
          type: string
x-extra:
  nested:
    owner: alice
  items:
    - one
---
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write skill: %v", err)
	}

	registry, err := NewRegistry(RegistryConfig{
		IncludeProjectSkills:  false,
		IncludeEcosystemPaths: false,
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	registry.searchPaths = []searchPath{{path: skillsDir, source: SourceUser}}

	first, err := registry.List()
	if err != nil {
		t.Fatalf("first List: %v", err)
	}
	first[0].Name = "mutated"
	first[0].Description = "Mutated description"
	first[0].Metadata["owner"] = "mallory"
	first[0].Extras["x-extra"].(map[string]any)["nested"].(map[string]any)["owner"] = "mallory"
	first[0].Tools[0].Input["properties"].(map[string]any)["message"].(map[string]any)["type"] = "integer"

	second, err := registry.List()
	if err != nil {
		t.Fatalf("second List: %v", err)
	}
	if second[0].Name != "copy-test" {
		t.Fatalf("cached Name = %q, want original", second[0].Name)
	}
	if second[0].Description != "Original description" {
		t.Fatalf("cached Description = %q, want original", second[0].Description)
	}
	if second[0].Metadata["owner"] != "alice" {
		t.Fatalf("cached metadata owner = %q, want alice", second[0].Metadata["owner"])
	}
	owner := second[0].Extras["x-extra"].(map[string]any)["nested"].(map[string]any)["owner"]
	if owner != "alice" {
		t.Fatalf("cached nested extra owner = %q, want alice", owner)
	}
	messageType := second[0].Tools[0].Input["properties"].(map[string]any)["message"].(map[string]any)["type"]
	if messageType != "string" {
		t.Fatalf("cached nested tool input type = %q, want string", messageType)
	}
}

func TestRegistryListCacheInvalidatesLowercaseSkillMD(t *testing.T) {
	tmpDir := t.TempDir()
	skillsDir := filepath.Join(tmpDir, "skills")
	skillDir := filepath.Join(skillsDir, "lowercase-test")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	writeSkill := func(description string) {
		t.Helper()
		content := fmt.Sprintf(`---
name: lowercase-test
description: %q
---
`, description)
		if err := os.WriteFile(filepath.Join(skillDir, "skill.md"), []byte(content), 0o644); err != nil {
			t.Fatalf("write skill.md: %v", err)
		}
	}
	writeSkill("First description")

	registry, err := NewRegistry(RegistryConfig{
		IncludeProjectSkills:  false,
		IncludeEcosystemPaths: false,
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	registry.searchPaths = []searchPath{{path: skillsDir, source: SourceUser}}

	first, err := registry.List()
	if err != nil {
		t.Fatalf("first List: %v", err)
	}
	if len(first) != 1 || first[0].Description != "First description" {
		t.Fatalf("first List = %#v", first)
	}

	writeSkill("Updated lowercase description with a different size")

	second, err := registry.List()
	if err != nil {
		t.Fatalf("second List: %v", err)
	}
	if len(second) != 1 || second[0].Description != "Updated lowercase description with a different size" {
		t.Fatalf("second List = %#v, want updated lowercase skill metadata", second)
	}
}

func TestRegistryGetNotFound(t *testing.T) {
	registry, err := NewRegistry(RegistryConfig{
		IncludeProjectSkills:  false,
		IncludeEcosystemPaths: false,
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
		IncludeProjectSkills:  false,
		IncludeEcosystemPaths: false,
	})
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}
	// Replace search paths in priority order
	registry.searchPaths = []searchPath{
		{path: highPrioDir, source: SourceUser},
		{path: lowPrioDir, source: SourceUser},
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
		IncludeProjectSkills:  false,
		IncludeEcosystemPaths: false,
	})
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}
	// Replace search paths with only the test directory
	registry.searchPaths = []searchPath{{
		path:   filepath.Join(tmpDir, "user-skills"),
		source: SourceUser,
	}}

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
		IncludeProjectSkills:  false,
		IncludeEcosystemPaths: false,
	})
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}
	// Replace search paths with only the test directory
	registry.searchPaths = []searchPath{{
		path:   skillsDir,
		source: SourceUser,
	}}

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

	// List notices the search path changed and refreshes its metadata cache.
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

	// Save the test search paths before reload
	testSearchPaths := registry.searchPaths

	// Reload clears the cache (and rebuilds search paths from config)
	if err := registry.Reload(); err != nil {
		t.Fatalf("failed to reload: %v", err)
	}

	// Restore the test search paths after reload
	registry.searchPaths = testSearchPaths

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

	if !cfg.AutoInvoke {
		t.Error("expected AutoInvoke to be true")
	}

	if cfg.MetadataBudgetTokens != 8000 {
		t.Errorf("expected MetadataBudgetTokens 8000, got %d", cfg.MetadataBudgetTokens)
	}

	if cfg.MaxVisibleSkills != 50 {
		t.Errorf("expected MaxVisibleSkills 50, got %d", cfg.MaxVisibleSkills)
	}

	if !cfg.IncludeProjectSkills {
		t.Error("expected IncludeProjectSkills to be true by default")
	}

	if !cfg.IncludeEcosystemPaths {
		t.Error("expected IncludeEcosystemPaths to be true")
	}
}

func BenchmarkRegistryListRepeated(b *testing.B) {
	const skillCount = 200

	skillsDir := createBenchmarkSkills(b, skillCount)
	registry := newBenchmarkRegistry(b, skillsDir)

	if skills, err := registry.List(); err != nil {
		b.Fatalf("warm List: %v", err)
	} else if len(skills) != skillCount {
		b.Fatalf("warm List returned %d skills, want %d", len(skills), skillCount)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		skills, err := registry.List()
		if err != nil {
			b.Fatalf("List: %v", err)
		}
		if len(skills) != skillCount {
			b.Fatalf("List returned %d skills, want %d", len(skills), skillCount)
		}
	}
}

func BenchmarkRegistryListCold(b *testing.B) {
	const skillCount = 200

	skillsDir := createBenchmarkSkills(b, skillCount)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		registry := newBenchmarkRegistry(b, skillsDir)
		skills, err := registry.List()
		if err != nil {
			b.Fatalf("List: %v", err)
		}
		if len(skills) != skillCount {
			b.Fatalf("List returned %d skills, want %d", len(skills), skillCount)
		}
	}
}

func createBenchmarkSkills(b *testing.B, skillCount int) string {
	b.Helper()

	tmpDir := b.TempDir()
	skillsDir := filepath.Join(tmpDir, "skills")
	for i := 0; i < skillCount; i++ {
		name := fmt.Sprintf("bench-skill-%03d", i)
		skillDir := filepath.Join(skillsDir, name)
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			b.Fatalf("mkdir skill dir: %v", err)
		}
		content := fmt.Sprintf(`---
name: %s
description: "Benchmark skill %03d for registry listing"
---

# %s

Instructions.
`, name, i, name)
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
			b.Fatalf("write SKILL.md: %v", err)
		}
	}
	return skillsDir
}

func newBenchmarkRegistry(b *testing.B, skillsDir string) *Registry {
	b.Helper()

	registry, err := NewRegistry(RegistryConfig{
		IncludeProjectSkills:  false,
		IncludeEcosystemPaths: false,
	})
	if err != nil {
		b.Fatalf("NewRegistry: %v", err)
	}
	registry.searchPaths = []searchPath{{path: skillsDir, source: SourceUser}}
	return registry
}
