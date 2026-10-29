package markdown

import (
	"strings"
	"testing"
)

func BenchmarkDecodeInlineText(b *testing.B) {
	for _, tc := range []struct{ name, source string }{
		{"plain", "Paragraph with ordinary Unicode text: café 世界."},
		{"ampersand_no_entity", "Paragraph with AT&T and &unknown; in ordinary text."},
		{"entities", "Paragraph with &amp; &ouml; &#x4e16; &lt;tag&gt; entities."},
		{"control_entities", "before&#27;]52;c;c2VjcmV0&#7;after"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(tc.source)))
			for i := 0; i < b.N; i++ {
				_ = decodeInlineText(tc.source)
			}
		})
	}
}

func BenchmarkANSIRenderEntityText(b *testing.B) {
	for _, tc := range []struct{ name, paragraph string }{
		{"plain", "Paragraph with **bold** text, `code`, and a link to [example](https://example.com)."},
		{"ampersand_no_entity", "Paragraph with **AT&T** text, `code`, and an &unknown; entity."},
		{"entities", "Paragraph with **&ouml; &amp; &#x4e16;** text and &lt;tag&gt; entities."},
	} {
		b.Run(tc.name, func(b *testing.B) {
			input := []byte(strings.Repeat("## Heading\n\n"+tc.paragraph+"\n\n", 32))
			renderer := NewANSI(Config{Palette: testPalette, Width: 100})
			b.ReportAllocs()
			b.SetBytes(int64(len(input)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := renderer.Render(input); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
