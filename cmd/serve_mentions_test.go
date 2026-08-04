package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mentions"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func utf16Len(value string) int {
	return len(utf16.Encode([]rune(value)))
}

func TestMentionSearchUsesSharedParserMatcherAndInsertion(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "types.go"), []byte("package example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "typed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "typed", "inside.txt"), []byte("inside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &serveServer{worktreeRootFn: func() (string, error) { return root, nil }}
	text := "😀 inspect @typ"
	body, err := json.Marshal(serveMentionSearchRequest{Text: text, CursorUTF16: utf16Len(text)})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/mentions/search", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.handleMentionSearch(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got serveMentionSearchResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Active || got.Token == nil || got.Token.Query != "typ" || got.Token.StartUTF16 != utf16Len("😀 inspect ") {
		t.Fatalf("token = %#v, active=%v", got.Token, got.Active)
	}
	if len(got.Items) < 2 {
		t.Fatalf("items = %#v, want file and directory", got.Items)
	}
	seen := map[string]serveMentionSearchItem{}
	for _, item := range got.Items {
		seen[item.Path] = item
	}
	if seen["types.go"].InsertText != "@types.go" || seen["types.go"].Kind != "file" {
		t.Fatalf("file item = %#v", seen["types.go"])
	}
	if seen["typed"].InsertText != "@typed/" || seen["typed"].Kind != "directory" {
		t.Fatalf("directory item = %#v", seen["typed"])
	}
	matched := false
	for _, segment := range seen["types.go"].Segments {
		matched = matched || segment.Matched
	}
	if !matched {
		t.Fatalf("file match did not include highlighted segments: %#v", seen["types.go"].Segments)
	}
}

func TestMentionSearchRejectsInvalidCursorAndIgnoresEmail(t *testing.T) {
	s := &serveServer{worktreeRootFn: func() (string, error) { return t.TempDir(), nil }}
	for _, tc := range []struct {
		name       string
		request    serveMentionSearchRequest
		wantStatus int
		wantActive bool
	}{
		{name: "split surrogate", request: serveMentionSearchRequest{Text: "😀 @a", CursorUTF16: 1}, wantStatus: http.StatusBadRequest},
		{name: "email", request: serveMentionSearchRequest{Text: "me@example.com", CursorUTF16: len("me@example.com")}, wantStatus: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(tc.request)
			req := httptest.NewRequest(http.MethodPost, "/v1/mentions/search", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			s.handleMentionSearch(rr, req)
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if rr.Code == http.StatusOK {
				var got serveMentionSearchResponse
				if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if got.Active != tc.wantActive {
					t.Fatalf("active = %v, want %v", got.Active, tc.wantActive)
				}
			}
		})
	}
}

func TestMentionSearchCapsResultsAtTen(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 15; i++ {
		name := fmt.Sprintf("file-%02d.txt", i)
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := &serveServer{worktreeRootFn: func() (string, error) { return root, nil }}
	body, _ := json.Marshal(serveMentionSearchRequest{Text: "@file", CursorUTF16: len("@file"), Limit: 999})
	req := httptest.NewRequest(http.MethodPost, "/v1/mentions/search", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.handleMentionSearch(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var got serveMentionSearchResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 10 {
		t.Fatalf("items = %d, want 10", len(got.Items))
	}
}

func TestMentionSearchRejectsUntrustedDraftWorktree(t *testing.T) {
	root := t.TempDir()
	s := &serveServer{worktreeRootFn: func() (string, error) { return root, nil }}
	if _, err := s.resolveMentionSearchRoot(context.Background(), "", t.TempDir()); err == nil {
		t.Fatal("untrusted draft worktree was accepted")
	}
}

func TestMentionSnapshotCoalescesConcurrentBuilds(t *testing.T) {
	root := t.TempDir()
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	s := &serveServer{mentionBuildFn: func(_ context.Context, gotRoot string, _ mentions.BuildOptions) (*mentions.Snapshot, error) {
		if gotRoot != root {
			t.Errorf("root = %q, want %q", gotRoot, root)
		}
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return &mentions.Snapshot{Root: root, BuiltAt: time.Now()}, nil
	}}
	errCh := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := s.mentionSnapshot(context.Background(), root)
			errCh <- err
		}()
	}
	<-started
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("build calls = %d, want 1", got)
	}
}

func TestMentionSnapshotBacksOffAfterFailedRefresh(t *testing.T) {
	root := t.TempDir()
	var calls atomic.Int32
	attempted := make(chan struct{})
	snapshot := &mentions.Snapshot{Root: root, BuiltAt: time.Now().Add(-time.Minute)}
	s := &serveServer{
		mentionsByRoot: map[string]*serveMentionCacheEntry{
			root: {snapshot: snapshot, builtAt: time.Now().Add(-time.Minute)},
		},
		mentionBuildFn: func(context.Context, string, mentions.BuildOptions) (*mentions.Snapshot, error) {
			if calls.Add(1) == 1 {
				close(attempted)
			}
			return nil, errors.New("transient git failure")
		},
	}
	got, err := s.mentionSnapshot(context.Background(), root)
	if err != nil || got != snapshot {
		t.Fatalf("stale snapshot = %#v, err=%v", got, err)
	}
	<-attempted
	for i := 0; i < 10_000; i++ {
		s.mentionsCacheMu.Lock()
		building := s.mentionsByRoot[root].building != nil
		s.mentionsCacheMu.Unlock()
		if !building {
			break
		}
		runtime.Gosched()
		if i == 9_999 {
			t.Fatal("failed refresh did not complete")
		}
	}
	for i := 0; i < 20; i++ {
		if _, err := s.mentionSnapshot(context.Background(), root); err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("failed refresh calls = %d, want one backed-off attempt", got)
	}
}

func TestMentionSnapshotRejectsNewRootWhenAllCacheEntriesBuild(t *testing.T) {
	started := make(chan struct{}, serveMentionCacheMaxEntries)
	release := make(chan struct{})
	s := &serveServer{mentionBuildFn: func(_ context.Context, root string, _ mentions.BuildOptions) (*mentions.Snapshot, error) {
		started <- struct{}{}
		<-release
		return &mentions.Snapshot{Root: root, BuiltAt: time.Now()}, nil
	}}
	errCh := make(chan error, serveMentionCacheMaxEntries)
	for i := 0; i < serveMentionCacheMaxEntries; i++ {
		root := filepath.Join(t.TempDir(), fmt.Sprintf("root-%d", i))
		go func() {
			_, err := s.mentionSnapshot(context.Background(), root)
			errCh <- err
		}()
	}
	for i := 0; i < serveMentionCacheMaxEntries; i++ {
		<-started
	}
	if _, err := s.mentionSnapshot(context.Background(), filepath.Join(t.TempDir(), "overflow")); err == nil {
		t.Fatal("cache accepted a fifth root while every bounded entry was building")
	}
	close(release)
	for i := 0; i < serveMentionCacheMaxEntries; i++ {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
	s.mentionsCacheMu.Lock()
	defer s.mentionsCacheMu.Unlock()
	if len(s.mentionsByRoot) > serveMentionCacheMaxEntries {
		t.Fatalf("cache entries = %d, max = %d", len(s.mentionsByRoot), serveMentionCacheMaxEntries)
	}
}

func TestHandleResponsesExpandsMentionsServerSide(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("web mention body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	provider := llm.NewMockProvider("mock").AddTextResponse("ok").AddTextResponse("ok again")
	manager := newServeSessionManager(time.Minute, 2, func(context.Context) (*serveRuntime, error) {
		runtime := &serveRuntime{
			provider:     provider,
			engine:       llm.NewEngine(provider, nil),
			defaultModel: "mock-model",
		}
		runtime.Touch()
		return runtime, nil
	})
	defer manager.Close()
	s := &serveServer{sessionMgr: manager, worktreeRootFn: func() (string, error) { return root, nil }}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"inspect @note.txt","client_message_id":"mention-message"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Term-LLM-UI-Version", "test")
	rr := httptest.NewRecorder()
	s.handleResponses(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(provider.Requests))
	}
	providerText := llm.MessageText(provider.Requests[0].Messages[len(provider.Requests[0].Messages)-1])
	if !strings.Contains(providerText, "inspect @note.txt") || !strings.Contains(providerText, "web mention body") {
		t.Fatalf("provider text missing server-expanded mention: %q", providerText)
	}

	externalReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"inspect @note.txt"}`))
	externalReq.Header.Set("Content-Type", "application/json")
	externalRR := httptest.NewRecorder()
	s.handleResponses(externalRR, externalReq)
	if externalRR.Code != http.StatusOK {
		t.Fatalf("external status = %d: %s", externalRR.Code, externalRR.Body.String())
	}
	if len(provider.Requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(provider.Requests))
	}
	externalText := llm.MessageText(provider.Requests[1].Messages[len(provider.Requests[1].Messages)-1])
	if strings.Contains(externalText, "web mention body") {
		t.Fatalf("external Responses request unexpectedly expanded mentions: %q", externalText)
	}
}

func TestAugmentMessagesWithMentionsKeepsDisplayClean(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("mention body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	toolCfg := tools.DefaultToolConfig()
	toolCfg.Enabled = []string{tools.ReadFileToolName}
	toolCfg.BaseDir = root
	toolCfg.ReadDirs = []string{root}
	toolMgr, err := tools.NewToolManager(&toolCfg, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &serveRuntime{toolMgr: toolMgr}
	messages := []llm.Message{llm.UserText("inspect @note.txt")}
	(&serveServer{}).augmentMessagesWithMentions(context.Background(), runtime, "", "", messages)
	providerText := llm.MessageText(messages[0])
	if !strings.Contains(providerText, "mention body") {
		t.Fatalf("provider text missing mention body: %q", providerText)
	}
	if visible := llm.StripEmbeddedFileText(providerText); visible != "inspect @note.txt" {
		t.Fatalf("visible text = %q", visible)
	}
	before := providerText
	(&serveServer{}).augmentMessagesWithMentions(context.Background(), runtime, "", "", messages)
	if after := llm.MessageText(messages[0]); after != before {
		t.Fatalf("mention context duplicated on second expansion:\nbefore=%q\nafter=%q", before, after)
	}

	explicitText := "compare @note.txt" + "\n\n" + llm.EmbeddedFileIntro + "\n\n" +
		llm.FormatEmbeddedFileText("explicit.txt", "text/plain", "explicit body")
	mixed := []llm.Message{llm.UserText(explicitText)}
	(&serveServer{}).augmentMessagesWithMentions(context.Background(), runtime, "", "", mixed)
	mixedProviderText := llm.MessageText(mixed[0])
	if !strings.Contains(mixedProviderText, "explicit body") || !strings.Contains(mixedProviderText, "mention body") {
		t.Fatalf("explicit attachment suppressed mention expansion: %q", mixedProviderText)
	}
	if mixed[0].DisplayText != "compare @note.txt" {
		t.Fatalf("mixed display text = %q", mixed[0].DisplayText)
	}

	stored := *session.NewMessage("session", messages[0], 0)
	if stored.TextContent != "inspect @note.txt" {
		t.Fatalf("persisted visible text = %q", stored.TextContent)
	}
	entries := (&serveServer{}).sessionMessageEntries([]session.Message{stored})
	if len(entries) != 1 {
		t.Fatalf("entries = %#v", entries)
	}
	var visibleText string
	var files []string
	for _, part := range entries[0].Parts {
		switch part.Type {
		case "text":
			visibleText += part.Text
		case "file":
			files = append(files, part.Text)
		}
	}
	if visibleText != "inspect @note.txt" || len(files) != 1 || files[0] != "note.txt" {
		t.Fatalf("serialized visible text/files = %q/%#v", visibleText, files)
	}
}
