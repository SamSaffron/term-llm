package image

import (
	"bytes"
	"context"
	"testing"
)

func TestDebugProviderGeneratesDeterministicImages(t *testing.T) {
	provider := NewDebugProvider(0)
	first, err := provider.Generate(context.Background(), GenerateRequest{Prompt: "robot cat"})
	if err != nil {
		t.Fatalf("first generate: %v", err)
	}
	second, err := provider.Generate(context.Background(), GenerateRequest{Prompt: "robot cat"})
	if err != nil {
		t.Fatalf("second generate: %v", err)
	}
	third, err := provider.Generate(context.Background(), GenerateRequest{Prompt: "different prompt"})
	if err != nil {
		t.Fatalf("third generate: %v", err)
	}

	if first.MimeType != "image/png" {
		t.Fatalf("MimeType = %q, want image/png", first.MimeType)
	}
	if !bytes.Equal(first.Data, second.Data) {
		t.Fatal("same prompt generated different image bytes")
	}
	if bytes.Equal(first.Data, third.Data) {
		t.Fatal("different prompts generated identical image bytes")
	}
	if !bytes.HasPrefix(first.Data, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatal("debug provider did not return PNG bytes")
	}
}
