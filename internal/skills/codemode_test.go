package skills

import (
	"strings"
	"testing"
)

// codeModeSkill builds a minimal valid SKILL.md fragment with the given code-mode block.
func codeModeSkill(codeMode string) string {
	return "---\nname: test-skill\ndescription: A test skill\n" + codeMode + "---\n# Body\n"
}

// TestParseCodeModeManifestValid verifies that a complete, valid code-mode block is parsed.
func TestParseCodeModeManifestValid(t *testing.T) {
	content := codeModeSkill(`code-mode:
  module: js/index.mjs
  files:
    - js/index.mjs
    - js/helpers.mjs
  types: js/index.d.ts
  requires:
    tools:
      - browser_navigate
      - browser_screenshot
`)
	skill, err := ParseSkillMDContent(content, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if skill.CodeMode == nil {
		t.Fatal("CodeMode should be non-nil")
	}
	if skill.CodeMode.Module != "js/index.mjs" {
		t.Errorf("Module = %q, want %q", skill.CodeMode.Module, "js/index.mjs")
	}
	if len(skill.CodeMode.Files) != 2 {
		t.Errorf("Files len = %d, want 2", len(skill.CodeMode.Files))
	}
	if skill.CodeMode.Types != "js/index.d.ts" {
		t.Errorf("Types = %q, want %q", skill.CodeMode.Types, "js/index.d.ts")
	}
	if len(skill.CodeMode.Requires.Tools) != 2 {
		t.Errorf("Requires.Tools len = %d, want 2", len(skill.CodeMode.Requires.Tools))
	}
	if skill.CodeMode.Requires.Tools[0] != "browser_navigate" {
		t.Errorf("Requires.Tools[0] = %q, want %q", skill.CodeMode.Requires.Tools[0], "browser_navigate")
	}
}

// TestParseCodeModeManifestAbsent verifies that skills without code-mode have nil CodeMode.
func TestParseCodeModeManifestAbsent(t *testing.T) {
	content := "---\nname: simple-skill\ndescription: No code mode\n---\n# Body\n"
	skill, err := ParseSkillMDContent(content, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if skill.CodeMode != nil {
		t.Errorf("CodeMode should be nil for skill without code-mode block, got %+v", skill.CodeMode)
	}
}

// TestParseCodeModeManifestMinimal verifies the minimal valid case (module + files only).
func TestParseCodeModeManifestMinimal(t *testing.T) {
	content := codeModeSkill("code-mode:\n  module: index.mjs\n  files:\n    - index.mjs\n")
	skill, err := ParseSkillMDContent(content, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if skill.CodeMode == nil {
		t.Fatal("CodeMode should be non-nil")
	}
	if skill.CodeMode.Module != "index.mjs" {
		t.Errorf("Module = %q", skill.CodeMode.Module)
	}
	if skill.CodeMode.Types != "" {
		t.Errorf("Types should be empty, got %q", skill.CodeMode.Types)
	}
	if len(skill.CodeMode.Requires.Tools) != 0 {
		t.Errorf("Requires.Tools should be empty, got %v", skill.CodeMode.Requires.Tools)
	}
}

// TestParseCodeModeModuleNotInFilesRejected verifies that module not in files errors.
func TestParseCodeModeModuleNotInFilesRejected(t *testing.T) {
	content := codeModeSkill("code-mode:\n  module: main.mjs\n  files:\n    - other.mjs\n")
	_, err := ParseSkillMDContent(content, true)
	if err == nil {
		t.Fatal("expected error when module not in files")
	}
	if !strings.Contains(err.Error(), "must be listed") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestParseCodeModeEmptyModuleRejected verifies that an empty module field errors.
func TestParseCodeModeEmptyModuleRejected(t *testing.T) {
	content := codeModeSkill("code-mode:\n  module: \"\"\n  files:\n    - index.mjs\n")
	_, err := ParseSkillMDContent(content, true)
	if err == nil {
		t.Fatal("expected error for empty module")
	}
}

// TestParseCodeModeTraversalRejected verifies that path traversal in module is rejected.
func TestParseCodeModeTraversalRejected(t *testing.T) {
	content := codeModeSkill("code-mode:\n  module: ../escape.mjs\n  files:\n    - ../escape.mjs\n")
	_, err := ParseSkillMDContent(content, true)
	if err == nil {
		t.Fatal("expected error for ../escape.mjs")
	}
	if !strings.Contains(err.Error(), "traversal") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestParseCodeModeAbsolutePathRejected verifies absolute paths in module are rejected.
func TestParseCodeModeAbsolutePathRejected(t *testing.T) {
	content := codeModeSkill("code-mode:\n  module: /etc/passwd\n  files:\n    - /etc/passwd\n")
	_, err := ParseSkillMDContent(content, true)
	if err == nil {
		t.Fatal("expected error for absolute path in module")
	}
}

// TestParseCodeModeUnsupportedExtensionRejected verifies that .ts source files are rejected.
func TestParseCodeModeUnsupportedExtensionRejected(t *testing.T) {
	content := codeModeSkill("code-mode:\n  module: index.ts\n  files:\n    - index.ts\n")
	_, err := ParseSkillMDContent(content, true)
	if err == nil {
		t.Fatal("expected error for .ts file (must be .mjs or .js)")
	}
}

// TestParseCodeModeDuplicateFilesRejected verifies that duplicate entries in files are rejected.
func TestParseCodeModeDuplicateFilesRejected(t *testing.T) {
	content := codeModeSkill("code-mode:\n  module: index.mjs\n  files:\n    - index.mjs\n    - index.mjs\n")
	_, err := ParseSkillMDContent(content, true)
	if err == nil {
		t.Fatal("expected error for duplicate file entry")
	}
}

// TestParseCodeModeCaseAliasRejected verifies that case-alias duplicates are rejected.
func TestParseCodeModeCaseAliasRejected(t *testing.T) {
	content := codeModeSkill("code-mode:\n  module: index.mjs\n  files:\n    - index.mjs\n    - Index.mjs\n")
	_, err := ParseSkillMDContent(content, true)
	if err == nil {
		t.Fatal("expected error for case-alias duplicate")
	}
}

// TestParseCodeModeTypesWrongExtensionRejected verifies that types must be .d.ts.
func TestParseCodeModeTypesWrongExtensionRejected(t *testing.T) {
	content := codeModeSkill("code-mode:\n  module: index.mjs\n  files:\n    - index.mjs\n  types: index.ts\n")
	_, err := ParseSkillMDContent(content, true)
	if err == nil {
		t.Fatal("expected error for types with .ts extension (must be .d.ts)")
	}
}

// TestParseCodeModeTypesTraversalRejected verifies traversal in types path is rejected.
func TestParseCodeModeTypesTraversalRejected(t *testing.T) {
	content := codeModeSkill("code-mode:\n  module: index.mjs\n  files:\n    - index.mjs\n  types: ../secret.d.ts\n")
	_, err := ParseSkillMDContent(content, true)
	if err == nil {
		t.Fatal("expected error for traversal in types path")
	}
}

// TestParseCodeModeRequiresToolsInvalidNameRejected verifies that invalid tool names fail.
func TestParseCodeModeRequiresToolsInvalidNameRejected(t *testing.T) {
	content := codeModeSkill("code-mode:\n  module: index.mjs\n  files:\n    - index.mjs\n  requires:\n    tools:\n      - Invalid Tool\n")
	_, err := ParseSkillMDContent(content, true)
	if err == nil {
		t.Fatal("expected error for invalid tool name in requires.tools")
	}
}

// TestParseCodeModeTooManyFilesRejected verifies the 32-file limit at parse time.
func TestParseCodeModeTooManyFilesRejected(t *testing.T) {
	filesYAML := "code-mode:\n  module: f000.mjs\n  files:\n"
	for i := 0; i <= maxCodeModeFiles; i++ {
		filesYAML += "    - f" + string(rune('0'+i/100)) + string(rune('0'+(i/10)%10)) + string(rune('0'+i%10)) + ".mjs\n"
	}
	content := codeModeSkill(filesYAML)
	_, err := ParseSkillMDContent(content, true)
	if err == nil {
		t.Fatal("expected error for too many files")
	}
	if !strings.Contains(err.Error(), "at most") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestParseCodeModeDoesNotLeakIntoExtras verifies that code-mode is not placed in Extras.
func TestParseCodeModeDoesNotLeakIntoExtras(t *testing.T) {
	content := codeModeSkill("code-mode:\n  module: index.mjs\n  files:\n    - index.mjs\n")
	skill, err := ParseSkillMDContent(content, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := skill.Extras["code-mode"]; ok {
		t.Error("code-mode must not appear in Extras")
	}
}

// TestCodeModeManifestValidateDirect tests Validate directly on CodeModeManifest.
func TestCodeModeManifestValidateDirect(t *testing.T) {
	t.Run("nil is valid", func(t *testing.T) {
		var m *CodeModeManifest
		if err := m.Validate(); err != nil {
			t.Errorf("nil.Validate() = %v, want nil", err)
		}
	})
	t.Run("valid minimal", func(t *testing.T) {
		m := &CodeModeManifest{
			Module: "index.mjs",
			Files:  []string{"index.mjs"},
		}
		if err := m.Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil", err)
		}
	})
	t.Run("backslash in module", func(t *testing.T) {
		m := &CodeModeManifest{
			Module: `js\index.mjs`,
			Files:  []string{`js\index.mjs`},
		}
		if err := m.Validate(); err == nil {
			t.Error("expected error for backslash in module path")
		}
	})
	t.Run("scheme in file", func(t *testing.T) {
		m := &CodeModeManifest{
			Module: "index.mjs",
			Files:  []string{"index.mjs", "node:fs"},
		}
		if err := m.Validate(); err == nil {
			t.Error("expected error for scheme in files")
		}
	})
}
