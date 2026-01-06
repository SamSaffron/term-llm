package edit

import (
	"testing"
)

func TestFindMatch_Exact(t *testing.T) {
	content := `func main() {
	fmt.Println("hello")
}
`
	search := `fmt.Println("hello")`

	result, err := FindMatch(content, search)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Level != MatchExact {
		t.Errorf("expected MatchExact, got %v", result.Level)
	}

	if result.Original != search {
		t.Errorf("expected original %q, got %q", search, result.Original)
	}
}

func TestFindMatch_Stripped(t *testing.T) {
	content := `func main() {
	fmt.Println("hello")
}
`
	// Search with different indentation
	search := `func main() {
fmt.Println("hello")
}`

	result, err := FindMatch(content, search)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Level != MatchStripped {
		t.Errorf("expected MatchStripped, got %v", result.Level)
	}
}

func TestFindMatch_NonContiguous(t *testing.T) {
	content := `func main() {
	x := 1
	y := 2
	z := 3
	fmt.Println(x, y, z)
}
`
	// Search with ... to skip middle lines
	search := `func main() {
...
	fmt.Println(x, y, z)
}`

	result, err := FindMatch(content, search)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Level != MatchNonContiguous {
		t.Errorf("expected MatchNonContiguous, got %v", result.Level)
	}

	// Verify the match spans from func main() to closing brace
	expected := `func main() {
	x := 1
	y := 2
	z := 3
	fmt.Println(x, y, z)
}`
	if result.Original != expected {
		t.Errorf("expected original:\n%s\ngot:\n%s", expected, result.Original)
	}
}

func TestFindMatch_Fuzzy(t *testing.T) {
	content := `func main() {
	fmt.Println("hello world")
}
`
	// Search with small typo (Prinltn instead of Println)
	search := `func main() {
	fmt.Prinltn("hello world")
}`

	result, err := FindMatch(content, search)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Level != MatchFuzzy {
		t.Errorf("expected MatchFuzzy, got %v", result.Level)
	}
}

func TestFindMatch_NotFound(t *testing.T) {
	content := `func main() {
	fmt.Println("hello")
}
`
	search := `func nonexistent() {
	completely different code
}`

	_, err := FindMatch(content, search)
	if err == nil {
		t.Error("expected error for non-matching search")
	}
}

func TestFindMatchWithGuard(t *testing.T) {
	content := `line 1
line 2
target line
line 4
line 5
`
	search := "target line"

	// Guard includes target line (line 3)
	result, err := FindMatchWithGuard(content, search, 2, 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Level != MatchExact {
		t.Errorf("expected MatchExact, got %v", result.Level)
	}

	// Guard excludes target line
	_, err = FindMatchWithGuard(content, search, 4, 5)
	if err == nil {
		t.Error("expected error when search is outside guard range")
	}
}

func TestApplyMatch(t *testing.T) {
	content := "hello world"
	search := "world"

	result, err := FindMatch(content, search)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	newContent := ApplyMatch(content, result, "universe")
	expected := "hello universe"

	if newContent != expected {
		t.Errorf("expected %q, got %q", expected, newContent)
	}
}

func TestLineRangeToByteRange(t *testing.T) {
	content := "line1\nline2\nline3\nline4\nline5"

	tests := []struct {
		name      string
		startLine int
		endLine   int
		wantStart int
		wantEnd   int
	}{
		{"full file", 1, 5, 0, len(content)},
		{"lines 2-3", 2, 3, 6, 18}, // "line2\nline3\n" starts at 6
		{"from line 3", 3, 0, 12, len(content)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end := lineRangeToByteRange(content, tt.startLine, tt.endLine)
			if start != tt.wantStart {
				t.Errorf("start: got %d, want %d", start, tt.wantStart)
			}
			if end != tt.wantEnd {
				t.Errorf("end: got %d, want %d", end, tt.wantEnd)
			}
		})
	}
}

func TestLevenshteinDistance(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"hello", "hello", 0},
		{"hello", "helo", 1},
		{"hello", "world", 4},
		{"kitten", "sitting", 3},
	}

	for _, tt := range tests {
		got := levenshteinDistance(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("levenshteinDistance(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestLineSimilarity(t *testing.T) {
	tests := []struct {
		a, b    string
		minSim  float64
		maxSim  float64
	}{
		{"hello", "hello", 1.0, 1.0},
		{"hello", "helo", 0.7, 0.9},
		{"hello", "world", 0.0, 0.3},
		{"  hello  ", "hello", 1.0, 1.0}, // whitespace trimmed
	}

	for _, tt := range tests {
		sim := lineSimilarity(tt.a, tt.b)
		if sim < tt.minSim || sim > tt.maxSim {
			t.Errorf("lineSimilarity(%q, %q) = %f, want between %f and %f",
				tt.a, tt.b, sim, tt.minSim, tt.maxSim)
		}
	}
}
