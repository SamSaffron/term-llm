package chat

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/clipboard"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	sharepkg "github.com/samsaffron/term-llm/internal/share"
)

func TestShareAudioOmissionNotice(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		update, noTools, referenced bool
	}{
		{name: "create", referenced: true},
		{name: "update", update: true, referenced: true},
		{name: "without tools", noTools: true, referenced: true},
		{name: "unreferenced recording"},
		{name: "unreferenced without tools", noTools: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caps := defaultShareCapabilities()
			publisher := &recordingSharePublisher{caps: caps, result: sharepkg.Result{Provider: "test", ID: "opaque", URL: "https://example.test/share", Visibility: sharepkg.VisibilityUnlisted, Ready: true}}
			sess := &session.Session{ID: "audio-share", Name: "Audio"}
			if tc.update {
				sess.Share = &session.ShareState{Provider: "test", ID: "opaque", Scope: session.ShareScopeSession, IncludedAudio: true}
			}
			item := llm.MediaArtifact{Reference: "0123456789abcdef0123456789abcdef", Name: "recording.mp3", MediaType: "audio/mpeg", SourcePath: "/private/recording.mp3"}
			text := "Final answer."
			if tc.referenced {
				text += " ![Listen](term-llm-media://" + item.Reference + ")"
			}
			messages := []session.Message{
				*session.NewMessage(sess.ID, llm.Message{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{Name: "show_media", Media: []llm.MediaArtifact{item}}}}}, -1),
				*session.NewMessage(sess.ID, llm.AssistantText(text), -1),
			}
			store := &mockStore{sessions: map[string]*session.Session{sess.ID: sess}, messages: map[string][]session.Message{sess.ID: messages}}
			m := newCmdTestModel(store)
			m.sess = sess
			installCopyBackend(t, func(string) (clipboard.CopyMethod, error) { return clipboard.CopyMethodNative, nil })
			_, cmd := m.startShare(shareRequest{publisher: publisher, capabilities: caps, visibility: sharepkg.VisibilityUnlisted, noTools: tc.noTools}, tc.update)
			result := shareWorkResult(t, cmd)
			if result.err != nil {
				t.Fatal(result.err)
			}
			for _, file := range publisher.lastRequest.Files {
				if strings.Contains(string(file.Content), "data:audio/") || strings.Contains(string(file.Content), "<audio controls") {
					t.Fatal("TUI share embedded audio without consent")
				}
			}
			updated, _ := m.handleShareDone(result)
			content := updated.(*Model).dialog.Content()
			want := "Audio recordings were omitted"
			if tc.update {
				want = "Audio recordings were removed from the existing share"
			}
			for _, notice := range []string{want, "term-llm sessions share --include-audio", "web Share dialog"} {
				if !strings.Contains(content, notice) {
					t.Fatalf("missing %q: %s", notice, content)
				}
			}
			if store.updated.Share.IncludedAudio {
				t.Fatal("TUI share incorrectly retained prior audio state")
			}
		})
	}
}

func TestShareAudioNoticeAbsentWithoutRecordings(t *testing.T) {
	if got := shareAudioNotice(shareDoneMsg{}); got != "" {
		t.Fatalf("unexpected omission notice: %q", got)
	}
}
