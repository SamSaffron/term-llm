package chat

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestFilterSearchToolSpecsWhenSearchDisabled(t *testing.T) {
	specs := []llm.ToolSpec{
		{Name: llm.WebSearchToolName},
		{Name: llm.ReadURLToolName},
		{Name: "read_file"},
	}

	got := filterSearchToolSpecs(specs, false)

	if len(got) != 1 || got[0].Name != "read_file" {
		t.Fatalf("filterSearchToolSpecs disabled = %+v, want only read_file", got)
	}
}

func TestFilterSearchToolSpecsWhenSearchEnabled(t *testing.T) {
	specs := []llm.ToolSpec{
		{Name: llm.WebSearchToolName},
		{Name: llm.ReadURLToolName},
		{Name: "read_file"},
	}

	got := filterSearchToolSpecs(specs, true)

	if len(got) != len(specs) {
		t.Fatalf("filterSearchToolSpecs enabled len = %d, want %d", len(got), len(specs))
	}
	for i := range specs {
		if got[i].Name != specs[i].Name {
			t.Fatalf("filterSearchToolSpecs enabled[%d] = %q, want %q", i, got[i].Name, specs[i].Name)
		}
	}
}
