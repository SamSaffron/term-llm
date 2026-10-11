package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/testutil/mediafixture"
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
	data := mediafixture.MP4("isom")
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

func TestShowMediaAudioFormatsAndLimit(t *testing.T) {
	for mime, data := range mediafixture.Audio() {
		t.Run(mime, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "misleading.png")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			tool := NewShowMediaTool(nil, &ToolConfig{BaseDir: dir})
			args, _ := json.Marshal(ShowMediaArgs{Path: path, Caption: "Listen\nnow\x1b"})
			out, err := tool.Execute(context.Background(), args)
			if err != nil || len(out.Media) != 1 || out.Media[0].MediaType != mime || len(out.Images) != 0 {
				t.Fatalf("audio output=%+v,%v", out, err)
			}
			if out.Media[0].Caption != "Listen now" || !strings.Contains(out.Content, "even for audio") || !strings.Contains(out.Content, "Media ready ("+mime+")") {
				t.Fatalf("audio instructions=%+v", out)
			}
		})
	}
	// Sparse MP3 preserves valid starting frames without allocating 100 MiB.
	for _, size := range []int64{100<<20 - 1, 100 << 20, 100<<20 + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "large.mp3")
			if err := os.WriteFile(path, mediafixture.MP3(), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(path, size); err != nil {
				t.Fatal(err)
			}
			tool := NewShowMediaTool(nil, &ToolConfig{BaseDir: dir})
			args, _ := json.Marshal(ShowMediaArgs{Path: path})
			out, err := tool.Execute(context.Background(), args)
			if err != nil || (len(out.Media) == 1) != (size <= 100<<20) {
				t.Fatalf("size=%d output=%+v,%v", size, out, err)
			}
			if size > 100<<20 && !strings.Contains(out.Content, "audio files up to 100 MiB") {
				t.Fatalf("limit message=%s", out.Content)
			}
		})
	}
}

func TestShowMediaAudioFailurePaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "speech.mp3")
	if err := os.WriteFile(path, mediafixture.MP3(), 0600); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(ShowMediaArgs{Path: path})
	mgr := newApprovalAutoTestManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeAuto)
	mgr.SetPolicyReviewFunc(func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
		return PolicyDecision{Allowed: false, Rationale: "not authorized"}, nil
	}, nil)
	denied := NewShowMediaTool(mgr, &ToolConfig{BaseDir: dir})
	out, err := denied.Execute(context.Background(), args)
	if err != nil || len(out.Media) != 0 || !strings.Contains(out.Content, "denied") {
		t.Fatalf("denial=%+v,%v", out, err)
	}
	tool := NewShowMediaTool(nil, &ToolConfig{BaseDir: dir})
	tool.SetPublisher(testMediaPublisher{})
	out, err = tool.Execute(context.Background(), args)
	if err != nil || len(out.Media) != 0 || !strings.Contains(out.Content, "no stored path") {
		t.Fatalf("empty publication=%+v,%v", out, err)
	}
	for _, path := range []string{filepath.Join(dir, "missing.wav"), dir} {
		args, _ = json.Marshal(ShowMediaArgs{Path: path})
		out, err = tool.Execute(context.Background(), args)
		if err != nil || len(out.Media) != 0 {
			t.Fatalf("invalid path=%+v,%v", out, err)
		}
	}
}
