package markdown

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	xansi "github.com/charmbracelet/x/ansi"
)

var testANSIEscapeRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

var testPalette = Palette{
	Primary:   "#b8bb26",
	Secondary: "#83a598",
	Success:   "#b8bb26",
	Warning:   "#fabd2f",
	Muted:     "#928374",
	Text:      "#ebdbb2",
}

func TestRenderString_GoldenVisibleOutput(t *testing.T) {
	tests := []struct {
		name   string
		width  int
		source string
		golden string
	}{
		{name: "heading", width: 80, source: "# Heading", golden: "heading_width80.golden"},
		{name: "setext", width: 80, source: "Heading\n=======", golden: "setext_width80.golden"},
		{name: "paragraph", width: 80, source: "Paragraph with **bold** and *em* and `code`.", golden: "paragraph_width80.golden"},
		{name: "wrap", width: 20, source: "Paragraph with **bold** and *em* and `code` and a long tail for wrapping behavior.", golden: "wrap_width20.golden"},
		{name: "list", width: 80, source: "- one\n- two", golden: "list_width80.golden"},
		{name: "ordered", width: 80, source: "1. one\n2. two", golden: "ordered_width80.golden"},
		{name: "blockquote", width: 80, source: "> quote\n> two", golden: "blockquote_width80.golden"},
		{name: "fence", width: 80, source: "```go\nfmt.Println(\"hi\")\n```", golden: "fence_width80.golden"},
		{name: "table", width: 20, source: "| a | b |\n|---|---|\n| c | d |", golden: "table_width20.golden"},
		{name: "link", width: 80, source: "[link](https://example.com)", golden: "link_width80.golden"},
		{name: "tabs", width: 80, source: "```\na\tb\n```", golden: "tabs_width80.golden"},
		{name: "mixed_headings", width: 80, source: "hello\n\n## sub\n\nworld", golden: "mixed_headings_width80.golden"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RenderString(tt.source, Config{
				Palette:           testPalette,
				Width:             tt.width,
				WrapOffset:        1,
				NormalizeTabs:     true,
				NormalizeNewlines: true,
				TrimSpace:         true,
			})
			if err != nil {
				t.Fatalf("RenderString returned error: %v", err)
			}

			expected := readGolden(t, tt.golden)
			if normalizeVisibleOutput(got) != normalizeVisibleOutput(expected) {
				t.Fatalf("visible output mismatch\nexpected:\n%s\nactual:\n%s", normalizeVisibleOutput(expected), normalizeVisibleOutput(got))
			}

			assertLinesWithinWidth(t, normalizeVisibleOutput(got), tt.width-1)
		})
	}
}

func TestRenderString_HeadingPreservesInlineFormatting(t *testing.T) {
	got, err := RenderString("## Why `--flag` matters and **you should care**", Config{
		Palette:           testPalette,
		Width:             80,
		WrapOffset:        1,
		NormalizeTabs:     true,
		NormalizeNewlines: true,
		TrimSpace:         true,
	})
	if err != nil {
		t.Fatalf("RenderString error: %v", err)
	}

	visible := normalizeVisibleOutput(got)
	if visible != "## Why --flag matters and you should care" {
		t.Fatalf("unexpected visible output: %q", visible)
	}

	if !strings.Contains(got, newANSIStyles(testPalette).code.Render("--flag")) {
		t.Fatalf("expected heading to preserve inline code styling, got: %q", got)
	}
}

func TestRenderString_ZeroWidthDoesNotError(t *testing.T) {
	_, err := RenderString("# title", Config{
		Palette:           testPalette,
		Width:             0,
		WrapOffset:        1,
		NormalizeTabs:     true,
		NormalizeNewlines: true,
		TrimSpace:         true,
	})
	if err != nil {
		t.Fatalf("RenderString must not fail for zero width: %v", err)
	}
}

func TestANSIResize_RewrapsOutput(t *testing.T) {
	r := NewANSI(Config{
		Palette:           testPalette,
		Width:             20,
		WrapOffset:        1,
		NormalizeTabs:     true,
		NormalizeNewlines: true,
		TrimSpace:         true,
	})

	source := []byte("Paragraph with **bold** and *em* and `code` and a long tail for wrapping behavior.")
	narrow, err := r.Render(source)
	if err != nil {
		t.Fatalf("narrow Render failed: %v", err)
	}

	r.Resize(40)
	wide, err := r.Render(source)
	if err != nil {
		t.Fatalf("wide Render failed: %v", err)
	}

	narrowVisible := normalizeVisibleOutput(string(narrow))
	wideVisible := normalizeVisibleOutput(string(wide))
	if narrowVisible == wideVisible {
		t.Fatalf("expected resize to change wrapping, but output was unchanged:\n%s", narrowVisible)
	}
	if strings.Count(wideVisible, "\n") >= strings.Count(narrowVisible, "\n") {
		t.Fatalf("expected wider render to use fewer lines\nnarrow:\n%s\nwide:\n%s", narrowVisible, wideVisible)
	}
	assertLinesWithinWidth(t, narrowVisible, 19)
	assertLinesWithinWidth(t, wideVisible, 39)
}

func TestRenderTable_WrapsInsteadOfTruncating(t *testing.T) {
	tests := []struct {
		name     string
		width    int
		source   string
		contains []string // substrings that must appear (no content lost)
		noEllip  bool     // if true, assert no ellipsis character
	}{
		{
			name:  "wide table wraps cells",
			width: 40,
			source: "| Feature | Description |\n" +
				"|---------|-------------|\n" +
				"| **API** | Programmatic access to all platform features |",
			contains: []string{"API", "Programmatic", "access", "platform", "features"},
			noEllip:  true,
		},
		{
			name:  "narrow table preserves all cell text",
			width: 60,
			source: "| Name | Details |\n" +
				"|------|--------|\n" +
				"| Alpha | This is a long description that should wrap nicely |",
			contains: []string{"Alpha", "This", "long", "description", "wrap", "nicely"},
			noEllip:  true,
		},
		{
			name:  "multi-column wide table",
			width: 80,
			source: "| Feature | Basic Plan | Enterprise Plan |\n" +
				"|---------|------------|----------------|\n" +
				"| **Support** | Email only | 24/7 phone support with dedicated account manager |",
			contains: []string{"Support", "Email only", "24/7", "phone", "dedicated", "account", "manager"},
			noEllip:  true,
		},
		{
			name:  "very narrow table hard-breaks long words",
			width: 25,
			source: "| Key | Value |\n" +
				"|-----|-------|\n" +
				"| AB | Hello world test |",
			contains: []string{"AB", "Hello", "world", "test"},
			noEllip:  true,
		},
		{
			name:     "table fits no wrapping needed",
			width:    80,
			source:   "| a | b |\n|---|---|\n| c | d |",
			contains: []string{"a", "b", "c", "d"},
			noEllip:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RenderString(tt.source, Config{
				Palette:           testPalette,
				Width:             tt.width,
				WrapOffset:        1,
				NormalizeTabs:     true,
				NormalizeNewlines: true,
				TrimSpace:         true,
			})
			if err != nil {
				t.Fatalf("RenderString error: %v", err)
			}

			visible := normalizeVisibleOutput(got)
			for _, want := range tt.contains {
				if !strings.Contains(visible, want) {
					t.Errorf("output missing %q\nvisible:\n%s", want, visible)
				}
			}
			if tt.noEllip && strings.Contains(visible, "…") {
				t.Errorf("output should not contain ellipsis truncation\nvisible:\n%s", visible)
			}
			assertLinesWithinWidth(t, visible, tt.width-1)
		})
	}
}

func TestRenderTable_InsertsSeparatorsBetweenAllRows(t *testing.T) {
	got, err := RenderString("| Name | Role |\n|------|------|\n| Alpha | Admin |\n| Beta | User |", Config{
		Palette:           testPalette,
		Width:             40,
		WrapOffset:        1,
		NormalizeTabs:     true,
		NormalizeNewlines: true,
		TrimSpace:         true,
	})
	if err != nil {
		t.Fatalf("RenderString error: %v", err)
	}

	visible := normalizeVisibleOutput(got)
	if !strings.Contains(visible, "┌") || !strings.Contains(visible, "┐") || !strings.Contains(visible, "└") || !strings.Contains(visible, "┘") {
		t.Fatalf("expected an outlined table\nvisible:\n%s", visible)
	}
	if strings.Count(visible, "┼") != 2 {
		t.Fatalf("expected a separator between each rendered row\nvisible:\n%s", visible)
	}
	if !strings.Contains(visible, "│ Alpha │ Admin │") || !strings.Contains(visible, "│ Beta  │ User  │") {
		t.Fatalf("expected boxed body rows in output\nvisible:\n%s", visible)
	}
	assertLinesWithinWidth(t, visible, 39)
}

func TestRenderTable_FallsBackToRecordsWhenCramped(t *testing.T) {
	got, err := RenderString("| Name | Description |\n|------|-------------|\n| Alpha | Programmatic access to all platform features |\n| Beta | Short note |", Config{
		Palette:           testPalette,
		Width:             16,
		WrapOffset:        1,
		NormalizeTabs:     true,
		NormalizeNewlines: true,
		TrimSpace:         true,
	})
	if err != nil {
		t.Fatalf("RenderString error: %v", err)
	}

	visible := normalizeVisibleOutput(got)
	if strings.Contains(visible, "│") || strings.Contains(visible, "┼") {
		t.Fatalf("expected cramped tables to fall back to record layout\nvisible:\n%s", visible)
	}
	for _, want := range []string{"Name:", "Description:", "Alpha", "Programmatic", "features", "Beta", "Short note"} {
		if !strings.Contains(visible, want) {
			t.Fatalf("record layout missing %q\nvisible:\n%s", want, visible)
		}
	}
	if !strings.Contains(visible, "───────────────") {
		t.Fatalf("expected a divider line between records\nvisible:\n%s", visible)
	}
	assertLinesWithinWidth(t, visible, 15)
}

func TestRenderTable_StylesHeadersAndBorders(t *testing.T) {
	got, err := RenderString("| Name | Role |\n|------|------|\n| Alpha | Admin |", Config{
		Palette:           testPalette,
		Width:             40,
		WrapOffset:        1,
		NormalizeTabs:     true,
		NormalizeNewlines: true,
		TrimSpace:         true,
	})
	if err != nil {
		t.Fatalf("RenderString error: %v", err)
	}

	styles := newANSIStyles(testPalette)
	if styles.tableHeader.prefix == "" || !strings.Contains(got, styles.tableHeader.prefix) {
		t.Fatalf("expected styled table header in output: %q", got)
	}
	if !strings.Contains(got, styles.tableBorder.Render("│")) {
		t.Fatalf("expected styled table outline in output: %q", got)
	}
	if !strings.Contains(got, styles.tableBorder.Render("┌───────┬───────┐")) {
		t.Fatalf("expected styled top border in output: %q", got)
	}
	if !strings.Contains(got, styles.tableBorder.Render("├───────┼───────┤")) {
		t.Fatalf("expected styled middle separator in output: %q", got)
	}
	if !strings.Contains(got, styles.tableBorder.Render("└───────┴───────┘")) {
		t.Fatalf("expected styled bottom border in output: %q", got)
	}
}

func TestRenderCodeBlock_HasBackground(t *testing.T) {
	got, err := RenderString("```\nhello\nworld\n```", Config{
		Palette:           testPalette,
		Width:             40,
		WrapOffset:        1,
		NormalizeTabs:     true,
		NormalizeNewlines: true,
		TrimSpace:         true,
	})
	if err != nil {
		t.Fatalf("RenderString error: %v", err)
	}
	if !strings.Contains(got, codeBgEsc) {
		t.Fatalf("code block output should contain background escape %q", codeBgEsc)
	}
	// Every line should start with the background escape and end with a reset.
	for i, line := range strings.Split(got, "\n") {
		if !strings.HasPrefix(line, codeBgEsc) {
			t.Errorf("line %d missing bg prefix: %q", i, line)
		}
		if !strings.HasSuffix(line, "\x1b[0m") {
			t.Errorf("line %d missing reset suffix: %q", i, line)
		}
	}
}

func TestFitColumnWidths_PreservesMinimum(t *testing.T) {
	widths := []int{20, 30, 25}
	fitColumnWidths(widths, 30, 3)
	for i, w := range widths {
		if w < 1 {
			t.Errorf("column %d width is %d, want >= 1", i, w)
		}
	}
}

func TestFitColumnWidths_FairShare(t *testing.T) {
	// Simulates: Provider | Model | Capabilities | Pricing
	// Natural widths: 9, 13, 50, 12. Total=84, available=70 → needs shrinking.
	// Equal share = 70/4 = 17. Cols 0,1,3 fit (9,13,12), release surplus.
	// Col 2 gets the rest (70-9-13-12 = 36).
	widths := []int{9, 13, 50, 12}
	fitColumnWidths(widths, 79, 4)

	// Small columns must keep their natural width — no squeezing.
	if widths[0] != 9 {
		t.Errorf("col 0: got %d, want 9 (natural width preserved)", widths[0])
	}
	if widths[1] != 13 {
		t.Errorf("col 1: got %d, want 13 (natural width preserved)", widths[1])
	}
	if widths[3] != 12 {
		t.Errorf("col 3: got %d, want 12 (natural width preserved)", widths[3])
	}
	// Widest column gets all the remaining space.
	if widths[2] != 36 {
		t.Errorf("col 2: got %d, want 36 (remaining space)", widths[2])
	}
}

func readGolden(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("testdata", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return string(data)
}

func normalizeVisibleOutput(s string) string {
	stripped := testANSIEscapeRe.ReplaceAllString(s, "")
	lines := strings.Split(stripped, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " ")
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

func assertLinesWithinWidth(t *testing.T, rendered string, width int) {
	t.Helper()
	if width < 1 {
		width = 1
	}
	for i, line := range strings.Split(rendered, "\n") {
		if xansi.StringWidth(line) > width {
			t.Fatalf("line %d exceeds width %d: %q", i, width, line)
		}
	}
}
