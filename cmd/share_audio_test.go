package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/share"
	"github.com/samsaffron/term-llm/internal/testutil/mediafixture"
	"github.com/spf13/cobra"
)

func addShareAudioFixture(t *testing.T, store session.Store, id string) int64 {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private-recording.mp3")
	if err := os.WriteFile(path, mediafixture.MP3(), 0o600); err != nil {
		t.Fatal(err)
	}
	item := llm.MediaArtifact{Reference: "0123456789abcdef0123456789abcdef", SourcePath: path, Name: "narration.mp3", MediaType: "audio/mpeg"}
	messages := []llm.Message{
		{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{Name: "show_media", Media: []llm.MediaArtifact{item}}}}},
		llm.AssistantText("![Listen](term-llm-media://" + item.Reference + ")"),
	}
	for _, message := range messages {
		if err := store.AddMessage(context.Background(), id, session.NewMessage(id, message, -1)); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := store.GetMessages(context.Background(), id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return rows[len(rows)-1].ID
}

func TestCreateSessionShareAudioConsent(t *testing.T) {
	for _, tc := range []struct {
		name, scope, choice string
		include             bool
	}{
		{"default response", "response", "", false},
		{"explicit exclusion", "response", `,"include_audio":false`, false},
		{"opt-in response", "response", `,"include_audio":true`, true},
		{"opt-in conversation without tools or images", "conversation", `,"include_audio":true,"include_tools":false,"include_images":false`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := newServeShareImageFixture(t)
			anchor := addShareAudioFixture(t, server.store, "share-session")
			mock := assetProviderMock()
			server.sharePublisherFactory = func() (share.Publisher, error) { return mock, nil }
			rr := serveShareRequest(t, server, `{"anchor_message_id":`+formatInt64(anchor)+`,"scope":"`+tc.scope+`"`+tc.choice+`}`)
			if rr.Code != 201 {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			if strings.Contains(mock.files["index.html"], "<audio controls") != tc.include {
				t.Fatalf("include_audio forwarding failed: %s", tc.name)
			}
			if strings.Contains(mock.files["session.md"], "data:audio/") || !strings.Contains(mock.files["session.md"], "Audio: narration.mp3") {
				t.Fatal("Markdown audio fallback missing")
			}
			if strings.Contains(tc.choice, "include_images") && strings.Contains(mock.files["index.html"], "<img") {
				t.Fatal("image exclusion ignored")
			}
		})
	}
}

func TestSessionsAudioFlagForwarding(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	oldDB, oldNoSession := sessionDBPath, noSession
	oldFactory := newSessionSharePublisher
	oldNew, oldJSON, oldNoImages := sessionsShareNew, sessionsShareJSON, sessionsShareNoImages
	oldVisibility := sessionsShareVisibility
	oldExportSystem, oldExportReasoning, oldExportRaw := sessionsExportIncludeSystem, sessionsExportIncludeReasoning, sessionsExportIncludeRawReasoning
	oldShareRaw := sessionsShareIncludeRawReasoning
	shareFlag := sessionsShareCmd.Flags().Lookup("include-audio")
	exportFlag := sessionsExportCmd.Flags().Lookup("include-audio")
	if shareFlag == nil || exportFlag == nil || shareFlag.DefValue != "false" || exportFlag.DefValue != "false" {
		t.Fatal("audio flags missing or not default-off")
	}
	oldShareAudio, oldExportAudio := shareFlag.Value.String(), exportFlag.Value.String()
	oldShareChanged, oldExportChanged := shareFlag.Changed, exportFlag.Changed
	t.Cleanup(func() {
		sessionDBPath, noSession = oldDB, oldNoSession
		newSessionSharePublisher = oldFactory
		sessionsShareNew, sessionsShareJSON, sessionsShareNoImages = oldNew, oldJSON, oldNoImages
		sessionsShareVisibility = oldVisibility
		sessionsShareIncludeRawReasoning = oldShareRaw
		sessionsExportIncludeSystem, sessionsExportIncludeReasoning, sessionsExportIncludeRawReasoning = oldExportSystem, oldExportReasoning, oldExportRaw
		_ = shareFlag.Value.Set(oldShareAudio)
		_ = exportFlag.Value.Set(oldExportAudio)
		shareFlag.Changed, exportFlag.Changed = oldShareChanged, oldExportChanged
	})
	sessionDBPath, noSession = filepath.Join(t.TempDir(), "sessions.db"), false
	sessionsShareNew, sessionsShareJSON, sessionsShareNoImages = false, true, true
	sessionsShareVisibility = "private"
	sessionsShareIncludeRawReasoning = false
	sessionsExportIncludeSystem, sessionsExportIncludeReasoning, sessionsExportIncludeRawReasoning = false, false, false
	store, err := session.NewStore(session.Config{Enabled: true, Path: sessionDBPath})
	if err != nil {
		t.Fatal(err)
	}
	sess := &session.Session{ID: "audio-cli", Name: "Audio", Provider: "mock", Model: "mock"}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	addShareAudioFixture(t, store, sess.ID)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	publisher := &sessionsSharePublisherMock{}
	newSessionSharePublisher = func(config.ShareConfig) (share.Publisher, error) { return publisher, nil }
	for _, tc := range []struct {
		name, flag string
		include    bool
		newShare   bool
		wantError  bool
	}{
		{"default create", "false", false, false, false},
		{"widening update rejected", "true", false, false, true},
		{"explicit new share", "true", true, true, false},
		{"explicit audio update", "true", true, false, false},
		{"next update needs fresh consent", "false", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := sessionsShareCmd.Flags().Set("include-audio", tc.flag); err != nil {
				t.Fatal(err)
			}
			sessionsShareNew = tc.newShare
			command := &cobra.Command{}
			var output, notices bytes.Buffer
			command.SetOut(&output)
			command.SetErr(&notices)
			err := runSessionsShare(command, []string{sess.ID})
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "--new --include-audio") || !strings.Contains(err.Error(), "already-distributed URL") {
					t.Fatalf("widening error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "next update needs fresh consent" && !strings.Contains(notices.String(), "audio recordings were removed") {
				t.Fatalf("missing removal note: %s", notices.String())
			}
			checkStore, err := session.NewStore(session.Config{Enabled: true, Path: sessionDBPath})
			if err != nil {
				t.Fatal(err)
			}
			saved, err := checkStore.Get(context.Background(), sess.ID)
			checkStore.Close()
			if err != nil || saved.Share == nil || saved.Share.IncludedAudio != tc.include {
				t.Fatalf("share state: %+v err=%v", saved, err)
			}
			var html string
			for _, file := range publisher.lastRequest.Files {
				if file.Name == "index.html" {
					html = string(file.Content)
				}
			}
			if strings.Contains(html, "<audio controls") != tc.include {
				t.Fatal("CLI share consent forwarding failed")
			}
			if err := sessionsExportCmd.Flags().Set("include-audio", tc.flag); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "export.html")
			// Existing world-readable files must also be made private when audio is embedded.
			if err := os.WriteFile(path, []byte("old export"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0644); err != nil {
				t.Fatal(err)
			}
			if err := runSessionsExport(command, []string{sess.ID, path}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "<audio controls") != tc.include {
				t.Fatal("CLI export consent forwarding failed")
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			wantMode := os.FileMode(0644)
			if tc.include {
				wantMode = 0600
			}
			if info.Mode().Perm() != wantMode {
				t.Fatalf("export mode = %o, want %o", info.Mode().Perm(), wantMode)
			}
			notices.Reset()
			path = filepath.Join(t.TempDir(), "export.md")
			if err := runSessionsExport(command, []string{sess.ID, path}); err != nil {
				t.Fatal(err)
			}
			data, err = os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "Audio: narration.mp3") || strings.Contains(string(data), "base64,") {
				t.Fatal("CLI Markdown did not use audio placeholder")
			}
			if strings.Contains(notices.String(), "--include-audio is ignored for non-HTML output") != tc.include {
				t.Fatalf("ignored-flag notice = %q", notices.String())
			}
		})
	}
	if publisher.createCalls != 2 || publisher.updateCalls != 2 {
		t.Fatalf("create/update calls %d/%d", publisher.createCalls, publisher.updateCalls)
	}
	testSessionsAudioOmissions(t, sess.ID)
}

func testSessionsAudioOmissions(t *testing.T, sessionID string) {
	t.Helper()
	store, err := session.NewStore(session.Config{Enabled: true, Path: sessionDBPath})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	messages, err := store.GetMessages(context.Background(), sessionID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	path := messages[0].Parts[0].ToolResult.Media[0].Path()
	for _, tc := range []struct {
		name string
		data []byte
		mime string
	}{
		{"missing", nil, "audio/mpeg"},
		{"invalid bytes", []byte("not audio"), "audio/mpeg"},
		{"invalid type", mediafixture.MP3(), "audio/x-unapproved"},
		{"over budget", append(mediafixture.MP3(), make([]byte, 2<<20)...), "audio/mpeg"},
	} {
		t.Run(tc.name+" recordings reported", func(t *testing.T) {
			messages[0].Parts[0].ToolResult.Media[0].MediaType = tc.mime
			if err := store.UpdateMessage(context.Background(), sessionID, &messages[0]); err != nil {
				t.Fatal(err)
			}
			if tc.data == nil {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			sessionsShareNew = true
			sessionsShareIncludeAudio, sessionsExportIncludeAudio = true, true
			command := &cobra.Command{}
			var output, notices bytes.Buffer
			command.SetOut(&output)
			command.SetErr(&notices)
			if err := runSessionsShare(command, []string{sessionID}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(notices.String(), "1 audio recording(s) omitted") {
				t.Fatalf("share omission note: %s", notices.String())
			}
			saved, err := store.Get(context.Background(), sessionID)
			if err != nil || saved.Share == nil || saved.Share.IncludedAudio {
				t.Fatalf("excluded recording counted as embedded: %+v err=%v", saved, err)
			}
			notices.Reset()
			outputPath := filepath.Join(t.TempDir(), "export.html")
			if err := os.WriteFile(outputPath, []byte("old"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(outputPath, 0644); err != nil {
				t.Fatal(err)
			}
			if err := runSessionsExport(command, []string{sessionID, outputPath}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(notices.String(), "1 audio recording(s) omitted") {
				t.Fatalf("export omission note: %s", notices.String())
			}
			info, err := os.Stat(outputPath)
			if err != nil || info.Mode().Perm() != 0644 {
				t.Fatalf("audio-free file mode changed: %v err=%v", info, err)
			}
		})
	}
}

func TestWriteSessionExportAudioPermissions(t *testing.T) {
	for _, existing := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "recording.html")
		if existing {
			if err := os.WriteFile(path, []byte("old"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0644); err != nil {
				t.Fatal(err)
			}
		}
		if err := writeSessionExport(path, "private recording", true); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("existing=%v mode=%o", existing, info.Mode().Perm())
		}
	}
}

func TestSessionExportAudioFilenameSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(path, mediafixture.MP3(), 0600); err != nil {
		t.Fatal(err)
	}
	item := llm.MediaArtifact{Reference: "0123456789abcdef0123456789abcdef", SourcePath: path, Name: "recording.mp3", MediaType: "audio/mpeg"}
	value := "![Listen](term-llm-media://" + item.Reference + ")"
	messages := []session.Message{*session.NewMessage("s", llm.AssistantText(value), -1)}
	for _, tc := range []struct {
		name string
		html bool
	}{
		{"export.html", true}, {"export.HTML", true}, {"export.htm", true}, {"export.md", false}, {"export.txt", false}, {"export", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content, result, err := renderSessionExport(&session.Session{ID: "s"}, messages, session.ExportOptions{IncludeAudio: true, Media: []llm.MediaArtifact{item}}, tc.name)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(content, "<audio controls") != tc.html || (result.AudioEmbedded > 0) != tc.html {
				t.Fatalf("filename selection mismatch: %+v", result)
			}
		})
	}
}
