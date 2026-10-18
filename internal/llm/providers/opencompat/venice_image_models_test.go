package opencompat

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/samsaffron/term-llm/internal/cache"
	"github.com/samsaffron/term-llm/internal/config"
)

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
