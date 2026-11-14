package tools

import (
	"fmt"
	"sync"
	"testing"

	"github.com/term-llm/term-llm/internal/agents"
)

func TestLocalToolRegistryConcurrentCustomRegistrationAndReads(t *testing.T) {
	cfg := DefaultToolConfig()
	registry, err := NewLocalToolRegistry(&cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				name := fmt.Sprintf("skill_tool_%d_%d", worker, j)
				err := registry.RegisterCustomTools([]agents.CustomToolDef{{
					Name:        name,
					Description: "test tool",
					Script:      "missing.sh",
				}}, "")
				if err != nil {
					t.Errorf("RegisterCustomTools: %v", err)
					return
				}
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = registry.GetSpecs()
				_, _ = registry.Get("skill_tool_0_0")
			}
		}()
	}
	wg.Wait()
}
