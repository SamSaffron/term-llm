package extensions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func iconFixture(t *testing.T, dir, id, manifest string, files map[string]string) {
	t.Helper()
	files["extension.yaml"] = "title: " + id + "\nformat_version: 1\n" + manifest
	for name, body := range files {
		p := filepath.Join(dir, id, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const testSprite = `<svg xmlns="http://www.w3.org/2000/svg">
  <symbol id="send" viewBox="0 0 24 24"><path d="M5 12h14"/></symbol>
  <symbol id="copy" viewBox="0 0 16 16"><rect width="8" height="8"/></symbol>
</svg>`

func TestIconSpriteEntries(t *testing.T) {
	dir := t.TempDir()
	iconFixture(t, dir, "icons-only", "icons: icons.svg\n", map[string]string{"icons.svg": testSprite})
	iconFixture(t, dir, "themed", "css: style.css\nicons: art/icons.svg\n", map[string]string{"style.css": "x", "art/icons.svg": testSprite})
	invalid := map[string]struct{ manifest, sprite, want string }{
		"not-svg-path":  {"icons: icons.png\n", testSprite, "relative SVG path"},
		"escaping-path": {"icons: ../icons.svg\n", testSprite, "relative SVG path"},
		"missing":       {"icons: gone.svg\n", testSprite, "entry point gone.svg is missing"},
		"malformed":     {"icons: icons.svg\n", `<svg xmlns="http://www.w3.org/2000/svg"><symbol id="send">`, "not well-formed"},
		"wrong-root":    {"icons: icons.svg\n", `<html><symbol id="send"/></html>`, "must be an <svg"},
		"no-namespace":  {"icons: icons.svg\n", `<svg><symbol id="send"/></svg>`, "must be an <svg xmlns"},
		"empty":         {"icons: icons.svg\n", `<svg xmlns="http://www.w3.org/2000/svg"/>`, "no <symbol id>"},
		"bad-id":        {"icons: icons.svg\n", `<svg xmlns="http://www.w3.org/2000/svg"><symbol id="Send Now"/></svg>`, "lowercase letters"},
		"duplicate":     {"icons: icons.svg\n", `<svg xmlns="http://www.w3.org/2000/svg"><symbol id="send"/><symbol id="send"/></svg>`, "defined twice"},
		"too-big":       {"icons: icons.svg\n", `<svg>` + strings.Repeat(" ", MaxIconSpriteBytes) + `</svg>`, "exceeds"},
	}
	for id, tc := range invalid {
		iconFixture(t, dir, id, tc.manifest, map[string]string{"icons.svg": tc.sprite})
	}

	s := reloadStatus(t, dir, []string{"icons-only", "themed"})
	if len(s.Errors) != 0 {
		t.Fatalf("errors: %v", s.Errors)
	}
	if len(s.Entries) != 2+len(invalid) {
		t.Fatalf("entries: %+v", s.Entries)
	}
	for _, entry := range s.Entries {
		if tc, ok := invalid[entry.ID]; ok {
			if !strings.Contains(entry.Error, tc.want) || entry.IconSymbols != nil {
				t.Errorf("%s: error %q symbols %v, want error containing %q", entry.ID, entry.Error, entry.IconSymbols, tc.want)
			}
		} else if entry.Error != "" || strings.Join(entry.IconSymbols, " ") != "send copy" {
			t.Errorf("%s: %+v", entry.ID, entry)
		}
	}
}

func reloadStatus(t *testing.T, dir string, ids []string) Snapshot {
	t.Helper()
	m := NewManager()
	if err := m.Reload(dir, ids, "config", false, "r", "config.yaml"); err != nil {
		t.Fatal(err)
	}
	return m.Status()
}

func TestIconMetaFollowsEnabledOrderAndSkipsBrokenOrDisabled(t *testing.T) {
	dir := t.TempDir()
	iconFixture(t, dir, "first", "icons: icons.svg\n", map[string]string{"icons.svg": testSprite})
	iconFixture(t, dir, "second", "css: style.css\nicons: icons.svg\n", map[string]string{"style.css": "x", "icons.svg": `<svg xmlns="http://www.w3.org/2000/svg"><symbol id="send"/></svg>`})
	iconFixture(t, dir, "broken", "icons: icons.svg\n", map[string]string{"icons.svg": `<svg>`})
	iconFixture(t, dir, "plain", "css: style.css\n", map[string]string{"style.css": "x"})
	m := NewManager()
	if err := m.Reload(dir, []string{"second", "broken", "plain", "first"}, "config", false, "r", "config.yaml"); err != nil {
		t.Fatal(err)
	}
	g := m.Status().Generation
	want := `<meta name="term-llm-extension-icons" data-extension="second" data-symbols="send" content="extensions/` + g + `/second/icons.svg">` +
		`<meta name="term-llm-extension-icons" data-extension="first" data-symbols="send copy" content="extensions/` + g + `/first/icons.svg">`
	if got := m.IconMeta(); got != want {
		t.Fatalf("IconMeta:\n got %s\nwant %s", got, want)
	}
	if _, ok := m.Asset(g, "first/icons.svg"); !ok {
		t.Fatal("enabled sprite must be served from the snapshot")
	}
	if err := m.Reload(dir, []string{"first"}, "config", true, "r", "config.yaml"); err != nil {
		t.Fatal(err)
	}
	if got := m.IconMeta(); got != "" {
		t.Fatalf("kill switch must render no icons, got %s", got)
	}
}

func TestIconSpriteChangesGeneration(t *testing.T) {
	dir := t.TempDir()
	iconFixture(t, dir, "art", "icons: icons.svg\n", map[string]string{"icons.svg": testSprite})
	before := reloadStatus(t, dir, []string{"art"}).Generation
	iconFixture(t, dir, "art", "icons: icons.svg\n", map[string]string{"icons.svg": strings.Replace(testSprite, "M5 12h14", "M6 12h12", 1)})
	if after := reloadStatus(t, dir, []string{"art"}).Generation; after == before {
		t.Fatal("editing the sprite must change the generation")
	}
}
