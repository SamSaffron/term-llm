package llm

import "testing"

func TestParseProviderModel(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantProvider string
		wantModel    string
		wantErr      bool
	}{
		{name: "provider only", input: "gemini", wantProvider: "gemini"},
		{name: "provider with model", input: "openai:gpt-4o", wantProvider: "openai", wantModel: "gpt-4o"},
		{name: "openrouter with model", input: "openrouter:x-ai/grok-code-fast-1", wantProvider: "openrouter", wantModel: "x-ai/grok-code-fast-1"},
		{name: "openai compat with model", input: "openai-compat:mixtral", wantProvider: "openai-compat", wantModel: "mixtral"},
		{name: "invalid provider", input: "unknown:model", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			provider, model, err := ParseProviderModel(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if provider != tc.wantProvider {
				t.Fatalf("provider=%q, want %q", provider, tc.wantProvider)
			}
			if model != tc.wantModel {
				t.Fatalf("model=%q, want %q", model, tc.wantModel)
			}
		})
	}
}
