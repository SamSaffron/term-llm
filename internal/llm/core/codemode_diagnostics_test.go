package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestMarshalCodeModeJSONDiagnostics(t *testing.T) {
	want := "script_error: TypeError: broken\n    at <anonymous> (cell.js:14:43)\n"
	raw, err := MarshalCodeModeJSON(CodeModeEnvelope{CellID: "cm_test", Status: "failed", Error: want})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "<anonymous>") || strings.Contains(string(raw), `\\n`) {
		t.Fatalf("unreadable diagnostic: %s", raw)
	}
	var got CodeModeEnvelope
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Error != want {
		t.Fatalf("error = %q, want %q", got.Error, want)
	}
}

func TestCodeModeProjectionKeepsDiagnosticFramesReadable(t *testing.T) {
	raw, err := MarshalCodeModeJSON(CodeModeEnvelope{CellID: "cm_test", Status: "failed", Error: "script_error: broken\n    at <anonymous> (cell.js:1:1)"})
	if err != nil {
		t.Fatal(err)
	}
	output := ToolOutput{Content: string(raw), PreserveJSON: true, ContentParts: []ToolContentPart{
		{Type: ToolContentPartText, Text: strings.Repeat("x", 1000)},
		{Type: ToolContentPartText, Text: "\n" + string(raw)},
	}}
	projected := ProjectCodeModeOutput(output, 500)
	if !json.Valid([]byte(projected.Content)) || !strings.Contains(projected.Content, "<anonymous>") {
		t.Fatalf("projected diagnostic: %s", projected.Content)
	}
}

func TestCodeModeEnvelopeMaxWaitSeconds(t *testing.T) {
	for _, seconds := range []int64{0, 60, 600} {
		envelope := CodeModeEnvelopeFor(CodeModeCallResult{Yield: time.Duration(seconds) * time.Second}, "wait")
		raw, err := MarshalCodeModeJSON(envelope)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if got, ok := decoded["max_wait"]; !ok || got != float64(seconds) {
			t.Fatalf("max_wait = %v, want %d: %s", got, seconds, raw)
		}
		if _, ok := decoded["yield_time_ms"]; ok {
			t.Fatalf("obsolete field: %s", raw)
		}
	}
}
