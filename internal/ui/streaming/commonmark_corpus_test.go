package streaming

import (
	"bytes"
	"fmt"
	"testing"
)

func TestCommonMarkFullSpecMarkdownStreamingParity(t *testing.T) {
	input := []byte(commonMarkSpecMarkdown(t))
	chunks := adversarialMarkdownChunks(input)
	type renderResult struct {
		output []byte
		err    error
	}
	directResult := make(chan renderResult, 1)
	streamedResult := make(chan renderResult, 1)
	go func() {
		output, err := renderDirectBytesResult(input)
		directResult <- renderResult{output: output, err: err}
	}()
	go func() {
		output, err := renderStreamedBytesResult(chunks)
		streamedResult <- renderResult{output: output, err: err}
	}()

	want := <-directResult
	got := <-streamedResult
	if want.err != nil {
		t.Fatalf("direct render failed: %v", want.err)
	}
	if got.err != nil {
		t.Fatalf("streamed render failed: %v", got.err)
	}
	assertRenderedEqual(t, want.output, got.output, chunks)
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
