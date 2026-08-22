package chat

import "testing"

func TestCanLoadModelMetadataByProviderType(t *testing.T) {
	cases := map[string]bool{
		"zen":         true,
		"opencode-go": true,
		"chatgpt":     true,
		"openai":      false,
	}
	for provider, want := range cases {
		m, _ := newEffortCmdTestModel(provider, "test-model")
		if got := m.canLoadModelMetadata(); got != want {
			t.Fatalf("canLoadModelMetadata(%q) = %v, want %v", provider, got, want)
		}
	}
}
