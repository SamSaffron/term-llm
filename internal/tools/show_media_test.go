package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

type testMediaPublisher struct {
	path string
	err  error
}

func (p testMediaPublisher) PublishMedia(context.Context, string, string) (string, error) {
	return p.path, p.err
}

func TestShowMediaToolPublishesTypedMedia(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clip.mp4")
	data := append([]byte{0, 0, 0, 24}, []byte("ftypisom00000000")...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	tool := NewShowMediaTool(nil, &ToolConfig{BaseDir: dir})
	tool.SetPublisher(testMediaPublisher{path: filepath.Join(dir, "stored.mp4")})
	args, _ := json.Marshal(ShowMediaArgs{Path: "clip.mp4", Caption: "  Demo\nclip  "})
	out, err := tool.Execute(context.Background(), args)
	if err != nil || out.IsError || len(out.Media) != 1 {
		t.Fatalf("Execute() = %#v, %v", out, err)
	}
	media := out.Media[0]
	if media.Reference == "" || !strings.Contains(out.Content, "term-llm-media://"+media.Reference) {
		t.Fatalf("media reference was not returned to the model: media=%#v content=%q", media, out.Content)
	}
	if !strings.Contains(out.Content, "![descriptive alt text]") {
		t.Fatalf("embedding guidance missing from %q", out.Content)
	}
	if media.MediaType != "video/mp4" || media.Name != "clip.mp4" || media.Caption != "Demo clip" {
		t.Fatalf("media = %#v", media)
	}
	if media.SourcePath != path || !strings.HasSuffix(media.StoredPath, "stored.mp4") {
		t.Fatalf("paths = %#v", media)
	}
	if len(out.Images) != 0 {
		t.Fatalf("legacy images unexpectedly populated: %v", out.Images)
	}
}

func TestShowMediaToolRejectsUnsafeTypeAndPublisherFailure(t *testing.T) {
	dir := t.TempDir()
	htmlPath := filepath.Join(dir, "page.png")
	if err := os.WriteFile(htmlPath, []byte("<html><script>alert(1)</script>"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := NewShowMediaTool(nil, &ToolConfig{BaseDir: dir})
	args, _ := json.Marshal(ShowMediaArgs{Path: htmlPath})
	out, err := tool.Execute(context.Background(), args)
	if err != nil || !strings.Contains(out.Content, "unsupported media format") {
		t.Fatalf("unsafe type output = %#v, %v", out, err)
	}

	pngPath := filepath.Join(dir, "ok.png")
	if err := os.WriteFile(pngPath, []byte("\x89PNG\r\n\x1a\nfixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool.SetPublisher(testMediaPublisher{err: errors.New("disk full")})
	args, _ = json.Marshal(ShowMediaArgs{Path: pngPath})
	out, err = tool.Execute(context.Background(), args)
	if err != nil || !strings.Contains(out.Content, "cannot publish media") || len(out.Media) != 0 {
		t.Fatalf("publisher failure output = %#v, %v", out, err)
	}
}

func TestDetectShowMediaTypeRejectsLookalikeContainers(t *testing.T) {
	avif := append([]byte{0, 0, 0, 24}, []byte("ftypavif00000000")...)
	if got := detectShowMediaType(avif); got != "" {
		t.Fatalf("AVIF detected as %q", got)
	}
	matroska := append([]byte{0x1a, 0x45, 0xdf, 0xa3}, []byte("matroska")...)
	if got := detectShowMediaType(matroska); got != "" {
		t.Fatalf("Matroska detected as %q", got)
	}
	webm := append([]byte{0x1a, 0x45, 0xdf, 0xa3}, []byte("\x42\x82\x84webm")...)
	if got := detectShowMediaType(webm); got != "video/webm" {
		t.Fatalf("WebM detected as %q", got)
	}
}

func TestRegistryRetainsMediaPublisherWithoutShowMedia(t *testing.T) {
	registry, err := NewLocalToolRegistry(&ToolConfig{Enabled: []string{SpawnAgentToolName}}, &config.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	publisher := testMediaPublisher{path: "stored.png"}
	registry.SetMediaPublisher(publisher)
	if registry.MediaPublisher() == nil {
		t.Fatal("registry discarded publisher when show_media was not enabled")
	}
}

func TestShowMediaSpecAndLegacyStandardSet(t *testing.T) {
	spec := NewShowMediaTool(nil).Spec()
	if spec.Name != ShowMediaToolName {
		t.Fatalf("spec name = %q", spec.Name)
	}
	properties, _ := spec.Schema["properties"].(map[string]any)
	if _, ok := properties["path"]; !ok {
		t.Fatalf("schema properties = %#v", properties)
	}
	standard := StandardToolNames()
	contains := func(name string) bool {
		for _, item := range standard {
			if item == name {
				return true
			}
		}
		return false
	}
	if !contains(ShowMediaToolName) || contains(ShowImageToolName) {
		t.Fatalf("standard tools = %v", standard)
	}
}
