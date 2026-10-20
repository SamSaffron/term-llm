//go:build !js

package session

import (
	"strings"
	"testing"
)

func TestNativeHTMLExportRetainsGFM(t *testing.T) {
	var output strings.Builder
	markdown := newHTMLExportMarkdown(nil, nil, nil)
	if err := markdown.Convert([]byte("| a |\n|---|\n| b |\n\n~~removed~~"), &output); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<table>", "<del>removed</del>"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("native HTML export missing GFM output %q", want)
		}
	}
}
