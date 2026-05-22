package llm

import (
	"fmt"
	"testing"
)

func BenchmarkBuildCompatTools(b *testing.B) {
	specs := make([]ToolSpec, 128)
	for i := range specs {
		specs[i] = ToolSpec{
			Name:        fmt.Sprintf("tool_%03d", i),
			Description: "benchmark tool",
			Schema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "path to read",
					},
					"query": map[string]interface{}{
						"type":        "string",
						"description": "search query",
					},
					"options": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"limit": map[string]interface{}{
								"type":    "integer",
								"minimum": 1,
								"maximum": 100,
							},
							"case_sensitive": map[string]interface{}{
								"type": "boolean",
							},
						},
					},
				},
				"required":             []string{"path"},
				"additionalProperties": false,
			},
		}
	}

	// Warm any immutable-schema caches so the benchmark represents repeated
	// agentic turns using the same registered tools.
	if _, err := buildCompatTools(specs); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tools, err := buildCompatTools(specs)
		if err != nil {
			b.Fatal(err)
		}
		if len(tools) != len(specs) {
			b.Fatalf("got %d tools, want %d", len(tools), len(specs))
		}
	}
}
