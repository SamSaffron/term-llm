package image

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/cache"
	"github.com/samsaffron/term-llm/internal/venice"
)

func seedVeniceSizingCache(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	tiers := &cache.ImageModelConstraints{Resolutions: []string{"1K", "2K", "4K"}}
	models := []cache.CachedModel{
		{ID: "test-model", Type: "image", ImageConstraints: tiers},
		{ID: "test-model-edit", Type: "inpaint", ImageConstraints: tiers},
		{ID: "new-native-model", Type: "image", ImageConstraints: &cache.ImageModelConstraints{}},
		{ID: "new-native-editor", Type: "inpaint", ImageConstraints: &cache.ImageModelConstraints{}},
	}
	if err := cache.WriteModelInfoCache(venice.ImageCacheKey, models); err != nil {
		t.Fatal(err)
	}
}

func TestVeniceNativeSizing(t *testing.T) {
	seedVeniceSizingCache(t)
	for _, tc := range []struct {
		name, model, editor, size, quality, wantSize string
		inputs                                       int
	}{
		{name: "native generation", model: "new-native-model", size: "4K"},
		{name: "native edit configured default", model: "test-model", editor: "new-native-editor", inputs: 1},
		{name: "native edit explicit size", model: "test-model", editor: "new-native-editor", size: "1K", inputs: 1},
		{name: "native edit with quality", model: "test-model", editor: "new-native-editor", size: "2K", quality: "high", inputs: 1},
		{name: "native multi edit", model: "test-model", editor: "new-native-editor", size: "4K", inputs: 2},
		{name: "tiered editor for native generator", model: "new-native-model", editor: "test-model-edit", size: "4K", wantSize: "4K", inputs: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := veniceHTTPClient
			t.Cleanup(func() { veniceHTTPClient = old })
			calls := 0
			veniceHTTPClient = &http.Client{Transport: openAIImageRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodPost {
					t.Fatal("fresh sizing cache should avoid network discovery")
				}
				fields := map[string]any{}
				if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
					if err := r.ParseMultipartForm(1 << 20); err != nil {
						t.Fatal(err)
					}
					defer r.MultipartForm.RemoveAll()
					for k, v := range r.MultipartForm.Value {
						fields[k] = v[0]
					}
				} else if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
					t.Fatal(err)
				}
				size, present := fields["resolution"]
				if tc.wantSize == "" {
					if present {
						t.Errorf("native-size model received resolution=%v", size)
					}
				} else if size != tc.wantSize {
					t.Errorf("resolution=%v, want %s", size, tc.wantSize)
				}
				if fields["aspect_ratio"] != "9:16" {
					t.Errorf("aspect ratio lost: %v", fields)
				}
				data := "\x89PNG\r\n\x1a\nimage"
				if tc.inputs == 0 {
					data = `{"images":["` + base64.StdEncoding.EncodeToString([]byte(data)) + `"]}`
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(data))}, nil
			})}
			p := NewVeniceProvider("key", tc.model, tc.editor, "2K")
			var err error
			if tc.inputs == 0 {
				_, err = p.Generate(context.Background(), GenerateRequest{Prompt: "test", Size: tc.size, AspectRatio: "9:16", Quality: tc.quality})
			} else {
				_, err = p.Edit(context.Background(), EditRequest{Prompt: "test", Size: tc.size, AspectRatio: "9:16", Quality: tc.quality, InputImages: make([]InputImage, tc.inputs)})
			}
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("calls=%d, want one image request", calls)
			}
		})
	}
}

func TestVeniceSizingDiscoveryAndCacheUpgrade(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	// An existing fresh ID-only cache must not hide the new sizing metadata.
	if err := cache.WriteModelInfoCache(venice.ImageCacheKey, []cache.CachedModel{{ID: "native", Type: "image"}}); err != nil {
		t.Fatal(err)
	}
	old := veniceHTTPClient
	t.Cleanup(func() { veniceHTTPClient = old })
	calls := 0
	veniceHTTPClient = &http.Client{Transport: openAIImageRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodGet || r.URL.String() != venice.BaseURL+"/models?type=all" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer key" {
			t.Error("missing model-discovery auth")
		}
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > veniceModelLookupTimeout {
			t.Error("missing bounded catalog timeout")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"native","type":"image","model_spec":{"constraints":{"aspectRatios":["1:1"]}}},{"id":"tiered-edit","type":"inpaint","model_spec":{"constraints":{"resolutions":["1K","2K"]}}}]}`))}, nil
	})}
	p := NewVeniceProvider("key", "native", "tiered-edit", "")
	native := p.modelConstraints(context.Background(), "native")
	if native == nil || len(native.Resolutions) != 0 {
		t.Fatalf("native constraints=%+v", native)
	}
	tiered := p.modelConstraints(context.Background(), "tiered-edit")
	if tiered == nil || len(tiered.Resolutions) != 2 {
		t.Fatalf("tiered constraints=%+v", tiered)
	}
	if calls != 1 {
		t.Fatalf("catalog requests=%d, want 1", calls)
	}
	// Persisted empty constraints must round-trip distinctly from unknown metadata.
	stored, err := cache.ReadModelCache(venice.ImageCacheKey)
	if err != nil {
		t.Fatal(err)
	}
	if c := veniceConstraintsForModel(stored.ModelInfos, "native"); c == nil || len(c.Resolutions) != 0 {
		t.Fatalf("persisted native constraints=%+v", c)
	}
}

func TestVeniceSizingDiscoveryFailure(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		err        error
	}{
		{name: "transport", err: errors.New("offline")},
		{name: "timeout", err: context.DeadlineExceeded},
		{name: "unauthorized", status: 401, body: `{}`},
		{name: "empty", status: 200, body: `{"data":[]}`},
		{name: "malformed", status: 200, body: `invalid`},
		{name: "missing constraints", status: 200, body: `{"data":[{"id":"native","type":"inpaint"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("XDG_CACHE_HOME", home)
			dir := filepath.Join(home, "term-llm")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, venice.ImageCacheKey+"-models.json")
			original := `{"models":["native"],"model_infos":[{"id":"native","type":"inpaint","image_constraints":{}}],"fetched_at":"2000-01-01T00:00:00Z"}`
			if err := os.WriteFile(path, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			old := veniceHTTPClient
			t.Cleanup(func() { veniceHTTPClient = old })
			veniceHTTPClient = &http.Client{Transport: openAIImageRoundTripper(func(r *http.Request) (*http.Response, error) {
				if tc.err != nil {
					return nil, tc.err
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			p := NewVeniceProvider("key", "native", "", "")
			opts, err := p.requestOptions(context.Background(), "native", "2K", "9:16", "")
			if err != nil || opts.Resolution != "" {
				t.Fatalf("stale native options=%+v, err=%v", opts, err)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != original {
				t.Fatalf("failed refresh changed stale cache: %s, err=%v", data, err)
			}
			opts, err = p.requestOptions(context.Background(), "unknown", "2K", "9:16", "")
			if err != nil || opts.Resolution != "2K" {
				t.Fatalf("unknown model lost requested resolution: %+v, err=%v", opts, err)
			}
		})
	}
}
