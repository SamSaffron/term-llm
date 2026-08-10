package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestMessagePreservesFileDataForStorageReplay(t *testing.T) {
	const b64 = "YSxiCg=="
	msg := NewMessage("sess_test", llm.Message{Role: llm.RoleUser, Parts: []llm.Part{{
		Type: llm.PartFile,
		Text: llm.FormatEmbeddedFileText("data.csv", "text/csv", "a,b\n"),
		FileData: &llm.ToolFileData{
			MediaType: "text/csv",
			Base64:    b64,
			Filename:  "data.csv",
			SizeBytes: 4,
		},
	}}}, 0)

	if msg.Parts[0].FileData == nil {
		t.Fatal("FileData was removed; want upload data preserved for session replay")
	}
	if msg.Parts[0].FileData.Base64 != b64 {
		t.Fatalf("stored base64 = %q, want %q", msg.Parts[0].FileData.Base64, b64)
	}
	if !strings.Contains(msg.TextContent, "a,b") {
		t.Fatalf("TextContent = %q, want file text fallback", msg.TextContent)
	}

	partsJSON, err := msg.PartsJSON()
	if err != nil {
		t.Fatalf("PartsJSON: %v", err)
	}
	if !strings.Contains(partsJSON, b64) {
		t.Fatalf("parts json did not preserve base64 data: %s", partsJSON)
	}

	var roundTrip Message
	if err := roundTrip.SetPartsFromJSON(partsJSON); err != nil {
		t.Fatalf("SetPartsFromJSON: %v", err)
	}
	encoded, _ := json.Marshal(roundTrip.Parts)
	if !strings.Contains(string(encoded), b64) {
		t.Fatalf("round-trip parts did not preserve base64: %s", encoded)
	}
}

func TestMessagePreservesImageBase64ByDefault(t *testing.T) {
	const b64 = "aW1hZ2UtYnl0ZXM="
	msg := NewMessage("sess_test", llm.Message{Role: llm.RoleUser, Parts: []llm.Part{{
		Type:      llm.PartImage,
		ImagePath: "/tmp/upload.png",
		ImageData: &llm.ToolImageData{MediaType: "image/png", Base64: b64, Detail: "high", Width: 1200, Height: 800},
	}}}, 0)

	partsJSON, err := msg.PartsJSON()
	if err != nil {
		t.Fatalf("PartsJSON: %v", err)
	}
	if !strings.Contains(partsJSON, b64) {
		t.Fatalf("parts json did not preserve image base64 by default: %s", partsJSON)
	}
}

func TestMessageStripsImageBase64WhenStorageOptionEnabled(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	uploadPath := filepath.Join(dataHome, "term-llm", "uploads", "upload.png")
	if err := os.MkdirAll(filepath.Dir(uploadPath), 0o700); err != nil {
		t.Fatalf("mkdir uploads: %v", err)
	}
	if err := os.WriteFile(uploadPath, []byte("image-bytes"), 0o600); err != nil {
		t.Fatalf("write upload: %v", err)
	}
	const b64 = "aW1hZ2UtYnl0ZXM="
	msg := NewMessage("sess_test", llm.Message{Role: llm.RoleUser, Parts: []llm.Part{{
		Type:      llm.PartImage,
		ImagePath: uploadPath,
		ImageData: &llm.ToolImageData{MediaType: "image/png", Base64: b64, Detail: "high", Width: 1200, Height: 800},
	}}}, 0)

	if msg.Parts[0].ImageData == nil || msg.Parts[0].ImageData.Base64 != b64 {
		t.Fatal("in-memory message should preserve image base64 for the active request")
	}
	partsJSON, err := msg.PartsJSONForStorage(true)
	if err != nil {
		t.Fatalf("PartsJSONForStorage: %v", err)
	}
	if strings.Contains(partsJSON, b64) {
		t.Fatalf("storage parts json retained image base64 despite strip option: %s", partsJSON)
	}

	var roundTrip Message
	if err := roundTrip.SetPartsFromJSON(partsJSON); err != nil {
		t.Fatalf("SetPartsFromJSON: %v", err)
	}
	part := roundTrip.Parts[0]
	if part.ImagePath != uploadPath {
		t.Fatalf("ImagePath = %q, want preserved path", part.ImagePath)
	}
	if part.ImageData == nil {
		t.Fatal("ImageData is nil, want media metadata preserved")
	}
	if part.ImageData.Base64 != "" {
		t.Fatalf("ImageData.Base64 = %q, want stripped", part.ImageData.Base64)
	}
	if part.ImageData.MediaType != "image/png" || part.ImageData.Detail != "high" || part.ImageData.Width != 1200 || part.ImageData.Height != 800 {
		t.Fatalf("ImageData metadata = %+v, want media type/detail/dimensions preserved", part.ImageData)
	}
}

func TestSQLiteStoreStripImageBase64Config(t *testing.T) {
	ctx := context.Background()
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	uploadPath := filepath.Join(dataHome, "term-llm", "uploads", "upload.png")
	if err := os.MkdirAll(filepath.Dir(uploadPath), 0o700); err != nil {
		t.Fatalf("mkdir uploads: %v", err)
	}
	if err := os.WriteFile(uploadPath, []byte("image-bytes"), 0o600); err != nil {
		t.Fatalf("write upload: %v", err)
	}
	store, err := NewSQLiteStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db"), StripImageBase64: true})
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	sess := &Session{ID: "sess_strip", Provider: "mock", Model: "mock"}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	const b64 = "aW1hZ2UtYnl0ZXM="
	msg := NewMessage(sess.ID, llm.Message{Role: llm.RoleUser, Parts: []llm.Part{{
		Type:      llm.PartImage,
		ImagePath: uploadPath,
		ImageData: &llm.ToolImageData{MediaType: "image/png", Base64: b64},
	}}}, -1)
	if err := store.AddMessage(ctx, sess.ID, msg); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	got, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	part := got[0].Parts[0]
	if part.ImagePath != uploadPath {
		t.Fatalf("ImagePath = %q, want preserved", part.ImagePath)
	}
	if part.ImageData == nil || part.ImageData.MediaType != "image/png" {
		t.Fatalf("ImageData = %+v, want media metadata preserved", part.ImageData)
	}
	if part.ImageData.Base64 != "" {
		t.Fatalf("Base64 = %q, want stripped by store config", part.ImageData.Base64)
	}
}

func TestMessageDoesNotStripImageBase64ForNonUploadPath(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	outsidePath := filepath.Join(t.TempDir(), "upload.png")
	if err := os.WriteFile(outsidePath, []byte("image-bytes"), 0o600); err != nil {
		t.Fatalf("write outside path: %v", err)
	}
	const b64 = "aW1hZ2UtYnl0ZXM="
	msg := NewMessage("sess_test", llm.Message{Role: llm.RoleUser, Parts: []llm.Part{{
		Type:      llm.PartImage,
		ImagePath: outsidePath,
		ImageData: &llm.ToolImageData{MediaType: "image/png", Base64: b64},
	}}}, 0)

	partsJSON, err := msg.PartsJSONForStorage(true)
	if err != nil {
		t.Fatalf("PartsJSONForStorage: %v", err)
	}
	if !strings.Contains(partsJSON, b64) {
		t.Fatalf("storage parts json stripped non-upload image base64: %s", partsJSON)
	}
}
