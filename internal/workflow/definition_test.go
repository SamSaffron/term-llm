package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

const testDefinition = `workflow {
  name = "research",
  description = "Compare two answers",
  inputs = { topic = "Topic to investigate" },
  phases = { "draft", "synthesis" },
}

local topic = input("topic", "Lua")
local answers = parallel {
  agent { prompt = "first " .. topic, label = "first" },
  agent { prompt = "second " .. topic, label = "second" },
}
return join(answers, "\n")
`

func TestParseDefinitionCapturesMetadataAndExactSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "research.lua")
	definition, err := ParseDefinition(path, []byte(testDefinition))
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	if definition.Name != "research" || definition.Description != "Compare two answers" {
		t.Fatalf("metadata = %#v", definition.Metadata)
	}
	if len(definition.Phases) != 2 || definition.Phases[1] != "synthesis" {
		t.Fatalf("phases = %#v", definition.Phases)
	}
	if definition.Source != testDefinition {
		t.Fatal("definition did not preserve exact source")
	}
	sum := sha256.Sum256([]byte(testDefinition))
	if definition.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("SHA256 = %q", definition.SHA256)
	}
}

func TestParseDefinitionFileReadsExplicitPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow.lua")
	if err := os.WriteFile(path, []byte(testDefinition), 0o600); err != nil {
		t.Fatal(err)
	}
	definition, err := ParseDefinitionFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if definition.Path != path || definition.Source != testDefinition {
		t.Fatalf("definition = %#v", definition)
	}
}

func TestParseDefinitionDoesNotExecuteWorkflowBody(t *testing.T) {
	source := []byte(`workflow { name = "safe-validation" }
error("workflow body executed during validation")`)
	if _, err := ParseDefinition("safe.lua", source); err != nil {
		t.Fatalf("ParseDefinition executed workflow body: %v", err)
	}
}

func TestParseDefinitionUsesLuaParserForMetadataTable(t *testing.T) {
	source := []byte(`workflow {
  name = "lua-syntax",
  description = [=[a long string containing } braces]=],
}
error("body must not run")`)
	definition, err := ParseDefinition("syntax.lua", source)
	if err != nil {
		t.Fatal(err)
	}
	if definition.Description != "a long string containing } braces" {
		t.Fatalf("description = %q", definition.Description)
	}
}

func TestParseDefinitionRequiresLeadingMetadataDeclaration(t *testing.T) {
	source := []byte(`local value = "before"
workflow { name = "late" }
return value`)
	if _, err := ParseDefinition("late.lua", source); err == nil {
		t.Fatal("ParseDefinition accepted a non-leading workflow declaration")
	}
}
