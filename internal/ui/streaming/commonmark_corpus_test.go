package streaming

import (
	"bytes"
	"fmt"
	"testing"
)

func TestCommonMarkFullSpecMarkdownStreamingParity(t *testing.T) {
	input := []byte(commonMarkSpecMarkdown(t))
	want := renderDirectBytes(t, input)
	cases := []corpusChunkCase{
		{name: "all-at-once", chunks: [][]byte{append([]byte(nil), input...)}},
		{name: "hundred-pieces", chunks: splitIntoNPieces(input, 100)},
		{name: "random-seed-1", chunks: randomChunks(input, 1, 32)},
		{name: "random-seed-89", chunks: randomChunks(input, 89, 32)},
		{name: "adversarial-markdown-cuts", chunks: adversarialMarkdownChunks(input)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderStreamedBytes(t, tc.chunks)
			assertRenderedEqual(t, want, got, tc.chunks)
		})
	}
}

func commonMarkSpecMarkdown(tb testing.TB) string {
	tb.Helper()
	examples := loadSpec(tb)
	var combined bytes.Buffer
	for _, ex := range examples {
		if combined.Len() > 0 {
			combined.WriteString("\n\n")
		}
		combined.WriteString(fmt.Sprintf("<!-- CommonMark example %d: %s -->\n", ex.Example, ex.Section))
		combined.WriteString(ex.Markdown)
	}
	return combined.String()
}
