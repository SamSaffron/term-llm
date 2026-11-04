package serveui

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Check the whole static graph: moving a workflow from app.js into an eagerly
// imported shared chunk must not masquerade as an initial-payload reduction.
func TestCommitAndReviewOperationsRemainLazy(t *testing.T) {
	body, err := StaticAsset("dist/asset-manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]struct {
		File           string   `json:"file"`
		Imports        []string `json:"imports"`
		IsEntry        bool     `json:"isEntry"`
		IsDynamicEntry bool     `json:"isDynamicEntry"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	eager := make(map[string]bool)
	var visit func(string)
	visit = func(key string) {
		if eager[key] {
			return
		}
		entry, ok := manifest[key]
		if !ok {
			t.Fatalf("manifest import %q is missing", key)
		}
		eager[key] = true
		for _, dependency := range entry.Imports {
			visit(dependency)
		}
	}
	for key, entry := range manifest {
		if entry.IsEntry {
			visit(key)
		}
	}
	if len(eager) == 0 {
		t.Fatal("manifest has no initial static graph")
	}
	for _, feature := range []struct {
		module  string
		markers []string
	}{
		{
			module:  "src/stores/commit-workflow.ts",
			markers: []string{"Running Git hooks and signing. This operation cannot be cancelled."},
		},
		{
			module: "src/stores/review-operations.ts",
			markers: []string{
				"This worktree diff was truncated; showing the available changes.",
				"Refresh the diff before sending these comments so their file snapshot can be anchored.",
				"GIT binary patch",
			},
		},
	} {
		t.Run(feature.module, func(t *testing.T) {
			entry, ok := manifest[feature.module]
			if !ok || !entry.IsDynamicEntry {
				t.Fatalf("%s must be an on-demand entry", feature.module)
			}
			if eager[feature.module] {
				t.Fatalf("%s is reachable through initial static imports", feature.module)
			}
			found := make(map[string]bool)
			for key, asset := range manifest {
				if !strings.HasSuffix(asset.File, ".js") {
					continue
				}
				code, err := StaticAsset("dist/" + asset.File)
				if err != nil {
					t.Fatal(err)
				}
				for _, marker := range feature.markers {
					if !bytes.Contains(code, []byte(marker)) {
						continue
					}
					found[marker] = true
					if eager[key] {
						t.Errorf("initial asset %s contains deferred workflow code %q", asset.File, marker)
					}
				}
			}
			for _, marker := range feature.markers {
				if !found[marker] {
					t.Errorf("build is missing deferred workflow code %q", marker)
				}
			}
		})
	}
}
