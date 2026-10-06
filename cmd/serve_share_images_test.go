package cmd

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/share"
)

func newServeShareImageFixture(t *testing.T) (*serveServer, int64) {
	t.Helper()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	img.SetNRGBA(1, 1, color.NRGBA{R: 200, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(t.TempDir(), "generated.png")
	if err := os.WriteFile(generated, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	sess := &session.Session{ID: "share-session", Name: "Images", Provider: "mock", Model: "model", Mode: session.ModeChat, Origin: session.OriginWeb, CreatedAt: now, UpdatedAt: now}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	call := llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "c1", Name: "image_generate", Arguments: []byte(`{}`)}}}}
	result := llm.Message{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "c1", Name: "image_generate", Content: "Generated image successfully.", Images: []string{generated}}}}}
	for _, message := range []llm.Message{llm.UserText("draw a dot"), call, result, llm.AssistantText("Here is your dot.")} {
		if err := store.AddMessage(context.Background(), sess.ID, session.NewMessage(sess.ID, message, -1)); err != nil {
			t.Fatal(err)
		}
	}
	messages, err := store.GetMessages(context.Background(), sess.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return &serveServer{store: store}, messages[len(messages)-1].ID
}

func assetProviderMock() *serveSharePublisherMock {
	return &serveSharePublisherMock{
		caps: share.Capabilities{
			Protocol: share.Protocol, Version: share.Version,
			Provider:   share.Provider{ID: "acme", Name: "Acme"},
			Operations: []share.Operation{share.OperationCreate}, Visibilities: []share.Visibility{share.VisibilityUnlisted},
			DefaultVisibility: share.VisibilityUnlisted, AssetMediaTypes: []string{"image/png", "image/jpeg"},
		},
		result: share.Result{Provider: "acme", ID: "x", URL: "https://share.example/x", Visibility: share.VisibilityUnlisted, Ready: true},
	}
}

func TestCreateSessionShareSendsImageAssetsForResponse(t *testing.T) {
	server, anchor := newServeShareImageFixture(t)
	mock := assetProviderMock()
	server.sharePublisherFactory = func() (share.Publisher, error) { return mock, nil }
	rr := serveShareRequest(t, server, `{"anchor_message_id":`+formatInt64(anchor)+`,"scope":"response"}`)
	if rr.Code != 201 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var asset string
	for name := range mock.files {
		if strings.HasPrefix(name, "assets/") && strings.HasSuffix(name, ".png") {
			asset = name
		}
	}
	if asset == "" {
		t.Fatalf("no image asset in bundle: %v", mapKeys(mock.files))
	}
	if !strings.Contains(mock.files["index.html"], `src="`+asset+`"`) || !strings.Contains(mock.files["session.md"], "("+asset+")") {
		t.Fatal("transcript does not reference the image asset")
	}
	if strings.Contains(mock.files["index.html"], "draw a dot") || strings.Contains(mock.files["index.html"], "image_generate") {
		t.Fatal("response share leaked the prompt or tool activity")
	}
}

func TestCreateSessionShareCanExcludeImages(t *testing.T) {
	server, anchor := newServeShareImageFixture(t)
	mock := assetProviderMock()
	server.sharePublisherFactory = func() (share.Publisher, error) { return mock, nil }
	rr := serveShareRequest(t, server, `{"anchor_message_id":`+formatInt64(anchor)+`,"scope":"conversation","include_images":false}`)
	if rr.Code != 201 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if len(mock.files) != 2 || strings.Contains(mock.files["index.html"], "<img") {
		t.Fatalf("images were included: %v", mapKeys(mock.files))
	}
}

func mapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
