package proxyadminui

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestEmbeddedProxyAdminAssets(t *testing.T) {
	for _, name := range []string{"index.html", "proxy-admin.css", "proxy-admin.js"} {
		asset, err := Asset(name)
		if err != nil {
			t.Fatalf("Asset(%q): %v", name, err)
		}
		if len(asset) == 0 {
			t.Fatalf("Asset(%q) is empty", name)
		}
	}
	index, _ := Asset("index.html")
	for _, expected := range [][]byte{[]byte("page-overview"), []byte("page-clients"), []byte("page-models"), []byte("page-requests"), []byte("page-activity")} {
		if !bytes.Contains(index, expected) {
			t.Errorf("index does not contain %q", expected)
		}
	}
}

func TestProxyAdminJavaScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	cmd := exec.Command(node, filepath.Join("static", "proxy-admin_test.js"))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("proxy admin JavaScript tests: %v\n%s", err, output)
	}
}
