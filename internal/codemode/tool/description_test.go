//go:build !js

package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/codemode"
)

// Execute the example agents actually see, rather than maintaining a separate copy.
func TestDescriptionExample(t *testing.T) {
	for name, description := range map[string]string{"legacy": New().Spec().Description, "async": NewExec().Spec().Description} {
		t.Run(name, func(t *testing.T) {
			testDescriptionExample(t, description, name == "async")
		})
	}
}

func testDescriptionExample(t *testing.T, description string, async bool) {
	if len(description) > 1600 {
		t.Fatalf("description grew beyond the compact prompt budget: %d characters", len(description))
	}
	_, example, ok := strings.Cut(description, "\nExample (read_file admitted): ")
	if !ok {
		t.Fatal("description is missing its executable example")
	}
	example, _, _ = strings.Cut(example, "\n")

	for _, tt := range []struct {
		name     string
		isError  bool
		timedOut bool
		reject   bool
	}{
		{name: "success"},
		{name: "tool error", isError: true},
		{name: "timeout", timedOut: true},
		{name: "bridge rejection", reject: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			payload, err := json.Marshal(map[string]any{"text": "example text", "isError": tt.isError, "timedOut": tt.timedOut})
			if err != nil {
				t.Fatal(err)
			}
			host := &descriptionHost{result: payload, reject: tt.reject}
			input := codemode.Input{Code: `store("staged", 1);` + example}
			var result codemode.Result
			if async {
				var output []codemode.Emission
				cell, err := codemode.StartCell(context.Background(), codemode.CellSpec{Input: input, Names: []string{"read_file"}, Host: host, Sink: func(kind string, value json.RawMessage) error {
					output = append(output, codemode.Emission{Kind: kind, Value: append(json.RawMessage(nil), value...)})
					return nil
				}})
				if err != nil {
					t.Fatal(err)
				}
				select {
				case <-cell.Done():
				case <-time.After(5 * time.Second):
					cell.Cancel()
					t.Fatal("example did not finish")
				}
				result, _ = cell.Result()
				result.Output = output
			} else {
				result = codemode.Run(context.Background(), input, []string{"read_file"}, host, nil)
			}
			if host.calls != 1 {
				t.Fatalf("example must call the documented tool once, got %d", host.calls)
			}
			if tt.isError || tt.timedOut || tt.reject {
				if result.Status != "failed" || result.StateCommitted || len(result.State) != 0 || len(result.Output) != 0 {
					t.Fatalf("example failed to stop on an unsuccessful tool result: %+v", result)
				}
				return
			}
			if result.Status != "completed" || !result.StateCommitted || len(result.Output) != 1 || string(result.Output[0].Value) != `"example text"` {
				t.Fatalf("example did not emit the native tool's text: %+v", result)
			}
			wantState := `{"staged":1}`
			if async {
				wantState = `{"set":{"staged":1},"delete":[]}`
			}
			if string(result.State) != wantState {
				t.Fatalf("state=%s, want %s", result.State, wantState)
			}
		})
	}
}

type descriptionHost struct {
	result json.RawMessage
	calls  int
	reject bool
}

func (h *descriptionHost) Call(_ context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	h.calls++
	var input struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, err
	}
	if name != "read_file" || input.Path != "README.md" {
		return nil, fmt.Errorf("unexpected example call: %s %s", name, args)
	}
	if h.reject {
		return nil, fmt.Errorf("bridge rejected")
	}
	return h.result, nil
}

func (*descriptionHost) Describe(context.Context, []string) (json.RawMessage, error) {
	return json.RawMessage(`[]`), nil
}
