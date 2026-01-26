package llm

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func TestDebugProviderName(t *testing.T) {
	tests := []struct {
		variant string
		want    string
	}{
		{"", "debug"},
		{"normal", "debug"},
		{"fast", "debug:fast"},
		{"slow", "debug:slow"},
		{"realtime", "debug:realtime"},
		{"burst", "debug:burst"},
		{"unknown", "debug:unknown"}, // Unknown variants still get named
	}

	for _, tt := range tests {
		t.Run(tt.variant, func(t *testing.T) {
			p := NewDebugProvider(tt.variant)
			if got := p.Name(); got != tt.want {
				t.Errorf("Name() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDebugProviderCredential(t *testing.T) {
	p := NewDebugProvider("")
	if got := p.Credential(); got != "none" {
		t.Errorf("Credential() = %q, want %q", got, "none")
	}
}

func TestDebugProviderCapabilities(t *testing.T) {
	p := NewDebugProvider("")
	caps := p.Capabilities()
	if caps.NativeWebSearch {
		t.Error("expected NativeWebSearch to be false")
	}
	if caps.NativeWebFetch {
		t.Error("expected NativeWebFetch to be false")
	}
}

func TestDebugProviderStream(t *testing.T) {
	p := NewDebugProvider("fast") // Use fast for quicker tests
	ctx := context.Background()

	stream, err := p.Stream(ctx, Request{})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	var fullText strings.Builder
	var gotUsage bool

	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		switch event.Type {
		case EventTextDelta:
			fullText.WriteString(event.Text)
		case EventUsage:
			gotUsage = true
			if event.Use.OutputTokens == 0 {
				t.Error("expected non-zero output tokens in usage")
			}
		case EventError:
			t.Fatalf("unexpected error event: %v", event.Err)
		}
	}

	text := fullText.String()

	// Verify markdown elements are present
	elements := []string{
		"# Debug Provider Output",
		"## Code Blocks",
		"```go",
		"```python",
		"```bash",
		"- First item",
		"1. First numbered item",
		"| Feature |",
		"> This is a simple blockquote",
		"**bold text**",
		"*italic text*",
	}

	for _, elem := range elements {
		if !strings.Contains(text, elem) {
			t.Errorf("stream output missing expected element: %q", elem)
		}
	}

	if !gotUsage {
		t.Error("stream did not emit usage event")
	}
}

func TestDebugProviderStreamCancellation(t *testing.T) {
	p := NewDebugProvider("slow") // Use slow to ensure we can cancel mid-stream
	ctx, cancel := context.WithCancel(context.Background())

	stream, err := p.Stream(ctx, Request{})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	// Read a few events then cancel
	eventCount := 0
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			// After cancel, we expect context.Canceled
			if err != context.Canceled {
				t.Errorf("expected context.Canceled error, got: %v", err)
			}
			return
		}
		if event.Type == EventTextDelta {
			eventCount++
			if eventCount >= 3 {
				cancel()
				// Continue receiving to see the cancellation error
			}
		}
	}
}

func TestDebugPresets(t *testing.T) {
	presets := GetDebugPresets()

	expectedPresets := []string{"fast", "normal", "slow", "realtime", "burst"}
	for _, name := range expectedPresets {
		preset, ok := presets[name]
		if !ok {
			t.Errorf("missing preset: %s", name)
			continue
		}
		if preset.ChunkSize <= 0 {
			t.Errorf("preset %s has invalid ChunkSize: %d", name, preset.ChunkSize)
		}
		if preset.Delay < 0 {
			t.Errorf("preset %s has invalid Delay: %v", name, preset.Delay)
		}
	}

	// Verify specific preset values from the plan
	if p := presets["fast"]; p.ChunkSize != 50 || p.Delay != 5*time.Millisecond {
		t.Errorf("fast preset mismatch: got %+v", p)
	}
	if p := presets["normal"]; p.ChunkSize != 20 || p.Delay != 20*time.Millisecond {
		t.Errorf("normal preset mismatch: got %+v", p)
	}
	if p := presets["slow"]; p.ChunkSize != 10 || p.Delay != 50*time.Millisecond {
		t.Errorf("slow preset mismatch: got %+v", p)
	}
	if p := presets["realtime"]; p.ChunkSize != 5 || p.Delay != 30*time.Millisecond {
		t.Errorf("realtime preset mismatch: got %+v", p)
	}
	if p := presets["burst"]; p.ChunkSize != 200 || p.Delay != 100*time.Millisecond {
		t.Errorf("burst preset mismatch: got %+v", p)
	}
}

func TestDebugProviderUnknownVariant(t *testing.T) {
	// Unknown variants should fall back to normal preset
	p := NewDebugProvider("nonexistent")

	// Should use normal preset values
	if p.preset.ChunkSize != 20 {
		t.Errorf("expected ChunkSize 20 for unknown variant, got %d", p.preset.ChunkSize)
	}
	if p.preset.Delay != 20*time.Millisecond {
		t.Errorf("expected Delay 20ms for unknown variant, got %v", p.preset.Delay)
	}
}
