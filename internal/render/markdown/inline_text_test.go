package markdown

import (
	"strings"
	"testing"
	"unicode"
)

var controlEntityPayloads = []struct {
	name, source, clean string
}{
	{"osc52_decimal", "&#27;]52;c;c2VjcmV0&#7;", ""},
	{"osc52_hex", "&#x1b;]52;c;c2VjcmV0&#x07;", ""},
	{"osc52_uppercase_hex", "&#X1B;]52;c;c2VjcmV0&#X7;", ""},
	{"osc52_leading_zeros", "&#00027;]52;c;c2VjcmV0&#0007;", ""},
	{"osc52_no_semicolons", "&#27]52;c;c2VjcmV0&#7", ""},
	{"osc52_encoded_bracket", "&#27;&#93;52;c;c2VjcmV0&#7;", ""},
	{"osc52_st", "&#27;]52;c;c2VjcmV0&#27;&#92;", ""},
	{"osc8_bel", "&#27;]8;;https://evil.example&#7;click&#27;]8;;&#7;", "click"},
	{"osc8_st", "&#x1b;]8;;https://evil.example&#x1b;&#92;click&#x1b;]8;;&#x1b;&#92;", "click"},
	{"title", "&#27;]0;spoofed title&#7;", ""},
	{"title_st", "&#27;]2;spoofed title&#27;&#92;", ""},
	{"csi_erase", "&#27;[2J", ""},
	{"csi_cursor", "&#x1B;[1;1H", ""},
	{"sgr_untrusted", "&#27;[31mred&#27;[0m", "red"},
	{"c1_osc", "&#157;52;c;c2VjcmV0&#7;", ""},
	{"c1_osc_hex", "&#x9D;52;c;c2VjcmV0&#x7;", ""},
	{"c1_dcs", "&#144;hidden&#27;&#92;", ""},
	{"bell", "&#7;", ""},
	{"backspace", "&#x08;", ""},
	{"vertical_tab", "&#11;", ""},
	{"form_feed", "&#x0c;", ""},
	{"escape", "&#27;", ""},
	{"carriage_return", "&#13;", "\n"},
	{"delete", "&#127;", ""},
	{"c1_controls", "&#129;&#141;", ""},
}

func TestDecodeInlineTextSanitizesEntityTerminalControls(t *testing.T) {
	for _, payload := range controlEntityPayloads {
		t.Run(payload.name, func(t *testing.T) {
			if got := decodeInlineText(payload.source); got != payload.clean {
				t.Fatalf("decodeInlineText() = %q, want %q", got, payload.clean)
			}
		})
	}
}

func TestANSIRenderSanitizesEntityTerminalControls(t *testing.T) {
	for _, payload := range controlEntityPayloads {
		t.Run(payload.name, func(t *testing.T) {
			for _, styled := range []bool{false, true} {
				name := "unstyled"
				var palette Palette
				if styled {
					name, palette = "styled", testPalette
				}
				t.Run(name, func(t *testing.T) {
					config := Config{Palette: palette, Width: 200}
					got, err := RenderString("before"+payload.source+" after", config)
					if err != nil {
						t.Fatal(err)
					}
					// Strip only renderer SGR, not arbitrary terminal sequences:
					// a general ANSI stripper would hide the injection under test.
					plain := testANSIEscapeRe.ReplaceAllString(got, "")
					for _, r := range plain {
						if unicode.IsControl(r) && r != '\n' && r != '\t' {
							t.Fatalf("entity introduced terminal control %U in %q", r, got)
						}
					}
					// Source SGR must not be mistaken for trusted renderer styling.
					for _, sgr := range testANSIEscapeRe.FindAllString(got, -1) {
						if !styled || (sgr != newANSIStyles(palette).text.prefix && sgr != "\x1b[0m") {
							t.Fatalf("untrusted SGR survived: %q in %q", sgr, got)
						}
					}
					// Goldmark can split escape sequences across text nodes. Their
					// printable remnants are harmless; controls must never survive.
					if !strings.Contains(plain, "before") || !strings.HasSuffix(plain, " after") {
						t.Fatalf("surrounding text was lost: %q", got)
					}
				})
			}
		})
	}
}

func TestANSIRenderEntitySanitizationPreservesMarkdown(t *testing.T) {
	for _, tc := range []struct{ name, source, clean string }{
		{"heading", "# before&#27;&#91;2Jafter &amp; café", "# beforeafter &amp; café"},
		{"emphasis", "**before&#7;after &ouml;**", "**beforeafter &ouml;**"},
		{"link_label", "[before&#7;after](https://example.com)", "[beforeafter](https://example.com)"},
		{"image_alt", "![before&#7;after](https://example.com/a.png)", "![beforeafter](https://example.com/a.png)"},
		{"table", "| label |\n| --- |\n| before&#7;after |", "| label |\n| --- |\n| beforeafter |"},
		{"list", "- before&#7;after", "- beforeafter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := Config{Palette: testPalette, Width: 80}
			got, err := RenderString(tc.source, config)
			if err != nil {
				t.Fatal(err)
			}
			want, err := RenderString(tc.clean, config)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("RenderString() = %q, want %q", got, want)
			}
			if !strings.Contains(got, "\x1b[") {
				t.Fatalf("renderer styling was lost: %q", got)
			}
		})
	}
}

func TestDecodeInlineTextPreservesLegitimateText(t *testing.T) {
	for _, tc := range []struct{ source, want string }{
		{"plain café 世界", "plain café 世界"},
		{"AT&T &unknown;", "AT&T &unknown;"},
		{`Use \* &amp; &ouml; &#x4e16; &lt;tag&gt;`, "Use * & ö 世 <tag>"},
		{"&amp;#27;", "&#27;"}, // Decode once, not recursively.
		{"&Tab;&NewLine;", "\t\n"},
		{"&#13;&#10;", "\n"},
		{"&#0; &#128; &#155;", "� € ›"}, // HTML's replacement/mapping rules.
	} {
		t.Run(tc.source, func(t *testing.T) {
			if got := decodeInlineText(tc.source); got != tc.want {
				t.Fatalf("decodeInlineText() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDecodeInlineTextFastPathAllocations(t *testing.T) {
	for _, source := range []string{"plain café 世界", "ordinary ASCII text"} {
		if allocs := testing.AllocsPerRun(100, func() {
			if got := decodeInlineText(source); got != source {
				t.Fatalf("decodeInlineText() = %q, want %q", got, source)
			}
		}); allocs != 0 {
			t.Errorf("decodeInlineText(%q) allocated %g times, want 0", source, allocs)
		}
	}
}

func TestANSIRenderPreservesLiteralCodeEntities(t *testing.T) {
	const literal = "&#27;]52;c;c2VjcmV0&#7; &amp;"
	for _, source := range []string{
		"`" + literal + "`",
		"```\n" + literal + "\n```",
		"```go\n// " + literal + "\n```",
	} {
		t.Run(source, func(t *testing.T) {
			got, err := RenderString(source, Config{Palette: testPalette, Width: 100})
			if err != nil {
				t.Fatal(err)
			}
			if plain := testANSIEscapeRe.ReplaceAllString(got, ""); !strings.Contains(plain, literal) {
				t.Fatalf("literal code entities changed: %q", got)
			}
			if !strings.Contains(got, "\x1b[") {
				t.Fatalf("trusted code styling was lost: %q", got)
			}
		})
	}
}
