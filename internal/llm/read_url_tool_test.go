package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type failAfterNReadCloser struct {
	remaining int
	err       error
}

func (r *failAfterNReadCloser) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, r.err
	}
	if len(p) > r.remaining {
		p = p[:r.remaining]
	}
	for i := range p {
		p[i] = 'a'
	}
	r.remaining -= len(p)
	return len(p), nil
}

func (r *failAfterNReadCloser) Close() error {
	return nil
}

func TestReadURLToolExecuteLimitsBodyReadBeforeTruncating(t *testing.T) {
	readErr := errors.New("read past limit")
	tool := NewReadURLTool()
	tool.client = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: &failAfterNReadCloser{
					remaining: maxReadURLChars + 1,
					err:       readErr,
				},
				Header: make(http.Header),
			}, nil
		}),
	}

	args, err := json.Marshal(map[string]string{"url": "example.com/article"})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if strings.Contains(out.Content, readErr.Error()) {
		t.Fatalf("expected limited read to avoid body read error, got %q", out.Content)
	}

	const truncationSuffix = "\n\n[Content truncated at 50,000 characters]"
	if !strings.HasSuffix(out.Content, truncationSuffix) {
		start := len(out.Content) - len(truncationSuffix) - 20
		if start < 0 {
			start = 0
		}
		t.Fatalf("expected truncated content suffix, got %q", out.Content[start:])
	}
	if got, want := len(out.Content), maxReadURLChars+len(truncationSuffix); got != want {
		t.Fatalf("expected content length %d, got %d", want, got)
	}
	if out.Content[:32] != strings.Repeat("a", 32) {
		t.Fatalf("expected response body prefix to be preserved")
	}
}

var _ io.ReadCloser = (*failAfterNReadCloser)(nil)
