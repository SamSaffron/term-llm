package cmd

import "testing"

func TestNormalizeWebPushSubject(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty uses https default", input: "", want: "https://github.com/samsaffron/term-llm"},
		{name: "whitespace empty uses https default", input: "   ", want: "https://github.com/samsaffron/term-llm"},
		{name: "bare email kept", input: "test@example.com", want: "test@example.com"},
		{name: "mailto stripped once", input: "mailto:test@example.com", want: "test@example.com"},
		{name: "mailto stripped case insensitive", input: "MAILTO:test@example.com", want: "test@example.com"},
		{name: "https URL kept", input: "https://example.com", want: "https://example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeWebPushSubject(tt.input); got != tt.want {
				t.Fatalf("normalizeWebPushSubject(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
