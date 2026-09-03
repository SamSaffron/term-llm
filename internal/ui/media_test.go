package ui

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestLocalFileURIForOS(t *testing.T) {
	tests := []struct {
		name string
		path string
		goos string
		want string
	}{
		{name: "unix escaping", path: "/tmp/a movie.mp4", goos: "linux", want: "file:///tmp/a%20movie.mp4"},
		{name: "windows drive", path: `C:\Users\Sam\a movie.mp4`, goos: "windows", want: "file:///C:/Users/Sam/a%20movie.mp4"},
		{name: "windows unc", path: `\\server\share\a movie.mp4`, goos: "windows", want: "file://server/share/a%20movie.mp4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := localFileURIForOS(tt.path, tt.goos); got != tt.want {
				t.Fatalf("localFileURIForOS() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMediaMarkdownStreamHoldsSplitReference(t *testing.T) {
	const reference = "0123456789abcdef0123456789abcdef"
	media := map[string]llm.MediaArtifact{reference: {
		Reference: reference, SourcePath: "/tmp/chart.mp4", MediaType: "video/mp4", Name: "chart.mp4",
	}}
	var stream MediaMarkdownStream
	if got := stream.Write("Before\n!", media); got != "Before\n" {
		t.Fatalf("first write = %q", got)
	}
	if got := stream.Write("[Chart](term-llm-media://0123456789abcdef", media); got != "" {
		t.Fatalf("partial reference emitted: %q", got)
	}
	got := stream.Write("0123456789abcdef)\nAfter", media) + stream.Finish(media)
	if !strings.Contains(got, "Open Chart") || !strings.Contains(got, "After") || strings.Contains(got, "term-llm-media://") {
		t.Fatalf("completed stream = %q", got)
	}
}

func TestRenderMediaMarkdownPlacesRegisteredArtifact(t *testing.T) {
	reference := "0123456789abcdef0123456789abcdef"
	artifact := llm.MediaArtifact{Reference: reference, SourcePath: "/tmp/chart.png", MediaType: "image/png", Name: "chart.png"}
	got := RenderMediaMarkdown(
		"Before\n\n![Sales chart](term-llm-media://"+reference+")\n\nAfter",
		map[string]llm.MediaArtifact{reference: artifact},
		func(value string) string { return "MD{" + value + "}" },
		func(path string) ImageArtifact { return ImageArtifact{Display: "IMAGE{" + path + "}"} },
	)
	for _, want := range []string{"MD{Before\n\n}", "IMAGE{/tmp/chart.png}", "MD{\n\nAfter}"} {
		if !strings.Contains(got, want) {
			t.Fatalf("RenderMediaMarkdown() = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "term-llm-media://") {
		t.Fatalf("resolved reference remained in %q", got)
	}
}

func TestReplaceMediaMarkdownMarksUnknownReferenceUnavailable(t *testing.T) {
	input := "![Unknown](term-llm-media://0123456789abcdef0123456789abcdef)"
	if got := ReplaceMediaMarkdown(input, nil); got != "[Unknown unavailable]" {
		t.Fatalf("ReplaceMediaMarkdown() = %q", got)
	}
}

func TestRenderMediaLinkIncludesClickableAndVisibleFallback(t *testing.T) {
	path := "/tmp/demo clip.mp4"
	got := RenderMediaLink(llm.MediaArtifact{SourcePath: path, MediaType: "video/mp4", Name: "demo.mp4", Caption: "Result"})
	uri := "file:///tmp/demo%20clip.mp4"
	for _, want := range []string{"Video:", "Open demo.mp4", uri, "Result", "\x1b]8;;" + uri} {
		if !strings.Contains(got, want) {
			t.Fatalf("RenderMediaLink() = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "term-llm-media://") {
		t.Fatalf("RenderMediaLink() leaked opaque reference: %q", got)
	}
}

func TestRenderMediaMarkdownScreenshotOutputBecomesFileLink(t *testing.T) {
	const reference = "65305cece88cb09c2a0a8276c31db6ce"
	const path = "/tmp/glossy red ball.png"
	content := "Here's a glossy red ball made with ImageMagick:" +
		"![A glossy red ball created with ImageMagick](term-llm-media://" + reference + ")"

	got := RenderMediaMarkdown(
		content,
		map[string]llm.MediaArtifact{reference: {
			Reference: reference, SourcePath: path, MediaType: "image/png", Name: "ball.png",
		}},
		func(value string) string { return strings.TrimSpace(value) },
		func(string) ImageArtifact {
			return ImageArtifact{Caption: "[Generated image: " + path + "]", Display: "<image>"}
		},
	)
	for _, want := range []string{"Here's a glossy red ball", "<image>", "Open A glossy red ball created with ImageMagick", "file:///tmp/glossy%20red%20ball.png"} {
		if !strings.Contains(got, want) {
			t.Fatalf("RenderMediaMarkdown() = %q, missing %q", got, want)
		}
	}
	if !strings.Contains(got, "ImageMagick:\n<image>") {
		t.Fatalf("RenderMediaMarkdown() did not start image on a fresh line: %q", got)
	}
	if strings.Contains(got, "term-llm-media://") {
		t.Fatalf("RenderMediaMarkdown() left opaque URL visible: %q", got)
	}
	if strings.Contains(got, "Generated image:") {
		t.Fatalf("RenderMediaMarkdown() retained duplicate legacy caption: %q", got)
	}
}

func TestRenderMediaMarkdownDoesNotResolveInlineCodeFenceReference(t *testing.T) {
	const reference = "0123456789abcdef0123456789abcdef"
	content := "```markdown\nText ![Chart](term-llm-media://" + reference + ")\n```"
	got := RenderMediaMarkdown(
		content,
		map[string]llm.MediaArtifact{reference: {Reference: reference, SourcePath: "/tmp/chart.png", MediaType: "image/png"}},
		func(value string) string { return value },
		func(string) ImageArtifact { return ImageArtifact{Display: "<image>"} },
	)
	if got != content {
		t.Fatalf("RenderMediaMarkdown() resolved fenced example: %q", got)
	}
}

func TestRenderMediaMarkdownKeepsSuppliedFreshLine(t *testing.T) {
	const reference = "0123456789abcdef0123456789abcdef"
	got := RenderMediaMarkdown(
		"Before\n![Chart](term-llm-media://"+reference+")",
		map[string]llm.MediaArtifact{reference: {
			Reference: reference, SourcePath: "/tmp/chart.png", MediaType: "image/png",
		}},
		func(value string) string { return value },
		func(string) ImageArtifact { return ImageArtifact{Display: "<image>"} },
	)
	if !strings.HasPrefix(got, "Before\n<image>") {
		t.Fatalf("RenderMediaMarkdown() did not preserve supplied line boundary: %q", got)
	}
}

func TestRenderMediaMarkdownAcceptsCaseAndAngleDestination(t *testing.T) {
	const reference = "ABCDEF0123456789ABCDEF0123456789"
	got := ReplaceMediaMarkdown(
		"![Preview](<TERM-LLM-MEDIA://"+reference+">)",
		map[string]llm.MediaArtifact{strings.ToLower(reference): {
			Reference: strings.ToLower(reference), SourcePath: "/tmp/preview.webm", MediaType: "video/webm",
		}},
	)
	if !strings.Contains(got, "file:///tmp/preview.webm") || strings.Contains(strings.ToLower(got), "term-llm-media://") {
		t.Fatalf("ReplaceMediaMarkdown() = %q", got)
	}
}
