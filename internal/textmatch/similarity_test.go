package textmatch

import "testing"

func TestSlug(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{" Hello, World ", "hello-world"},
		{"a/b/../c", "ab-c"},
		{"one___two...three", "one-two-three"},
		{"東京 42", "東京-42"},
		{"../", ""},
	} {
		if got := Slug(tc.in); got != tc.want {
			t.Errorf("Slug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNumberedLineRange(t *testing.T) {
	content := "zero\none\ntwo\nthree"
	for _, tc := range []struct {
		name       string
		start, end int
		want       string
	}{
		{name: "middle", start: 2, end: 3, want: "2: one\n3: two"},
		{name: "nonpositive start", start: 0, end: 1, want: "1: zero"},
		{name: "open end", start: 3, end: 0, want: "3: two\n4: three"},
		{name: "end beyond content", start: 4, end: 99, want: "4: three"},
		{name: "start beyond content", start: 9, end: 10, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NumberedLineRange(content, tc.start, tc.end); got != tc.want {
				t.Fatalf("NumberedLineRange() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLineSimilarityAndDistance(t *testing.T) {
	if got := LineSimilarity(" same ", "same"); got != 1 {
		t.Fatalf("trimmed similarity = %v, want 1", got)
	}
	if got := LevenshteinDistance("kitten", "sitting"); got != 3 {
		t.Fatalf("distance = %d, want 3", got)
	}
	if got := LineSimilarity("abc", "axc"); got <= 0 || got >= 1 {
		t.Fatalf("partial similarity = %v, want between zero and one", got)
	}
}
