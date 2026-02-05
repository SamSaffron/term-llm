package sessions

import "testing"

func TestTruncateDisplay(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxWidth int
		suffix   string
		want     string
	}{
		{
			name:     "exact fit no truncation",
			input:    "abc",
			maxWidth: 3,
			suffix:   "...",
			want:     "abc",
		},
		{
			name:     "truncate with suffix when suffix fits",
			input:    "abcde",
			maxWidth: 4,
			suffix:   "...",
			want:     "a...",
		},
		{
			name:     "fallback dots when suffix does not fit",
			input:    "abcde",
			maxWidth: 2,
			suffix:   "...",
			want:     "..",
		},
		{
			name:     "double width boundary with suffix",
			input:    "文ab",
			maxWidth: 3,
			suffix:   "...",
			want:     "...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateDisplay(tt.input, tt.maxWidth, tt.suffix)
			if got != tt.want {
				t.Fatalf("truncateDisplay(%q, %d, %q) = %q, want %q", tt.input, tt.maxWidth, tt.suffix, got, tt.want)
			}
		})
	}
}

func TestFitToDisplayWidth(t *testing.T) {
	tests := []struct {
		name  string
		input string
		width int
		want  string
	}{
		{
			name:  "pad empty to width",
			input: "",
			width: 10,
			want:  "          ",
		},
		{
			name:  "truncate over width",
			input: "hello",
			width: 2,
			want:  "..",
		},
		{
			name:  "pad double width rune",
			input: "文",
			width: 3,
			want:  "文 ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fitToDisplayWidth(tt.input, tt.width)
			if got != tt.want {
				t.Fatalf("fitToDisplayWidth(%q, %d) = %q, want %q", tt.input, tt.width, got, tt.want)
			}
		})
	}
}
