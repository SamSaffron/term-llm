package input

import "testing"

func TestParseFileSpec(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantPath   string
		wantStart  int
		wantEnd    int
		wantRegion bool
		wantErr    bool
	}{
		{name: "no region", input: "main.go", wantPath: "main.go"},
		{name: "range", input: "main.go:11-22", wantPath: "main.go", wantStart: 11, wantEnd: 22, wantRegion: true},
		{name: "start only", input: "main.go:11-", wantPath: "main.go", wantStart: 11, wantEnd: 0, wantRegion: true},
		{name: "end only", input: "main.go:-22", wantPath: "main.go", wantStart: 0, wantEnd: 22, wantRegion: true},
		{name: "invalid", input: "", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := ParseFileSpec(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if spec.Path != tc.wantPath {
				t.Fatalf("path=%q, want %q", spec.Path, tc.wantPath)
			}
			if spec.StartLine != tc.wantStart || spec.EndLine != tc.wantEnd || spec.HasRegion != tc.wantRegion {
				t.Fatalf("got start=%d end=%d region=%v, want start=%d end=%d region=%v",
					spec.StartLine, spec.EndLine, spec.HasRegion, tc.wantStart, tc.wantEnd, tc.wantRegion)
			}
		})
	}
}

func TestExtractLines(t *testing.T) {
	content := "line1\nline2\nline3\nline4\nline5"

	tests := []struct {
		name  string
		start int
		end   int
		want  string
	}{
		{name: "full content", start: 0, end: 0, want: "line1\nline2\nline3\nline4\nline5"},
		{name: "lines 2-4", start: 2, end: 4, want: "line2\nline3\nline4"},
		{name: "from line 3", start: 3, end: 0, want: "line3\nline4\nline5"},
		{name: "to line 2", start: 0, end: 2, want: "line1\nline2"},
		{name: "single line", start: 3, end: 3, want: "line3"},
		{name: "start beyond end", start: 10, end: 0, want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractLines(content, tc.start, tc.end)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
