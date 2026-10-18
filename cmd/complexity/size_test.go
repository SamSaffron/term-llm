package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/complexity"
)

func sizeReport(files ...complexity.File) complexity.Report { return complexity.Report{Files: files} }

// file builds a file whose `wc -l` count is lines; the analyzer reports one
// more because it counts newlines plus one.
func file(p string, lines int) complexity.File {
	return complexity.File{Module: ".", Package: "x", Path: p, Lines: lines + 1}
}

func TestSizeRatchet(t *testing.T) {
	base := sizeReport(file("pkg/big.go", 1500), file("pkg/small.go", 200), file("other/ok.go", 900),
		complexity.File{Path: "pkg/big_test.go", Lines: 5001, Test: true})
	b := buildSizeBaseline(base)
	if len(b.Files) != 1 || b.Files[0].Lines != 1500 || len(b.Packages) != 0 {
		t.Fatalf("baseline = %+v", b)
	}
	for _, tc := range []struct {
		name      string
		rep       complexity.Report
		violation string
		note      string
	}{
		{"unchanged", base, "", ""},
		{"threshold file is allowed", sizeReport(file("pkg/big.go", 1500), file("new/edge.go", 1000)), "", ""},
		{"growth within slack", sizeReport(file("pkg/big.go", 1500+fileGrowthSlack)), "", ""},
		{"growth over slack", sizeReport(file("pkg/big.go", 1501+fileGrowthSlack)), "file grew too much", ""},
		{"new offender", sizeReport(file("pkg/big.go", 1500), file("new/huge.go", 1001)), "new file over 1000", ""},
		{"moved file", sizeReport(file("pkg/renamed.go", 1500)), "", "appears moved: pkg/big.go -> pkg/renamed.go"},
		{"moved and grown is not a rename", sizeReport(file("pkg/renamed.go", 1600)), "new file over 1000", "stale entry: pkg/big.go"},
		{"below threshold is a note", sizeReport(file("pkg/big.go", 1000)), "", "stale entry: pkg/big.go"},
		{"shrink is a note", sizeReport(file("pkg/big.go", 1400)), "", "file shrank"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, n := compareSizes(b, tc.rep)
			got, notes := strings.Join(v, "\n"), strings.Join(n, "\n")
			if tc.violation == "" && got != "" {
				t.Fatalf("unexpected violations: %s", got)
			}
			if tc.violation != "" && !strings.Contains(got, tc.violation) {
				t.Fatalf("violations = %q, want %q", got, tc.violation)
			}
			if tc.note != "" && !strings.Contains(notes, tc.note) {
				t.Fatalf("notes = %q, want %q", notes, tc.note)
			}
		})
	}
}

func TestSizePackageRatchet(t *testing.T) {
	base := sizeReport(file("pkg/one.go", 800), file("pkg/two.go", 700))
	for i := 0; i < 20; i++ {
		base.Files = append(base.Files, file(fmt.Sprintf("pkg/f%02d.go", i), 900))
	}
	b := buildSizeBaseline(base) // 1500 + 18000 = 19500
	if len(b.Files) != 0 || len(b.Packages) != 1 || b.Packages[0].Lines != 19500 {
		t.Fatalf("baseline = %+v", b)
	}
	within := sizeReport(append(append([]complexity.File{}, base.Files...), file("pkg/extra.go", 390))...)
	if v, _ := compareSizes(b, within); len(v) != 0 {
		t.Fatalf("growth within 2%% should pass, got %v", v)
	}
	over := sizeReport(append(append([]complexity.File{}, base.Files...), file("pkg/extra.go", 391))...)
	if v, _ := compareSizes(b, over); !strings.Contains(strings.Join(v, "\n"), "package grew too much") {
		t.Fatalf("expected package growth violation, got %v", v)
	}
	var fresh []complexity.File
	for i := 0; i < 17; i++ {
		fresh = append(fresh, file(fmt.Sprintf("new/f%02d.go", i), 900))
	}
	withNew := sizeReport(append(append([]complexity.File{}, base.Files...), fresh...)...)
	if v, _ := compareSizes(b, withNew); !strings.Contains(strings.Join(v, "\n"), "new package over 15000") {
		t.Fatalf("expected new package violation, got %v", v)
	}
}

func TestSizeBaselineRoundTrip(t *testing.T) {
	rep := sizeReport(file("pkg/big.go", 1200))
	p := filepath.Join(t.TempDir(), "size.json")
	if err := writeSizeBase(p, rep); err != nil {
		t.Fatal(err)
	}
	violations, _, err := checkSizes(p, rep)
	if err != nil || len(violations) != 0 {
		t.Fatalf("violations = %v, err = %v", violations, err)
	}
	if _, _, err := checkSizes(filepath.Join(t.TempDir(), "missing.json"), rep); err == nil {
		t.Fatal("missing baseline must be an error")
	}
}

func TestSizeCountsLikeWC(t *testing.T) {
	// 1001 analyzer lines is a 1000-line file ending in a newline: allowed.
	f := complexity.File{Path: "pkg/edge.go", Lines: 1001}
	if files, _ := measureSizes(sizeReport(f)); len(files) != 0 {
		t.Fatalf("1000 wc lines must not be oversize: %v", files)
	}
	f.Lines = 1002
	if files, _ := measureSizes(sizeReport(f)); files["pkg/edge.go"] != 1001 {
		t.Fatalf("want 1001 wc lines, got %v", files)
	}
}

func TestSizeMoveMatching(t *testing.T) {
	b := sizeBaseline{Files: []sizeFile{
		{Path: "a/big.go", Lines: 1500},
		{Path: "a/mid.go", Lines: 1300},
		{Path: "a/exact.go", Lines: 1200},
		{Path: "a/other.go", Lines: 1100},
	}}
	for _, tc := range []struct {
		name      string
		rep       complexity.Report
		note      string
		violation string
	}{
		{"exact size beats smaller fit", sizeReport(file("n/x.go", 1300), file("a/big.go", 1500), file("a/exact.go", 1200), file("a/other.go", 1100)), "a/mid.go -> n/x.go", ""},
		{"smallest recorded fit with same basename", sizeReport(file("n/big.go", 1250), file("a/mid.go", 1300), file("a/exact.go", 1200), file("a/other.go", 1100)), "a/big.go -> n/big.go", ""},
		{"different basename and size is not a move", sizeReport(file("n/x.go", 1050), file("a/big.go", 1500), file("a/mid.go", 1300), file("a/exact.go", 1200), file("a/other.go", 1100)), "", "new file over 1000"},
		{"same basename but larger is not a move", sizeReport(file("n/other.go", 1101), file("a/big.go", 1500), file("a/mid.go", 1300), file("a/exact.go", 1200)), "", "new file over 1000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, n := compareSizes(b, tc.rep)
			if tc.note != "" && !strings.Contains(strings.Join(n, "\n"), tc.note) {
				t.Fatalf("notes = %q, want %q", n, tc.note)
			}
			if tc.violation != "" && !strings.Contains(strings.Join(v, "\n"), tc.violation) {
				t.Fatalf("violations = %q, want %q", v, tc.violation)
			}
			if tc.violation == "" && len(v) != 0 {
				t.Fatalf("unexpected violations: %v", v)
			}
		})
	}
}

func TestSizeMoveClaimsOnce(t *testing.T) {
	b := sizeBaseline{Files: []sizeFile{{Path: "a/big.go", Lines: 1500}}}
	v, _ := compareSizes(b, sizeReport(file("b/big.go", 1500), file("c/big.go", 1500)))
	if len(v) != 1 {
		t.Fatalf("second claimant must violate, got %v", v)
	}
}

func TestSizePackageMoveNote(t *testing.T) {
	var old, moved []complexity.File
	for i := 0; i < 17; i++ {
		old = append(old, file(fmt.Sprintf("old/pkg/f%02d.go", i), 900))
		moved = append(moved, file(fmt.Sprintf("new/pkg/f%02d.go", i), 900))
	}
	b := buildSizeBaseline(sizeReport(old...))
	v, n := compareSizes(b, sizeReport(moved...))
	if len(v) != 0 || !strings.Contains(strings.Join(n, "\n"), "package appears moved: old/pkg -> new/pkg") {
		t.Fatalf("violations = %v, notes = %v", v, n)
	}
	var renamed []complexity.File
	for i := 0; i < 17; i++ {
		renamed = append(renamed, file(fmt.Sprintf("new/other/f%02d.go", i), 900))
	}
	if v, _ := compareSizes(b, sizeReport(renamed...)); len(v) != 0 {
		t.Fatalf("exact-size package move should pass, got %v", v)
	}
	renamed = append(renamed, file("new/other/extra.go", 10))
	shrunk := append([]complexity.File(nil), renamed[:len(renamed)-1]...)
	shrunk[len(shrunk)-1] = file("new/other/f16.go", 800)
	if v, n := compareSizes(b, sizeReport(shrunk...)); len(v) != 0 || !strings.Contains(strings.Join(n, "\n"), "package appears moved: old/pkg -> new/other") {
		t.Fatalf("renamed and smaller package should be a note, violations = %v, notes = %v", v, n)
	}
	if v, _ := compareSizes(b, sizeReport(renamed...)); !strings.Contains(strings.Join(v, "\n"), "new package over") {
		t.Fatalf("bigger package with other name must violate, got %v", v)
	}
}
