package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"sort"

	"github.com/samsaffron/term-llm/internal/complexity"
)

// sizeThreshold is the largest non-test file, in lines, that may exist without
// being recorded in the size baseline. A file counts lines as the analyzer does
// (newline count plus one).
const sizeThreshold = 1000

// fileGrowthSlack is how far a recorded oversize file may grow: enough for a
// bug fix, not enough to keep piling features into it.
const fileGrowthSlack = 25

// packageThreshold is the non-test line total above which a package directory is
// recorded. Packages grow with features, so only genuinely huge ones are tracked.
const packageThreshold = 15000

// packageGrowthSlackPercent is how far a recorded package may grow.
const packageGrowthSlackPercent = 2

const sizeRules = "new non-test .go files may not exceed 1000 lines; recorded oversize files may grow by at most 25 lines; " +
	"new packages may not exceed 15000 lines; recorded packages may grow by at most 2%; shrinking never fails"

type sizeBaseline struct {
	Rules     string        `json:"rules"`
	Threshold int           `json:"threshold"`
	Files     []sizeFile    `json:"files"`
	Packages  []sizePackage `json:"packages"`
}

type sizeFile struct {
	Path  string `json:"path"`
	Lines int    `json:"lines"`
}

// sizePackage is the non-test line total of a directory above the threshold.
type sizePackage struct {
	Dir   string `json:"dir"`
	Lines int    `json:"lines"`
}

// measureSizes returns oversize files and oversize package totals.
func measureSizes(report complexity.Report) (map[string]int, map[string]int) {
	totals := map[string]int{}
	files := map[string]int{}
	for _, f := range report.Files {
		if f.Test {
			continue
		}
		totals[path.Dir(f.Path)] += f.Lines
		if f.Lines > sizeThreshold {
			files[f.Path] = f.Lines
		}
	}
	packages := map[string]int{}
	for dir, lines := range totals {
		if lines > packageThreshold {
			packages[dir] = lines
		}
	}
	return files, packages
}

func buildSizeBaseline(report complexity.Report) sizeBaseline {
	files, packages := measureSizes(report)
	b := sizeBaseline{Rules: sizeRules, Threshold: sizeThreshold, Files: []sizeFile{}, Packages: []sizePackage{}}
	for p, n := range files {
		b.Files = append(b.Files, sizeFile{Path: p, Lines: n})
	}
	for d, n := range packages {
		b.Packages = append(b.Packages, sizePackage{Dir: d, Lines: n})
	}
	sort.Slice(b.Files, func(i, j int) bool { return b.Files[i].Path < b.Files[j].Path })
	sort.Slice(b.Packages, func(i, j int) bool { return b.Packages[i].Dir < b.Packages[j].Dir })
	return b
}

func writeSizeBase(p string, report complexity.Report) error {
	data, err := json.MarshalIndent(buildSizeBaseline(report), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(data, '\n'), 0o644)
}

// checkSizes returns blocking violations of the size ratchet and
// informational notes (shrinks, renames, stale entries) that only suggest
// regenerating the baseline.
func checkSizes(p string, report complexity.Report) (violations, notes []string, err error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, nil, err
	}
	var b sizeBaseline
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, nil, fmt.Errorf("parse size baseline: %w", err)
	}
	violations, notes = compareSizes(b, report)
	return violations, notes, nil
}

func packageAllowance(recorded int) int {
	return recorded + recorded*packageGrowthSlackPercent/100
}

func compareSizes(b sizeBaseline, report complexity.Report) (violations, notes []string) {
	files, packages := measureSizes(report)
	recordedFiles := map[string]int{}
	for _, f := range b.Files {
		recordedFiles[f.Path] = f.Lines
	}
	recordedPkgs := map[string]int{}
	for _, d := range b.Packages {
		recordedPkgs[d.Dir] = d.Lines
	}

	// Recorded paths that are gone or now under the threshold. A new oversize
	// file no larger than one of these is treated as a move or rename.
	var vanished []string
	for p := range recordedFiles {
		if _, ok := files[p]; !ok {
			vanished = append(vanished, p)
		}
	}
	sort.Strings(vanished)
	claimed := map[string]bool{}

	var newPaths []string
	for p := range files {
		newPaths = append(newPaths, p)
	}
	sort.Strings(newPaths)
	for _, p := range newPaths {
		n := files[p]
		want, ok := recordedFiles[p]
		switch {
		case !ok:
			if from := claimRename(vanished, claimed, recordedFiles, n); from != "" {
				notes = append(notes, fmt.Sprintf("oversize file appears moved: %s -> %s (%d lines)", from, p, n))
				continue
			}
			violations = append(violations, fmt.Sprintf("new file over %d lines: %s (%d); split it into files of at most %d lines", sizeThreshold, p, n, sizeThreshold))
		case n > want+fileGrowthSlack:
			violations = append(violations, fmt.Sprintf("file grew too much: %s recorded %d, now %d, allowance %d; split it or move new code into a separate file of at most %d lines", p, want, n, want+fileGrowthSlack, sizeThreshold))
		case n < want:
			notes = append(notes, fmt.Sprintf("file shrank: %s %d -> %d", p, want, n))
		}
	}
	for _, p := range vanished {
		if !claimed[p] {
			notes = append(notes, fmt.Sprintf("stale entry: %s is missing or at most %d lines", p, sizeThreshold))
		}
	}
	for d, n := range packages {
		want, ok := recordedPkgs[d]
		switch {
		case !ok:
			violations = append(violations, fmt.Sprintf("new package over %d lines: %s (%d); split it, or regenerate the baseline if it was moved", packageThreshold, d, n))
		case n > packageAllowance(want):
			violations = append(violations, fmt.Sprintf("package grew too much: %s recorded %d, now %d, allowance %d; move code into a smaller package", d, want, n, packageAllowance(want)))
		case n < want:
			notes = append(notes, fmt.Sprintf("package shrank: %s %d -> %d", d, want, n))
		}
	}
	for d := range recordedPkgs {
		if _, ok := packages[d]; !ok {
			notes = append(notes, fmt.Sprintf("stale entry: package %s is missing or at most %d lines", d, packageThreshold))
		}
	}
	sort.Strings(violations)
	sort.Strings(notes)
	return violations, notes
}

// claimRename returns the first unclaimed vanished path whose recorded size is
// at least n, marking it claimed.
func claimRename(vanished []string, claimed map[string]bool, recorded map[string]int, n int) string {
	for _, p := range vanished {
		if !claimed[p] && recorded[p] >= n {
			claimed[p] = true
			return p
		}
	}
	return ""
}
