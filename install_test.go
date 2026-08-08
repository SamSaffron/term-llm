package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallScriptResolvesLatestReleaseWithoutGitHubAPI(t *testing.T) {
	tempDir := t.TempDir()
	binDir := filepath.Join(tempDir, "bin")
	installDir := filepath.Join(tempDir, "install")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	writeExecutable := func(name, contents string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(contents), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	harnessPath := filepath.Join(binDir, "install-harness.sh")
	writeExecutable("install-harness.sh", `#!/bin/sh
set -eu
installer=$1
shift
curl() {
  case "$*" in
    *https://github.com/samsaffron/term-llm/releases/latest*)
      printf '%s\n' 'https://github.com/samsaffron/term-llm/releases/tag/v1.2.3'
      ;;
    *https://github.com/samsaffron/term-llm/releases/download/v1.2.3/*)
      while [ "$#" -gt 0 ]; do
        if [ "$1" = "-o" ]; then
          : > "$2"
          return 0
        fi
        shift
      done
      echo 'missing output path' >&2
      return 1
      ;;
    *)
      echo "unexpected curl request: $*" >&2
      return 22
      ;;
  esac
}
uname() {
  case "$1" in
    -s) printf 'Darwin\n' ;;
    -m) printf 'arm64\n' ;;
  esac
}
mktemp() {
  mkdir -p "$MOCK_TMPDIR"
  printf '%s\n' "$MOCK_TMPDIR"
}
tar() {
  while [ "$#" -gt 0 ]; do
    if [ "$1" = "-C" ]; then dest=$2; break; fi
    shift
  done
  cat > "$dest/term-llm" <<'EOF'
#!/bin/sh
echo 'term-llm v1.2.3'
EOF
  chmod +x "$dest/term-llm"
}
install() {
  src=$3
  dest=$4
  cat "$src" > "$dest"
  chmod 755 "$dest"
}
rm() { :; }
. "$installer"
`)

	cmd := exec.Command("sh", harnessPath, "install.sh")
	cmd.Env = append(os.Environ(),
		"TERM_LLM_INSTALL_DIR="+installDir,
		"MOCK_TMPDIR="+filepath.Join(tempDir, "extract"),
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "Downloading term-llm_1.2.3_") {
		t.Fatalf("installer did not resolve redirected release tag:\n%s", output)
	}
	if _, err := os.Stat(filepath.Join(installDir, "term-llm")); err != nil {
		t.Fatalf("installed binary missing: %v", err)
	}
}
