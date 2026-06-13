package streaming

import (
	"bytes"
	"testing"
	"unicode/utf8"
)

func FuzzStreamingCorpusParity(f *testing.F) {
	for _, doc := range loadCorpusDocuments(f) {
		f.Add(string(doc.content), int64(1), byte(0))
	}
	for _, ex := range loadSpec(f) {
		if len(ex.Markdown) <= 2048 {
			f.Add(ex.Markdown, int64(ex.Example), byte(ex.Example))
		}
	}
	seeds := []string{
		"# Heading\n\nParagraph with **bold** and `code`.\n",
		"```go\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n```\n",
		"- one\n- two\n\nDone.\n",
		"A setext heading\n---\n\nDone.\n",
		"| a | b |\n|---|---|\n| 1 | 2 |\n",
	}
	for i, seed := range seeds {
		f.Add(seed, int64(i+1), byte(i))
	}

	f.Fuzz(func(t *testing.T, input string, seed int64, mode byte) {
		if len(input) > 2048 {
			t.Skip()
		}
		if !utf8.ValidString(input) {
			t.Skip()
		}
		content := []byte(input)
		want := renderDirectBytes(t, content)
		var chunks [][]byte
		switch mode % 5 {
		case 0:
			chunks = [][]byte{append([]byte(nil), content...)}
		case 1:
			chunks = fixedSizeChunks(content, 1)
		case 2:
			chunks = splitIntoNPieces(content, 100)
		case 3:
			chunks = randomChunks(content, seed, 32)
		default:
			chunks = adversarialMarkdownChunks(content)
		}
		got := renderStreamedBytes(t, chunks)
		if !bytes.Equal(want, got) {
			assertRenderedEqual(t, want, got, chunks)
		}
	})
}
