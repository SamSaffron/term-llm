package memory

import (
	"testing"

	"github.com/term-llm/term-llm/internal/llm/core"
)

func TestMiningToolCodeModeExposureInventory(t *testing.T) {
	// These dedicated-engine registration families deliberately remain direct.
	tools := map[string]core.Tool{
		"memory_search_fragments": &memorySearchFragmentsTool{},
		"memory_list_fragments":   &memoryListFragmentsTool{},
		"memory_get_fragment":     &memoryGetFragmentTool{},
		"memory_create_fragment":  &memoryCreateFragmentTool{},
		"memory_update_fragment":  &memoryUpdateFragmentTool{},
	}
	for name, tool := range tools {
		if spec := tool.Spec(); spec.Name != name {
			t.Fatalf("mining inventory %s registered as %s", name, spec.Name)
		}
		provider, ok := tool.(core.CodeModeExposureProvider)
		if !ok {
			t.Fatalf("%s lacks exposure declaration", name)
		}
		if exposure := provider.CodeModeExposure(); exposure.Allowed || exposure.Reason != "mining_scope" {
			t.Errorf("%s exposure = %+v", name, exposure)
		}
	}
}
