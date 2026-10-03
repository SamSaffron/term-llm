package cmd

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestClassifyImageFileAndNamedProvider(t *testing.T) {
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "picture.png")
	if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	srv := newClassifyTestServer(t)
	cfg := &config.Config{Classify: config.ClassifyConfig{Providers: map[string]config.ClassifyProviderConfig{"local": {Type: "typesafe", APIKey: "test-key", Model: "local", BaseURL: srv.server.URL, SupportsImages: true}}}}
	_, err := executeClassifyTest(t, []string{"inspect", "--provider", "local", "--image", path, "--samples", "2", "--type", "noul", "--question", "Is it red?"}, "", false, cfg, srv)
	if err != nil {
		t.Fatal(err)
	}
	if len(srv.requests) != 1 || len(srv.requests[0].Images) != 1 || srv.requests[0].Samples != 2 {
		t.Fatalf("requests: %#v", srv.requests)
	}
	got, err := base64.StdEncoding.DecodeString(srv.requests[0].Images[0].Base64)
	if err != nil || !bytes.Equal(got, data.Bytes()) {
		t.Fatal("image bytes changed")
	}
	p := cfg.Classify.Providers["local"]
	p.SupportsImages = false
	cfg.Classify.Providers["local"] = p
	_, err = executeClassifyTest(t, []string{"inspect", "--provider", "local", "--image", path, "--type", "noul", "--question", "Is it red?"}, "", false, cfg, srv)
	if err == nil || !strings.Contains(err.Error(), "does not support images") {
		t.Fatalf("unsupported: %v", err)
	}
}

func TestClassifyImageLimits(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("not an image"), make([]byte, (4<<20)+1)} {
		path := filepath.Join(t.TempDir(), "bad")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := classifyImages([]string{path}); err == nil {
			t.Fatal("accepted invalid image")
		}
	}
	if _, err := classifyImages([]string{"https://example.test/picture.png"}); err == nil {
		t.Fatal("accepted remote URL")
	}
	if _, err := classifyImages([]string{"a", "b", "c", "d", "e"}); err == nil {
		t.Fatal("accepted too many images")
	}
}
