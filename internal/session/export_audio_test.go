package session

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/share"
	"github.com/samsaffron/term-llm/internal/testutil/mediafixture"
	"golang.org/x/net/html"
)

const exportAudioReference = "0123456789abcdef0123456789abcdef"

func exportAudioFixture(t *testing.T, data []byte, mime string) core.MediaArtifact {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private-recording")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return core.MediaArtifact{Reference: exportAudioReference, SourcePath: path, MediaType: mime, Name: "narration.mp3", Caption: "Narrated summary"}
}

func exportAudioMessages(text string) []Message {
	return []Message{{Role: core.RoleAssistant, Parts: []core.Part{{Type: core.PartText, Text: text}}, TextContent: text}}
}

func TestExportAudioConsentAndFormats(t *testing.T) {
	for mime, data := range mediafixture.Audio() {
		for _, include := range []bool{false, true} {
			t.Run(mime+map[bool]string{false: "/default", true: "/opt-in"}[include], func(t *testing.T) {
				item := exportAudioFixture(t, data, mime)
				opts := ExportOptions{IncludeAudio: include, Media: []core.MediaArtifact{item}}
				messages := exportAudioMessages("Before\n\n![Listen](term-llm-media://" + item.Reference + ")\n\nAfter")
				html, err := ExportToHTML(&Session{ID: "audio"}, messages, opts)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(html, "<audio controls") != include || strings.Contains(html, "media-src data:") != include {
					t.Fatalf("player/CSP consent mismatch: include=%v", include)
				}
				if include {
					for _, want := range []string{"data:" + mime + ";base64,", `preload="metadata"`, `aria-label="Listen"`, `download="narration.mp3"`, "Narrated summary"} {
						if !strings.Contains(html, want) {
							t.Errorf("missing %q", want)
						}
					}
					if !(strings.Index(html, "Before") < strings.Index(html, "<audio controls") && strings.Index(html, "<audio controls") < strings.Index(html, "After")) {
						t.Fatal("audio displaced surrounding prose")
					}
				} else if !strings.Contains(html, "Audio: narration.mp3 (not included in export)") {
					t.Fatal("missing descriptive exclusion")
				}
				for _, secret := range []string{item.SourcePath, "term-llm-media://", "file://", "/media/"} {
					if strings.Contains(html, secret) {
						t.Errorf("leaked %q", secret)
					}
				}
				markdown := ExportToMarkdown(&Session{ID: "audio"}, messages, opts)
				if !strings.Contains(markdown, "Audio: narration.mp3 (not included in Markdown export)") || strings.Contains(markdown, "base64,") || strings.Contains(markdown, "term-llm-media://") {
					t.Fatal("Markdown was not text-only")
				}
			})
		}
	}
}

func TestExportAudioUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name               string
		data               []byte
		mime               string
		missing, directory bool
		reason             string
	}{
		{name: "invalid type", data: mediafixture.MP3(), mime: "audio/x-unapproved", reason: "invalid audio format"},
		{name: "bad bytes", data: []byte("ID3not audio"), mime: "audio/mpeg", reason: "invalid or mismatched"},
		{name: "mislabelled audio", data: mediafixture.WAV(), mime: "audio/mpeg", reason: "invalid or mismatched"},
		{name: "mislabelled video", data: mediafixture.MP4("mp42", "vide"), mime: "audio/mp4", reason: "invalid or mismatched"},
		{name: "missing", data: mediafixture.MP3(), mime: "audio/mpeg", missing: true, reason: "file unavailable"},
		{name: "directory", mime: "audio/mpeg", directory: true, reason: "file unavailable"},
		// Invalid bytes plus an oversized stat prove size rejection precedes
		// format inspection; the file can be sparse and is never read.
		{name: "above cap", mime: "audio/mpeg", reason: "exceeds the 2 MiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := exportAudioFixture(t, tc.data, tc.mime)
			if tc.missing {
				if err := os.Remove(item.SourcePath); err != nil {
					t.Fatal(err)
				}
			}
			if tc.directory {
				item.SourcePath = t.TempDir()
			}
			if tc.name == "above cap" {
				if err := os.Truncate(item.SourcePath, maxHTMLExportInlineAudioBytes+1); err != nil {
					t.Fatal(err)
				}
			}
			html, err := ExportToHTML(&Session{ID: "s"}, exportAudioMessages("![Listen](term-llm-media://"+item.Reference+")"), ExportOptions{IncludeAudio: true, Media: []core.MediaArtifact{item}})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(html, tc.reason) || strings.Contains(html, "<audio controls") || strings.Contains(html, "media-src data:") || strings.Contains(html, item.SourcePath) || strings.Contains(html, "term-llm-media://") {
				t.Fatalf("invalid omission for %s", tc.name)
			}
		})
	}
}

func TestExportAudioBudgetAndDedup(t *testing.T) {
	data := append(mediafixture.MP3(), make([]byte, (maxHTMLExportInlineAudioBytes/2)+1-len(mediafixture.MP3()))...)
	first := exportAudioFixture(t, data, "audio/mpeg")
	duplicate := exportAudioFixture(t, data, "audio/mpeg")
	otherData := bytes.Clone(data)
	otherData[len(otherData)-1] = 1
	other := exportAudioFixture(t, otherData, "audio/mpeg")
	audio := newExportAudio(ExportOptions{IncludeAudio: true})
	result := audio.resolve(first)
	if result.URL == "" {
		t.Fatal(result.Reason)
	}
	for _, item := range []core.MediaArtifact{first, duplicate} {
		if got := audio.resolve(item); got != result {
			t.Fatal("duplicate was not cached")
		}
	}
	if audio.used != len(data) {
		t.Fatalf("budget consumed %d, want %d", audio.used, len(data))
	}
	if got := audio.resolve(other); got.URL != "" || !strings.Contains(got.Reason, "budget") {
		t.Fatalf("overflow = %+v", got)
	}
	// Exact cap is accepted independently; it is not confused with MaxBytes.
	exact := exportAudioFixture(t, append(mediafixture.MP3(), make([]byte, maxHTMLExportInlineAudioBytes-len(mediafixture.MP3()))...), "audio/mpeg")
	if got := newExportAudio(ExportOptions{IncludeAudio: true}).resolve(exact); got.URL == "" {
		t.Fatal(got.Reason)
	}
}

func TestExportAudioCodeAndEscaping(t *testing.T) {
	item := exportAudioFixture(t, mediafixture.MP3(), "audio/mpeg")
	item.Name = `name" onerror="alert(1).mp3`
	item.Caption = `<script>alert(2)</script>`
	reference := "![Listen](term-llm-media://" + item.Reference + ")"
	literal := "```markdown\n" + reference + "\n```\n\n`" + reference + "`\n\n    " + reference
	for _, text := range []string{literal, "Before " + reference + " after\n\n" + literal} {
		html, err := ExportToHTML(&Session{ID: "s"}, exportAudioMessages(text), ExportOptions{IncludeAudio: true, Media: []core.MediaArtifact{item}})
		if err != nil {
			t.Fatal(err)
		}
		wantPlayers := 0
		if strings.HasPrefix(text, "Before") {
			wantPlayers = 1
		}
		if got := strings.Count(html, "<audio controls"); got != wantPlayers {
			t.Fatalf("players = %d, want %d", got, wantPlayers)
		}
		if !strings.Contains(html, reference) {
			t.Fatal("literal code reference changed")
		}
		if strings.Contains(html, `<script>alert(2)</script>`) || strings.Contains(html, `download="name" onerror=`) || strings.Contains(html, "autoplay") {
			t.Fatal("unescaped audio metadata")
		}
		if wantPlayers == 0 && strings.Contains(html, "media-src data:") {
			t.Fatal("CSP enabled by a literal reference")
		}
		markdown := ExportToMarkdown(&Session{ID: "s"}, exportAudioMessages(text), ExportOptions{Media: []core.MediaArtifact{item}})
		if !strings.Contains(markdown, literal) {
			t.Fatal("Markdown code reference changed")
		}
	}
}

func TestExportAudioPrivateMetadataAndDurablePath(t *testing.T) {
	item := exportAudioFixture(t, mediafixture.MP3(), "audio/mpeg")
	item.StoredPath = item.SourcePath
	item.SourcePath = "/private/deleted-original.mp3"
	item.Name = "/private/narration.mp3"
	item.Caption = item.StoredPath + " https://private.example/media/secret.mp3"
	html, err := ExportToHTML(&Session{ID: "s"}, exportAudioMessages("![](term-llm-media://"+item.Reference+")"), ExportOptions{IncludeAudio: true, Media: []core.MediaArtifact{item}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, "<audio controls") || !strings.Contains(html, "narration.mp3") {
		t.Fatal("durable audio not resolved")
	}
	for _, private := range []string{item.SourcePath, item.StoredPath, "https://private.example", "term-llm-media://"} {
		if strings.Contains(html, private) {
			t.Errorf("private location leaked: %s", private)
		}
	}
}

func TestAudioShareBundleAndFiles(t *testing.T) {
	item := exportAudioFixture(t, mediafixture.MP3(), "audio/mpeg")
	messages := exportAudioMessages("![Listen](term-llm-media://" + item.Reference + ")")
	for _, include := range []bool{false, true} {
		opts := ExportOptions{IncludeAudio: include, Images: ExportImagesAssets, AssetMediaTypes: share.AssetImageMediaTypes, Media: []core.MediaArtifact{item}}
		files, err := ShareBundle(&Session{ID: "s"}, messages, opts)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 2 {
			t.Fatalf("audio sidecar created: %d files", len(files))
		}
		request := share.Request{RequestID: "audio", Visibility: share.VisibilityUnlisted, Entrypoint: "index.html", Files: files}
		if err := share.ValidateRequest(request); err != nil {
			t.Fatal(err)
		}
		inline, err := ShareFiles(&Session{ID: "s"}, messages, opts)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(inline["index.html"], "<audio controls") != include {
			t.Fatal("ShareFiles consent mismatch")
		}
		if !strings.Contains(inline["session.md"], "Audio: narration.mp3") || strings.Contains(inline["session.md"], "base64,") {
			t.Fatal("Markdown shared audio bytes")
		}
	}
}

func TestAudioShareFinalSizeLimits(t *testing.T) {
	item := exportAudioFixture(t, append(mediafixture.MP3(), make([]byte, maxHTMLExportInlineAudioBytes-len(mediafixture.MP3()))...), "audio/mpeg")
	// Repeated references must not multiply base64 payloads or duplicate the
	// payload in the download link; the bounded export now fits the provider.
	messages := exportAudioMessages(strings.Repeat("![Listen](term-llm-media://"+item.Reference+")\n\n", 4))
	files, err := ShareBundle(&Session{ID: "s"}, messages, ExportOptions{IncludeAudio: true, Media: []core.MediaArtifact{item}})
	if err != nil {
		t.Fatal(err)
	}
	request := share.Request{RequestID: "large", Visibility: share.VisibilityUnlisted, Entrypoint: "index.html", Files: files}
	if err := share.ValidateRequest(request); err != nil {
		t.Fatalf("bounded audio share failed: %v", err)
	}
	for _, file := range files {
		if file.Name != "index.html" {
			continue
		}
		if len(file.Content) > base64.StdEncoding.EncodedLen(maxHTMLExportInlineAudioBytes)+32<<10 {
			t.Fatalf("HTML exceeds bounded audio overhead: %d bytes", len(file.Content))
		}
		content := string(file.Content)
		if strings.Count(content, "data:audio/mpeg;base64,") != 1 || strings.Count(content, "<audio controls") != 1 || strings.Count(content, `href="#audio-1"`) != 4 {
			t.Fatal("recording payload/player not deduplicated across references and download")
		}
	}
}

func TestAudioShareSelectionsRetainMedia(t *testing.T) {
	item := exportAudioFixture(t, mediafixture.MP3(), "audio/mpeg")
	messages := []Message{
		shareTestMessage(1, 1, core.RoleUser, "question", ""),
		{ID: 2, Sequence: 2, Role: core.RoleTool, ResponseID: "r", Parts: []core.Part{{Type: core.PartToolResult, ToolResult: &core.ToolResult{Name: "show_media", Media: []core.MediaArtifact{item}}}}},
		shareTestMessage(3, 3, core.RoleAssistant, "![Listen](term-llm-media://"+item.Reference+")", "r"),
		shareTestMessage(4, 4, core.RoleAssistant, "later", "later"),
	}
	for _, scope := range []ShareScope{ShareScopeResponse, ShareScopeConversation} {
		for _, noTools := range []bool{false, true} {
			selection, err := SelectShare(messages, 3, scope)
			if err != nil {
				t.Fatal(err)
			}
			if noTools {
				var media []core.MediaArtifact
				selection.Messages, media = StripToolActivity(selection.Messages)
				selection.Media = append(selection.Media, media...)
			}
			for _, include := range []bool{false, true} {
				html, err := ExportToHTML(&Session{ID: "s"}, selection.Messages, ExportOptions{Media: selection.Media, IncludeAudio: include})
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(html, "<audio controls") != include || strings.Contains(html, ">later<") {
					t.Fatalf("scope %s noTools=%v include=%v lost media or expanded prefix", scope, noTools, include)
				}
			}
		}
	}
}

func TestExportAudioRequiresParsedRegisteredReference(t *testing.T) {
	item := exportAudioFixture(t, mediafixture.MP3(), "audio/mpeg")
	for _, tc := range []struct {
		name, text, want string
	}{
		{"raw HTML", `<audio src="https://private.example/media/recording.mp3" controls autoplay onplay="alert(1)"></audio>`, "raw HTML omitted"},
		{"unregistered", "![Unavailable](term-llm-media://ffffffffffffffffffffffffffffffff)", "Media: Unavailable"},
		{"video", "![Video](term-llm-media://" + item.Reference + ")", "Video: Video"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			media := item
			if tc.name == "video" {
				media.MediaType = "video/mp4"
			}
			html, err := ExportToHTML(&Session{ID: "s"}, exportAudioMessages(tc.text), ExportOptions{IncludeAudio: true, Media: []core.MediaArtifact{media}})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(html, "<audio controls") || strings.Contains(html, "media-src data:") || strings.Contains(html, "https://private.example") || strings.Contains(html, "term-llm-media://") || !strings.Contains(html, tc.want) {
				t.Fatal("nonregistered audio escaped the safe rendering boundary")
			}
		})
	}
}

func TestExportAudioLinkedImage(t *testing.T) {
	item := exportAudioFixture(t, mediafixture.MP3(), "audio/mpeg")
	value := "Before [![Listen](term-llm-media://" + item.Reference + ")](https://example.com) after"
	content, result, err := ExportToHTMLWithResult(&Session{ID: "s"}, exportAudioMessages(value), ExportOptions{IncludeAudio: true, Media: []core.MediaArtifact{item}})
	if err != nil {
		t.Fatal(err)
	}
	if result.AudioEmbedded != 1 || !strings.Contains(content, "Before") || !strings.Contains(content, "after") {
		t.Fatal("linked recording or surrounding prose lost")
	}
	doc, err := html.Parse(strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	var walk func(*html.Node, bool)
	walk = func(node *html.Node, inAnchor bool) {
		if node.Type == html.ElementNode && (node.Data == "audio" || node.Data == "a") && inAnchor {
			t.Fatal("nested audio/anchor")
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child, inAnchor || node.Data == "a")
		}
	}
	walk(doc, false)
}

func TestExportAudioResultCounts(t *testing.T) {
	good := exportAudioFixture(t, mediafixture.MP3(), "audio/mpeg")
	duplicate := exportAudioFixture(t, mediafixture.MP3(), "audio/mpeg")
	missing := exportAudioFixture(t, mediafixture.MP3(), "audio/mpeg")
	if err := os.Remove(missing.SourcePath); err != nil {
		t.Fatal(err)
	}
	invalid := exportAudioFixture(t, []byte("not audio"), "audio/mpeg")
	oversize := exportAudioFixture(t, mediafixture.MP3(), "audio/mpeg")
	if err := os.Truncate(oversize.SourcePath, maxHTMLExportInlineAudioBytes+1); err != nil {
		t.Fatal(err)
	}
	items := []core.MediaArtifact{good, duplicate, missing, invalid, oversize}
	var value strings.Builder
	for i := range items {
		items[i].Reference = fmt.Sprintf("%032x", i+1)
		value.WriteString(strings.Repeat("![Listen](term-llm-media://"+items[i].Reference+")\n\n", 2))
	}
	files, result, err := ShareBundleWithResult(&Session{ID: "s"}, exportAudioMessages(value.String()), ExportOptions{IncludeAudio: true, Media: items})
	if err != nil {
		t.Fatal(err)
	}
	if result != (ExportResult{AudioEmbedded: 1, AudioOmitted: 3}) {
		t.Fatalf("result = %+v", result)
	}
	for _, file := range files {
		if file.Name == "index.html" && strings.Count(string(file.Content), "data:audio/mpeg;base64,") != 1 {
			t.Fatal("identical recording bytes at different paths/references were embedded more than once")
		}
	}
}

func TestExportAudioNameShared(t *testing.T) {
	for _, name := range []string{`private\nested\recording.mp3`, "/private/narration.mp3", "", "https://private.example/media/secret.mp3"} {
		item := core.MediaArtifact{Name: name}
		card := buildHTMLAudio(newExportAudio(ExportOptions{}), item, "")
		if card.Name != exportAudioName(item) || !strings.Contains(markdownAudioPlaceholder(item), card.Name) {
			t.Fatalf("name differs across exports for %q", name)
		}
	}
}
