package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/complexity"
)

func sizeReport(files ...complexity.File) complexity.Report { return complexity.Report{Files: files} }

func file(p string, lines int) complexity.File {
	return complexity.File{Module: ".", Package: "x", Path: p, Lines: lines}
}

func TestSizeRatchet(t *testing.T) {
	base := sizeReport(file("pkg/big.go", 1500), file("pkg/small.go", 200), file("other/ok.go", 900),
		complexity.File{Path: "pkg/big_test.go", Lines: 5000, Test: true})
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
