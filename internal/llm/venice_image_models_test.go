package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/cache"
	"github.com/samsaffron/term-llm/internal/config"
)

func TestVeniceImageModelDiscovery(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("VENICE_API_KEY", "")
	old := defaultHTTPClient
	t.Cleanup(func() { defaultHTTPClient = old })
	calls := 0
	defaultHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != veniceBaseURL+"/models?type=all" || r.Header.Get("Authorization") != "Bearer image-key" {
			t.Errorf("unexpected request: %s, auth %q", r.URL, r.Header.Get("Authorization"))
		}
		if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > 2*time.Second {
			t.Error("completion request lacks short deadline")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"new-image","type":"image","model_spec":{"constraints":{"resolutions":["1K","2K"]}}},{"id":"new-edit","type":"inpaint","model_spec":{"constraints":{"aspectRatios":["1:1"]}}},{"id":"chat","type":"text"},{"id":"video","type":"video"},{"id":"new-image","type":"image"},{"id":" ","type":"image"}]}`))}, nil
	})}
	cfg := &config.Config{}
	cfg.Image.Venice.APIKey = " Bearer image-key "
	if got := GetImageModelIDs("venice", cfg); !reflect.DeepEqual(got, []string{"new-edit", "new-image"}) {
		t.Fatalf("models = %v", got)
	}
	if got := GetProviderCompletions("venice:new", true, cfg); !reflect.DeepEqual(got, []string{"venice:new-edit", "venice:new-image"}) {
		t.Fatalf("completions = %v", got)
	}
	for kind, want := range map[string][]string{"image": {"new-image"}, "inpaint": {"new-edit"}} {
		if got := GetVeniceImageModelIDs(cfg, kind); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s models = %v, want %v", kind, got, want)
		}
	}
	cfg.Image.Venice.Model = "custom"
	if got := GetVeniceImageModelIDs(cfg, "image"); !reflect.DeepEqual(got, []string{"custom", "new-image"}) {
		t.Fatalf("configured model missing: %v", got)
	}
	if calls != 1 {
		t.Fatalf("fresh cache fetched again: %d calls", calls)
	}
	stored, err := cache.ReadModelCache(veniceImageCacheKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range stored.ModelInfos {
		if model.ImageConstraints == nil {
			t.Fatalf("discovery lost constraints for %s", model.ID)
		}
		if model.ID == "new-image" && len(model.ImageConstraints.Resolutions) != 2 {
			t.Fatalf("lost resolution tiers: %+v", model)
		}
		if model.ID == "new-edit" && len(model.ImageConstraints.Resolutions) != 0 {
			t.Fatalf("native edit model gained tiers: %+v", model)
		}
	}

	if _, err := cache.ReadModelCache(veniceCacheKey); !os.IsNotExist(err) {
		t.Fatalf("image discovery touched text cache: %v", err)
	}
}

func TestVeniceImageModelStaleRefresh(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
		err              error
	}{
		{name: "success", body: `{"data":[{"id":"fresh","type":"image"}]}`, status: 200, want: "fresh"},
		{name: "empty", body: `{"data":[]}`, status: 200, want: "stale"},
		{name: "invalid_json", body: `invalid`, status: 200, want: "stale"},
		{name: "unauthorized", status: 401, want: "stale"},
		{name: "server_error", status: 500, want: "stale"},
		{name: "transport_error", err: errors.New("offline"), want: "stale"},
		{name: "timeout", err: context.DeadlineExceeded, want: "stale"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("XDG_CACHE_HOME", home)
			t.Setenv("VENICE_API_KEY", "key")
			dir := filepath.Join(home, "term-llm")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, veniceImageCacheKey+"-models.json")
			if err := os.WriteFile(path, []byte(`{"models":["stale"],"model_infos":[{"id":"stale","type":"image"}],"fetched_at":"2000-01-01T00:00:00Z"}`), 0600); err != nil {
				t.Fatal(err)
			}
			old := defaultHTTPClient
			t.Cleanup(func() { defaultHTTPClient = old })
			calls := 0
			defaultHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if tc.err != nil {
					return nil, tc.err
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			// No background waits: the cache must be written before completion returns
			// so it survives the completion process exiting immediately afterwards.
			if got := GetImageModelIDs("venice", nil); !reflect.DeepEqual(got, []string{tc.want}) {
				t.Fatalf("models = %v, want %s", got, tc.want)
			}
			if calls != 1 {
				t.Fatalf("refresh requests = %d", calls)
			}
			stored, err := cache.ReadModelCache(veniceImageCacheKey)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(stored.Models, []string{tc.want}) {
				t.Fatalf("cached models = %v, want %s", stored.Models, tc.want)
			}
		})
	}
}

func TestVeniceImageModelDeferredCredentials(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("VENICE_API_KEY", "")
	marker := filepath.Join(t.TempDir(), "credential-executed")
	cfg := &config.Config{}
	cfg.Image.Venice.APIKey = "$(touch " + marker + "; exit 1)"
	cfg.Image.Venice.Model = "custom"
	for _, warm := range []bool{false, true} {
		if warm {
			if err := cache.WriteModelInfoCache(veniceImageCacheKey, []cache.CachedModel{{ID: "cached", Type: "image"}}); err != nil {
				t.Fatal(err)
			}
		}
		want := []string{"custom"}
		if warm {
			want = []string{"cached", "custom"}
		}
		if got := GetVeniceImageModelIDs(cfg, "image"); !reflect.DeepEqual(got, want) {
			t.Fatalf("models = %v, want %v", got, want)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("completion executed deferred credential: %v", err)
		}
	}
}

func TestVeniceImageModelConfiguredFallback(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("VENICE_API_KEY", "")
	cfg := &config.Config{}
	cfg.Image.Venice.Model = "custom"
	cfg.Image.Venice.EditModel = "custom-edit"
	if got := GetImageModelIDs("venice", cfg); !reflect.DeepEqual(got, []string{"custom", "custom-edit"}) {
		t.Fatalf("fallback = %v", got)
	}
	if got := GetImageModelIDs("venice", nil); len(got) != 0 {
		t.Fatalf("unexpected hardcoded models: %v", got)
	}
}
