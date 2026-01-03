package cmd

import "testing"

func TestFindEditMatchExact(t *testing.T) {
	content := "alpha\nbeta\ngamma\n"

	match, err := findEditMatch(content, "beta\n")
	if err != nil {
		t.Fatalf("findEditMatch returned error: %v", err)
	}

	newContent := applyEditMatch(content, match, "BETA\n")
	expected := "alpha\nBETA\ngamma\n"
	if newContent != expected {
		t.Fatalf("unexpected content:\nwant: %q\ngot:  %q", expected, newContent)
	}
}

func TestFindEditMatchWildcardAcrossLines(t *testing.T) {
	content := "start\none\ntwo\nend\n"
	oldString := "start\n" + editWildcardToken + "\nend"

	match, err := findEditMatch(content, oldString)
	if err != nil {
		t.Fatalf("findEditMatch returned error: %v", err)
	}

	expectedMatch := "start\none\ntwo\nend"
	if match.text != expectedMatch {
		t.Fatalf("unexpected match text:\nwant: %q\ngot:  %q", expectedMatch, match.text)
	}

	newContent := applyEditMatch(content, match, "REPLACED")
	expected := "REPLACED\n"
	if newContent != expected {
		t.Fatalf("unexpected content:\nwant: %q\ngot:  %q", expected, newContent)
	}
}

func TestFindEditMatchWildcardMultipleSegments(t *testing.T) {
	content := "xx a 1 b 2 c yy"
	oldString := "a" + editWildcardToken + "b" + editWildcardToken + "c"

	match, err := findEditMatch(content, oldString)
	if err != nil {
		t.Fatalf("findEditMatch returned error: %v", err)
	}

	newContent := applyEditMatch(content, match, "X")
	expected := "xx X yy"
	if newContent != expected {
		t.Fatalf("unexpected content:\nwant: %q\ngot:  %q", expected, newContent)
	}
}

func TestFindEditMatchWildcardNoAnchors(t *testing.T) {
	content := "abc"

	if _, err := findEditMatch(content, editWildcardToken); err == nil {
		t.Fatalf("expected error for wildcard token without anchors")
	}
}

func TestValidateGuardForReplaceWithWildcard(t *testing.T) {
	content := "line1\nline2\nline3\nline4\nline5\n"
	oldString := "line2\n" + editWildcardToken + "\nline4"

	match, err := findEditMatch(content, oldString)
	if err != nil {
		t.Fatalf("findEditMatch returned error: %v", err)
	}

	specOK := FileSpec{Path: "file.go", StartLine: 2, EndLine: 4, HasGuard: true}
	if err := validateGuardForReplace(content, match, specOK); err != nil {
		t.Fatalf("expected guard to pass: %v", err)
	}

	specBad := FileSpec{Path: "file.go", StartLine: 3, EndLine: 4, HasGuard: true}
	if err := validateGuardForReplace(content, match, specBad); err == nil {
		t.Fatalf("expected guard to fail for out-of-range edit")
	}
}

func TestLineRangeToByteRange(t *testing.T) {
	content := "line1\nline2\nline3\nline4\nline5\n"

	tests := []struct {
		name      string
		startLine int
		endLine   int
		wantStart int
		wantEnd   int
	}{
		{"full file", 1, 5, 0, 30},
		{"lines 2-4", 2, 4, 6, 24},
		{"line 3 only", 3, 3, 12, 18},
		{"from start to line 2", 1, 2, 0, 12},
		{"line 4 to end", 4, 0, 18, 30},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			start, end := lineRangeToByteRange(content, tc.startLine, tc.endLine)
			if start != tc.wantStart || end != tc.wantEnd {
				t.Errorf("lineRangeToByteRange(%d, %d) = (%d, %d), want (%d, %d)",
					tc.startLine, tc.endLine, start, end, tc.wantStart, tc.wantEnd)
			}
		})
	}
}

func TestFindEditMatchWithGuard(t *testing.T) {
	content := "line1\nline2\nline3\nline4\nline5\n"

	t.Run("exact match within guard", func(t *testing.T) {
		match, err := findEditMatchWithGuard(content, "line3\n", 2, 4)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if match.text != "line3\n" {
			t.Errorf("unexpected match text: %q", match.text)
		}
		newContent := applyEditMatch(content, match, "LINE3\n")
		expected := "line1\nline2\nLINE3\nline4\nline5\n"
		if newContent != expected {
			t.Errorf("unexpected content:\nwant: %q\ngot:  %q", expected, newContent)
		}
	})

	t.Run("exact match outside guard fails", func(t *testing.T) {
		_, err := findEditMatchWithGuard(content, "line1\n", 2, 4)
		if err == nil {
			t.Fatal("expected error when matching outside guard")
		}
		if err.Error() != "not found within lines 2-4" {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("wildcard match within guard", func(t *testing.T) {
		oldString := "line2\n" + editWildcardToken + "\nline4"
		match, err := findEditMatchWithGuard(content, oldString, 2, 4)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		expected := "line2\nline3\nline4"
		if match.text != expected {
			t.Errorf("unexpected match text:\nwant: %q\ngot:  %q", expected, match.text)
		}
	})

	t.Run("wildcard match spanning outside guard fails", func(t *testing.T) {
		oldString := "line1\n" + editWildcardToken + "\nline3"
		_, err := findEditMatchWithGuard(content, oldString, 2, 4)
		if err == nil {
			t.Fatal("expected error when wildcard spans outside guard")
		}
	})
}

func TestExtractLineRange(t *testing.T) {
	content := "line1\nline2\nline3\nline4\nline5"

	tests := []struct {
		name      string
		startLine int
		endLine   int
		want      string
	}{
		{"lines 2-4", 2, 4, "2: line2\n3: line3\n4: line4"},
		{"line 3 only", 3, 3, "3: line3"},
		{"all lines", 1, 5, "1: line1\n2: line2\n3: line3\n4: line4\n5: line5"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractLineRange(content, tc.startLine, tc.endLine)
			if got != tc.want {
				t.Errorf("extractLineRange(%d, %d):\nwant: %q\ngot:  %q",
					tc.startLine, tc.endLine, tc.want, got)
			}
		})
	}
}
